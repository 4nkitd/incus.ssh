import { execFileSync } from 'node:child_process';
import assert from 'node:assert/strict';
import { mkdirSync } from 'node:fs';

const session = `ifd-ui-check-${Date.now()}`;
const base = process.env.UI_PREVIEW_URL || 'http://localhost:8273/ui';
const artifacts = 'ops/ui-20261110';
const name = `qa-ui-${Date.now().toString(36)}`;
mkdirSync(artifacts, { recursive: true });
const run = (...args) => execFileSync('agent-browser', ['--session', session, ...args], {
  encoding: 'utf8', timeout: 35000, env: { ...process.env, AGENT_BROWSER_USE_HELIUM: '1' },
}).trim();
const click = (name) => run('find', 'role', 'button', 'click', '--name', name, '--exact');
const evaluate = (code) => run('eval', code);
const check = (code, label) => { assert.match(evaluate(`Boolean(${code})`), /^true$/, label); console.log(`PASS: ${label}`); };

try {
  run('tab', 'new'); run('open', base); run('set', 'viewport', '1440', '1000');
  run('fill', '#token', 'invalid-preview-token'); click('Connect');
  run('wait', '--fn', 'document.querySelector("#login-error").textContent.length > 0');
  check('document.querySelector("#workspace").hidden', 'invalid token does not open workspace');
  run('fill', '#token', 'preview-token'); click('Connect'); run('wait', '--text', 'Recent activity');
  check('[...document.styleSheets].some(s=>s.href?.endsWith("/pico.min.css"))', 'Pico is bundled locally');
  check('![...Object.values(localStorage),...Object.values(sessionStorage)].includes("preview-token")', 'token is not in browser storage');
  run('wait', '--fn', 'document.querySelector("#recent-activity .activity-row") !== null');
  run('screenshot', `${artifacts}/workbench-services.png`);

  click('New service');
  for(const label of ['Kali Linux','Alpine Linux','Go dev box','PHP dev box','Python dev box'])check(`[...document.querySelectorAll('#image-catalog button')].some(b=>b.getAttribute('aria-label')===${JSON.stringify(label)})`,`${label} preset is available`);
  check('document.querySelector("#deploy svg.button-icon use")?.getAttribute("href").includes("icons.svg")','action buttons use bundled SVG icons');
  click('Continue'); run('wait', '--text', 'Enter an image');
  run('fill', '#deploy-form [name=image]', 'https://ghcr.io/stefanprodan/podinfo:6.17.0'); click('Continue');
  run('fill', '#deploy-form [name=name]', name);
  run('click', '#deploy-form details.advanced-settings:first-of-type summary');
  run('fill', '#deploy-form [name=environment]', 'APP_ENV=production\nAPP_ENV=duplicate'); click('Review app');
  run('wait', '--text', 'no duplicate variable names');
  run('fill', '#deploy-form [name=environment]', 'APP_ENV=production'); click('Review app');
  check('document.querySelector("#deploy-review").textContent.includes("ghcr.io/stefanprodan/podinfo:6.17.0")', 'custom registry URL is included in review');
  run('screenshot', `${artifacts}/workbench-deploy-review.png`);
  click('Create app'); run('wait', '--fn', `location.hash.includes(${JSON.stringify(name)}) && !document.querySelector('#service-view').hidden`);
  run('wait', '--fn', 'document.querySelector("#metric-memory")?.textContent !== "—"');
  console.log('PASS: guided custom-image deployment opens service workspace');

  run('find', 'role', 'tab', 'click', '--name', 'Environment', '--exact');
  run('wait', '--text', 'Add or replace a variable');
  run('fill', '#service-panel form [name=name]', 'NEW_FLAG'); run('fill', '#service-panel form [name=value]', 'fixture-secret-value');
  click('Save variable'); run('wait', '--text', 'NEW_FLAG');
  check('!document.body.textContent.includes("fixture-secret-value")', 'environment values stay hidden after save');
  check('!document.querySelector("#pending-restart").hidden', 'environment save marks restart as pending');
  check('[...document.querySelectorAll(".notification-warning")].some(e=>e.textContent.includes("Restart"))','restart warning appears as a toast');
  run('screenshot', `${artifacts}/workbench-environment.png`);

  run('find', 'role', 'tab', 'click', '--name', 'Logs', '--exact'); run('wait', '--text', 'App started.');
  run('fill', '[aria-label="Filter log lines"]', 'GET');
  check('document.querySelector("#service-panel pre").textContent.trim()==="GET / 200"', 'log filter shows matching lines only');
  run('check', '#log-auto');
  run('find', 'role', 'tab', 'click', '--name', 'Commands', '--exact');
  run('fill', '#service-panel textarea', 'uname -a'); click('Run command'); run('wait', '--text', 'Exit code: 0');
  console.log('PASS: logs and command controls work against preview fixtures');

  run('find', 'role', 'tab', 'click', '--name', 'Networking', '--exact');
  run('check', '#service-panel input[type=checkbox]');click('Publish app');run('wait','--text','Published on port 80');
  check(`document.querySelector('.access-link').href === 'https://${name}.apps.example.test/'`, 'published app uses a separate origin');
  click('Restart'); click('Cancel');run('wait','--text','Published on port 80');
  click('Restart');click('Restart app');run('wait','--text','Publish an HTTP port');
  run('wait','--fn','document.querySelector("#pending-restart").hidden');
  check('document.querySelector("#pending-restart").hidden', 'restart clears pending environment notice');
  check('![...document.querySelectorAll(".notification-warning")].some(e=>e.textContent.includes("Restart"))','resolved restart warning toast is cleared');

  run('find', 'role', 'tab', 'click', '--name', 'Overview', '--exact');
  run('wait', '--fn', 'document.querySelector("#metric-cpu") && !["—","Measuring…"].includes(document.querySelector("#metric-cpu").textContent)');
  run('screenshot', `${artifacts}/workbench-service.png`);
  click('Delete service and its data');run('fill','#delete-name','wrong-name');click('Delete app');
  run('wait','--text','Nothing has been deleted');run('fill','#delete-name',name);click('Delete app');
  run('wait','--fn','!document.querySelector("#services-view").hidden');
  check(`![...document.querySelectorAll('.service-name')].some(e=>e.textContent===${JSON.stringify(name)})`, 'typed confirmation protects deletion');

  click('Manage website');run('find','role','tab','click','--name','Activity','--exact');run('wait','--text','Service activity');
  click('All services');
  for (const width of [390, 320]) {
    run('set','viewport',String(width),'844');
    check('document.documentElement.scrollWidth <= innerWidth', `${width}px page has no horizontal overflow`);
    run('screenshot', `${artifacts}/workbench-mobile-${width}.png`);
  }
  run('set','viewport','1440','1000');
  if(evaluate('document.documentElement.dataset.theme === "dark"')==='true')click('Toggle color theme');
  run('screenshot',`${artifacts}/workbench-light.png`);click('Toggle color theme');run('screenshot',`${artifacts}/workbench-dark.png`);click('Toggle color theme');
  click('Sign out');check('document.querySelector("#workspace").hidden && document.querySelector("#token").value === ""', 'sign-out clears session and returns to login');
  const errors = JSON.parse(run('errors','--json'));
  assert.deepEqual(errors.data.errors, [], 'browser has no uncaught page errors');
  console.log('PASS: no uncaught browser errors');
} finally {
  try { run('tab','close'); } catch {}
  try { run('close'); } catch {}
}
