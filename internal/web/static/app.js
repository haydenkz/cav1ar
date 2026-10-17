'use strict';

// cav1ar's web UI: one page, no build step. State comes from the daemon's
// event stream; the pages that edit settings work on a draft until saved.

const icons = {
  list: '<path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  cpu: '<rect x="6" y="6" width="12" height="12" rx="2"/><path d="M9 2v4M15 2v4M9 18v4M15 18v4M2 9h4M2 15h4M18 9h4M18 15h4"/>',
  folder: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
  sliders: '<path d="M4 21v-7M4 10V3M12 21v-9M12 8V3M20 21v-5M20 12V3M1 14h6M9 8h6M17 16h6"/>',
  info: '<circle cx="12" cy="12" r="9"/><path d="M12 16v-4M12 8h.01"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  pause: '<rect x="6" y="4" width="4" height="16" rx="1"/><rect x="14" y="4" width="4" height="16" rx="1"/>',
  play: '<path d="M7 4l13 8-13 8z"/>',
  hold: '<circle cx="12" cy="12" r="9"/><path d="M10 9v6M14 9v6"/>',
  x: '<path d="M18 6L6 18M6 6l12 12"/>',
  retry: '<path d="M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5"/>',
  check: '<path d="M20 6L9 17l-5-5"/>',
  alert: '<path d="M12 9v4M12 17h.01"/><path d="M10.3 3.9L1.8 18a2 2 0 0 0 1.7 3h17a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z"/>',
  save: '<path d="M19 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11l5 5v11a2 2 0 0 1-2 2z"/><path d="M17 21v-8H7v8M7 3v5h8"/>',
  refresh: '<path d="M21 12a9 9 0 1 1-3-6.7L21 8M21 3v5h-5"/>',
  menu: '<path d="M3 6h18M3 12h18M3 18h18"/>',
  spinner: '<path d="M21 12a9 9 0 1 1-6.2-8.6"/>',
  dot: '<circle cx="12" cy="12" r="3"/>',
  file: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/>',
  up: '<path d="M12 19V5M5 12l7-7 7 7"/>',
  logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9"/>',
};
const icon = (name, cls = '') => `<svg viewBox="0 0 24 24" class="${cls}">${icons[name]}</svg>`;

const pages = [
  { id: 'queue', label: 'Queue', icon: 'list' },
  { id: 'history', label: 'History', icon: 'clock' },
  { id: 'encoders', label: 'Encoders', icon: 'cpu' },
  { id: 'folders', label: 'Folders', icon: 'folder' },
  { id: 'settings', label: 'Settings', icon: 'sliders' },
  { id: 'system', label: 'System', icon: 'info' },
];
const editing = ['encoders', 'folders', 'settings']; // pages that edit a draft of the settings

const app = {
  page: 'queue',
  jobs: new Map(),
  status: null,
  connected: false,
  view: null, // the settings, and what's needed to offer their choices
  draft: null,
  saving: false,
  formError: '',
  history: { filter: '', entries: [], end: false, loading: false, open: null, error: '' },
  detail: null, // the job whose details are open
  full: false,
  lines: [],
  dialog: null,
};

// --- Helpers -----------------------------------------------------------------

const $ = (sel) => document.querySelector(sel);
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
const base = (p) => String(p || '').split('/').pop();
const clone = (v) => JSON.parse(JSON.stringify(v));

