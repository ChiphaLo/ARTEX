const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const { webcrypto } = require("node:crypto");

function moduleIn(file, globals, requireFn = require) {
  const mod = { exports: {} };
  const source = fs.readFileSync(path.join(__dirname, "../src", file), "utf8");
  const js = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX } }).outputText;
  vm.runInNewContext(`(function(module,exports,require){${js}\n})(module,module.exports,require)`, { ...globals, module: mod, require: requireFn });
  return mod.exports;
}

test("HTTPS uses native UUID with the correct receiver", () => {
  const crypto = { randomUUID() { assert.equal(this, crypto); return "native-uuid"; } };
  const { newRequestID } = moduleIn("lib/request-id.ts", { crypto });
  assert.equal(newRequestID(), "native-uuid");
});

test("HTTP without randomUUID uses random bytes and produces unique v4 UUIDs", () => {
  const crypto = { getRandomValues: (bytes) => webcrypto.getRandomValues(bytes) };
  const { newRequestID } = moduleIn("lib/request-id.ts", { crypto });
  const ids = Array.from({ length: 1000 }, newRequestID);
  assert.equal(new Set(ids).size, 1000);
  for (const id of ids) assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
});

test("no Crypto API still sends distinct IDs with frozen time and randomness", () => {
  const { newRequestID } = moduleIn("lib/request-id.ts", { crypto: undefined, Date: { now: () => 1 }, Math: { random: () => 0.5 } });
  assert.notEqual(newRequestID(), newRequestID());
});

test("actual inbox hook preserves retry ID over HTTP, changes it for edits and clears", () => {
  const crypto = { getRandomValues: (bytes) => webcrypto.getRandomValues(bytes) };
  const helper = moduleIn("lib/request-id.ts", { crypto });
  const ref = { current: { payload: "", id: "" } };
  const { useInboxRequestID } = moduleIn("components/agent-inbox.tsx", { crypto }, (name) => {
    if (name === "react") return { useRef: () => ref };
    if (name === "@/lib/request-id") return helper;
    return {};
  });
  const hook = useInboxRequestID();
  const payload = { message: "diagnostic draft", mode: "queue", scope: "main:1" };
  const first = hook.getID(payload);
  assert.equal(hook.getID({ ...payload }), first);
  assert.notEqual(hook.getID({ ...payload, mode: "steer" }), first);
  const second = hook.getID({ ...payload, message: "edited draft" });
  assert.notEqual(second, first);
  hook.clear();
  assert.notEqual(hook.getID({ ...payload, message: "edited draft" }), second);
});
