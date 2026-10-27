const assert=require("node:assert/strict");
const fs=require("node:fs");
const path=require("node:path");
const vm=require("node:vm");
const ts=require("typescript");
const {test}=require("node:test");
function load(name){const file=path.join(__dirname,"../src/lib",name);const mod={exports:{}};const js=ts.transpileModule(fs.readFileSync(file,"utf8"),{compilerOptions:{module:ts.ModuleKind.CommonJS}}).outputText;vm.runInThisContext(`(function(module,exports){${js}\n})`)(mod,mod.exports);return mod.exports;}
const {modelProgressLabel}=load("model-progress.ts");
const {advanceActivityUnread}=load("activity-stream.ts");
const status=p=>({seq:1,kind:"model_status",metadata:{model_progress:{stage:"model",phase:"connecting",started_at:10000,...p}}});
test("distinguishes local queue, HTTP headers, first byte and receiving",()=>{
 assert.match(modelProgressLabel([status({phase:"rate_limit"})],20000),/本地限流/);
 assert.match(modelProgressLabel([status({})],20000),/响应头.*10s/);
 assert.match(modelProgressLabel([status({phase:"waiting_first_byte"})],20000),/首字节/);
 assert.match(modelProgressLabel([status({phase:"receiving",bytes_received:2048,last_data_at:18000})],20000),/2.0 KiB.*2s/);
});
test("old connecting event is a potentially stale status, not proof of an active request",()=>{
 assert.match(modelProgressLabel([status({})],8_015_000),/状态可能已失联/);
 assert.doesNotMatch(modelProgressLabel([status({})],8_015_000),/已等待 8005s/);
 assert.match(modelProgressLabel([status({})],140_000),/响应头/);
});
test("hidden compaction shows batch progress without becoming an assistant reply",()=>{
 assert.match(modelProgressLabel([status({stage:"compacting",phase:"receiving",chunk:2,groups_done:20,total_groups:50,bytes_received:4096,last_data_at:1000})],2000),/压缩上下文.*第 2 批.*20\/50.*4.0 KiB/);
 assert.match(modelProgressLabel([status({stage:"compacting",phase:"chunk_finished",groups_done:50,total_groups:50})],2000),/本批摘要完成/);
 const file=fs.readFileSync(path.join(__dirname,"../src/components/transcript.tsx"),"utf8");
 assert.match(file,/s.kind === "model_status"\) continue/);
});
test("status updates advance the cursor but never unread reply counts",()=>{
 assert.deepEqual(advanceActivityUnread({unread:2,unreadSeq:0},status({}),false),{unread:2,unreadSeq:1});
});
test("tool, new user, terminal and retries replace old request state",()=>{
 for(const kind of ["user","result","round"]){assert.equal(modelProgressLabel([status({}),{seq:2,kind}],20000),"处理中…");}
 assert.match(modelProgressLabel([status({}),{seq:2,kind:"tool_use",tool:"Read"}],20000),/工具：Read/);
 assert.equal(modelProgressLabel([status({}),{seq:2,kind:"retry",summary:"waiting 20s (1/5)"}],20000),"waiting 20s (1/5)");
});