function fmtSize(b) {
  if (!b) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (b >= 1024 && i < units.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(i >= 3 ? 2 : i ? 1 : 0)} ${units[i]}`;
}

function fmtDur(s) {
  s = Math.round(s || 0);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  return `${Math.floor(m / 60)}h ${String(m % 60).padStart(2, '0')}m`;
}

function fmtAgo(unix) {
  if (!unix) return '';
  const s = Date.now() / 1000 - unix;
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  if (s < 7 * 86400) return `${Math.floor(s / 86400)} d ago`;
  return new Date(unix * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

const unix = (iso) => (iso && !iso.startsWith('0001') ? Date.parse(iso) / 1000 : 0);

function toast(msg, err = false) {
  const t = document.createElement('div');
  t.className = 'toast' + (err ? ' err' : '');
  t.textContent = msg;
  document.body.append(t);
  setTimeout(() => t.remove(), err ? 5000 : 2500);
}

async function api(method, path, body) {
  const opt = { method, headers: {} };
  if (body !== undefined) {
    opt.headers['Content-Type'] = 'application/json';
    opt.body = JSON.stringify(body);
  }
  const r = await fetch(path, opt);
  if (r.status === 401) {
    showLogin();
    throw new Error('Log in first');
  }
  const data = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(data.error || r.statusText);
  return data;
}

// --- Starting, logging in and staying connected ------------------------------

async function start() {
  const auth = await fetch('/api/auth').then((r) => r.json()).catch(() => ({}));
  if (auth.required && !auth.logged_in) return showLogin();
  $('.login')?.remove();
  connect();
  route();
}

function showLogin() {
  app.es?.close();
  if ($('.login')) return;
  const el = document.createElement('div');
  el.className = 'login';
  el.innerHTML = `<form>
    <div class="logo"><span class="logo-mark"></span><span>cav<b>1</b>ar</span></div>
    <input type="password" name="password" placeholder="Password" autocomplete="current-password" autofocus>
    <button class="button primary" type="submit">Log in</button>
    <div class="form-error" hidden></div></form>`;
  el.querySelector('form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const err = el.querySelector('.form-error');
    try {
      await api('POST', '/api/login', { password: e.target.password.value });
      start();
    } catch (x) {
      err.hidden = false;
      err.textContent = x.message;
    }
  });
  document.body.append(el);
}

function connect() {
  app.es?.close();
  const es = new EventSource('/api/events');
  app.es = es;
  es.addEventListener('snapshot', (e) => {
    const d = JSON.parse(e.data);
    app.jobs = new Map(d.jobs.map((j) => [j.id, j]));
    app.status = d.status;
    app.connected = true;
    render();
  });
  es.addEventListener('jobs', (e) => {
    for (const j of JSON.parse(e.data)) app.jobs.set(j.id, j);
    live();
  });
  es.addEventListener('status', (e) => {
    app.status = JSON.parse(e.data);
    live();
  });
  es.onerror = () => {
    app.connected = false;
    live();
    if (es.readyState === EventSource.CLOSED) setTimeout(start, 3000); // the browser gave up; log in again if need be
  };
}

// --- Navigation ----------------------------------------------------------------

function dirty() {
  return app.draft && app.view && JSON.stringify(app.draft) !== JSON.stringify(app.view.settings);
}

async function route() {
  const id = location.hash.replace(/^#\/?/, '') || 'queue';
  const next = pages.some((p) => p.id === id) ? id : 'queue';
  if (dirty() && next !== app.page && !confirm('Leave without saving your changes?')) {
    history.replaceState(null, '', '#/' + app.page);
    return;
  }
  app.page = next;
  app.formError = '';
  $('#sidebar').classList.remove('open');
  if (editing.includes(app.page)) await loadSettings();
  if (app.page === 'history') loadHistory(true);
  render();
  window.scrollTo(0, 0);
}

async function loadSettings() {
  try {
    app.view = await api('GET', '/api/settings');
    app.draft = clone(app.view.settings);
  } catch (e) {
    toast(e.message, true);
  }
}

async function save() {
  app.saving = true;
  app.formError = '';
  renderSavebar();
  try {
    await api('PUT', '/api/settings', app.draft);
    await loadSettings();
    toast('Saved');
  } catch (e) {
    app.formError = e.message;
  }
  app.saving = false;
  render();
}

// --- Rendering -----------------------------------------------------------------

let liveQueued = false;
function live() {
  if (liveQueued) return;
  liveQueued = true;
  requestAnimationFrame(() => {
    liveQueued = false;
    renderNav();
    renderBanner();
    renderToolbar();
    if (app.page === 'queue' || app.page === 'system') renderPage();
    if (app.page === 'encoders') updateBusy();
    if (app.detail !== null) renderOverlay();
  });
}

function render() {
  renderNav();
  renderBanner();
  renderToolbar();
  renderPage();
  renderOverlay();
}

function queueCounts() {
  let running = 0, queued = 0, failed = 0;
  for (const j of app.jobs.values()) {
    if (j.state === 'running') running++;
    else if (j.state === 'queued') queued++;
    else if (j.state === 'failed') failed++;
  }
  return { running, queued, failed };
}

function renderNav() {
  const c = queueCounts();
  $('#nav').innerHTML = pages.map((p) => {
    const count = p.id === 'queue' && c.running + c.queued ? `<span class="count">${c.running + c.queued}</span>` : '';
    return `<a href="#/${p.id}" class="${app.page === p.id ? 'active' : ''}">${icon(p.icon)}${p.label}${count}</a>`;
  }).join('');
}

function renderBanner() {
  const b = $('#banner');
  const open = app.status && !app.status.password_set;
  b.hidden = !open;
  if (open) b.innerHTML = 'No password is set, so anyone who can reach this page can control cav1ar. <a href="#/settings">Set one in Settings</a>.';
}

function tool(act, ic, label, opts = {}) {
  return `<button class="tool ${opts.on ? 'on' : ''}" data-act="${act}" ${opts.disabled ? 'disabled' : ''} title="${esc(opts.title || '')}">${icon(ic)}<span>${label}</span></button>`;
}

function renderToolbar() {
  const s = app.status || {};
  let tools = `<button class="tool menu-button" data-act="menu">${icon('menu')}<span>Menu</span></button>`;
  tools += `<div class="title">${pages.find((p) => p.id === app.page).label}</div>`;
  switch (app.page) {
    case 'queue':
      tools += tool('add', 'plus', 'Add', { title: 'Queue a file, or every video in a folder' });
      tools += s.paused
        ? tool('resume', 'play', 'Resume', { on: true, title: 'Carry on where the encodes stopped' })
        : tool('pause', 'pause', 'Pause', { title: 'Stop the running encodes where they are' });
      tools += tool('hold', 'hold', s.holding ? 'Holding' : 'Hold', { on: s.holding, title: 'Start no new jobs; running ones finish' });
      break;
    case 'history':
      tools += tool('refresh-history', 'refresh', 'Refresh');
      break;
    case 'folders':
      tools += tool('add-folder', 'plus', 'Add folder');
      tools += tool('save', 'save', 'Save', { disabled: !dirty() || app.saving });
      break;
    case 'encoders':
    case 'settings':
      tools += tool('save', 'save', 'Save', { disabled: !dirty() || app.saving });
      break;
    case 'system':
      if (s.password_set) tools += tool('logout', 'logout', 'Log out');
      break;
  }
  const chips = [];
  if (!app.connected) chips.push('<span class="chip err">Reconnecting…</span>');
  if (s.paused) chips.push('<span class="chip warn">Paused</span>');
  if (s.holding) chips.push('<span class="chip warn">Holding</span>');
  $('#toolbar').innerHTML = `${tools}<div class="spacer"></div><div class="chips">${chips.join('')}</div>`;
}

function renderPage() {
  const el = $('#page');
  switch (app.page) {
    case 'queue': el.innerHTML = queuePage(); break;
    case 'history': el.innerHTML = historyPage(); break;
    case 'encoders': el.innerHTML = encodersPage(); break;
    case 'folders': el.innerHTML = foldersPage(); break;
    case 'settings': el.innerHTML = settingsPage(); break;
    case 'system': el.innerHTML = systemPage(); break;
  }
}

function savebar() {
  const changed = dirty();
  if (!changed && !app.formError) return '';
  return `<div class="savebar">
    ${app.formError ? `<span class="form-error">${esc(app.formError)}</span>` : '<span class="note">You have unsaved changes</span>'}
    <button class="button" data-act="discard" ${app.saving ? 'disabled' : ''}>Discard</button>
    <button class="button primary" data-act="save" ${!changed || app.saving ? 'disabled' : ''}>${app.saving ? 'Saving…' : 'Save'}</button>
  </div>`;
}

function renderSavebar() {
  const old = document.querySelector('.savebar');
  const html = savebar();
  if (old) old.outerHTML = html || '<span hidden></span>';
  else if (html) $('#page').insertAdjacentHTML('beforeend', html);
  renderToolbar();
}

// --- Queue ------------------------------------------------------------------

const actionLabels = { encode: 'Encoding', remux: 'Remuxing', retag: 'Fixing tags', rename: 'Renaming', none: 'Nothing to do' };
const doneLabels = { encode: 'Encoded', remux: 'Remuxed', retag: 'Tags fixed', rename: 'Renamed', none: 'Nothing to do' };

function stateIcon(state) {
  switch (state) {
    case 'running': return `<span class="state running">${icon('spinner', 'spin')}</span>`;
    case 'done': return `<span class="state done">${icon('check')}</span>`;
    case 'failed': return `<span class="state failed">${icon('alert')}</span>`;
    case 'canceled': return `<span class="state">${icon('x')}</span>`;
    default: return `<span class="state">${icon('clock')}</span>`;
  }
}

function queuePage() {
  const all = [...app.jobs.values()];
  const running = all.filter((j) => j.state === 'running').sort((a, b) => a.started - b.started || a.id - b.id);
  const queued = all.filter((j) => j.state === 'queued')
    .sort((a, b) => b.priority - a.priority || (b.next ? 1 : 0) - (a.next ? 1 : 0) || a.id - b.id);
  const failed = all.filter((j) => j.state === 'failed').sort((a, b) => b.finished - a.finished);
  const rows = [...running, ...queued, ...failed];
  if (!app.status) return '';
  if (!rows.length) {
    return `<div class="empty">${icon('check')}<div>Nothing to encode.</div>
      <div>New videos in watched folders show up here as they arrive, or <a href="#" data-act="add">add some</a>.</div></div>`;
  }
  const parts = [`${running.length} running`, `${queued.length} queued`];
  if (failed.length) parts.push(`${failed.length} failed`);
  return `<p class="summary">${parts.join(' · ')}</p>
    <div class="table-wrap"><table>
    <thead><tr><th></th><th>Name</th><th class="hide-small">Device</th><th>Status</th><th>Progress</th>
      <th class="num hide-small">Speed</th><th class="num">Time left</th><th></th></tr></thead>
    <tbody>${rows.map(queueRow).join('')}</tbody></table></div>`;
}

function queueRow(j) {
  const name = base(j.input);
  const out = j.output && base(j.output) !== name ? base(j.output) : '';
  let status = 'Queued';
  if (j.state === 'running') status = j.step ? j.step[0].toUpperCase() + j.step.slice(1) : actionLabels[j.action] || 'Checking';
  else if (j.state === 'failed') status = 'Failed';
  const tags = [];
  if (j.state === 'queued' && j.next) tags.push('<span class="tag accent">next</span>');
  if (j.state === 'queued' && j.priority) tags.push(`<span class="tag">${j.priority > 0 ? '+' : ''}${j.priority}</span>`);
  const progress = j.state === 'running' && j.progress > 0
    ? `<div class="progress"><div class="bar"><i style="width:${(j.progress * 100).toFixed(1)}%"></i></div><span>${Math.floor(j.progress * 100)}%</span></div>` : '';
  const speed = j.state === 'running' && j.speed > 0 ? `${j.speed >= 10 ? Math.round(j.speed) : j.speed.toFixed(1)}×` : '';
  const action = j.state === 'running' || j.state === 'queued'
    ? `<button class="icon-button danger" data-act="cancel" data-id="${j.id}" title="Cancel">${icon('x')}</button>`
    : `<button class="icon-button" data-act="retry" data-id="${j.id}" title="Retry">${icon('retry')}</button>`;
  return `<tr class="click" data-job="${j.id}">
    <td class="icon">${stateIcon(j.state)}</td>
    <td class="name"><div class="main-line" title="${esc(j.input)}">${esc(name)} ${tags.join(' ')}</div>
      ${out ? `<div class="sub">→ ${esc(out)}</div>` : ''}${j.error ? `<div class="err-text">${esc(j.error)}</div>` : ''}</td>
    <td class="muted hide-small">${esc(j.device || '')}</td>
    <td class="muted">${esc(status)}</td>
    <td>${progress}</td>
    <td class="num muted hide-small">${speed}</td>
    <td class="num muted">${j.state === 'running' && j.eta > 0 ? fmtDur(j.eta) : ''}</td>
    <td class="actions">${action}</td></tr>`;
}

// --- History ----------------------------------------------------------------

async function loadHistory(reset) {
  const h = app.history;
  if (h.loading) return;
  if (reset) Object.assign(h, { entries: [], end: false, open: null, error: '' });
  h.loading = true;
  const before = h.entries.length ? h.entries[h.entries.length - 1].id : 0;
  try {
    const d = await api('GET', `/api/history?before=${before}&result=${h.filter}`);
    const entries = d.entries || [];
    h.entries.push(...entries);
    h.end = entries.length < 50;
  } catch (e) {
    h.error = e.message;
  }
  h.loading = false;
  if (app.page === 'history') renderPage();
}

function historyPage() {
  const h = app.history;
  const filters = [['', 'All'], ['done', 'Done'], ['failed', 'Failed']];
  let body;
  if (h.error) body = `<div class="empty">${icon('alert')}<div>${esc(h.error)}</div></div>`;
  else if (!h.entries.length) body = h.loading ? '' : `<div class="empty">${icon('clock')}<div>No finished jobs yet.</div></div>`;
  else {
    body = `<div class="table-wrap"><table>
      <thead><tr><th></th><th>Name</th><th class="hide-small">What</th><th class="hide-small">Device</th>
        <th class="num">Size</th><th class="num hide-small">Took</th><th class="num">Finished</th></tr></thead>
      <tbody>${h.entries.map(historyRow).join('')}</tbody></table></div>
      ${h.end ? '' : `<p style="text-align:center"><button class="button" data-act="more-history" ${h.loading ? 'disabled' : ''}>${h.loading ? 'Loading…' : 'Load more'}</button></p>`}`;
  }
  return `<div class="segmented">${filters.map(([v, l]) =>
    `<button data-act="history-filter" data-filter="${v}" class="${h.filter === v ? 'active' : ''}">${l}</button>`).join('')}</div>${body}`;
}

function historyRow(e) {
  const started = unix(e.started), finished = unix(e.finished);
  let size = '';
  if (e.in_size && e.out_size && e.output !== e.input && (e.action === 'encode' || e.action === 'remux')) {
    const change = Math.round((e.out_size / e.in_size - 1) * 100);
    size = `${fmtSize(e.in_size)} → ${fmtSize(e.out_size)} <span class="tag ${change < 0 ? 'accent' : ''}">${change > 0 ? '+' : ''}${change}%</span>`;
  }
  const open = app.history.open === e.id;
  const detail = open ? `<tr><td></td><td colspan="6" class="muted" style="font-size:12.5px">
      <div>From ${esc(e.input)}</div>${e.output && e.output !== e.input ? `<div>To ${esc(e.output)}</div>` : ''}</td></tr>` : '';
  return `<tr class="click" data-history="${e.id}">
    <td class="icon">${stateIcon(e.result)}</td>
    <td class="name"><div class="main-line" title="${esc(e.input)}">${esc(base(e.output && e.result === 'done' ? e.output : e.input))}</div>
      ${e.error ? `<div class="err-text">${esc(e.error)}</div>` : ''}</td>
    <td class="muted hide-small">${esc((e.result === 'done' ? doneLabels : actionLabels)[e.action] || e.action || '')}</td>
    <td class="muted hide-small">${esc(e.device || '')}</td>
    <td class="num muted">${size}</td>
    <td class="num muted hide-small">${started && finished ? fmtDur(finished - started) : ''}</td>
    <td class="num muted" title="${finished ? new Date(finished * 1000).toLocaleString() : ''}">${fmtAgo(finished)}</td></tr>${detail}`;
}

// --- Encoders ---------------------------------------------------------------

const kinds = {
  nvidia: { label: 'NVIDIA · NVENC', speedKey: 'nvenc_preset' },
  vaapi: { label: 'AMD/Intel · VA-API', speedKey: 'vaapi_preset' },
  cpu: { label: 'CPU · SVT-AV1', speedKey: 'preset' },
};

function shownSlots(d) {
  const enc = app.draft.encoders || {};
  if (d.id in enc) return enc[d.id];
  if (d.kind in enc) return enc[d.kind];
  return 'auto';
}

// effectiveSlots is what the draft gives d, as the daemon will count it.
function effectiveSlots(d, devices) {
  const shown = shownSlots(d);
  if (shown !== 'auto') return shown;
  if (d.kind !== 'cpu') return 2;
  return devices.some((o) => o.kind !== 'cpu' && effectiveSlots(o, devices) > 0) ? 0 : 1;
}

function deviceGroups(devices) {
  const hasGPU = new Set(devices.filter((d) => d.kind !== 'cpu').map((d) => d.worker || ''));
  const groups = new Map([['', []]]);
  for (const d of devices) {
    const w = d.worker || '';
    if (w && d.kind === 'cpu' && hasGPU.has(w) && !(shownSlots(d) > 0)) continue;
    if (!groups.has(w)) groups.set(w, []);
    groups.get(w).push(d);
  }
  if (!groups.get('').length) groups.delete('');
  return groups;
}

function busy(d) {
  let n = 0;
  for (const j of app.jobs.values()) if (j.state === 'running' && j.device === d.name) n++;
  return n;
}

function encodersPage() {
  if (!app.draft || !app.status) return '';
  const devices = app.status.devices || [];
  if (!devices.length) return `<div class="empty">${icon('cpu')}<div>Nothing here can encode AV1.</div></div>`;
  const shownSpeed = new Set();
  let html = '<div class="settings">';
  for (const [worker, list] of deviceGroups(devices)) {
    html += `<div class="card"><h2>${worker ? `${esc(worker)} <span class="tag">worker</span>` : 'This machine'}</h2>`;
    for (const d of list) {
      const shown = shownSlots(d);
      const opts = [['auto', d.kind === 'cpu' ? 'off while a GPU encodes (auto)' : '2 at once (auto)'], [0, 'off']];
      for (let n = 1; n <= Math.max(8, shown === 'auto' ? 0 : shown); n++) opts.push([n, `${n} at once`]);
      let speed = '';
      const kind = kinds[d.kind];
      if (kind && effectiveSlots(d, devices) > 0 && !shownSpeed.has(d.kind)) {
        shownSpeed.add(d.kind);
        speed = `<label>Speed</label>${speedSelect(d.kind)}`;
      }
      html += `<div class="device"><div>
          <div class="name">${esc(d.name.replace(/ \([^)]*\)$/, worker ? '' : '$&'))}</div>
          <div class="sub">${kind ? kind.label : esc(d.kind)} · <span data-busy="${esc(d.id)}">${busyText(d)}</span></div></div>
        <div class="controls">${speed}<select class="small" data-slots="${esc(d.id)}">${opts.map(([v, l]) =>
          `<option value="${v}" ${String(v) === String(shown) ? 'selected' : ''}>${l}</option>`).join('')}</select></div></div>`;
    }
    html += '</div>';
  }
  return html + '</div>' + savebar();
}

function busyText(d) {
  const n = busy(d);
  return n ? `${n} running` : 'idle';
}

function updateBusy() {
  for (const d of app.status?.devices || []) {
    const el = document.querySelector(`[data-busy="${CSS.escape(d.id)}"]`);
    if (el) el.textContent = busyText(d);
  }
}

function speedSelect(kind) {
  const key = kinds[kind].speedKey;
  const v = app.draft[key] ?? 0;
  let opts;
  if (kind === 'nvidia') opts = [1, 2, 3, 4, 5, 6, 7].map((p) => [p, `p${p}${p <= 2 ? ' · fastest' : p >= 7 ? ' · slowest' : ''}`]);
  else if (kind === 'vaapi') opts = [[0, "driver's default"], ...[1, 2, 3, 4, 5, 6, 7].map((p) => [p, `${p}${p === 1 ? ' · slowest' : p === 7 ? ' · fastest' : ''}`])];
  else opts = [...Array(14).keys()].map((p) => [p, `${p}${p <= 2 ? ' · slowest' : p >= 11 ? ' · fastest' : ''}`]);
  return `<select class="small" data-key="${key}" data-type="num">${opts.map(([o, l]) =>
    `<option value="${o}" ${o === v ? 'selected' : ''}>${l}</option>`).join('')}</select>`;
}

// setSlots sets a device's number at once. Once one changes, every device is
// named by its ID, as the terminal UI saves them.
function setSlots(id, value) {
  const devices = app.status.devices;
  const enc = {};
  for (const d of devices) {
    const shown = shownSlots(d);
    if (shown !== 'auto') enc[d.id] = shown;
  }
  for (const [k, v] of Object.entries(app.draft.encoders || {})) {
    if (!(k in kinds) && !devices.some((d) => d.id === k)) enc[k] = v; // devices not here now keep theirs
  }
  if (value === 'auto') delete enc[id];
  else enc[id] = Number(value);
  app.draft.encoders = Object.keys(enc).length ? enc : undefined;
}

// --- Folders ----------------------------------------------------------------

function foldersPage() {
  if (!app.draft) return '';
  const watch = app.draft.watch || [];
  let body;
  if (!watch.length) {
    body = `<div class="empty">${icon('folder')}<div>No folders are watched.</div>
      <div>Watched folders' videos are kept encoded, including new ones as they arrive.</div></div>`;
  } else {
    body = `<div class="table-wrap"><table>
      <thead><tr><th>Folder</th><th>Names come from</th><th>Priority</th><th></th></tr></thead>
      <tbody>${watch.map((f, i) => `<tr>
        <td class="name"><div class="main-line" title="${esc(f.path)}">${esc(f.path)}</div></td>
        <td><select class="small" data-watch="${i}" data-field="kind">
          ${[['', 'each file'], ['movies', 'movie folders'], ['shows', 'show folders']].map(([v, l]) =>
            `<option value="${v}" ${(f.kind || '') === v ? 'selected' : ''}>${l}</option>`).join('')}</select></td>
        <td><input type="number" class="small" data-watch="${i}" data-field="priority" value="${f.priority || 0}" min="-9" max="9"></td>
        <td class="actions"><button class="icon-button danger" data-act="remove-folder" data-index="${i}" title="Stop watching">${icon('x')}</button></td>
      </tr>`).join('')}</tbody></table></div>
      <p class="summary" style="margin-top:12px">Videos in folders with a higher priority are encoded first.</p>`;
  }
  return body + `<div class="card settings" style="margin-top:18px">${field('Videos already there', select('watch_existing', [[true, 'encode them too'], [false, 'leave them']]),
    'When a folder is first watched, whether the videos already in it are encoded, or only new ones.')}</div>` + savebar();
}

