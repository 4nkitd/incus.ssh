import { execFileSync } from 'node:child_process';
import assert from 'node:assert/strict';

const base=new URL(process.env.UI_PREVIEW_URL||'http://localhost:8273/ui');
assert.ok(['localhost','127.0.0.1'].includes(base.hostname),'regressions require the loopback preview');
const session=`ifd-ui-races-${Date.now()}`;
const app=`qa-race-${Date.now().toString(36)}`;
const stopped=`${app}-b`;
const run=(...args)=>execFileSync('agent-browser',['--session',session,...args],{encoding:'utf8',timeout:35000,env:{...process.env,AGENT_BROWSER_USE_HELIUM:'1'}}).trim();
const click=name=>run('find','role','button','click','--name',name,'--exact');
const tab=name=>run('find','role','tab','click','--name',name,'--exact');
const evaluate=code=>run('eval',code);
const check=(code,label)=>{assert.equal(evaluate(`Boolean(${code})`),'true',label);console.log(`PASS: ${label}`);};
const request=async(path,method='GET',body)=>{
  const response=await fetch(new URL(`/api/v1/${path}`,base),{method,headers:{Authorization:'Bearer preview-token','Content-Type':'application/json'},body:body===undefined?undefined:JSON.stringify(body)});
  assert.ok(response.ok,`preview ${method} ${path}: ${response.status}`);return response.json();
};
const refresh=()=>evaluate('document.querySelector("#refresh").click()');
const restoreFetch=()=>evaluate('if(window.__originalFetch){window.fetch=window.__originalFetch;delete window.__originalFetch;}');
const delayResponse=(suffix,method)=>evaluate(`window.__originalFetch=window.fetch;window.__release=null;window.fetch=(url,options={})=>{if(String(url).endsWith(${JSON.stringify(suffix)})&&options.method===${JSON.stringify(method)}){const result=window.__originalFetch(url,options);return new Promise(resolve=>{window.__release=async()=>resolve(await result)});}return window.__originalFetch(url,options);};`);

assert.equal((await request('system')).preview,true,'never run mutations against a real backend');
try{
  await request('apps','POST',{name:app,image:'alpine:latest',development_box:true});
  await request('apps','POST',{name:stopped,image:'alpine:latest',development_box:true});
  await request(`vms/${stopped}/stop`,'POST');
  run('tab','new');run('open',base.href);run('fill','#token','preview-token');click('Connect');run('wait','--text',app);
  click(`Manage ${app}`);tab('Commands');run('fill','#service-panel textarea','printf keep-this-draft');
  await request(`vms/${app}/stop`,'POST');refresh();
  run('wait','--fn','document.querySelector("#service-header .badge")?.textContent === "Stopped"');
  check('document.querySelector("#service-panel textarea").value === "printf keep-this-draft"','external stop preserves command draft');
  check('document.querySelector("#service-panel button[type=submit]").disabled','external stop disables command action');
  await request(`vms/${app}/start`,'POST');refresh();run('wait','--fn','document.querySelector("#service-header .badge")?.textContent === "Running"');
  tab('Networking');run('fill','#service-panel input[type=number]','8081');
  await request(`apps/${app}/publish`,'POST',{port:80});refresh();run('wait','--text','Your draft has been kept');
  check('document.querySelector("#service-panel input[type=number]").value === "8081" && document.querySelector("#service-panel button[type=submit]").disabled','external publication preserves draft and requires reconciliation');
  click('Load current access settings');run('wait','--text','Published on port 80');

  tab('Environment');run('wait','--text','Add or replace a variable');
  run('fill','#service-panel form [name=name]','RACE_FLAG');run('fill','#service-panel form [name=value]','fixture-value');
  delayResponse('/environment','PATCH');click('Save variable');tab('Commands');run('fill','#service-panel textarea','printf new-command-draft');
  evaluate('window.__release();');run('wait','--fn','!document.querySelector("#deploy").disabled');
  check('document.querySelector("#service-panel textarea").value === "printf new-command-draft"','environment completion does not erase a newer tab draft');restoreFetch();
  click('Restart');run('wait','--fn','document.querySelector("#notifications").hidden');
  check('document.querySelector("#notifications").hidden','notifications defer while a modal is open');
  click('Cancel');run('wait','--fn','!document.querySelector("#notifications").hidden');
  check('!document.querySelector("#notifications").hidden','deferred notification returns after modal closes');

  delayResponse('/exec','POST');click('Run command');click('All services');click(`Manage ${stopped}`);tab('Commands');
  check('document.querySelector("#service-panel button[type=submit]").disabled','stopped service command starts disabled');
  evaluate('window.__release();');run('wait','--fn','!document.querySelector("#deploy").disabled');
  check('document.querySelector("#service-panel button[type=submit]").disabled','other request completion does not enable stopped-service commands');
  tab('Networking');check('document.querySelector("#service-panel button[type=submit]").disabled','stopped-service publish remains disabled');restoreFetch();

  click('All services');click(`Manage ${app}`);
  evaluate('window.__originalFetch=window.fetch;window.fetch=(url,options)=>String(url).endsWith("/details")?Promise.resolve(new Response(JSON.stringify({error:"fixture inspection failure"}),{status:502,headers:{"Content-Type":"application/json"}})):window.__originalFetch(url,options);');
  tab('Environment');run('wait','--text','Could not load environment variables');
  restoreFetch();click('Retry loading variables');run('wait','--text','RACE_FLAG');
  console.log('PASS: environment inspection failure has a working retry');

  evaluate('window.__originalFetch=window.fetch;window.__failList=false;window.fetch=(url,options={})=>{if(String(url).endsWith("/stop")&&options.method==="POST")return window.__originalFetch(url,options).then(r=>{window.__failList=true;return r;});if(String(url).endsWith("/vms")&&window.__failList){window.__failList=false;return Promise.resolve(new Response(JSON.stringify({error:"fixture list failure"}),{status:503,headers:{"Content-Type":"application/json"}}));}return window.__originalFetch(url,options);};');
  click('Stop');click('Stop app');run('wait','--text','The display could not refresh');
  check('!document.querySelector("#confirm-dialog").open && [...document.querySelectorAll(".notification-error")].some(e=>e.textContent.includes("Stop completed"))','successful action plus refresh failure produces a visible persistent toast');
  assert.equal((await request('vms')).vms.find(item=>item.name===app).status,'Stopped');
  restoreFetch();
  await request(`vms/${stopped}/start`,'POST');refresh();
  click('All services');click(`Manage ${stopped}`);tab('Environment');run('wait','--text','Add or replace a variable');
  run('fill','#service-panel form [name=name]','DELETE_WARNING');run('fill','#service-panel form [name=value]','fixture');click('Save variable');
  run('wait','--fn',`[...document.querySelectorAll('.notification-warning')].some(e=>e.textContent.includes(${JSON.stringify(stopped)}))`);
  tab('Overview');click('Delete service and its data');run('fill','#delete-name',stopped);click('Delete app');run('wait','--fn','!document.querySelector("#services-view").hidden');
  check(`![...document.querySelectorAll('.notification-warning')].some(e=>e.textContent.includes(${JSON.stringify(stopped)}))`,'deletion clears service-specific restart warning toasts');
  const errors=JSON.parse(run('errors','--json'));assert.deepEqual(errors.data.errors,[]);
  console.log('PASS: race regressions completed without uncaught browser errors');
}finally{
  try{restoreFetch();}catch{}
  try{run('tab','close');}catch{}
  try{run('close');}catch{}
  for(const name of [app,stopped]){try{await request(`vms/${name}`,'DELETE');}catch{}}
}
