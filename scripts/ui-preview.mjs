import http from 'node:http';
import { readFile } from 'node:fs/promises';

const port = Number(process.env.PORT || 8270);
const files = new URL('../internal/controlapi/ui/', import.meta.url);
const available = process.env.OCI_UNAVAILABLE !== '1';
const presets=[
  {id:'kali',name:'Kali Linux',description:'Minimal Kali rolling base. Add the tools you need.',image:'kalilinux/kali-rolling:latest',development_box:true},
  {id:'alpine',name:'Alpine Linux',description:'A small Linux environment with apk package management.',image:'alpine:latest',development_box:true},
  {id:'go-dev',name:'Go dev box',description:'Go compiler and standard tooling from the official image.',image:'golang:latest',development_box:true},
  {id:'php-dev',name:'PHP dev box',description:'PHP CLI environment. Composer is not preinstalled.',image:'php:cli',development_box:true},
  {id:'python-dev',name:'Python dev box',description:'Python 3 and pip from the official image.',image:'python:3',development_box:true},
];
let apps = [
  { name: 'website', status: 'Running', kind: 'application', app_image: 'library/nginx:alpine', cpu: '1', memory: '512MiB', processes: '256', http_port: 80 },
  { name: 'notes', status: 'Running', kind: 'application', app_image: 'your-team/notes:v1', cpu: '1', memory: '1GiB', processes: '256' },
  { name: 'playground', status: 'Stopped', cpu: '2', memory: '2GiB', processes: '256' },
];
const startedAt = Date.now();
const envNames = new Map([['website',new Set(['NGINX_ENTRYPOINT_QUIET_LOGS','PATH'])],['notes',new Set(['APP_ENV','PORT'])]]);
const pendingEnvironment = new Set();
let events = [
  {at:new Date(startedAt-3*60000).toISOString(),action:'publish',name:'website',success:true},
  {at:new Date(startedAt-7*60000).toISOString(),action:'deploy_app',name:'notes',success:true},
  {at:new Date(startedAt-12*60000).toISOString(),action:'deploy_app',name:'website',success:true},
];
function record(action,name,success=true){events.unshift({at:new Date().toISOString(),action,name,success});events=events.slice(0,200);}
const json = (res, status, body) => { res.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); res.end(JSON.stringify(body)); };