// --- Settings ---------------------------------------------------------------

function field(label, control, help = '') {
  return `<div class="field"><label>${label}</label><div>${control}${help ? `<div class="help">${help}</div>` : ''}</div></div>`;
}

function select(key, opts, type = 'json') {
  const v = app.draft[key];
  return `<select data-key="${key}" data-type="${type}">${opts.map(([o, l]) =>
    `<option value="${esc(JSON.stringify(o))}" ${JSON.stringify(o) === JSON.stringify(v ?? (typeof o === 'boolean' ? false : v)) ? 'selected' : ''}>${l}</option>`).join('')}</select>`;
}

function text(key, placeholder = '', browse = '') {
  const input = `<input type="text" data-key="${key}" data-type="str" value="${esc(app.draft[key] ?? '')}" placeholder="${esc(placeholder)}">`;
  return browse ? `<div class="with-button">${input}<button class="button" data-act="browse" data-for="${key}" data-mode="${browse}">Browse</button></div>` : input;
}

function audioChoices() {
  const opts = [['copy', 'copy original']];
  for (const [codec, name, rates] of [['opus', 'Opus', [64, 96, 128, 160, 192, 256, 384]], ['aac', 'AAC', [96, 128, 192, 256, 384, 512]]]) {
    opts.push([`${codec}:-1`, `${name} · auto bitrate`]);
    for (const r of rates) opts.push([`${codec}:${r}`, `${name} ${r} kbps`]);
  }
  const d = app.draft;
  const v = d.audio_kbps === 0 ? 'copy' : `${d.audio_codec}:${d.audio_kbps ?? -1}`;
  return `<select data-act-change="audio">${opts.map(([o, l]) => `<option value="${o}" ${o === v ? 'selected' : ''}>${l}</option>`).join('')}</select>`;
}

