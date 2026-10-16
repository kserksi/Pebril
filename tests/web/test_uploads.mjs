import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
class Element {
 constructor(){this.children=[];this.listeners={};this.files=[];this.disabled=false;this.value='';this.textContent='';}
 addEventListener(name,callback){this.listeners[name]=callback;}
 append(...values){this.children.push(...values);}
 replaceChildren(...values){this.children=values;}
 setAttribute(name,value){this[name]=value;}
 removeAttribute(name){delete this[name];}
}
const elements=new Map();
const document={querySelector(selector){if(!elements.has(selector))elements.set(selector,new Element());return elements.get(selector);},createElement(){return new Element();}};
const saved=new Map([['pebril-upload-failures-1',JSON.stringify([{name:'owner-one-file'}])],['pebril-upload-failures-2',JSON.stringify([{name:'owner-two-file'}])]]);
let user={id:1,username:'fixture',used:0,quota:100};let clipboard='';const alerts=[];
class FormData {append(key,value){if(key==='file')this.file=value;}}
class XMLHttpRequest {
 constructor(){this.upload={};}
 open(){}
 setRequestHeader(){}
 send(body){this.upload.onprogress({lengthComputable:true,loaded:5,total:10});queueMicrotask(()=>{const svg=body.file.name.endsWith('.svg');this.status=svg?201:202;this.responseText=JSON.stringify(svg?{id:'1',state:'completed',url:'/image/1'}:{id:'2',state:'queued'});this.onload();});}
}
const timers=[];let currentImage={id:'2',state:'queued'};
const context=vm.createContext({alert:message=>alerts.push(message),setTimeout:callback=>timers.push(callback),document,FormData,XMLHttpRequest,URL,location:{href:'https://example.invalid/'},navigator:{clipboard:{async writeText(value){clipboard=value;}}},localStorage:{getItem:key=>saved.get(key),setItem:(key,value)=>saved.set(key,value)},fetch:async path=>({ok:true,json:async()=>path.includes('/uploads/')?currentImage:path.endsWith('/status')?{version:'test',config_revision:1,sqlite:true,native:{svg:true,gif:true}}:{initialized:true,user,csrf:'fixture'}})});
vm.runInContext(fs.readFileSync(new URL('../../internal/server/web/app.js',import.meta.url),'utf8'),context);
await new Promise(resolve=>setImmediate(resolve));
assert(document.querySelector('#total-progress').textContent.includes('owner-one-file'));
assert(!document.querySelector('#total-progress').textContent.includes('owner-two-file'));
document.querySelector('#files').files=[{name:'first.svg'},{name:'second.png'}];
const uploading=document.querySelector('#upload').listeners.click();
assert.equal(document.querySelector('#logout').disabled,true);
await uploading;
assert.equal(document.querySelector('#logout').disabled,false);
assert.match(document.querySelector('#total-progress').textContent,/Completed: 1\/2/);
const rows=document.querySelector('#upload-results').children;
assert.equal(rows[0].children[2].value,100);
assert.notEqual(rows[1].children[2].value,100);
await document.querySelector('#copy-markdown').listeners.click();
assert.equal(clipboard,'![](https://example.invalid/image/1)');
currentImage={id:'2',state:'retry_wait',error:'Image conversion failed',temporary:true,url:'/image/2'};timers.shift()();
await new Promise(resolve=>setImmediate(resolve));
assert.match(document.querySelector('#total-progress').textContent,/Failed: 1/);
assert.notEqual(rows[1].children[2].value,100);
await document.querySelector('#copy-urls').listeners.click();
assert.equal(clipboard,'https://example.invalid/image/1\nhttps://example.invalid/image/2');
currentImage={id:'2',state:'completed',url:'/image/2'};timers.shift()();
await new Promise(resolve=>setImmediate(resolve));
assert.equal(rows[1].children[2].value,100);
assert.match(document.querySelector('#total-progress').textContent,/Completed: 2\/2/);
user={id:2,username:'other-fixture',used:0,quota:100};
await context.loadAccount();
assert.equal(document.querySelector('#upload-results').children.length,0);
assert(document.querySelector('#total-progress').textContent.includes('owner-two-file'));
assert(!document.querySelector('#total-progress').textContent.includes('owner-one-file'));
console.log('Web logic passed: account-bound history, batch order, logout guard, no premature raster completion, and link copying.');

context.showLoginWarnings({user:{admin:false},resource_warnings:[{message:'must remain private'}]});
assert.equal(alerts.length,0);
context.showLoginWarnings({user:{admin:true},resource_warnings:[]});
assert.equal(alerts.length,0);
context.showLoginWarnings({user:{admin:true},resource_warnings:[{message:'Restricted startup'},{message:'Storage warning'}]});
assert.deepEqual(alerts,['Restricted startup\nStorage warning']);
await context.loadAccount();
assert.equal(alerts.length,1,'Account refresh must not repeat the login popup');
console.log('Administrator login warning summary passed; ordinary accounts and refreshes do not display it.');
