const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const react = { __esModule: true, useRef: value => ({current:value}), useState: value => [value,()=>{}], useEffect:()=>{}, useId:()=>"test-id" };
const jsx = (type,props)=>({type,props:props??{}});
function load(name) {
 const file=path.resolve(__dirname,"../src/components",name);
 const js=ts.transpileModule(fs.readFileSync(file,"utf8"),{compilerOptions:{module:ts.ModuleKind.CommonJS,jsx:ts.JsxEmit.ReactJSX,esModuleInterop:true},fileName:file}).outputText;
 const module={exports:{}};
 const req=name=>{
  if(name==="react")return react;
  if(name==="react/jsx-runtime")return {jsx,jsxs:jsx,Fragment:"fragment"};
  if(name==="@/lib/chat-mentions")return {mentionKinds:[],activeMention:()=>null,mentionSearch:()=>({categories:[]}),selectedMentions:()=>[]};
  if(name==="@/lib/utils")return {cn:(...a)=>a.filter(Boolean).join(" ")};
  return new Proxy({__esModule:true},{get:(o,k)=>k in o?o[k]:k});
 };
 vm.runInThisContext(`(function(require,module,exports){${js}\n})`,{filename:file})(req,module,module.exports);
 return module.exports;
}
function find(node,predicate) {
 if(!node||typeof node!=="object")return null;
 if(predicate(node))return node;
 for(const child of [node.props?.children].flat(Infinity)){const found=find(child,predicate);if(found)return found;}
 return null;
}
const {MentionTextarea}=load("mention-textarea.tsx");
const {ImagePicker}=load("chat-images.tsx");
const image={name:"screenshot.png",type:"image/png"};
function event(files){return {defaultPrevented:false,clipboardData:{files},dataTransfer:{files,types:["Files"]},preventDefault(){this.defaultPrevented=true;}};}
test("image picker snapshots selected files and permits choosing the same image again",()=>{
 let received;
 const tree=ImagePicker({onFiles:files=>{received=files;}});
 const input=find(tree,n=>n.type==="input");
 assert.match(input.props.accept,/image\/png/);
 assert.equal(input.props.multiple,true);
 const target={files:[image],value:"chosen"};input.props.onChange({target});
 assert.deepEqual(received,[image]);assert.equal(target.value,"");
});
test("pasted screenshots become uploads; ordinary text keeps native paste",()=>{
 let received=[];
 const tree=MentionTextarea({value:"",onValueChange:()=>{},onFiles:files=>received.push(files)});
 const input=find(tree,n=>typeof n.props.onPaste==="function");
 const paste=event([image]);input.props.onPaste(paste);
 assert.equal(paste.defaultPrevented,true);assert.deepEqual(received,[[image]]);
 const text=event([]);input.props.onPaste(text);assert.equal(text.defaultPrevented,false);assert.equal(received.length,1);
});
test("drag/drop uploads files but disabled composers do not consume them",()=>{
 for(const disabled of [false,true]){
  let count=0;
  const tree=MentionTextarea({value:"",onValueChange:()=>{},disabled,onFiles:()=>count++});
  const input=find(tree,n=>typeof n.props.onDrop==="function");const drop=event([image]);input.props.onDrop(drop);
  assert.equal(count,disabled?0:1);assert.equal(drop.defaultPrevented,!disabled);
 }
});
test("long composer input is preserved without a 4000-character maxlength",()=>{
 const value="中".repeat(12000);
 const tree=MentionTextarea({value,onValueChange:()=>{}});
 const input=find(tree,n=>typeof n.props.onPaste==="function");
 assert.equal(input.props.value,value);assert.equal(input.props.maxLength,undefined);
 for(const name of ["agent-inbox.tsx","agent-run-controls.tsx","side-question-workspace.tsx"]){
  const source=fs.readFileSync(path.resolve(__dirname,"../src/components",name),"utf8");assert.doesNotMatch(source,/4000/);
 }
});