function settingsPage() {
  if (!app.draft) return '';
  const d = app.draft, v = app.view;
  const heights = [[0, 'keep original'], ...(v.max_heights || []).map((h) => [h, `scale down to ${h}p`])];
  let html = '<div class="settings">';
  html += `<div class="card"><h2>Video</h2>
    ${field('Quality', `<input type="number" min="1" max="63" data-key="crf" data-type="num" value="${d.crf}">`,
      `1–63. Lower is better quality and bigger files. Each encoder is set to give about the same quality for the same number. Recommended: ${v.recommended.crf}.`)}
    ${field('Resolution', select('max_height', heights), 'Bigger video is scaled down to fit. AV1 video that already fits is copied, not re-encoded.')}
  </div>`;
  html += `<div class="card"><h2>Audio</h2>
    ${field('Audio', audioChoices(), 'Auto is 128 kbps for stereo and 256 for 5.1 with Opus. Tracks already in that codec and layout are copied.')}
    ${d.audio_kbps !== 0 ? field('Channels', select('downmix', [[false, 'keep original'], [true, 'downmix to stereo']]), 'Downmixing makes stereo from surround, for TVs and headphones.') : ''}
    ${field('Language', `<input type="text" class="small" maxlength="3" data-key="language" data-type="str" value="${esc(d.language)}">`, 'A 2 or 3 letter code such as eng, used to pick audio and subtitle tracks.')}
    ${field('Audio tracks', select('audio_tracks', [['all', 'keep all'], ['language', 'only that language'], ['best', 'the best one in that language']]), 'If no track is in the language, all are kept.')}
  </div>`;
  html += `<div class="card"><h2>Subtitles</h2>
    ${field('Subtitles', select('subtitles', [['all', 'keep all'], ['language', 'only the language set under Audio'], ['none', 'remove all']]))}
    ${d.subtitles !== 'none' ? field('Tidy subtitles', select('tidy_subs', [[false, 'keep as they are'], [true, 'tidy']]),
      'When two tracks in a language look alike, reads them to drop duplicates and ones in the wrong language, and to flag forced and SDH tracks.') : ''}
  </div>`;
  html += `<div class="card"><h2>Naming</h2>
    ${field('File name', text('name_template'), (v.placeholders || []).map((p) => `<code>${esc(p)}</code>`).join(' '))}
    ${field('Title shown in players', text('title', "keep the source's title"), `Stored inside the file. Recommended: ${esc(v.recommended.title)}`)}
    ${field('Release group', text('release_group', 'none'), 'Used for {group} in names, and saved in the ENCODED_BY tag.')}
    ${field('Original metadata', select('strip_metadata', [[false, 'keep'], [true, 'strip']]), 'Stripping removes old titles, tags and chapters. Track languages stay.')}
  </div>`;
  html += `<div class="card"><h2>Output</h2>
    ${field('Output folder', text('out_dir', 'next to the source', 'folder'), 'Where results go. Empty puts each next to its source.')}
    ${field('Original file', select('delete_original', [[false, 'keep'], [true, 'delete after a checked encode']]), 'Deleting is permanent, and only happens once the result checks out.')}
    ${field('Preroll', text('preroll', 'none', 'file'), 'A clip to play before every result, such as an intro. Changing or clearing it rewrites every file that has it.')}
  </div>`;
  html += `<div class="card"><h2>Workers</h2>
    ${field('Take workers on', text('worker_listen', 'off, or an address such as :7310'), 'Other machines or containers lend this cav1ar their GPUs. Takes effect when cav1ar restarts.')}
    ${field('Shared folder', text('worker_dir', '', 'folder'), 'A folder this cav1ar and its workers all reach at this path, for the files workers write.')}
  </div>`;
  html += `<div class="card"><h2>Web UI</h2>
    ${field('Address', text('web_listen', 'off'), 'Where this page is served, :6767 unless changed. Empty turns it off. Takes effect when cav1ar restarts.')}
    ${field('Password', `<div class="with-button"><span style="padding-top:8px;flex:1;color:var(--muted)">${v.password_set ? 'Set' : 'Not set'}</span>
      <button class="button" data-act="password">${v.password_set ? 'Change' : 'Set password'}</button>
      ${v.password_set ? '<button class="button danger" data-act="remove-password">Remove</button>' : ''}</div>`,
      'Asked for before anything here can be seen or changed.')}
  </div>`;
  return html + '</div>' + savebar();
}