http.createServer(async (req, res) => {
  try {
    if (req.headers.host !== `127.0.0.1:${port}` && req.headers.host !== `localhost:${port}`) return json(res, 403, { error: 'Preview is loopback-only.' });
    const url = new URL(req.url, `http://127.0.0.1:${port}`);
    if (url.pathname === '/') { res.writeHead(302, { Location: '/ui' }); res.end(); return; }
    if (url.pathname === '/ui' || url.pathname.startsWith('/ui/')) {
      const name = url.pathname === '/ui' || url.pathname === '/ui/' ? 'index.html' : url.pathname.slice(4);
      const types = { 'index.html': 'text/html', 'app.css': 'text/css', 'pico.min.css': 'text/css', 'icons.svg':'image/svg+xml', 'app.js': 'text/javascript', 'IBMPlexSans-Regular.woff2': 'font/woff2', 'IBMPlexSans-SemiBold.woff2': 'font/woff2' };
      if (!types[name] || req.method !== 'GET') return json(res, 404, { error: 'Not found' });
      let content = await readFile(new URL(name, files));
      if (name === 'index.html') content = content.toString().replace('<form id="login-form">', '<p class="notice">Local preview with sample data. Use <strong>preview-token</strong> to connect. No real server is affected.</p><form id="login-form">');
      res.writeHead(200, { 'Content-Type': types[name], 'Cache-Control': 'no-store', 'X-Content-Type-Options': 'nosniff', 'Content-Security-Policy': "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'" }); res.end(content); return;
    }
    if (req.headers.origin && req.headers.origin !== `http://${req.headers.host}`) return json(res, 403, { error: 'Cross-origin requests are rejected.' });
    if (req.headers.authorization !== 'Bearer preview-token') return json(res, 401, { error: 'Use preview-token in this local preview.' });
    let raw = '';
    for await (const chunk of req) { raw += chunk; if (raw.length > 65536) return json(res, 413, { error: 'Body too large' }); }
    const input = raw ? JSON.parse(raw) : {};
    const path = url.pathname.slice('/api/v1/'.length);
    if (path === 'system' && req.method === 'GET') return json(res, 200, { preview: true, inspection:true, version: 'UI preview', principal: { name: 'dev', project: 'my-apps' }, applications_domain: 'apps.example.test', hosting: { available, development_boxes:available,presets, images: ['nginx:alpine', 'httpd:alpine'], registries: ['docker.io','ghcr.io','quay.io'], server_version: available ? '7.0.2' : '6.0.7', reason: available ? '' : 'This Incus server does not support Docker/OCI images. Ask the operator to upgrade Incus in a maintenance window.', limits: { max_instances: 6, cpu: 2, memory: '4GiB', processes: 512 } } });
    if(path==='events'&&req.method==='GET')return json(res,200,{events,persistent:false});
    if (path === 'vms' && req.method === 'GET') return json(res, 200, { vms: apps });
    if (path === 'apps' && req.method === 'POST') {
      if (!available) return json(res, 400, { error: 'App hosting is not supported by this server.' });
      const image = String(input.image || '').replace(/^https:\/\//, '');
      const first = image.split('/')[0];
      if (image.includes('/') && /[.:]/.test(first) && !['docker.io','ghcr.io','quay.io'].includes(first)) return json(res, 400, { error: 'This registry is not approved in the preview.' });
      if (!/^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$/.test(input.name) || !input.image) return json(res, 400, { error: 'Enter a valid name and image.' });
      if (apps.some((app) => app.name === input.name)) return json(res, 400, { error: 'That app name is already in use.' });
      const app = { name: input.name, app_image: input.image, kind: 'application',development_box:Boolean(input.development_box), status: 'Running', cpu: String(input.cpu || 1), memory: input.memory || '512MiB', processes: '256' };
      apps.push(app);envNames.set(app.name,new Set(Object.keys(input.environment||{})));record('deploy_app',app.name); return json(res, 201, { vm: app });
    }
    const [resource, name, action] = path.split('/');
    const app = apps.find((item) => item.name === name);
    if (!app || !['vms', 'apps'].includes(resource)) return json(res, 404, { error: 'App not found.' });
    if(action==='details'&&req.method==='GET'){
      const elapsed=(Date.now()-startedAt)/1000;
      const running=app.status==='Running';
      return json(res,200,{instance:app,environment_names:[...(envNames.get(name)||[])].sort(),environment_pending_restart:pendingEnvironment.has(name),metrics:{sampled_at:new Date().toISOString(),cpu_usage_ns:running?Math.floor(elapsed*33000000):0,memory_bytes:running?Math.floor((72+Math.sin(elapsed/9)*3)*1048576):0,disk_bytes:Math.floor(128.4*1048576),network_rx_bytes:Math.floor(240000+elapsed*860),network_tx_bytes:Math.floor(75000+elapsed*240),processes:running?4:0}});
    }
    if(action==='environment'&&req.method==='PATCH'){
      if(app.kind!=='application')return json(res,400,{error:'Existing Linux containers keep their operator-managed configuration.'});
      const names=envNames.get(name)||new Set();for(const [key,value]of Object.entries(input.values||{})){if(value===null)names.delete(key);else names.add(key);}envNames.set(name,names);pendingEnvironment.add(name);record('update_environment',name);return json(res,200,{name,restart_required:true});
    }
    if (action === 'logs' && req.method === 'GET') return json(res, 200, { text: 'Preview console output\nApp started. Listening on port 80.\nGET / 200\n' });
    if (action === 'exec' && req.method === 'POST') {record('run_command',name);return json(res, 200, { stdout: input.command.includes('uname') ? 'Linux\n' : 'Preview only. The command was not executed.\n', stderr: '', exit_code: 0, truncated: false });}
    if (action === 'publish' && req.method === 'POST') { app.http_port = input.port;record(input.port?'publish':'unpublish',name); return json(res, 200, { name, url: input.port ? `https://${name}.apps.example.test` : '' }); }
    if (!action && req.method === 'DELETE') { apps = apps.filter((item) => item.name !== name);record('delete',name); return json(res, 200, { name, action: 'delete' }); }
    if (['start', 'stop', 'restart'].includes(action) && req.method === 'POST') { app.status = action === 'stop' ? 'Stopped' : 'Running'; app.http_port = 0;if(action!=='stop')pendingEnvironment.delete(name);record(action,name); return json(res, 200, { name, action }); }
    json(res, 404, { error: 'Unknown endpoint.' });
  } catch { json(res, 400, { error: 'Invalid preview request.' }); }
}).listen(port, '127.0.0.1', () => console.log(`UI preview: http://127.0.0.1:${port}/ui (sample data, token: preview-token)`));
