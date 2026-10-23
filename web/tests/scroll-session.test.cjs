const assert=require("node:assert/strict");
const fs=require("node:fs");const path=require("node:path");const vm=require("node:vm");const ts=require("typescript");const {test}=require("node:test");
const file=path.join(__dirname,"../src/lib/scroll-session.ts"),mod={exports:{}};
vm.runInThisContext(`(function(module,exports){${ts.transpileModule(fs.readFileSync(file,"utf8"),{compilerOptions:{module:ts.ModuleKind.CommonJS}}).outputText}\n})`)(mod,mod.exports);
const {ScrollSession}=mod.exports;
test("switching A to B or back to A invalidates old pagination scroll restoration",()=>{const s=new ScrollSession();s.select("A");const old=s.ticket();s.select("B");assert.equal(s.isCurrent(old),false);s.select("A");assert.equal(s.isCurrent(old),false);assert.equal(s.isCurrent(s.ticket()),true);});
test("layout and programmatic scrolls do not unpin or fetch older history",()=>{const s=new ScrollSession();s.select("A");assert.equal(s.tracksScroll(100),false);s.input(100);assert.equal(s.tracksScroll(110),true);assert.equal(s.tracksScroll(1101),false);s.input(2000);s.select("B");assert.equal(s.tracksScroll(2001),false);});
test("task activation refreshes loaded caches and scopes older-page rAF to selected epoch",()=>{const text=fs.readFileSync(path.join(__dirname,"../src/app/(main)/function/tasks/detail/_tabs/sessions-tab.tsx"),"utf8");assert.match(text,/if \(!approvalFocus.state \|\| !st/);assert.match(text,/isCurrent\(scrollTicket\)/);assert.match(text,/tracksScroll\(\)/);assert.match(text,/activeState.loading;/);});