// --- System -----------------------------------------------------------------

function systemPage() {
  const s = app.status;
  if (!s) return '';
  const stat = (label, value) => `<div class="stat"><div class="label">${label}</div><div class="value">${value}</div></div>`;
  const devices = s.devices || [];
  return `<div class="stats">
      ${stat('Files encoded', (s.results || 0).toLocaleString())}
      ${stat('Space saved', fmtSize(s.saved) || '0 B')}
      ${stat('Encoders', devices.filter((d) => s.slots[d.id] > 0).length + ' of ' + devices.length + ' on')}
      ${stat('Up for', fmtDur(Date.now() / 1000 - s.started))}
    </div>
    <div class="card settings"><h2>About</h2><dl class="info">
      <dt>Version</dt><dd>${esc(s.version)}</dd>
      <dt>Watching folders</dt><dd>${!s.folders ? 'None yet: <a href="#/folders">add some</a>' : s.watching ? `Yes, ${s.folders}` : 'No: another cav1ar is watching them'}</dd>
      <dt>Encoders</dt><dd>${devices.map((d) => `${esc(d.name)} <span class="tag">${s.slots[d.id] ? s.slots[d.id] + ' at once' : 'off'}</span>`).join('<br>') || 'none'}</dd>
      <dt>Source</dt><dd><a href="https://github.com/haydenkz/cav1ar" target="_blank" rel="noopener">github.com/haydenkz/cav1ar</a></dd>
    </dl></div>`;
}

