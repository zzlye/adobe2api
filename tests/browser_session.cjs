const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

// 运行服务真正使用的页面脚本，用虚拟时钟覆盖风险会话的多阶段初始化。
const source = fs.readFileSync(process.argv[2] || 'browser.go', 'utf8');
const script = source.match(/const browserSessionScript = `([\s\S]*?)`\r?\n/)[1];
const encode = value => Buffer.from(JSON.stringify(value)).toString('base64url');
const initial = encode({sid:'测试会话编号'});
const complete = phase => encode({sid:'测试会话编号',ftr:'测试验证数据'+phase,ark:'测试验证数据',bfp:'测试验证数据'});

async function run(events, componentDelay = 0, refreshDelay = 0, options = {}) {
 let now = 100000, sequence = 0, settled = false, result, error;
 const tasks = new Map(), observers = {token:new Set(),profile:new Set(),arp:new Set()};
 const subscriptions = {token:0,profile:0,arp:0};
 const schedule = (callback, delay) => {const id=++sequence;tasks.set(id,{at:now+delay,callback});return id;};
 // 三类订阅都保留真实的首值与后续更新，覆盖空首值、延迟发值和退出清理。
 const observable = (kind, initialValue, changes, emitInitially = true) => {
  let latest = initialValue;
  for(const [offset,value] of changes)schedule(()=>{latest=value;for(const observer of observers[kind])observer(value);},offset);
  return callback=>{subscriptions[kind]++;observers[kind].add(callback);if(emitInitially)callback(latest);return ()=>observers[kind].delete(callback);};
 };
 const auth = {
  accessToken:observable('token',Object.hasOwn(options,'initialToken')?options.initialToken:'测试访问令牌',options.tokenEvents||[],options.tokenEmitsInitially!==false),
  userProfile:observable('profile',Object.hasOwn(options,'initialProfile')?options.initialProfile:{userId:'测试用户'},options.profileEvents||[],options.profileEmitsInitially!==false),
  arpSessionToken:observable('arp',null,events)
 };
 const context = {Date:class extends Date {static now(){return now;}},setTimeout:schedule,clearTimeout:id=>tasks.delete(id),queueMicrotask,atob,window:{adobeIMS:{refreshToken:async()=>{if(refreshDelay)await new Promise(resolve=>schedule(resolve,refreshDelay))},getAccessToken:()=>({expire:200000})}},document:{querySelector:()=>now-100000<componentDelay?null:({__reactFiberFixture:{memoizedProps:{service:{host:{authentication:auth}}}}})}};
 Promise.resolve(vm.runInNewContext(script,context)).then(value=>{result=value;settled=true;},failure=>{error=failure;settled=true;});
 while(!settled&&now<300000){
  await new Promise(resolve=>setImmediate(resolve));
  if(settled)break;
  const next=[...tasks].sort((a,b)=>a[1].at-b[1].at||a[0]-b[0])[0];
  if(!next)throw Error('等待逻辑没有后续任务');
  tasks.delete(next[0]);now=next[1].at;next[1].callback();
 }
 await new Promise(resolve=>setImmediate(resolve));
 const remainingByType=Object.fromEntries(Object.entries(observers).map(([kind,list])=>[kind,list.size]));
 return {result,error,elapsed:now-100000,remainingObservers:Object.values(remainingByType).reduce((sum,count)=>sum+count,0),remainingByType,subscriptions};
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
 // 与后端保持一致，单个供应商或对象格式都不能提前结束采集。
 for(const value of [encode({sid:'测试会话编号',fpjs:{requestId:'测试请求编号'}}),encode({sid:'测试会话编号',bfp:'测试验证数据'})]){
  const invalid=await run([[0,value]]);
  assert.match(invalid.error?.message || '',/尚未完成初始化/);
  assert.equal(invalid.result,undefined);
  assert.equal(invalid.remainingObservers,0);
 }
 // 中间态即使稳定二十秒也须继续等齐所有供应商。
 const staged=await run([[0,encode({sid:'测试会话编号',bfp:'测试验证数据'})],[35000,complete(10)]]);
 assert.equal(staged.result.arp_session,complete(10));
 assert.ok(staged.elapsed>=36500);
 // 不透明供应商标记原样传递，不在页面端发明与官方不同的失效语义。
 for(const ftr of ['fixture___','fixture__UDF9_13ck_tt','fixture__tt']){
  const value=encode({sid:'测试会话编号',ftr,ark:'ark',bfp:'bfp'});
  const opaque=await run([[0,value]]);
  assert.equal(opaque.result.arp_session,value);
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
 // 慢页面不能消耗后续会话采集的等待窗口。
 const slowPage=await run([[0,complete(8)]],45000);
 assert.equal(slowPage.error,undefined);
 assert.equal(slowPage.result.arp_session,complete(8));
 assert.ok(slowPage.elapsed>=65000);
 // SDK 续签较慢时，风险采集仍必须拥有独立等待窗口。
 const slowRefresh=await run([[0,complete(11)]],0,50000);
 assert.equal(slowRefresh.error,undefined,'SDK 续签耗时挤占了风险采集窗口');
 assert.equal(slowRefresh.result.arp_session,complete(11));
 assert.ok(slowRefresh.elapsed>=70000);
 // 首次发出空令牌或空用户时继续等有效值，不能重启二十秒风险采集窗口。
 for(const delay of [500,21000]){
  const delayedToken=await run([[0,complete(12)]],0,0,{initialToken:null,tokenEvents:[[delay,'延迟令牌']]});
  assert.equal(delayedToken.error,undefined,`令牌在 ${delay} 毫秒就绪后仍然超时`);
  assert.equal(delayedToken.result?.access_token,'延迟令牌');
  assert.ok(delayedToken.elapsed>=Math.max(delay,20000)&&delayedToken.elapsed<25000,'空首值额外重启了风险等待窗口');
  assert.equal(delayedToken.subscriptions.arp,1,'单次同步重复采集风险会话');
  assert.equal(delayedToken.remainingObservers,0);
 }
 const delayedProfile=await run([[0,complete(13)]],0,0,{initialProfile:null,profileEvents:[[21000,{userId:'延迟用户'}]]});
 assert.equal(delayedProfile.error,undefined,'用户信息在有效窗口内就绪后仍然超时');
 assert.equal(delayedProfile.result?.profile.userId,'延迟用户');
 assert.ok(delayedProfile.elapsed>=21000&&delayedProfile.elapsed<25000);
 assert.equal(delayedProfile.subscriptions.arp,1);
 assert.equal(delayedProfile.remainingObservers,0);
 // 第一次回调超过十秒但仍在完整采集窗口内，不能提前放弃并遗留订阅。
 const firstTokenLate=await run([[0,complete(14)]],0,0,{tokenEmitsInitially:false,tokenEvents:[[12000,'首次延迟令牌']]});
 assert.equal(firstTokenLate.error,undefined,'首次发值被旧十秒超时提前终止');
 assert.equal(firstTokenLate.result?.access_token,'首次延迟令牌');
 assert.ok(firstTokenLate.elapsed>=20000&&firstTokenLate.elapsed<25000);
 assert.equal(firstTokenLate.remainingObservers,0);
 // 风险采集期间令牌会更新，最终返回同一时刻的最新凭据，而不是早先的首值。
 const rotatedToken=await run([[0,complete(16)]],0,0,{initialToken:'旧令牌',tokenEvents:[[1000,'最新令牌']]});
 assert.equal(rotatedToken.error,undefined);
 assert.equal(rotatedToken.result?.access_token,'最新令牌','风险采集完成后仍返回二十秒前的旧令牌');
 assert.ok(rotatedToken.elapsed>=20000&&rotatedToken.elapsed<25000);
 assert.equal(rotatedToken.subscriptions.arp,1);
 assert.equal(rotatedToken.remainingObservers,0);
 // 某个源永不发值时仍须有界退出，其他源和风险采集也必须统一退订。
 for(const options of [{tokenEmitsInitially:false},{profileEmitsInitially:false}]){
  const silent=await run([[0,complete(15)]],0,0,options);
  assert.ok(silent.error,'无有效会话的订阅没有超时退出');
  assert.equal(silent.result,undefined);
  assert.ok(silent.elapsed>=60000&&silent.elapsed<65000,'没有使用完整且有界的会话采集窗口');
  assert.equal(silent.remainingObservers,0,'超时后仍有订阅留在页面中');
 }
 console.log('浏览器会话初始化测试通过：完整凭据、独立采集窗口、延迟首值、单次风险采集与订阅清理');
})().catch(error=>{console.error(error.message);process.exitCode=1;});
