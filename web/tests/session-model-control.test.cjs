const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const React = require("react");

const source = fs.readFileSync(path.join(__dirname, "../src/components/session-model-control.tsx"), "utf8");
const js = ts.transpileModule(source, {
  compilerOptions: { target: ts.ScriptTarget.ES2020, module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX },
}).outputText;
const mod = { exports: {} };
const req = (name) => {
  if (name.startsWith("@/components/") || name === "lucide-react") return new Proxy({}, { get: (_, key) => key });
  return require(name);
};
vm.runInThisContext(`(function(module,exports,require){${js}\n})`)(mod, mod.exports, req);
const { SessionModelControl } = mod.exports;
function all(node) {
  if (!React.isValidElement(node)) return [];
  return [node, ...React.Children.toArray(node.props.children).flatMap(all)];
}
const resolution = { profile_id: 1, name: "Shared Provider", model: "beta", source: "task_chain", available: true };

test("session shows the actual task model and opens the task editor on click", () => {
  let opened = 0;
  const tree = SessionModelControl({ resolution, onEdit: () => opened++ });
  const button = all(tree).find((n) => n.type === "button");
  assert.equal(button.props.type, "button");
  assert.equal(button.props["aria-label"], "切换任务模型：beta");
  assert.match(button.props.className, /pointer:coarse/);
  button.props.onClick();
  assert.equal(opened, 1);
});

test("an unavailable model is still editable; inherited history stays read-only", () => {
  const editable = SessionModelControl({ resolution: { ...resolution, available: false }, onEdit() {} });
  assert.equal(all(editable).find((n) => n.type === "button").props.disabled, undefined);
  const inherited = SessionModelControl({ resolution });
  assert.equal(all(inherited).filter((n) => n.type === "button").length, 0);
  assert.equal(all(inherited).find((n) => n.type === "Badge").props["aria-label"], "当前模型：beta");
});

test("explicit role bindings are identified instead of promising a task override", () => {
  const tree = SessionModelControl({ resolution: { ...resolution, source: "agent_binding" }, onEdit() {} });
  assert.ok(all(tree).some((n) => typeof n.props.children === "string" && n.props.children.includes("优先于任务选择")));
});