// --- Job details --------------------------------------------------------------

let logTimer = null;

async function openDetail(id) {
  app.detail = id;
  app.full = false;
  app.lines = [];
  renderOverlay();
  await refreshLog();
  clearInterval(logTimer);
  logTimer = setInterval(() => {
    const j = app.jobs.get(app.detail);
    if (j && (j.state === 'running' || j.state === 'queued')) refreshLog();
  }, 2000);
}

async function refreshLog() {
  const id = app.detail;
  try {
    const d = await api('GET', `/api/jobs/${id}/log`);
    if (app.detail === id) {
      app.lines = d.lines || [];
      renderOverlay();
    }
  } catch { /* shown as no output */ }
}

function closeOverlay() {
  app.detail = null;
  app.dialog = null;
  clearInterval(logTimer);
  renderOverlay();
}

function detailHTML() {
  const j = app.jobs.get(app.detail);
  if (!j) return '';
  const own = app.lines.filter((l) => l.startsWith('cav1ar: ') && !l.startsWith('cav1ar: ffmpeg '));
  const steps = own.map((l) => {
    const msg = l.slice(8);
    return msg.startsWith('▸ ') ? `<li class="stage">${esc(msg.slice(2))}</li>` : `<li>${esc(msg)}</li>`;
  }).join('');
  const rows = [
    ['Source', esc(j.input)],
    j.output && j.output !== j.input ? ['Result', esc(j.output)] : null,
    j.device ? ['Device', esc(j.device)] : null,
    j.in_size ? ['Size', `${fmtSize(j.in_size)}${j.out_size ? ' → ' + fmtSize(j.out_size) : ''}`] : null,
    j.elapsed ? ['Time', fmtDur(j.elapsed)] : null,
    j.error ? ['Error', `<span style="color:var(--err)">${esc(j.error)}</span>`] : null,
  ].filter(Boolean);
  const act = j.state === 'running' || j.state === 'queued'
    ? `<button class="button danger" data-act="cancel" data-id="${j.id}">Cancel</button>`
    : `<button class="button" data-act="retry" data-id="${j.id}">Retry</button>`;
  return `<div class="shade" data-act="close"></div><aside class="drawer">
    <header>${stateIcon(j.state)}<h3 title="${esc(j.input)}">${esc(base(j.input))}</h3>${act}
      <button class="icon-button" data-act="close" title="Close">${icon('x')}</button></header>
    <div class="body"><dl class="info">${rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('')}</dl>
      <div class="section-title">${app.full ? 'Output' : 'Steps'}
        <button class="button" data-act="full-log">${app.full ? 'Steps only' : 'Full output'}</button></div>
      ${app.full ? `<pre class="log">${esc(app.lines.join('\n')) || 'No output kept for this job.'}</pre>`
        : `<ul class="steps">${steps || '<li>No steps yet.</li>'}</ul>`}
    </div></aside>`;
}

