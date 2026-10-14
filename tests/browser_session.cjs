const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

// 运行服务真正使用的页面脚本，用虚拟时钟覆盖风险会话的多阶段初始化。
const source = fs.readFileSync(process.argv[2] || 'browser.go', 'utf8');
const script = source.match(/const browserSessionScript = `([\s\S]*?)`\r?\n/)[1];
const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url');
const initial = encode({sid:'测试会话编号'});
const complete = phase => encode({sid:'测试会话编号',ftr:'测试验证数据'+phase,ark:'测试验证数据',bfp:'测试验证数据'});

async function run(events) {
 let now = 100000, sequence = 0, latest = null, settled = false, result, error;
 const tasks = new Map(), observers = new Set();
 const schedule = (callback, delay) => {const id=++sequence;tasks.set(id,{at:now+delay,callback});return id;};
 for(const [offset,value] of events)schedule(()=>{latest=value;for(const observer of observers)observer(value);},offset);
 const immediate = value => callback => {callback(value);return ()=>{};};
 const auth = {accessToken:immediate('测试访问令牌'),userProfile:immediate({userId:'测试用户'}),arpSessionToken:callback=>{observers.add(callback);callback(latest);return ()=>observers.delete(callback);}};
 const context = {Date:class extends Date {static now(){return now;}},setTimeout:schedule,clearTimeout:id=>tasks.delete(id),queueMicrotask,atob,window:{adobeIMS:{refreshToken:async()=>{},getAccessToken:()=>({expire:200000})}},document:{querySelector:()=>({__reactFiberFixture:{memoizedProps:{service:{host:{authentication:auth}}}}})}};
 Promise.resolve(vm.runInNewContext(script,context)).then(value=>{result=value;settled=true;},failure=>{error=failure;settled=true;});
 while(!settled&&now<180000){
  await new Promise(resolve=>setImmediate(resolve));
  if(settled)break;
  const next=[...tasks].sort((a,b)=>a[1].at-b[1].at||a[0]-b[0])[0];
  if(!next)throw Error('等待逻辑没有后续任务');
  tasks.delete(next[0]);now=next[1].at;next[1].callback();
 }
 await new Promise(resolve=>setImmediate(resolve));
 return {result,error,elapsed:now-100000,remainingObservers:observers.size};
}

(async()=>{
 const partial=await run([[0,initial]]);
 assert.match(partial.error?.message || '',/尚未完成初始化/);
 assert.equal(partial.result,undefined);
 assert.equal(partial.remainingObservers,0);
 // 使用浏览器同样严格的解码行为，拒绝异常格式和缺少有效供应商数据的值。
 const invalidCases = [
  ['非法编码','!'+complete(9)],
  ['非 JSON',Buffer.from('异常正文').toString('base64url')],
  ['空对象',encode({})],
  ['数组',encode([{sid:'测试会话编号',ftr:'测试验证数据'}])],
  ['空值',encode(null)],
  ['非字符串',123],
  ['缺少会话编号',encode({ftr:'测试验证数据'})],
  ['空会话编号',encode({sid:'',ftr:'测试验证数据'})],
  ['空白会话编号',encode({sid:'  ',ftr:'测试验证数据'})],
  ['会话编号类型错误',encode({sid:123,ftr:'测试验证数据'})],
  ['未知供应商',encode({sid:'测试会话编号',unknown:'测试验证数据'})],
  ['空供应商数据',encode({sid:'测试会话编号',ftr:'  ',ark:{},bfp:null,fpjs:[]})],
  ['供应商类型错误',encode({sid:'测试会话编号',ftr:123,ark:true})]
 ];
 for(const [name,value] of invalidCases){
  const invalid=await run([[0,value]]);
  assert.match(invalid.error?.message || '',/尚未完成初始化/,name);
  assert.equal(invalid.result,undefined,name);
  assert.equal(invalid.remainingObservers,0,name);
 }
 // 官方供应商允许对象格式，也允许仅部分供应商已产生凭据。
 for(const value of [encode({sid:'测试会话编号',fpjs:{requestId:'测试请求编号'}}),encode({sid:'测试会话编号',bfp:'测试验证数据'})]){
  const valid=await run([[0,value]]);
  assert.equal(valid.error,undefined);
  assert.equal(valid.result.arp_session,value);
  assert.ok(valid.elapsed>=20000);
  assert.equal(valid.remainingObservers,0);
 }
 const periodic=await run([[0,initial],[1000,complete(1)],[5000,complete(2)],[10000,complete(3)],[15000,complete(4)],[20000,complete(5)],[25000,complete(6)]]);
 assert.equal(periodic.error,undefined);
 assert.equal(periodic.result.arp_session,complete(5));
 assert.ok(periodic.elapsed>=21500&&periodic.elapsed<25000);
 assert.equal(periodic.remainingObservers,0);
 const late=await run([[0,initial],[19750,complete(7)]]);
 assert.equal(late.result.arp_session,complete(7));
 assert.ok(late.elapsed>=21250);
 const existing=await run([[0,complete(8)]]);
 assert.equal(existing.result.arp_session,complete(8));
 assert.ok(existing.elapsed>=20000);
 console.log('浏览器会话初始化测试通过：异常格式拒绝、合法供应商对象、部分凭据、周期更新与稳定窗口');
})().catch(error=>{console.error(error.message);process.exitCode=1;});
