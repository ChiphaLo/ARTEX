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
const { coalesceThinking } = mod.exports;
const delta = (seq, id, text) => ({ seq, kind: "thinking", metadata: { stream_id: id, stream_delta: text } });

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
