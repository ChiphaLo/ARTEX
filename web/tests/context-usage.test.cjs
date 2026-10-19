const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const React = require("react");
const { renderToString } = require("react-dom/server");

const cache = new Map();
function load(relative) {
  const base = path.join(__dirname, "../src", relative);
  const file = [base + ".ts", base + ".tsx"].find(fs.existsSync);
  if (cache.has(file)) return cache.get(file).exports;
  const mod = { exports: {} };
  cache.set(file, mod);
  const js = ts.transpileModule(fs.readFileSync(file, "utf8"), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
  }).outputText;
  const req = (name) => name.startsWith("@/") ? load(name.slice(2)) : require(name);
  vm.runInThisContext(`(function(module,exports,require){${js}\n})`, { filename: file })(mod, mod.exports, req);
  return mod.exports;
}
const { latestContextUsage, contextPercent, contextTokens } = load("lib/context-usage");
const usage = { used_tokens: 283000, window_tokens: 475000, input_budget: 450000, reserved_tokens: 25000, trimmed_messages: 10, estimated: true };
const event = (seq, u = usage, kind = "context_usage") => ({ seq, kind, metadata: { context_usage: u } });

test("uses the last request snapshot even with out-of-order history and cumulative bills", () => {
  assert.equal(latestContextUsage([event(4), { seq: 5, kind: "usage", input_tokens: 9000000 }, event(2, { ...usage, used_tokens: 10 })]).used_tokens, 283000);
  assert.equal(latestContextUsage([event(8, usage, "result")]).used_tokens, 283000);
  assert.equal(latestContextUsage([]), null);
});
test("provider switch invalidates old capacity; bad data does not show false zero", () => {
  assert.equal(latestContextUsage([event(1), { seq: 2, kind: "llm_switch" }]), null);
  assert.equal(latestContextUsage([event(1, { ...usage, window_tokens: 0 })]), null);
  assert.equal(latestContextUsage([event(1, { ...usage, used_tokens: NaN })]), null);
  assert.equal(contextPercent(usage), 60);
  assert.equal(contextTokens(283000), "283K");
  assert.equal(contextTokens(1048576), "1.05M");
});
test("indicator renders only an accessible circle while tooltip is closed", () => {
  const { ContextWindowIndicator } = load("components/context-window-indicator");
  const html = renderToString(React.createElement(ContextWindowIndicator, { activity: [event(1)] }));
  assert.match(html, /<button/);
  assert.match(html, /aria-label="上下文用量"/);
  assert.match(html, /stroke-dasharray="60 100"/);
  assert.doesNotMatch(html, /283K|475K|60%|非累计消耗/);
});
