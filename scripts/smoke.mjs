#!/usr/bin/env node
// Real Nakama + real Kubernetes/Agones + a UDP protocol fixture. Node >=22.
import assert from 'node:assert/strict';
import { readFileSync, writeFileSync } from 'node:fs';
import { randomUUID } from 'node:crypto';
import dgram from 'node:dgram';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';

const root=fileURLToPath(new URL('../',import.meta.url));
const config=JSON.parse(readFileSync(root+'.local/client.json','utf8'));
const base=config.base_url;
assert.equal(base,'http://127.0.0.1:17850','Smoke only targets the isolated loopback stack');
const sockets=[], games=[], checks=[];
const sleep=ms=>new Promise(r=>setTimeout(r,ms));
const kube=async(...args)=>{
  const {stdout}=await promisify(execFile)('kubectl',['--kubeconfig',root+'.local/kubeconfig','--context','k3d-agones-nakama',...args],{timeout:90000});
  return stdout;
};
async function request(path,{body,token,headers={}}={}){
  if(token)headers.Authorization='Bearer '+token;
  if(body!==undefined)headers['Content-Type']='application/json';
  const res=await fetch(base+path,{method:body===undefined?'GET':'POST',body:body===undefined?undefined:JSON.stringify(body),headers,signal:AbortSignal.timeout(5000)});
  if(!res.ok){const error=new Error(`HTTP ${res.status} ${path}`);error.status=res.status;throw error;}
  return res.json();
}
const admin=(name,body)=>request('/agones/fleet/v1/admin/'+name,{body,token:config.admin_token});
async function wait(label,fn,timeout=150000){
  const until=Date.now()+timeout;
  while(Date.now()<until){
    try{const result=await fn();if(result)return result;}
    catch(e){if(![404,409,503].includes(e.status)&&!['TypeError','TimeoutError'].includes(e.name))throw e;}
    await sleep(500);
  }
  throw new Error('Timed out: '+label);
}
function pass(name){checks.push(name);console.log('PASS: '+name);}
async function player(){
  const session=await request('/v2/account/authenticate/device?create=true',{body:{id:'agones-smoke-'+randomUUID()},headers:{Authorization:'Basic '+Buffer.from(config.server_key+':').toString('base64')}});
  const ws=new WebSocket(base.replace('http:','ws:')+'/ws?lang=en&status=true&format=json&token='+encodeURIComponent(session.token));
  sockets.push(ws);const messages=[];
  ws.addEventListener('message',e=>messages.push(JSON.parse(e.data)));
  await Promise.race([new Promise((resolve,reject)=>{ws.addEventListener('open',resolve,{once:true});ws.addEventListener('error',()=>reject(new Error('Websocket connect failed')),{once:true});}),sleep(10000).then(()=>{throw new Error('Websocket connect timeout');})]);
  return {token:session.token,refreshToken:session.refresh_token,ws,messages};
}
async function rpc(p,name,body={}){
  let result;
  try { result=await request('/v2/rpc/agones_fleet_'+name+'_v1',{body:JSON.stringify(body),token:p.token}); }
  catch(e) {
    if(e.status!==401)throw e;
    const session=await request('/v2/account/session/refresh',{body:{token:p.refreshToken},headers:{Authorization:'Basic '+Buffer.from(config.server_key+':').toString('base64')}});
    p.token=session.token;p.refreshToken=session.refresh_token||p.refreshToken;
    result=await request('/v2/rpc/agones_fleet_'+name+'_v1',{body:JSON.stringify(body),token:p.token});
  }
  return JSON.parse(result.payload);
}
async function match(){
  const players=await Promise.all([player(),player()]);
  const group=randomUUID().replaceAll('-','');
  for(const p of players)p.ws.send(JSON.stringify({cid:'match',matchmaker_add:{min_count:2,max_count:2,query:'+properties.smoke_group:'+group,string_properties:{region:'local',build_hash:'local-build',smoke_group:group}}}));
  await wait('Nakama matchmaker',()=>{
    for(const p of players)assert(!p.messages.some(m=>m.error),'Matchmaker rejected the request');
    return players.every(p=>p.messages.some(m=>m.matchmaker_matched));
  },30000);
  const assignments=await Promise.all(players.map(p=>wait('signed assignment',async()=>{
    const out=await rpc(p,'assignment_get');
    assert(!['failed','expired','cancelled'].includes(out.state),'Allocation failed: '+out.error);
    return out.state==='assigned'&&out.admission_token?out:null;
  })));
  assert.equal(assignments[0].allocation_id,assignments[1].allocation_id);
  assert.notEqual(assignments[0].seat,assignments[1].seat);
  for(const a of assignments){assert.equal(a.endpoint.host,'127.0.0.1');assert(a.endpoint.port>=17770&&a.endpoint.port<=17789);assert.equal(a.endpoint.transport,'tugboat-udp');}
  const game={players,assignments,worker:assignments[0].worker_id,allocation:assignments[0].allocation_id,udp:[]};games.push(game);return game;
}
function connection(endpoint){
  const sock=dgram.createSocket('udp4');sockets.push(sock);
  return {sock,send:body=>new Promise((resolve,reject)=>{
    const timeout=setTimeout(()=>{sock.off('message',receive);reject(new Error('UDP timeout'));},3000);
    const receive=data=>{clearTimeout(timeout);resolve(JSON.parse(data));};sock.once('message',receive);
    sock.send(Buffer.from(JSON.stringify(body)),endpoint.port,endpoint.host,e=>{if(e){clearTimeout(timeout);sock.off('message',receive);reject(e);}});
  })};
}
async function join(game){
  for(const a of game.assignments){
    const c=connection(a.endpoint);game.udp.push(c);
    assert.equal((await c.send({op:'join',ticket:a.admission_token})).ok,true);
    assert.equal((await c.send({op:'join',ticket:a.admission_token})).ok,false,'Nonce replay must fail');
  }
  await wait('active occupants',async()=>(await admin('status')).allocations[game.allocation].state==='active');
}
async function close(game){assert.equal((await game.udp[0].send({op:'close'})).ok,true);}
let success=false;
try{
  const initial=await admin('status');
  assert(!Object.values(initial.workers).some(w=>!['stopped','failed','lost'].includes(w.state)),'Smoke needs an idle isolated fleet');
  const denied=await fetch(base+'/agones/fleet/v1/admin/status');assert.equal(denied.status,403);
  pass('operator API rejects unauthenticated access');
  const first=await match();await join(first);
  const second=await match();await join(second);
  const third=await match();await join(third);
  assert.equal(first.worker,second.worker);assert.notEqual(first.worker,third.worker);
  const gs=JSON.parse(await kube('-n','agones-games','get','gameservers','-o','json'));
  assert.equal(gs.items.length,2);assert(gs.items.every(g=>g.status.state==='Allocated'));
  assert(gs.items.every(g=>g.spec.template.spec.containers[0].env.every(e=>e.valueFrom?.secretKeyRef&&!e.value)));
  pass('real websocket matches → three rooms on two Allocated GameServers → signed UDP admission');
  await assert.rejects(rpc(third.players[0],'assignment_get',{allocation_id:first.allocation}),e=>e.status===403);
  pass('seat nonce replay and cross-user assignment access are rejected');
  const cancelled=await match();
  await rpc(cancelled.players[0],'assignment_cancel',{allocation_id:cancelled.allocation});
  await wait('cancel acknowledged',async()=>(await admin('status')).allocations[cancelled.allocation].state==='cancelled');
  pass('pending reservation cancellation waits for game acknowledgement');

  // A short process restart exercises persistent controller state without replacing games.
  const before=gs.items.map(g=>g.metadata.uid).sort();
  await kube('-n','agones-control','rollout','restart','deployment/nakama');
  await kube('-n','agones-control','rollout','status','deployment/nakama','--timeout=60s');
  await promisify(execFile)('python3',[root+'scripts/local_cluster.py','forward'],{timeout:75000});
  await wait('recovery',async()=>{const s=await admin('status');return [first,second,third].every(g=>s.allocations[g.allocation].state==='active'&&s.workers[g.worker].state==='ready');});
  const after=JSON.parse(await kube('-n','agones-games','get','gameservers','-o','json')).items.map(g=>g.metadata.uid).sort();assert.deepEqual(before,after);
  await first.udp[0].send({op:'disconnect'});
  const resumed=await rpc(first.players[0],'resume',{allocation_id:first.allocation});
  const resumedSocket=connection(resumed.endpoint);first.udp[0]=resumedSocket;
  assert.equal((await resumedSocket.send({op:'join',ticket:resumed.admission_token})).ok,true);
  pass('Nakama restart preserves process UIDs and active rooms; fresh ticket resumes the same seat');

  await admin('drain',{worker_id:first.worker});
  await wait('drain acknowledgement',async()=>(await admin('status')).workers[first.worker].drain_ack);
  await first.udp[0].send({op:'disconnect'});
  await sleep(2500);
  const drainingResume=await rpc(first.players[0],'resume',{allocation_id:first.allocation});
  first.udp[0]=connection(drainingResume.endpoint);
  assert.equal((await first.udp[0].send({op:'join',ticket:drainingResume.admission_token})).ok,true);
  pass('draining process still permits a valid reserved-seat reconnect');
  assert.equal((await third.udp[0].send({op:'ping'})).ok,true);
  await close(first);await close(second);
  await wait('pending results observed',async()=>{const w=(await admin('status')).workers[first.worker];return w.state==='draining'&&w.metrics.pending_results===1;},10000);
  await wait('first safe stop',async()=>(await admin('status')).workers[first.worker].state==='stopped');
  assert.equal((await third.udp[0].send({op:'ping'})).ok,true);
  pass('drain waits for final result work and does not interrupt other processes');
  await admin('drain',{worker_id:third.worker});await close(third);
  await wait('last safe stop',async()=>(await admin('status')).workers[third.worker].state==='stopped');
  await wait('owned resource cleanup',async()=>{
    const a=JSON.parse(await kube('-n','agones-games','get','gameservers','-l','app.kubernetes.io/managed-by=nakama-agones','-o','json'));
    const b=JSON.parse(await kube('-n','agones-games','get','secrets','-l','app.kubernetes.io/managed-by=nakama-agones','-o','json'));
    return a.items.length===0&&b.items.length===0;
  });
  pass('GameServer and per-process credential Secret are fully removed');success=true;
}finally{
  for(const s of sockets){try{s.close();}catch{}}
  // Failed runs remain inspectable. Request graceful drain, never delete a random namespace.
  if(!success)for(const worker of new Set(games.map(g=>g.worker))){try{await admin('drain',{worker_id:worker});}catch{}}
  writeFileSync(root+'.local/smoke-result.json',JSON.stringify({timestamp:new Date().toISOString(),success,checks,scope:'real K3s + Agones + Nakama, protocol fixture; no Unity/game performance claim'},null,2));
}