// --- Dialogs ----------------------------------------------------------------

async function browse(mode, path, onPick) {
  try {
    const d = await api('GET', '/api/browse?path=' + encodeURIComponent(path || '/'));
    app.dialog = { kind: 'browse', mode, path: d.path, parent: d.parent, entries: d.entries || [], onPick };
    renderOverlay();
  } catch (e) {
    if (path && path !== '/') return browse(mode, '/', onPick);
    toast(e.message, true);
  }
}

function dialogHTML() {
  const g = app.dialog;
  if (g.kind === 'password') {
    return `<div class="shade" data-act="close"></div><form class="dialog" id="password-form">
      <header>${app.view?.password_set ? 'Change the password' : 'Set a password'}</header>
      <div style="padding:16px 18px;display:flex;flex-direction:column;gap:10px">
        <input type="password" name="password" placeholder="New password, at least 8 characters" autocomplete="new-password" autofocus>
        <input type="password" name="again" placeholder="The same again" autocomplete="new-password">
        <div class="form-error"></div></div>
      <footer><button class="button" type="button" data-act="close">Cancel</button><button class="button primary" type="submit">Save</button></footer></form>`;
  }
  const titles = { add: 'Add videos', folder: 'Choose a folder', file: 'Choose a file' };
  const up = g.path !== '/' ? `<li data-act="browse-to" data-path="${esc(g.parent)}">${icon('up')}..</li>` : '';
  const items = g.entries.map((e) => {
    const path = (g.path === '/' ? '' : g.path) + '/' + e.name;
    if (e.dir) return `<li data-act="browse-to" data-path="${esc(path)}">${icon('folder')}${esc(e.name)}</li>`;
    if (g.mode === 'folder') return '';
    return `<li data-act="pick" data-path="${esc(path)}">${icon('file')}${esc(e.name)}<span class="size">${fmtSize(e.size)}</span></li>`;
  }).join('');
  const pickFolder = g.mode !== 'file'
    ? `<button class="button primary" data-act="pick" data-path="${esc(g.path)}">${g.mode === 'add' ? 'Add this folder' : 'Choose this folder'}</button>` : '';
  return `<div class="shade" data-act="close"></div><div class="dialog">
    <header>${titles[g.mode]}</header><div class="path">${esc(g.path)}</div>
    <ul>${up}${items || '<li style="cursor:default;color:var(--muted)">Nothing here</li>'}</ul>
    <footer><button class="button" data-act="close">Cancel</button>${pickFolder}</footer></div>`;
}

