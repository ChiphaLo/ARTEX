const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const React = require("react");

// Inspect the real controlled component and invoke its actual callbacks without
// opening a live task or requiring a browser/testing-library dependency.
const source = fs.readFileSync(path.join(__dirname, "../src/components/task-llm-profile-chain.tsx"), "utf8");
const js = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
}).outputText;
const mod = { exports: {} };
const req = (name) => {
  if (name === "react") return { ...React, useMemo: (fn) => fn(), useCallback: (fn) => fn };
  if (name.startsWith("@/components/") || name === "lucide-react") return new Proxy({}, { get: (_, key) => key });
  return require(name);
};
vm.runInThisContext(`(function(module,exports,require){${js}\n})`)(mod, mod.exports, req);
const { TaskLLMProfileChain } = mod.exports;
const profiles = [{ id: "1", name: "Shared", model: "alpha", models: ["alpha", "beta", "beta"] }];
function all(node) {
  if (!React.isValidElement(node)) return [];
  return [node, ...React.Children.toArray(node.props.children).flatMap(all)];
}
const selects = (tree) => all(tree).filter((e) => e.type === "Select");
const baseProps = { profiles, value: ["1"], onValueChange() {} };

test("each task has independent model controls, with inherited default and deduplicated models", () => {
  const a = TaskLLMProfileChain({ ...baseProps, models: { "1": "beta" }, onModelsChange() {} });
  const b = TaskLLMProfileChain({ ...baseProps, models: {}, onModelsChange() {} });
  assert.equal(selects(a)[0].props.value, "model:beta");
  assert.equal(selects(b)[0].props.value, "default");
  const items = all(a).filter((e) => e.type === "SelectItem").map((e) => e.props.value);
  assert.deepEqual(items, ["default", "model:alpha", "model:beta"]);
  assert.equal(profiles[0].model, "alpha");
});

test("selection/reset callbacks clone model pins without mutating another task", () => {
  const pins = { "1": "alpha", "2": "other" };
  let updated;
  const tree = TaskLLMProfileChain({ ...baseProps, models: pins, onModelsChange: (next) => { updated = next; } });
  const change = selects(tree)[0].props.onValueChange;
  change("model:beta");
  assert.deepEqual(updated, { "1": "beta", "2": "other" });
  assert.equal(pins["1"], "alpha");
  change("default");
  assert.deepEqual(updated, { "2": "other" });
});

test("removing Providers filters pins; reordering leaves their identity unchanged", () => {
  const props = { ...baseProps, value: ["1", "2"], models: { "1": "beta", "2": "other" } };
  let ids, pins;
  const tree = TaskLLMProfileChain({ ...props, onValueChange: (next) => { ids = next; }, onModelsChange: (next) => { pins = next; } });
  all(tree).find((e) => e.type === "Combobox").props.onValueChange(["2"]);
  assert.deepEqual(ids, ["2"]);
  assert.deepEqual(pins, { "2": "other" });
  const moved = all(tree).filter((e) => e.type === "Button" && e.props["aria-label"] === "下移配置")[0];
  moved.props.onClick();
  assert.deepEqual(ids, ["2", "1"]);
  assert.equal(props.models["1"], "beta");
});

test("a removed model remains visible but disabled so it can be repaired", () => {
  const tree = TaskLLMProfileChain({ ...baseProps, models: { "1": "removed" }, onModelsChange() {} });
  const item = all(tree).find((e) => e.type === "SelectItem" && e.props.value === "model:removed");
  assert.equal(item.props.disabled, true);
  assert.equal(selects(tree)[0].props.value, "model:removed");
});
