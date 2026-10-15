const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const React = require("react");
const { renderToStaticMarkup } = require("react-dom/server");

const src = path.resolve(__dirname, "../src");
const cache = new Map();
// Render the real TSX components without adding a frontend test dependency.
function load(file) {
  if (cache.has(file)) return cache.get(file).exports;
  const mod = { exports: {} };
  cache.set(file, mod);
  const js = ts.transpileModule(fs.readFileSync(file, "utf8"), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, jsx: ts.JsxEmit.ReactJSX, esModuleInterop: true },
    fileName: file,
  }).outputText;
  const localRequire = (name) => {
    if (!name.startsWith("@/")) return require(name);
    const base = path.join(src, name.slice(2));
    return load([`${base}.tsx`, `${base}.ts`].find(fs.existsSync));
  };
  vm.runInThisContext(`(function(require,module,exports){${js}\n})`, { filename: file })(
    localRequire,
    mod,
    mod.exports,
  );
  return mod.exports;
}
const { Input } = load(path.join(src, "components/ui/input.tsx"));
const { Textarea } = load(path.join(src, "components/ui/textarea.tsx"));
const { SecretInput } = load(path.join(src, "components/ui/secret-input.tsx"));
const render = (Component, props = {}) => renderToStaticMarkup(React.createElement(Component, props));

test("service keys stay masked text controls, never account password inputs", () => {
  const html = render(SecretInput, { id: "p-api-key", defaultValue: "synthetic-key-only" });
  assert.match(html, /type="text"/);
  assert.match(html, /autoComplete="off"/);
  assert.match(html, /-webkit-text-security:disc/);
  assert.match(html, /name="artex-service-p-api-key"/);
  assert.match(html, /spellCheck="false"/);
  for (const flag of ["data-lpignore", "data-1p-ignore", "data-bwignore", "data-sensitive-input"]) {
    assert.match(html, new RegExp(`${flag}="true"`));
  }
  assert.doesNotMatch(html, /type="password"|current-password|new-password/);
});

test("ordinary fields opt out; account credentials explicitly retain autofill", () => {
  assert.match(render(Input), /type="text"/);
  assert.match(render(Input), /autoComplete="off"/);
  assert.match(render(Textarea), /autoComplete="off"/);
  assert.match(render(Input, { type: "number", min: 0 }), /type="number"/);
  for (const mode of ["username", "current-password", "new-password"]) {
    const html = render(Input, { type: mode === "username" ? "text" : "password", autoComplete: mode });
    assert.match(html, new RegExp(`autoComplete="${mode}"`));
    assert.doesNotMatch(html, /data-lpignore|data-1p-ignore|data-bwignore/);
  }
});

test("all native inputs and password declarations are audited", () => {
  let nativeFiles = 0;
  let passwordFields = 0;
  let secretFields = 0;
  const authFiles = new Set([
    "app/(auth)/login/page.tsx",
    "app/(auth)/setup/page.tsx",
    "app/(main)/_components/sidebar/change-password-dialog.tsx",
  ]);
  function walk(dir) {
    for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
      const file = path.join(dir, entry.name);
      if (entry.isDirectory()) {
        walk(file);
        continue;
      }
      if (!file.endsWith(".tsx")) continue;
      const relative = path.relative(src, file).split(path.sep).join("/");
      const tree = ts.createSourceFile(
        file,
        fs.readFileSync(file, "utf8"),
        ts.ScriptTarget.Latest,
        true,
        ts.ScriptKind.TSX,
      );
      const visit = (node) => {
        if (ts.isJsxSelfClosingElement(node) || ts.isJsxOpeningElement(node)) {
          const tag = node.tagName.getText(tree);
          const attrs = new Map(
            node.attributes.properties.filter(ts.isJsxAttribute).map((a) => [a.name.getText(tree), a.initializer]),
          );
          const type = attrs.get("type");
          if (tag === "SecretInput") secretFields++;
          if (type && ts.isStringLiteral(type) && type.text === "password") {
            passwordFields++;
            assert.ok(authFiles.has(relative), `non-auth password field: ${relative}`);
            const autocomplete = attrs.get("autoComplete");
            assert.ok(
              autocomplete &&
                ts.isStringLiteral(autocomplete) &&
                ["current-password", "new-password"].includes(autocomplete.text),
              relative,
            );
            assert.ok(attrs.has("name"), `unnamed auth field: ${relative}`);
          }
          if (tag === "input" && relative !== "components/ui/input.tsx") {
            nativeFiles++;
            assert.ok(type && ts.isStringLiteral(type), `untyped native input: ${relative}`);
            if (type.text !== "file") {
              assert.equal(relative, "app/(main)/chat/page.tsx");
              assert.equal(type.text, "text");
              assert.equal(attrs.get("name")?.text, "artex-conversation-title");
              assert.equal(attrs.get("autoComplete")?.text, "off");
            }
          }
          if (tag === "textarea") assert.equal(relative, "components/ui/textarea.tsx");
        }
        ts.forEachChild(node, visit);
      };
      visit(tree);
    }
  }
  walk(src);
  assert.equal(passwordFields, 6);
  assert.equal(secretFields, 5); // LLM, search x2, ScopeSentry, dynamic notification secret.
  assert.ok(nativeFiles >= 7);
});
