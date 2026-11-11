(() => {
  'use strict';
  const $ = (id) => document.getElementById(id);
  let token = '';
  let generation = 0;
  let apps = [];
  let system = null;
  let selected = '';
  let tab = 'overview';
  let busy = false;
  let pendingAction = null;
  let deployStep = 1;
  let view = 'services';
  let detailGeneration = 0;
  let activity = [];
  let activityPersistent = true;
  let activeLogRefresh = null;
  let reconcileAccess = null;
  let focusServiceTab = false;
  const inspection = new Map();
  const samples = new Map();
  const requests = new Set();
  const toastTimers=new Map();
  const toastControls=new WeakMap();
  const warningStates=new Set();
  const tabsAvailable = [['overview','Overview'],['environment','Environment'],['access','Networking'],['logs','Logs'],['commands','Commands'],['activity','Activity']];
  try { const saved = localStorage.getItem('ifd-ui-theme'); if (saved === 'light' || saved === 'dark') document.documentElement.dataset.theme = saved; } catch {}

  function element(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = String(text);
    return node;
  }
  function button(text, className, action, mutation = false) {
    const node = element('button', className, text);
    labelButton(node,text);
    node.type = 'button';
    node.addEventListener('click', action);
    if (mutation) { node.dataset.mutation = 'true'; node.disabled = busy; }
    return node;
  }
  function icon(name) {
    const svg=document.createElementNS('http://www.w3.org/2000/svg','svg');svg.classList.add('button-icon');svg.setAttribute('viewBox','0 0 24 24');svg.setAttribute('aria-hidden','true');svg.setAttribute('focusable','false');
    const use=document.createElementNS('http://www.w3.org/2000/svg','use');use.setAttribute('href',`/ui/icons.svg#${name}`);svg.append(use);return svg;
  }
  function labelButton(node,text) {
    const icons={connect:'arrow-right',continue:'arrow-right',details:'arrow-right',view:'arrow-right',review:'check',create:'plus',new:'plus',refresh:'refresh',retry:'refresh',restart:'refresh',start:'play',stop:'stop',delete:'trash',remove:'trash',save:'check',copy:'copy',run:'terminal',publish:'globe',make:'lock',back:'arrow-left',cancel:'close',sign:'logout',load:'refresh',downloading:'refresh'};
    const glyph=node.dataset.icon||(node.classList.contains('icon-close')?'close':icons[text.trim().split(/\s+/)[0].toLowerCase()]);
    node.replaceChildren();
    if(glyph&&!node.classList.contains('service-name'))node.append(icon(glyph));
    if(!node.classList.contains('icon-close'))node.append(document.createTextNode(text));
  }
  document.querySelectorAll('button').forEach(node=>labelButton(node,node.textContent.trim()));
  function message(text, error = false) {
    if (!token) return;
    const severity=error==='warning'?'warning':error?'error':'success';
    const stack=$('notifications');
    const existing=[...stack.children].find(item=>item.querySelector('.notification-text')?.textContent===text);
    if(existing)return existing;
    while(stack.children.length>=4)dismissToast(stack.firstElementChild);
    const toast=element('div',`notification notification-${severity}`);toast.setAttribute('aria-atomic','true');
    const symbol=icon(severity==='warning'?'warning':severity==='error'?'close':'check');symbol.classList.add('notification-icon');
    const close=button('','notification-dismiss',()=>dismissToast(toast));close.setAttribute('aria-label','Dismiss notification');close.append(icon('close'));
    toast.append(symbol,element('div','notification-text',text),close);stack.append(toast);
    const pause=()=>{clearTimeout(toastTimers.get(toast));toastTimers.delete(toast);};
    const resume=()=>{pause();if(severity==='success'&&toast.isConnected&&!stack.hidden&&!document.querySelector('dialog[open]'))toastTimers.set(toast,setTimeout(()=>dismissToast(toast),5500));};
    toast.addEventListener('pointerenter',pause);toast.addEventListener('pointerleave',resume);
    toast.addEventListener('focusin',pause);toast.addEventListener('focusout',resume);
    toastControls.set(toast,{pause,resume});syncNotifications();if(!stack.hidden)resume();return toast;
  }
  function dismissToast(toast){
    clearTimeout(toastTimers.get(toast));toastTimers.delete(toast);toast.remove();
    syncNotifications();
  }
  function syncNotifications(){
    const stack=$('notifications');const blocked=Boolean(document.querySelector('dialog[open]'));
    if(blocked||!stack.children.length){
      for(const toast of stack.children)toastControls.get(toast)?.pause();
      if(typeof stack.hidePopover==='function'){try{stack.hidePopover();}catch{}}
      stack.hidden=true;return;
    }
    const reveal=stack.hidden;const pending=reveal?[...stack.children]:[];
    if(reveal)stack.replaceChildren();stack.hidden=false;
    if(typeof stack.showPopover==='function'){try{if(!stack.matches(':popover-open'))stack.showPopover();}catch{}}else stack.removeAttribute('popover');
    if(reveal){stack.append(...pending);for(const toast of pending)toastControls.get(toast)?.resume();}
  }
  const modalObserver=new MutationObserver(syncNotifications);
  document.querySelectorAll('dialog').forEach(dialog=>modalObserver.observe(dialog,{attributes:true,attributeFilter:['open']}));
  function warnOnce(key,text){if(!warningStates.has(key)){warningStates.add(key);const toast=message(text,'warning');if(toast)toast.dataset.warningKey=key;}}
  function resolveWarning(key){warningStates.delete(key);for(const toast of [...$('notifications').children]){if(toast.dataset.warningKey===key)dismissToast(toast);}}
  function clearServiceWarnings(name){resolveWarning(`environment:${name}`);resolveWarning(`access:${name}`);}
  function setBusy(value) {
    busy = value;
    updateMutationEligibility();
    for (const form of [$('deploy-form'), $('confirm-form')]) {
      form.querySelectorAll('input,select,textarea,button').forEach((item) => { item.disabled = value; });
    }
    $('deploy').disabled = value || !system?.hosting.available;
    $('deploy-form').elements.development_box.disabled=value||!system?.hosting.development_boxes;
  }
  function updateMutationEligibility() {
    document.querySelectorAll('[data-mutation]').forEach((item) => {
      const running = !item.dataset.runningApp || apps.some(app => app.name === item.dataset.runningApp && app.status === 'Running');
      item.disabled = busy || !running || item.dataset.stateConflict === 'true';
    });
  }
  function sameDetail(name, epoch) { return view === 'service' && selected === name && detailGeneration === epoch; }
  async function api(path, method = 'GET', data) {
    const current = generation;
    const controller = new AbortController();
    requests.add(controller);
    try {
      const response = await fetch(`/api/v1/${path}`, {
        method, signal: controller.signal, cache: 'no-store', credentials: 'omit',
        headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' },
        body: data === undefined ? undefined : JSON.stringify(data),
      });
      if (current !== generation) throw new DOMException('Session changed', 'AbortError');
      if (response.status === 401) { signOut(); $('login-error').textContent = 'Your token was rejected or revoked. Connect with a valid token.'; throw new Error($('login-error').textContent); }
      const result = await response.json();
      if (current !== generation) throw new DOMException('Session changed', 'AbortError');
      if (!response.ok) throw new Error(result.error || `Request failed (${response.status}).`);
      return result;
    } finally { requests.delete(controller); }
  }
  function signOut() {
    generation++;
    token = '';
    for (const request of requests) request.abort();
    requests.clear();
    apps = []; system = null; selected = ''; pendingAction = null; busy = false;
    inspection.clear(); samples.clear(); activity = []; activeLogRefresh = null; reconcileAccess = null; detailGeneration++;
    warningStates.clear();
    $('workspace').hidden = true; $('login').hidden = false;
    $('app-list').replaceChildren(); $('detail').replaceChildren(); $('server-detail').replaceChildren();
    $('token').value = ''; $('deploy-form').reset();
    for(const toast of [...$('notifications').children])dismissToast(toast);
    $('deploy-review').replaceChildren();
    document.querySelectorAll('dialog[open]').forEach((dialog) => dialog.close());
    $('token').focus();
  }
  $('login-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    $('login-error').textContent = '';
    const submit = event.currentTarget.querySelector('button');
    submit.disabled = true;labelButton(submit,'Connecting…');
    token = $('token').value.trim(); $('token').value = ''; generation++;
    try {
      system = await api('system');
      await refresh();
      $('login').hidden = true; $('workspace').hidden = false;
      $('server-name').textContent = location.hostname;
      $('identity').textContent = `${system.principal.name} / ${system.principal.project}`;
      $('preview-banner').hidden = !system.preview;
      $('notice').hidden = system.hosting.available;
      $('notice').textContent = system.hosting.reason || '';
      if(!system.hosting.available)warnOnce('hosting-unavailable',system.hosting.reason||'App hosting is not available on this server.');
      const cpu = $('deploy-form').elements.cpu;
      cpu.max = system.hosting.limits.cpu || 1;
      configureMemoryOptions();
      renderCatalog();
      setBusy(false);
      $('project-name').textContent = system.principal.project;
      $('breadcrumb-project').textContent = system.principal.project;
      $('capacity-label').textContent=system.hosting.limits.running_memory?'Running RAM budget':'Inventory limit';
      $('project-capacity').textContent = system.hosting.limits.running_memory || system.hosting.limits.max_instances || 'Not set';
      applyRoute(); refreshActivity();
    } catch (error) {
      if (error.name !== 'AbortError') { signOut(); $('login-error').textContent = error.message; }
    } finally { submit.disabled = false;labelButton(submit,'Connect'); }
  });
  $('logout').addEventListener('click', signOut);

  async function refresh() {
    const previous = apps.find(app => app.name === selected);
    const result = await api('vms');
    for(const removed of apps.filter(app=>!result.vms.some(current=>current.name===app.name))){clearServiceWarnings(removed.name);inspection.delete(removed.name);samples.delete(removed.name);}
    apps = result.vms;
    if (!apps.some((app) => app.name === selected)) { selected = ''; tab = 'overview'; }
    renderList();
    if (view === 'service' && !apps.some((app) => app.name === selected)) navigate('services');
    else if (view === 'service') {
      const current = apps.find(app => app.name === selected);
      if (JSON.stringify(previous) !== JSON.stringify(current)) {
        const header = $('service-header');
        if (header) header.replaceWith(serviceHeader(current));
        if (tab === 'access' && reconcileAccess) reconcileAccess(current);
        if ($('command-status')) $('command-status').hidden = current.status === 'Running';
        if (system.inspection) loadInspection(current.name, detailGeneration);
      }
    }
    updateMutationEligibility();
    $('last-updated').textContent = `Updated ${new Date().toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}`;
  }
  const refreshWithFeedback = async () => {
    try { await refresh(); } catch (error) { if (error.name !== 'AbortError') message(error.message, true); }
  };
  $('refresh').addEventListener('click', refreshWithFeedback);
  $('search').addEventListener('input', renderList);
  $('filter').addEventListener('change', renderList);
  $('nav-services').addEventListener('click', () => navigate('services'));
  $('nav-activity').addEventListener('click', () => navigate('activity'));
  $('view-activity').addEventListener('click', () => navigate('activity'));
  $('back-services').addEventListener('click', () => navigate('services'));
  $('refresh-activity').addEventListener('click', refreshActivity);
  $('theme-toggle').addEventListener('click', () => { const next = document.documentElement.dataset.theme === 'dark' ? 'light' : 'dark'; document.documentElement.dataset.theme = next; try { localStorage.setItem('ifd-ui-theme', next); } catch {} });
  let ticks = 0;
  setInterval(() => {
    if (!token || busy || document.hidden) return;
    ticks++;
    if (ticks % 3 === 0) refreshWithFeedback();
    if (ticks % 2 === 0) refreshActivity();
    if (view === 'service' && tab === 'overview') loadInspection(selected, detailGeneration);
    if (view === 'service' && tab === 'logs' && $('log-auto')?.checked && activeLogRefresh) activeLogRefresh();
  }, 5000);

  function navigate(next, name = '', section = 'overview') {
    const hash = next === 'service' ? `#service/${encodeURIComponent(name)}/${section}` : `#${next}`;
    if (location.hash === hash) applyRoute(); else location.hash = hash;
  }
  function applyRoute() {
    if (!token || !system) return;
    for(const key of [...warningStates]){if(key.startsWith('access:'))resolveWarning(key);}
    const parts = location.hash.slice(1).split('/');
    let name = ''; try { name = decodeURIComponent(parts[1] || ''); } catch {}
    if (parts[0] === 'service' && apps.some((app) => app.name === name)) {
      view = 'service'; selected = name; tab = tabsAvailable.some(([key]) => key === parts[2]) ? parts[2] : 'overview';
    } else view = parts[0] === 'activity' ? 'activity' : 'services';
    $('services-view').hidden = view !== 'services'; $('service-view').hidden = view !== 'service'; $('activity-view').hidden = view !== 'activity';
    $('nav-services').classList.toggle('active', view !== 'activity'); $('nav-activity').classList.toggle('active', view === 'activity');
    $('breadcrumb-service').textContent = view === 'service' ? `/ ${selected}` : view === 'activity' ? '/ Activity' : '';
    activeLogRefresh = null; reconcileAccess = null;
    if (view === 'service') renderDetail();
    if (view === 'activity') { renderActivity($('activity-list'), activity); refreshActivity(); }
  }
  window.addEventListener('hashchange', applyRoute);

  function badge(status) { return element('span', `badge${status === 'Running' ? ' running' : ''}`, status); }
  function imageLabel(image) {
    const repository = image.split('/').at(-1).split('@')[0].split(':')[0];
    return ({ nginx: 'Nginx web server', httpd: 'Apache web server', redis: 'Redis', postgres: 'PostgreSQL' })[repository] || repository || 'Application';
  }
  function openApp(name) {
    navigate('service', name);
  }
  function renderList() {
    const query = $('search').value.toLowerCase();
    const filter = $('filter').value;
    $('count-running').textContent = apps.filter((app) => app.status === 'Running').length;
    $('count-stopped').textContent = apps.filter((app) => app.status === 'Stopped').length;
    $('count-total').textContent = apps.length;
    const visible = apps.filter((app) => `${app.name} ${app.app_image || ''}`.toLowerCase().includes(query) && (filter === 'all' || app.status === filter));
    $('empty').hidden = apps.length !== 0;
    $('empty-deploy').disabled = busy || !system?.hosting.available;
    $('no-results').hidden = !apps.length || visible.length !== 0;
    const table = element('table', 'service-table');
    const head = element('thead'); const heading = element('tr');
    for (const [label, className] of [['Service',''],['State',''],['Image','image-column'],['Limits','limits-column'],['Access','access-column'],['','']]) heading.append(element('th', className, label));
    head.append(heading); const body = element('tbody');
    for (const app of visible) {
      const row = element('tr');
      const name = element('td'); const identity = element('div','service-identity');
      const title = button(app.name,'service-name',()=>openApp(app.name)); title.setAttribute('aria-label', `Manage ${app.name}`);
      const caption = element('div'); caption.append(title, element('small','',app.development_box?'Development box':app.kind === 'application' ? 'Application' : 'Linux container'));
      identity.append(element('span','app-icon',app.name[0].toUpperCase()), caption); name.append(identity);
      const status = element('td'); status.append(badge(app.status));
      const image = element('td','image-column'); const source = element('code','',app.app_image || 'Operator-managed image'); source.title = app.app_image || ''; image.append(source);
      const limits = element('td','limits-column'); limits.append(element('span','',`${app.cpu || '—'} CPU`),element('small','',app.memory || 'Not reported'));
      const access = element('td','access-column'); access.append(element('span','access-label',app.http_port ? 'Published' : app.kind === 'application' ? 'Private' : 'Operator managed'));
      const action = element('td','row-action');
      if (app.http_port && system.applications_domain) { const link = element('a','quiet','Open ↗'); link.href=`https://${app.name}.${system.applications_domain}`; link.target='_blank';link.rel='noopener noreferrer';link.setAttribute('aria-label',`Open ${app.name}`);action.append(link); }
      else { const manage = button('Details','quiet',()=>openApp(app.name)); manage.setAttribute('aria-label',`Details for ${app.name}`);action.append(manage); }
      row.append(name,status,image,limits,access,action);body.append(row);
    }
    table.append(head,body); $('app-list').replaceChildren(table); $('app-list').hidden = !visible.length;
  }

  function serviceHeader(app) {
    const header = element('header', 'detail-header');
    header.id = 'service-header';
    const meta = element('div', 'detail-meta');
    meta.append(element('span', '', app.app_image || 'Linux container'), badge(app.status));
    const title = element('div');const heading=element('h1','',app.name);heading.tabIndex=-1;title.append(heading,meta);
    const actions=element('div','service-actions');
    if(app.http_port&&system.applications_domain){const link=element('a','secondary outline','Open app ↗');link.href=`https://${app.name}.${system.applications_domain}`;link.target='_blank';link.rel='noopener noreferrer';link.setAttribute('role','button');actions.append(link);}
    actions.append(button(app.status==='Running'?'Restart':'Start','secondary outline',()=>confirmAction(app,app.status==='Running'?'restart':'start'),true));
    if(app.status==='Running')actions.append(button('Stop','secondary outline',()=>confirmAction(app,'stop'),true));
    header.append(title,actions);
    return header;
  }
  function renderDetail() {
    const epoch = ++detailGeneration;
    activeLogRefresh = null; reconcileAccess = null;
    const app = apps.find((item) => item.name === selected);
    if (!app) {
      const empty = element('div', 'detail-empty');
      empty.append(element('h2', '', 'A closer look'), element('p', '', 'Select an app to see its resources, logs and controls.'));
      $('detail').replaceChildren(empty); return;
    }
    const header = serviceHeader(app);
    const tabs = element('div', 'detail-tabs'); tabs.setAttribute('role', 'tablist');tabs.setAttribute('aria-label','Service sections');
    for (const [key, label] of tabsAvailable) {
      const item = button(label, tab === key ? 'active' : '', () => {focusServiceTab=true;navigate('service', app.name, key);});
      item.id=`service-tab-${key}`;item.dataset.tab=key;item.tabIndex=key===tab?0:-1;
      item.setAttribute('role', 'tab'); item.setAttribute('aria-selected', String(tab === key));item.setAttribute('aria-controls','service-panel'); tabs.append(item);
    }
    tabs.addEventListener('keydown',(event)=>{if(!['ArrowLeft','ArrowRight'].includes(event.key))return;event.preventDefault();const index=tabsAvailable.findIndex(([key])=>key===tab);const next=tabsAvailable[(index+(event.key==='ArrowRight'?1:-1)+tabsAvailable.length)%tabsAvailable.length][0];focusServiceTab=true;navigate('service',app.name,next);});
    const pending=element('div','notice','Environment changes are saved. Restart this service to apply them to its main process.');pending.id='pending-restart';pending.hidden=!inspection.get(app.name)?.environment_pending_restart;
    const body = element('div', 'detail-body'); body.id='service-panel';body.setAttribute('role', 'tabpanel');body.setAttribute('aria-labelledby',`service-tab-${tab}`);
    $('detail').replaceChildren(header,pending,tabs,body);
    if (tab === 'overview') renderOverview(body, app, epoch);
    if (tab === 'environment') renderEnvironment(body,app,epoch);
    if (tab === 'logs') renderLogs(body, app);
    if (tab === 'commands') renderCommands(body, app);
    if (tab === 'access') renderAccess(body, app);
    if (tab === 'activity'){const heading=element('div','section-heading');heading.append(element('h2','','Service activity'),element('p','','Recorded API actions for this service.'));body.append(heading);const list=element('div');list.id='service-activity';body.append(list);renderActivity(list,activity.filter(event=>event.name===app.name));refreshActivity();}
    if(focusServiceTab){$(`service-tab-${tab}`).focus();focusServiceTab=false;}else header.querySelector('h1').focus();
    updateMutationEligibility();
  }
  function renderOverview(body, app, epoch) {
    const heading=element('div','section-heading');heading.append(element('h2','','Runtime overview'),element('p','','Readings refresh every 5 seconds while this page is visible. Running is a process state, not a health check.'));body.append(heading);
    if(system.inspection){
      const grid=element('div','metrics-grid');
      for(const [key,label,unit]of[['cpu','CPU usage','of the configured CPU limit'],['memory','Memory','current usage'],['disk','Storage','reported root filesystem usage']]){
        const card=element('section','metric-card');card.append(element('span','metric-label',label));const value=element('strong','metric-value','—');value.id=`metric-${key}`;card.append(value,element('small','',unit));
        const chart=document.createElementNS('http://www.w3.org/2000/svg','svg');chart.setAttribute('viewBox','0 0 240 54');chart.setAttribute('aria-hidden','true');chart.classList.add('sparkline');chart.id=`chart-${key}`;card.append(chart);grid.append(card);
      }
      body.append(grid);
      const caption=element('p','fine-print','Charts show samples collected while this service page is open.');caption.id='metrics-caption';body.append(caption);
      updateMetrics(app.name);loadInspection(app.name,epoch);
    }else body.append(element('p','notice','Runtime readings are not supported by this backend.'));
    const lower=element('div','overview-columns');
    const resourcePanel=element('section','settings-panel');resourcePanel.append(element('h3','','Configured resources'));
    const resources = element('dl', 'resource-grid');
    for (const [label, value] of [['CPU limit', `${app.cpu || '—'} CPU`], ['Memory limit', app.memory || 'Not reported'], ['Process limit', app.processes || 'Not reported'], ['Disk limit', app.disk || 'Not configured']]) {
      const pair = element('div'); pair.append(element('dt', '', label), element('dd', '', value)); resources.append(pair);
    }
    resourcePanel.append(resources);
    const network=element('section','settings-panel');network.append(element('h3','','Network counters'));const counters=element('dl','resource-grid');
    for(const[key,label]of[['rx','Received since start'],['tx','Sent since start'],['processes','Processes']]){const pair=element('div');pair.append(element('dt','',label));const value=element('dd','','—');value.id=`metric-${key}`;pair.append(value);counters.append(pair)}network.append(counters,element('p','fine-print','Loopback traffic is excluded. Counters may reset when the container restarts.'));
    lower.append(resourcePanel,network);body.append(lower);
    const note = element('div', 'data-note');
    note.append(element('strong', '', 'Persistent container files, not a backup.'), element('p', '', 'Files survive stop and restart. Deleting the service removes its files. External storage and backup management are not included in this version.'));
    body.append(note,button('Delete service and its data','delete-button',()=>confirmAction(app,'delete'),true));
  }

  const humanBytes=(value)=>{if(value===null||value===undefined||!Number.isFinite(value))return 'Not reported';if(value<1024)return `${value} B`;const units=['KiB','MiB','GiB','TiB'];let scaled=value/1024,index=0;while(scaled>=1024&&index<units.length-1){scaled/=1024;index++;}return `${scaled.toFixed(scaled<10?1:0)} ${units[index]}`;};
  async function loadInspection(name,epoch){
    if(!system?.inspection)return null;
    try{
      const data=await api(`apps/${encodeURIComponent(name)}/details`);
      if(view!=='service'||selected!==name||epoch!==detailGeneration)return null;
      const previous=inspection.get(name);if(previous&&Date.parse(previous.metrics.sampled_at)>Date.parse(data.metrics.sampled_at))return previous;
      const history=samples.get(name)||[];const prior=history.at(-1);let cpu=null;
      const at=Date.parse(data.metrics.sampled_at);
      if(prior&&typeof data.metrics.cpu_usage_ns==='number'&&typeof prior.cpuNS==='number'&&data.metrics.cpu_usage_ns>=prior.cpuNS&&at>prior.at){cpu=(data.metrics.cpu_usage_ns-prior.cpuNS)/((at-prior.at)*1e6)/Math.max(Number(data.instance.cpu)||1,1)*100;}
      if(!prior||at>prior.at){history.push({at,cpu,cpuNS:data.metrics.cpu_usage_ns,memory:data.metrics.memory_bytes,disk:data.metrics.disk_bytes});if(history.length>30)history.shift();samples.set(name,history);}
      inspection.set(name,data);
      if($('pending-restart'))$('pending-restart').hidden=!data.environment_pending_restart;
      if(data.environment_pending_restart)warnOnce(`environment:${name}`,`${name} has saved environment changes. Restart it to apply them to its main process.`);else resolveWarning(`environment:${name}`);
      if(tab==='overview')updateMetrics(name);
      return data;
    }catch(error){if(error.name!=='AbortError'&&selected===name&&epoch===detailGeneration&&$('metrics-caption'))$('metrics-caption').textContent=error.message;return null;}
  }
  function updateMetrics(name){
    const data=inspection.get(name);if(!data)return;const history=samples.get(name)||[];const current=history.at(-1);const metrics=data.metrics;
    const values={cpu:current?.cpu===null||current?.cpu===undefined?'Measuring…':`${Math.max(0,current.cpu).toFixed(1)}%`,memory:humanBytes(metrics.memory_bytes),disk:humanBytes(metrics.disk_bytes),rx:humanBytes(metrics.network_rx_bytes),tx:humanBytes(metrics.network_tx_bytes),processes:metrics.processes??'Not reported'};
    for(const[key,value]of Object.entries(values)){if($(`metric-${key}`))$(`metric-${key}`).textContent=value;}
    for(const key of ['cpu','memory','disk']){
      const svg=$(`chart-${key}`);if(!svg)continue;svg.replaceChildren();const points=history.map(sample=>sample[key]).filter(value=>typeof value==='number'&&Number.isFinite(value));if(points.length<2)continue;
      const low=Math.min(...points),high=Math.max(...points,low+1);const line=document.createElementNS('http://www.w3.org/2000/svg','polyline');line.setAttribute('points',points.map((value,index)=>`${index/(points.length-1)*240},${47-(value-low)/(high-low)*39}`).join(' '));svg.append(line);
    }
  }

  async function renderEnvironment(body,app,epoch){
    const heading=element('div','section-heading');heading.append(element('h2','','Environment variables'),element('p','','Stored values stay hidden. Add a value or replace it without exposing the others.'));body.append(heading);
    if(!system.inspection){body.append(element('p','notice','Environment management is not supported by this backend.'));return;}
    const listing=element('div','environment-list');listing.textContent='Loading variable names…';body.append(listing);
    const data=await loadInspection(app.name,epoch);
    if(!data){
      if(sameDetail(app.name,epoch))listing.replaceChildren(element('p','error-text','Could not load environment variables. Check the connection and try again.'),button('Retry loading variables','secondary outline',()=>{body.replaceChildren();renderEnvironment(body,apps.find(item=>item.name===app.name)||app,epoch);}));
      return;
    }
    listing.replaceChildren();
    if(!data.environment_names.length)listing.append(element('p','fine-print','No environment variables configured.'));
    for(const key of data.environment_names){const row=element('div','environment-row');row.append(element('code','',key),element('span','masked-value','••••••••'));
      if(app.kind==='application')row.append(button('Remove','quiet',()=>confirmEnvironmentRemoval(app,key),true));listing.append(row);}
    if(app.kind!=='application'){body.append(element('p','notice','This Linux container keeps its operator-managed configuration. Environment editing is available only for apps deployed through this panel.'));return;}
    const form=element('form','settings-panel');form.append(element('h3','','Add or replace a variable'));
    const fields=element('div','field-pair');const nameLabel=element('label','','Name');const name=element('input');name.name='name';name.required=true;name.pattern='[A-Za-z_][A-Za-z0-9_]{0,63}';name.placeholder='DATABASE_URL';name.autocomplete='off';nameLabel.append(name);
    const valueLabel=element('label','','New value');const value=element('input');value.name='value';value.type='password';value.maxLength=4096;value.autocomplete='off';valueLabel.append(value);fields.append(nameLabel,valueLabel);
    const submit=button('Save variable','primary',()=>{},true);submit.type='submit';form.append(fields,element('p','fine-print','Saving does not restart the service. Restart it when you are ready to apply the new environment.'),submit);
    form.addEventListener('submit',async(event)=>{event.preventDefault();if(busy)return;const session=generation;const origin=detailGeneration;setBusy(true);name.disabled=true;value.disabled=true;
      try{await api(`apps/${encodeURIComponent(app.name)}/environment`,'PATCH',{values:{[name.value]:value.value}});value.value='';if(sameDetail(app.name,origin))renderDetail();else if(view==='service'&&selected===app.name)loadInspection(app.name,detailGeneration);warnOnce(`environment:${app.name}`,`Variable saved for ${app.name}. Restart that service to apply its new environment.`);refreshActivity();}
      catch(error){if(error.name!=='AbortError')message(error.message,true);}finally{name.disabled=false;value.disabled=false;if(session===generation)setBusy(false);}
    });body.append(form);
  }
  const actionNames={deploy_app:'Deploy service',create_container:'Create container',start:'Start',stop:'Stop',restart:'Restart',delete:'Delete',publish:'Publish port',unpublish:'Make private',run_command:'Run command',update_environment:'Update environment'};
  function renderActivity(target,events){
    target.replaceChildren();if(!activityPersistent)target.append(element('p',system?.preview?'fine-print':'notice',system?.preview?'Preview activity resets when this preview process stops.':'Activity could not be saved to disk. The latest entries may be lost on restart.'));
    if(!events.length){target.append(element('p','empty-activity','No recorded actions yet. Actions performed over SSH are not included here.'));return;}
    for(const event of events){const row=element('div','activity-row');const status=element('span',`activity-result ${event.success?'success':'failed'}`,event.success?'✓':'!');status.setAttribute('aria-label',event.success?'Completed':'Failed');const description=element('div');description.append(element('strong','',actionNames[event.action]||event.action),element('span','',event.name||'Project'));const at=new Date(event.at);const time=element('time','',Number.isNaN(at.getTime())?'Unknown time':at.toLocaleString([], {month:'short',day:'numeric',hour:'2-digit',minute:'2-digit',second:'2-digit'}));row.append(status,description,element('span','activity-source','API'),time);target.append(row);}
  }
  async function refreshActivity(){
    if(!token)return;
    try{const result=await api('events');activity=result.events;activityPersistent=result.persistent;renderActivity($('recent-activity'),activity.slice(0,5));if(view==='activity')renderActivity($('activity-list'),activity);if(view==='service'&&tab==='activity'&&$('service-activity'))renderActivity($('service-activity'),activity.filter(event=>event.name===selected));}
    catch(error){if(error.name!=='AbortError')$('recent-activity').textContent=`Activity is currently unavailable: ${error.message}`;}
  }
  function confirmEnvironmentRemoval(app,key){pendingAction={name:app.name,action:'remove_environment',key};$('confirm-title').textContent=`Remove ${key}?`;$('confirm-description').textContent='The saved variable will be removed. Restart the service to apply the change to its main process.';$('delete-label').hidden=true;$('delete-name').required=false;$('confirm-error').textContent='';$('confirm-action').textContent='Remove variable';$('confirm-action').className='danger';$('confirm-dialog').showModal();}
  function renderLogs(body, app) {
    const heading=element('div','section-heading');heading.append(element('h2','','Runtime logs'),element('p','','Latest console output, up to 64 KiB. Logs can contain secrets written by the app.'));body.append(heading);
    const controls=element('div','log-toolbar');const search=element('input');search.type='search';search.placeholder='Filter log lines';search.setAttribute('aria-label','Filter log lines');
    const autoLabel=element('label');const auto=element('input');auto.id='log-auto';auto.type='checkbox';autoLabel.append(auto,document.createTextNode('Refresh every 5s'));
    const output = element('pre', '', 'Loading console output…');
    output.tabIndex=0;output.setAttribute('aria-label','Runtime log output');
    let text='',loading=false;
    const filter=()=>{const query=search.value.toLowerCase();const lines=text.split('\n').filter(line=>!query||line.toLowerCase().includes(query));output.textContent=text?(lines.join('\n')||'No matching lines.'):'No console output recorded yet.';};search.addEventListener('input',filter);
    const load = async () => {
      if(loading)return;loading=true;
      try { const result = await api(`apps/${encodeURIComponent(app.name)}/logs`); text=result.text;filter(); }
      catch (error) { if (error.name !== 'AbortError') output.textContent = error.message; }
      finally{loading=false;}
    };
    controls.append(search,autoLabel,button('Refresh','secondary outline',load),button('Copy visible','secondary outline',async()=>{try{await navigator.clipboard.writeText(output.textContent);message('Visible log lines copied. Treat application logs as sensitive.');}catch{message('Clipboard permission was denied. Select the log text to copy it.',true);}}));
    body.append(controls,output);activeLogRefresh=load;load();
  }
  function renderCommands(body, app) {
    body.append(element('p', '', 'Run a shell command inside this container, not on the host. Commands can change or delete its data. This is not an interactive terminal.'));
    const status = element('p','notice','This service is stopped. Start it before running a command.');status.id='command-status';status.hidden=app.status==='Running';body.append(status);
    const form = element('form');
    const label = element('label', '', 'Command');
    const input = element('textarea'); input.rows = 3; input.required = true; input.maxLength = 16384; input.spellcheck = false; input.placeholder = 'uname -a';
    label.append(input);
    const submit = button('Run command', 'primary', () => {}, true); submit.type = 'submit';submit.dataset.runningApp=app.name;
    const output = element('pre', '', app.status === 'Running' ? 'Output will appear here.' : 'Start the container before running a command.');
    output.tabIndex=0;output.setAttribute('aria-label','Command output');
    form.append(label, submit);
    form.addEventListener('submit', async (event) => {
      event.preventDefault();if(busy||!apps.some(item=>item.name===app.name&&item.status==='Running'))return; const session=generation;setBusy(true); submit.disabled = true; output.textContent = 'Running…';
      try {
        const result = await api(`vms/${encodeURIComponent(app.name)}/exec`, 'POST', { command: input.value });
        output.textContent = `${result.stdout}${result.stderr ? `\n${result.stderr}` : ''}\nExit code: ${result.exit_code}${result.truncated ? '\nOutput was truncated.' : ''}`;
        if(result.truncated)message(`${app.name}: command output reached the size limit and was truncated.`,'warning');
      } catch (error) { if (error.name !== 'AbortError') output.textContent = error.message; }
      finally { if(session===generation)setBusy(false);refreshActivity(); }
    });
    body.append(form, output);
  }
  function renderAccess(body, app) {
    resolveWarning(`access:${app.name}`);
    const epoch=detailGeneration;
    reconcileAccess = (current) => {
      if (!sameDetail(app.name,epoch)) return;
      if ((current.http_port || 0) === (app.http_port || 0)) return;
      const draft=body.querySelector('form');
      if(draft?.dataset.dirty==='true'){
        let notice=body.querySelector('[data-access-change]');
        if(!notice){notice=element('div','notice');notice.dataset.accessChange='true';body.prepend(notice);}
        notice.replaceChildren(element('p','',`${current.http_port?`The service is now published on port ${current.http_port}`:'The service is now private'}. Your draft has been kept. Load the current settings before publishing.`),button('Load current access settings','secondary outline',()=>{body.replaceChildren();renderAccess(body,apps.find(item=>item.name===app.name)||current);updateMutationEligibility();}));
        warnOnce(`access:${app.name}`,`${app.name}: access settings changed in another request. Your draft was kept; load the current settings before publishing.`);
        draft.querySelector('[type=submit]').dataset.stateConflict='true';
      }else{body.replaceChildren();renderAccess(body,current);}
    };
    if (app.kind !== 'application') { body.append(element('h3', '', 'Managed by your operator'), element('p', '', 'Existing Linux containers keep their current network configuration. This panel does not replace their existing Caddy routes.')); return; }
    if (!system.applications_domain) { body.append(element('h3', '', 'Private app'), element('p', '', 'Your operator needs to configure an applications domain, wildcard DNS and a Caddy route before you can publish this app. It must use a different hostname from this control panel.'));warnOnce('applications-domain','Publishing is not configured. Ask the operator to set an applications domain, DNS and a Caddy route.');return; }
    if (app.http_port) {
      body.append(element('h3', '', `Published on port ${app.http_port}`));
      const link = element('a', 'access-link', `https://${app.name}.${system.applications_domain}`);
      link.href = `https://${app.name}.${system.applications_domain}`; link.target = '_blank'; link.rel = 'noopener noreferrer'; body.append(link);
      body.append(button('Make private', 'secondary', async () => {
        if(busy)return;
        const session=generation;
        setBusy(true);
        let completed=false;
        try { await api(`apps/${app.name}/publish`, 'POST', { port: 0 });completed=true;message(`${app.name} is private.`);await refresh(); }
        catch (error) { if (error.name !== 'AbortError') message(completed?`${app.name} was made private, but its display could not refresh. Refresh before repeating the action.`:error.message, true); }
        finally { if(session===generation)setBusy(false);refreshActivity(); }
      }, true));
    } else {
      body.append(element('h3', '', 'Publish an HTTP port'), element('p', '', 'Anyone who can reach the application hostname can access this port. Add authentication in your app if needed. Restarting the app makes it private again.'));
      const form = element('form');
      form.addEventListener('input',()=>{form.dataset.dirty='true';});
      const label = element('label', '', 'App’s HTTP port');
      const port = element('input'); port.type = 'number'; port.min = 1; port.max = 65535; port.value = '80'; port.required = true; label.append(port);
      const consent = element('label', '', ''); const check = element('input'); check.type = 'checkbox'; check.required = true;
      consent.append(check, document.createTextNode('I want to make this app accessible at its application hostname.'));
      const submit = button('Publish app', 'primary', () => {}, true); submit.type = 'submit';submit.dataset.runningApp=app.name;
      form.append(label, consent, submit);
      form.addEventListener('submit', async (event) => {
      event.preventDefault();if(busy||submit.dataset.stateConflict==='true'||!apps.some(item=>item.name===app.name&&item.status==='Running'))return;const session=generation; setBusy(true);port.disabled=true;check.disabled=true;
        let completed=false;
        try { await api(`apps/${app.name}/publish`, 'POST', { port: Number(port.value) });completed=true;form.dataset.dirty='false';message(`${app.name} is published. Its app must listen on the selected port.`);await refresh(); }
        catch (error) { if (error.name !== 'AbortError') message(completed?`${app.name} was published, but its display could not refresh. Refresh before repeating the action.`:error.message, true); }
        finally { port.disabled=false;check.disabled=false;if(session===generation)setBusy(false);refreshActivity(); }
      }); body.append(form);
    }
  }

  function openDeploy() {
    if (!system?.hosting.available) { message(system?.hosting.reason || 'App hosting is unavailable.', true); return; }
    if (busy) return;
    $('deploy-error').textContent = ''; updateResourceSummary();renderCatalog(); showDeployStep(1); $('deploy-dialog').showModal();
  }
  function parseBytes(value){const match=/^(\d+)(B|kB|KB|MB|GB|TB|KiB|MiB|GiB|TiB)$/.exec(value||'');const unit={B:1,kB:1e3,KB:1e3,MB:1e6,GB:1e9,TB:1e12,KiB:1024,MiB:1048576,GiB:1073741824,TiB:1099511627776};return match?Number(match[1])*unit[match[2]]:Infinity;}
  function updateResourceSummary(){const form=$('deploy-form');$('resource-defaults').textContent=`${form.elements.cpu.value} CPU / ${form.elements.memory.value}`;}
  function configureMemoryOptions(){
    const select=$('deploy-form').elements.memory;const limit=Math.min(parseBytes(system.hosting.limits.memory),parseBytes(system.hosting.limits.running_memory));
    for(const option of select.options)option.disabled=parseBytes(option.value)>limit;
    let choices=[...select.options].filter(option=>!option.disabled);
    if(!choices.length&&system.hosting.limits.memory){const option=new Option(system.hosting.limits.memory,system.hosting.limits.memory);select.append(option);choices=[option];}
    const defaultOption=choices.find(option=>option.value==='512MiB')||choices[0];
    if(defaultOption){for(const option of select.options)option.defaultSelected=option===defaultOption;select.value=defaultOption.value;}
    updateResourceSummary();
  }
  $('deploy-form').elements.cpu.addEventListener('input',updateResourceSummary);
  $('deploy-form').elements.memory.addEventListener('change',updateResourceSummary);
  function renderCatalog() {
    const form=$('deploy-form');const presets=system?.hosting.presets||[];
    const options=[...presets,...(system?.hosting.images||[]).filter(image=>!presets.some(preset=>preset.image===image)).map(image=>({image,name:imageLabel(image),description:image,development_box:false}))];
    form.elements.development_box.disabled=!system?.hosting.development_boxes;
    if(!system?.hosting.development_boxes)form.elements.development_box.checked=false;
    const choices = options.map((preset) => {
      const image=preset.image;
      const choice = button('', 'image-choice', () => {
        form.elements.image.value = image;form.elements.development_box.checked=Boolean(preset.development_box);renderCatalog();
        $('deploy-error').textContent = '';
        $('image-catalog').querySelector('[aria-pressed=true]')?.focus();
      });
      const chosen=form.elements.image.value===image&&form.elements.development_box.checked===Boolean(preset.development_box);
      choice.setAttribute('aria-label',preset.name);choice.setAttribute('aria-pressed',String(chosen));
      const text=element('div');text.append(element('strong','',preset.name),element('small','',preset.description));
      if(preset.description!==image)text.append(element('code','preset-source',image));
      const glyph=element('span','app-icon');glyph.append(icon(preset.development_box?'terminal':'globe'));choice.append(glyph,text);
      if(chosen)choice.append(element('span','selection-indicator','✓'));
      return choice;
    });
    $('image-catalog').replaceChildren(...choices);
    $('quick-picks-title').hidden = choices.length === 0;
    $('registry-hint').textContent = `Available registries: ${(system?.hosting.registries || []).join(', ') || 'not configured'}.`;
  }
  $('deploy-form').elements.image.addEventListener('input', renderCatalog);
  $('deploy-form').elements.development_box.addEventListener('change',renderCatalog);
  function showDeployStep(step) {
    deployStep = step;
    const titles = ['Choose your app', 'Make it yours', 'Ready to create?'];
    const descriptions = ['Bring an image from Docker Hub or an approved OCI registry.', 'Give it a name. Change the defaults only if you need to.', 'A quick check before we download and start your app.'];
    $('deploy-title').textContent = titles[step - 1]; $('deploy-description').textContent = descriptions[step - 1];
    document.querySelectorAll('[data-deploy-step]').forEach((section) => { section.hidden = Number(section.dataset.deployStep) !== step; });
    document.querySelectorAll('[data-step-label]').forEach((item) => { if (Number(item.dataset.stepLabel) === step) item.setAttribute('aria-current', 'step'); else item.removeAttribute('aria-current'); });
    $('deploy-back').hidden = step === 1; $('deploy-next').hidden = step === 3; $('deploy-submit').hidden = step !== 3;
    labelButton($('deploy-next'),step === 2 ? 'Review app' : 'Continue'); $('deploy-error').textContent = '';
    if ($('deploy-dialog').open) $('deploy-title').focus();
  }
  function readEnvironment() {
    const environment = Object.create(null);
    for (const line of $('deploy-form').elements.environment.value.split('\n').filter((line) => line.trim())) {
      const index = line.indexOf('='); const key = line.slice(0, index).trim();
      if (index < 1 || !/^[A-Za-z_][A-Za-z0-9_]{0,63}$/.test(key) || Object.hasOwn(environment, key)) throw new Error('Use NAME=value on each line, with no duplicate variable names.');
      environment[key] = line.slice(index + 1);
    }
    return environment;
  }
  function advanceDeployment() {
    const form = $('deploy-form');
    if (deployStep === 1) {
      form.elements.image.value = form.elements.image.value.trim();
      if (!form.elements.image.value) { $('deploy-error').textContent = 'Enter an image or choose a quick pick to continue.'; return; }
      if (form.elements.image.value.includes('://') && !form.elements.image.value.startsWith('https://')) { $('deploy-error').textContent = 'Only HTTPS registry URLs are accepted.'; return; }
      if (!form.elements.name.value) {
        const base = form.elements.image.value.split('/').at(-1).split(/[:@]/)[0].replace(/[^a-z0-9-]/g, '').slice(0, 40) || 'my-app';
        let name = base; let suffix = 2;
        while (apps.some((app) => app.name === name)) name = `${base}-${suffix++}`;
        form.elements.name.value = name;
      }
      showDeployStep(2); form.elements.name.focus(); return;
    }
    if (!form.elements.name.reportValidity()) return;
    if (!form.elements.cpu.checkValidity()) { form.elements.cpu.closest('details').open = true; form.elements.cpu.reportValidity(); return; }
    try {
      const environment = readEnvironment();
      const values = [['App name', form.elements.name.value], ['Image', form.elements.image.value], ['Startup',form.elements.development_box.checked?'Development box (keepalive)':'Image default'],['Resources', `${form.elements.cpu.value} CPU / ${form.elements.memory.value}`], ['Environment', `${Object.keys(environment).length} variables`], ['Access', 'Private until you publish']];
      $('deploy-review').replaceChildren(...values.map(([label, value]) => { const pair = element('div'); pair.append(element('dt', '', label), element('dd', '', value)); return pair; }));
      showDeployStep(3);
    } catch (error) { form.elements.environment.closest('details').open = true; $('deploy-error').textContent = error.message; }
  }
  $('deploy-next').addEventListener('click', advanceDeployment);
  $('deploy-back').addEventListener('click', () => showDeployStep(Math.max(1, deployStep - 1)));
  for (const id of ['deploy', 'empty-deploy']) $(id).addEventListener('click', openDeploy);
  document.querySelectorAll('[data-close]').forEach((item) => item.addEventListener('click', () => { if (!busy) $(item.dataset.close).close(); }));
  document.querySelectorAll('dialog').forEach((dialog) => dialog.addEventListener('cancel', (event) => { if (busy) event.preventDefault(); }));
  $('deploy-form').addEventListener('submit', async (event) => {
    event.preventDefault(); $('deploy-error').textContent = '';
    if(busy)return;
    if (deployStep < 3) { advanceDeployment(); return; }
    const values = new FormData(event.currentTarget);
    let environment;
    try { environment = readEnvironment(); } catch (error) { $('deploy-error').textContent = error.message; return; }
    const input = { name: values.get('name').trim(), image: values.get('image').trim(), cpu: Number(values.get('cpu')), memory: values.get('memory'), environment,development_box:$('deploy-form').elements.development_box.checked };
    const submit = event.currentTarget.querySelector('[type=submit]');
    const session=generation;
    let createdName='';
    setBusy(true);labelButton(submit,'Downloading and starting…');
    try {
      const result = await api('apps', 'POST', input);
      createdName=result.vm.name;
      selected = result.vm.name; tab = 'overview';
      $('deploy-dialog').close(); $('deploy-form').reset();
      await refresh();
      openApp(result.vm.name);
      message(`${result.vm.name} was created. Current state: ${result.vm.status}. It is private.`);
    } catch (error) {
      if (error.name !== 'AbortError') {
        if(createdName)message(`${createdName} was created, but the service list could not refresh. Refresh before attempting another deployment.`,true);
        else {$('deploy-error').textContent = error.message;await refreshWithFeedback();}
      }
    } finally { if(session===generation)setBusy(false);labelButton(submit,'Create app');refreshActivity(); }
  });

  function confirmAction(app, action) {
    pendingAction = { name: app.name, action };
    $('confirm-title').textContent = `${action[0].toUpperCase() + action.slice(1)} ${app.name}?`;
    $('confirm-description').textContent = action === 'delete' ? 'This permanently removes this container and all of its files. There is no undo.' : action === 'start' ? 'Start this container within the server’s resource limits. This does not publish its HTTP port.' : 'This interrupts running processes. Files are preserved. Published app access must be enabled again after a restart.';
    $('delete-label').hidden = action !== 'delete';
    $('delete-name').value = ''; $('delete-name').required = action === 'delete';
    $('confirm-error').textContent = '';
    labelButton($('confirm-action'),`${action[0].toUpperCase() + action.slice(1)} app`);
    $('confirm-action').className = action === 'delete' ? 'danger' : 'primary';
    $('confirm-dialog').showModal();
  }
  $('confirm-form').addEventListener('submit', async (event) => {
    event.preventDefault();
    if (!pendingAction || busy) return;
    const { name, action, key } = pendingAction;
    if (action === 'delete' && $('delete-name').value !== name) { $('confirm-error').textContent = 'The name does not match. Nothing has been deleted.'; return; }
    const session=generation;const origin=detailGeneration;let completed=false;setBusy(true);
    const successMessage=action==='remove_environment'?`Variable removed from ${name}. Restart that service to apply the change.`:`${action[0].toUpperCase()+action.slice(1)} completed for ${name}.`;
    try {
      if(action==='remove_environment')await api(`apps/${encodeURIComponent(name)}/environment`,'PATCH',{values:{[key]:null}});
      else await api(`vms/${encodeURIComponent(name)}${action === 'delete' ? '' : `/${action}`}`, action === 'delete' ? 'DELETE' : 'POST');
      completed=true;
      $('confirm-dialog').close();
      if (action === 'delete'){clearServiceWarnings(name);view='services';navigate('services');}
      if(['start','restart','stop'].includes(action))resolveWarning(`environment:${name}`);
      if(['start','restart','delete'].includes(action)){inspection.delete(name);samples.delete(name);}
      if(action==='remove_environment')warnOnce(`environment:${name}`,successMessage);else message(successMessage);
      if(action==='remove_environment'&&sameDetail(name,origin)){
        for(const row of document.querySelectorAll('.environment-row')){if(row.querySelector('code')?.textContent===key)row.remove();}
      }
      await refresh();
      if(view==='service'&&selected===name&&system.inspection)loadInspection(name,detailGeneration);
    } catch (error) {
      if(error.name!=='AbortError'){
        if(completed)message(`${successMessage} The display could not refresh. Refresh before repeating the action.`,true);
        else $('confirm-error').textContent=error.message;
      }
    }
    finally { if(session===generation)setBusy(false);refreshActivity(); }
  });

  $('nav-server').addEventListener('click', () => {
    if (!system) return;
    const list = element('dl');
    for (const [label, value] of [['Gateway version', system.version], ['Incus version', system.hosting.server_version || 'Not reported'], ['Your project', system.principal.project], ['Admission policy',system.hosting.limits.running_memory?`Dynamic running-only capacity within ${system.hosting.limits.running_memory}. Stopped containers do not reserve RAM.`:`Inventory limit: ${system.hosting.limits.max_instances || 'Not reported'}`], ['Applications domain', system.applications_domain || 'Not configured'], ['Image registries', (system.hosting.registries || []).join(', ') || 'Not configured'], ['Docker image hosting', system.hosting.available ? 'Available' : system.hosting.reason]]) {
      const pair = element('div'); pair.append(element('dt', '', label), element('dd', '', value)); list.append(pair);
    }
    const note = element('div', 'data-note');
    note.append(element('strong', '', 'Keep your token private.'), element('p', '', 'It can create and delete containers and run guest commands. Ask your operator to rotate it if it is lost. Signing out clears this tab, but does not revoke other clients.'));
    $('server-detail').replaceChildren(list, note); $('server-dialog').showModal();
  });
})();
