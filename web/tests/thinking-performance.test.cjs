const assert=require("node:assert/strict"),fs=require("node:fs"),path=require("node:path"),vm=require("node:vm"),ts=require("typescript");
const {test}=require("node:test");
const React=require("react"),{renderToString}=require("react-dom/server");
const cache=new Map();
function load(relative){
 const file=path.join(__dirname,"../src",relative);
 if(cache.has(file))return cache.get(file).exports;
 const mod={exports:{}};cache.set(file,mod);
 const js=ts.transpileModule(fs.readFileSync(file,"utf8"),{compilerOptions:{target:ts.ScriptTarget.ES2020,module:ts.ModuleKind.CommonJS,jsx:ts.JsxEmit.ReactJSX}}).outputText;
 const req=name=>name.startsWith("./")?load("lib/"+name.slice(2)+".ts"):name.startsWith("@/")?load(name.slice(2)+".ts"):require(name);
 vm.runInThisContext(`(function(module,exports,require){${js}\n})`,{filename:file})(mod,mod.exports,req);
 return mod.exports;
}
const {createActivityBatcher}=load("lib/activity-batcher.ts");
const {applySessionActivityBatch,emptySessionState}=load("lib/session-activity.ts");
const {textPage,TEXT_PAGE_SIZE}=load("lib/text-page.ts");
const {mergeActivities}=load("lib/activity-merge.ts");

test("frame bursts use one ordered commit, and disposal cannot leak into another task",()=>{
 let callback,commits=[],cancelled=0;
 const batch=createActivityBatcher(items=>commits.push(items),()=>100,fn=>{callback=fn;return 1},()=>cancelled++);
 for(let i=0;i<500;i++)batch.push(i);
 assert.equal(commits.length,0);callback();
 assert.equal(commits.length,1);assert.deepEqual(commits[0],Array.from({length:500},(_,i)=>i));
 batch.push("stale");const stale=callback;batch.dispose();stale();batch.push("late");
 assert.equal(commits.length,1);assert.ok(cancelled>0);
});

test("pending batches are bounded and hidden pages can use a slower interval",()=>{
 const calls=[];let interval;
 const batch=createActivityBatcher(a=>calls.push(a),()=>1000,(_fn,ms)=>{interval=ms;return 1},()=>{});
 for(let i=0;i<1001;i++)batch.push(i);
 assert.equal(interval,1000);assert.equal(calls.length,1);assert.equal(calls[0].length,1000);
 batch.flush();assert.equal(calls[1].length,1);
});

test("batched session reducer preserves raw rows, unread finals, accounting, trim and replays",()=>{
 const events=Array.from({length:40},(_,i)=>({seq:i+1,ts:String(i),kind:"thinking",metadata:{stream_id:"a",stream_delta:"x"}}));
 events.push({seq:41,ts:"done",kind:"thinking",metadata:{stream_id:"a",stream_done:true}},{seq:42,ts:"usage",kind:"usage"});
 const cold=applySessionActivityBatch(emptySessionState(),events,false,true,4000);
 const loaded=applySessionActivityBatch({...emptySessionState(),loaded:true},events,false,true,4000);
 assert.equal(cold.unread,1);assert.equal(loaded.unread,1);assert.equal(loaded.items.length,42);assert.equal(cold.items.length,1);
 assert.equal(applySessionActivityBatch(loaded,events,false,true,4000).unread,1);
 assert.equal(applySessionActivityBatch(loaded,events,true,true,4000).unread,0);
 const trimmed=applySessionActivityBatch({...emptySessionState(),loaded:true},events,true,true,10);
 assert.equal(trimmed.items.length,10);assert.equal(trimmed.hasMore,true);assert.equal(trimmed.earliestSeq,33);
 assert.equal(applySessionActivityBatch({...emptySessionState(),loaded:true},events,true,false,10).items.length,42);
});

test("merge fast path retains overlap/replacement ordering semantics",()=>{
 const a={seq:1},b={seq:2},replacement={seq:2,summary:"changed"};
 assert.deepEqual(mergeActivities([a],[b]),[a,b]);
 assert.deepEqual(mergeActivities([b],[a,replacement]),[a,replacement]);
 const input=[a];assert.equal(mergeActivities(input,[]),input);
});

test("paged long text is reconstructable with Unicode boundaries and bounded DOM",()=>{
 const text=("字😀long line\n").repeat(25000);
 const count=textPage(text,0,123).count;
 const pages=Array.from({length:count},(_,i)=>textPage(text,i,123).text);
 assert.equal(pages.join(""),text);
 assert.ok(pages.every(p=>p.length<=124));
 assert.equal(textPage("",999).text,"");
 assert.equal(textPage("short",-1).index,0);
 const {PagedText}=load("components/paged-text.tsx");
 const html=renderToString(React.createElement(PagedText,{text}));
 assert.ok(html.length<TEXT_PAGE_SIZE+6000,`oversized DOM payload ${html.length}`);
 assert.match(html,/长内容分段|上一段/);
});

test("following scrolls within the bounded text viewport, but manual reading pauses it",()=>{
 const {PagedText}=load("components/paged-text.tsx");
 const saved={useState:React.useState,useRef:React.useRef,useLayoutEffect:React.useLayoutEffect};
 let selected=null,effects=[];
 const pre={scrollTop:0,scrollHeight:1000,clientHeight:288};
 const all=node=>React.isValidElement(node)?[node,...React.Children.toArray(node.props.children).flatMap(all)]:[];
 try {
  React.useState=()=>[selected,value=>{selected=value}];
  React.useRef=()=>({current:pre});React.useLayoutEffect=callback=>effects.push(callback);
  const render=()=>{effects=[];const tree=PagedText.type({text:"x".repeat(20000),followTail:true});effects.forEach(fn=>fn());return tree};
  let tree=render();assert.equal(pre.scrollTop,1000);
  all(tree).find(n=>n.type==="pre").props.onWheel();
  pre.scrollTop=43;tree=render();assert.equal(pre.scrollTop,43);
  all(tree).find(n=>n.type==="button"&&n.props.children==="跟随末尾").props.onClick();
  render();assert.equal(pre.scrollTop,1000);
 } finally { Object.assign(React,saved); }
});