function renderOverlay() {
  const el = $('#overlay');
  if (app.dialog) el.innerHTML = dialogHTML();
  else if (app.detail !== null) el.innerHTML = detailHTML();
  else el.innerHTML = '';
}

// --- Events -----------------------------------------------------------------

document.addEventListener('click', async (e) => {
  const t = e.target.closest('[data-act]');
  if (!t) {
    const row = e.target.closest('tr[data-job]');
    if (row) return openDetail(Number(row.dataset.job));
    const hrow = e.target.closest('tr[data-history]');
    if (hrow) {
      const id = Number(hrow.dataset.history);
      app.history.open = app.history.open === id ? null : id;
      return renderPage();
    }
    return;
  }
  e.preventDefault();
  const id = Number(t.dataset.id);
  try {
    switch (t.dataset.act) {
      case 'menu': $('#sidebar').classList.toggle('open'); break;
      case 'add': browse('add', '/', async (path) => {
        const r = await api('POST', '/api/add', { path });
        toast(r.added === 1 ? 'Added 1 video' : `Added ${r.added} videos`);
      }); break;
      case 'pause': await api('POST', '/api/pause', { paused: true }); break;
      case 'resume': await api('POST', '/api/pause', { paused: false }); break;
      case 'hold': await api('POST', '/api/hold', { holding: !app.status.holding }); break;
      case 'cancel': await api('POST', `/api/jobs/${id}/cancel`, {}); break;
      case 'retry': await api('POST', `/api/jobs/${id}/retry`, {}); break;
      case 'close': closeOverlay(); break;
      case 'full-log': app.full = !app.full; renderOverlay(); break;
      case 'refresh-history': loadHistory(true); break;
      case 'more-history': loadHistory(false); break;
      case 'history-filter': app.history.filter = t.dataset.filter; loadHistory(true); renderPage(); break;
      case 'save': await save(); break;
      case 'discard': app.draft = clone(app.view.settings); app.formError = ''; render(); break;
      case 'add-folder': browse('folder', '/', (path) => {
        app.draft.watch = [...(app.draft.watch || []), { path }];
        render();
      }); break;
      case 'remove-folder': app.draft.watch.splice(Number(t.dataset.index), 1); render(); break;
      case 'browse': browse(t.dataset.mode, app.draft[t.dataset.for] || '/', (path) => {
        app.draft[t.dataset.for] = path;
        render();
      }); break;
      case 'browse-to': browse(app.dialog.mode, t.dataset.path, app.dialog.onPick); break;
      case 'pick': {
        const pick = app.dialog.onPick;
        closeOverlay();
        await pick(t.dataset.path);
        break;
      }
      case 'password': app.dialog = { kind: 'password' }; renderOverlay(); break;
      case 'remove-password':
        if (confirm('Stop asking for a password? Anyone who can reach this page could then control cav1ar.')) {
          await api('PUT', '/api/password', { password: '' });
          await loadSettings();
          toast('Password removed');
          render();
        }
        break;
      case 'logout': await api('POST', '/api/logout', {}); showLogin(); break;
    }
  } catch (x) {
    toast(x.message, true);
  }
});

document.addEventListener('submit', async (e) => {
  if (e.target.id !== 'password-form') return;
  e.preventDefault();
  const f = e.target, err = f.querySelector('.form-error');
  if (f.password.value !== f.again.value) return (err.textContent = "The two don't match");
  try {
    await api('PUT', '/api/password', { password: f.password.value });
    closeOverlay();
    await loadSettings();
    toast('Password saved');
    render();
  } catch (x) {
    err.textContent = x.message;
  }
});

function onEdit(e) {
  const t = e.target;
  if (!app.draft) return;
  let rerender = false;
  if (t.dataset.key) {
    let v = t.value;
    if (t.dataset.type === 'num') v = v === '' ? 0 : Number(v);
    else if (t.dataset.type === 'json') v = JSON.parse(v);
    if (t.dataset.type === 'str' && v === '' && !(t.dataset.key in app.view.settings)) delete app.draft[t.dataset.key];
    else app.draft[t.dataset.key] = v;
    rerender = t.tagName === 'SELECT';
  } else if (t.dataset.slots) {
    setSlots(t.dataset.slots, t.value === 'auto' ? 'auto' : Number(t.value));
    rerender = true;
  } else if (t.dataset.watch) {
    const f = app.draft.watch[Number(t.dataset.watch)];
    if (t.dataset.field === 'priority') f.priority = Number(t.value) || undefined;
    else f.kind = t.value || undefined;
  } else if (t.dataset.actChange === 'audio') {
    if (t.value === 'copy') app.draft.audio_kbps = 0;
    else {
      const [codec, kbps] = t.value.split(':');
      app.draft.audio_codec = codec;
      app.draft.audio_kbps = Number(kbps);
    }
    rerender = true;
  } else return;
  app.formError = '';
  if (rerender) render();
  else renderSavebar();
}
document.addEventListener('change', onEdit);
document.addEventListener('input', (e) => { if (e.target.tagName === 'INPUT') onEdit(e); });
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && (app.dialog || app.detail !== null)) closeOverlay(); });
window.addEventListener('hashchange', route);
window.addEventListener('beforeunload', (e) => { if (dirty()) e.preventDefault(); });

start();
