const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");

const file = path.join(__dirname, "../src/lib/activity-stream.ts");
const mod = { exports: {} };
const js = ts.transpileModule(fs.readFileSync(file, "utf8"), {
  compilerOptions: { module: ts.ModuleKind.CommonJS },
}).outputText;
vm.runInThisContext(`(function(module,exports){${js}\n})`, { filename: file })(mod, mod.exports);
const { coalesceThinking, advanceActivityUnread, ThinkingProjection } = mod.exports;
const delta = (seq, id, text) => ({ seq, kind: "thinking", metadata: { stream_id: id, stream_delta: text } });

test("context usage updates cursor but is not an unread response", () => {
  const state = advanceActivityUnread({ unread: 2, unreadSeq: 1 }, { seq: 3, kind: "context_usage" }, false);
  assert.equal(state.unread, 2);
  assert.equal(state.unreadSeq, 3);
});

test("thinking grows across multiline deltas without mutating raw history", () => {
  const rows = [delta(1, "a", "first line\n"), delta(2, "a", "second line")];
  const merged = coalesceThinking(rows);
  assert.equal(merged.length, 1);
  assert.equal(merged[0].detail, "first line\nsecond line");
  assert.match(merged[0].summary, /second line/);
  assert.equal(rows[0].detail, undefined);
});

test("final thinking without detail discards incomplete paginated deltas for lazy loading", () => {
  const rows = [
    delta(2, "a", "only tail"),
    { seq: 3, kind: "thinking", metadata: { stream_id: "a", stream_done: true } },
  ];
  const [final] = coalesceThinking(rows);
  assert.equal(final.seq, 3);
  assert.equal(final.detail, undefined);
  assert.equal(final.metadata.stream_done, true);
});

test("complete final is authoritative and independent streams stay distinct", () => {
  const rows = [
    delta(1, "a", "part"),
    delta(2, "b", "other"),
    { seq: 3, kind: "thinking", detail: "full final", metadata: { stream_id: "a", stream_done: true } },
  ];
  const merged = coalesceThinking(rows);
  assert.equal(merged.length, 2);
  assert.equal(merged[0].detail, "full final");
  assert.equal(merged[1].detail, "other");
});

test("many thinking lines and usage frames count once at thought completion, not once per frame", () => {
  let state = { unread: 0, unreadSeq: 0 };
  const rows = Array.from({ length: 28 }, (_, i) => delta(i + 1, "thought", `line ${i}\n`));
  for (const row of rows) state = advanceActivityUnread(state, row, false);
  assert.equal(state.unread, 0);
  state = advanceActivityUnread(state, { seq: 29, kind: "usage" }, false);
  assert.equal(state.unread, 0);
  const final = { seq: 30, kind: "thinking", metadata: { stream_id: "thought", stream_done: true } };
  state = advanceActivityUnread(state, final, false);
  assert.equal(state.unread, 1);
  state = advanceActivityUnread(state, final, false);
  assert.equal(state.unread, 1, "SSE replay counted the same final row twice");
  state = advanceActivityUnread(state, rows[0], false);
  assert.equal(state.unread, 1, "old replay inflated the badge");
  state = advanceActivityUnread(state, { seq: 31, kind: "result" }, false);
  assert.equal(state.unread, 2);
  assert.equal(coalesceThinking([...rows, final]).length, 1);
});

test("unread accounting is identical for cold and loaded sessions and isolated by session", () => {
  let cold = { unread: 0, unreadSeq: 0 };
  let loaded = { unread: 0, unreadSeq: 0, items: [] };
  let other = { unread: 0, unreadSeq: 0 };
  const events = [
    delta(1, "a", "one"),
    delta(3, "a", "two"),
    { seq: 4, kind: "thinking", metadata: { stream_id: "a", stream_done: true } },
    { seq: 5, kind: "usage" },
    { seq: 6, kind: "text" },
  ];
  for (const event of events) {
    cold = advanceActivityUnread(cold, event, false);
    loaded = { ...loaded, ...advanceActivityUnread(loaded, event, false), items: [...loaded.items, event] };
  }
  other = advanceActivityUnread(other, { seq: 2, kind: "text" }, false);
  assert.equal(cold.unread, 2);
  assert.equal(loaded.unread, cold.unread);
  assert.equal(other.unread, 1);
});

test("active view stays read; legacy thinking still counts once; excluded records never notify", () => {
  const initial = { unread: 4, unreadSeq: 10 };
  const active = advanceActivityUnread(initial, { seq: 11, kind: "text" }, true);
  assert.deepEqual(initial, { unread: 4, unreadSeq: 10 }, "React state updater input mutated");
  assert.equal(active.unread, 0);
  assert.equal(advanceActivityUnread(active, { seq: 11, kind: "text" }, false).unread, 0);
  const legacy = advanceActivityUnread(active, { seq: 12, kind: "thinking" }, false);
  assert.equal(legacy.unread, 1);
  const hidden = advanceActivityUnread(
    legacy,
    { seq: 13, kind: "thinking", metadata: { context_excluded: true } },
    false,
  );
  assert.equal(hidden.unread, 1);
});

test("live session dispatcher uses filtered accounting in both cache branches", () => {
  const page = fs.readFileSync(
    path.join(__dirname, "../src/app/(main)/function/tasks/detail/_tabs/sessions-tab.tsx"),
    "utf8",
  );
  assert.match(page, /createActivityBatcher<Activity>/);
  assert.match(page, /applySessionActivityBatch\(/);
  assert.match(page, /disposeBatch\(\)/);
  assert.doesNotMatch(page, /cur\.unread\s*\+\s*1/, "per-frame counters reintroduced");
});

test("append projection reuses completed rows without mutating published snapshots", () => {
  const projection = new ThinkingProjection();
  const first = Object.freeze({ ...delta(1, "a", "first"), summary: "first" });
  const history = [first, { seq: 2, kind: "result", summary: "done" }];
  const before = projection.project(history);
  const after = projection.project([...history, delta(3, "a", " second")]);
  assert.equal(before[0].detail, "first");
  assert.equal(after[0].detail, "first second");
  assert.equal(after[1], before[1]);
  assert.notEqual(after[0], before[0]);
  assert.equal(projection.project(history)[0].detail, "first", "abandoned concurrent render must be reversible");
});

test("prepend, replacement and trimmed pages rebuild exactly; final detail remains authoritative", () => {
  const projection = new ThinkingProjection();
  const a = delta(2, "a", "middle"), b = delta(3, "a", "tail");
  projection.project([a, b]);
  assert.equal(projection.project([delta(1,"a","head"),a,b])[0].detail,"headmiddletail");
  assert.equal(projection.project([a,{...b, metadata:{stream_id:"a",stream_delta:"replacement"}}])[0].detail,"middlereplacement");
  assert.equal(projection.project([b])[0].detail,"tail");
  const final={seq:4,kind:"thinking",summary:"finished",metadata:{stream_id:"a",stream_done:true}};
  const rows=projection.project([b,final]);
  assert.equal(rows[0].detail,undefined);
  assert.equal(rows[0].seq,4);
});

test("summary normalisation scans only new chunks and a bounded tail, not all prior text", () => {
  const rows=Array.from({length:3000},(_,i)=>({...delta(i+1,"big",("synthetic line "+i+"\n").repeat(20)),summary:"start"}));
  let scanned=0;
  const original=String.prototype.replace;
  String.prototype.replace=function(...args){ scanned+=this.length;return original.apply(this,args); };
  let merged;
  try { merged=coalesceThinking(rows); } finally { String.prototype.replace=original; }
  const full=rows.map(r=>r.metadata.stream_delta).join("");
  assert.equal(merged[0].detail,full);
  assert.equal(merged[0].summary,full.replace(/\s+/g," ").slice(-200));
  assert.ok(scanned<=full.length+rows.length*200,`quadratic text scanning returned: ${scanned}`);
});
