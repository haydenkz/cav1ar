'use strict';

// cav1ar's web UI: one page, no build step. State comes from the daemon's
// event stream; the pages that edit settings work on a draft until saved.

const icons = {
  list: '<path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  chart: '<path d="M4 20V11M10 20V5M16 20v-6M2 20h20"/>',
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
  file: '<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/>',
  up: '<path d="M12 19V5M5 12l7-7 7 7"/>',
  next: '<path d="M17 11l-5-5-5 5M17 18l-5-5-5 5"/>',
  search: '<circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/>',
  logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9"/>',
};
const icon = (name, cls = '') => `<svg viewBox="0 0 24 24" class="${cls}">${icons[name]}</svg>`;

const pages = [
  { id: 'queue', label: 'Queue', icon: 'list' },
  { id: 'history', label: 'History', icon: 'clock' },
  { id: 'stats', label: 'Stats', icon: 'chart' },
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
  stale: false, // the draft's settings were changed elsewhere since it was loaded
  queue: { tab: 'queue', search: '' },
  history: { filter: '', search: '', entries: [], end: false, loading: false, open: null, error: '', seq: 0 },
  stats: { days: 30, metric: 'files', data: null, error: '', loading: false, table: false },
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
const store = {
  get(key) { try { return localStorage.getItem('cav1ar.' + key); } catch { return null; } },
  set(key, v) { try { localStorage.setItem('cav1ar.' + key, v); } catch { /* kept for this visit only */ } },
};

function fmtSize(b) {
  if (!b) return '';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (Math.abs(b) >= 1024 && i < units.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(i >= 3 ? 2 : i ? 1 : 0)} ${units[i]}`;
}

function fmtDur(s) {
  s = Math.round(s || 0);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, '0')}s`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${String(m % 60).padStart(2, '0')}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
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

// fmtCount writes a count compactly once it's big: 950, 12.9K, 1.2M.
function fmtCount(n) {
  n = n || 0;
  if (n < 10000) return n.toLocaleString();
  if (n < 1e6) return `${(n / 1000).toFixed(n < 1e5 ? 1 : 0)}K`;
  return `${(n / 1e6).toFixed(1)}M`;
}

const fmtSpeed = (x) => (x >= 10 ? Math.round(x) : x.toFixed(1)) + '×';
const unix = (iso) => (iso && !iso.startsWith('0001') ? Date.parse(iso) / 1000 : 0);

function median(xs) {
  const v = xs.filter((x) => x > 0).sort((a, b) => a - b);
  if (!v.length) return 0;
  const m = Math.floor(v.length / 2);
  return v.length % 2 ? v[m] : (v[m - 1] + v[m]) / 2;
}

// sizeChange is how much smaller or bigger a result came out, as a tag.
function sizeChange(inSize, outSize) {
  if (!inSize || !outSize) return '';
  const change = Math.round((outSize / inSize - 1) * 100);
  return `<span class="tag ${change < 0 ? 'accent' : ''}">${change > 0 ? '+' : ''}${change}%</span>`;
}

function toast(msg, err = false) {
  let box = $('#toasts');
  if (!box) {
    box = document.createElement('div');
    box.id = 'toasts';
    document.body.append(box);
  }
  const t = document.createElement('div');
  t.className = 'toast' + (err ? ' err' : '');
  t.setAttribute('role', err ? 'alert' : 'status');
  t.textContent = msg;
  box.append(t);
  setTimeout(() => t.remove(), err ? 6000 : 2500);
}

async function api(method, path, body, headers = {}) {
  const opt = { method, headers: { ...headers } };
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
  if (!r.ok) {
    const e = new Error(data.error || r.statusText);
    e.status = r.status;
    throw e;
  }
  return data;
}

// --- Updating the page in place ------------------------------------------------

// morph makes el's contents match html, changing only what differs, so
// buttons stay put under the pointer and scrolled boxes stay scrolled while
// the page updates. Elements with a data-key are matched by it, so a row
// that moves is moved rather than rewritten as its neighbour.
function morph(el, html) {
  if (el._html === html) return;
  el._html = html;
  const tpl = document.createElement('template');
  tpl.innerHTML = html;
  syncChildren(el, tpl.content);
}

// put replaces el's contents outright: for pages with inputs whose values
// must go back to what's saved.
function put(el, html) {
  el._html = html;
  el.innerHTML = html;
}

function syncChildren(from, to) {
  const keyed = new Map();
  for (const c of from.children) if (c.dataset?.key) keyed.set(c.dataset.key, c);
  let cur = from.firstChild;
  for (const next of [...to.childNodes]) {
    const key = next.nodeType === 1 ? next.dataset?.key : undefined;
    let match = null;
    if (key) {
      const old = keyed.get(key);
      if (old && old.tagName === next.tagName) {
        match = old;
        keyed.delete(key);
      }
    } else if (cur && cur.nodeType === next.nodeType && (cur.nodeType !== 1 || (cur.tagName === next.tagName && !cur.dataset?.key))) {
      match = cur;
    }
    if (!match) {
      from.insertBefore(next, cur);
      continue;
    }
    if (match === cur) cur = cur.nextSibling;
    else from.insertBefore(match, cur);
    syncNode(match, next);
  }
  while (cur) {
    const n = cur.nextSibling;
    cur.remove();
    cur = n;
  }
}

function syncNode(a, b) {
  if (a.nodeType !== 1) {
    if (a.nodeValue !== b.nodeValue) a.nodeValue = b.nodeValue;
    return;
  }
  for (const { name } of [...a.attributes]) if (!b.hasAttribute(name)) a.removeAttribute(name);
  for (const { name, value } of b.attributes) if (a.getAttribute(name) !== value) a.setAttribute(name, value);
  syncChildren(a, b);
}

// --- Starting, logging in and staying connected ------------------------------

async function start() {
  const r = await fetch('/api/auth').catch(() => null);
  const auth = r ? await r.json().catch(() => ({})) : {};
  if (r && r.status === 403) return showRefused(auth.error);
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
    <input type="password" name="password" placeholder="Password" autocomplete="current-password" autofocus aria-label="Password">
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

// showRefused explains why a daemon with no password won't answer to the
// name this page was opened by.
function showRefused(msg) {
  const el = document.createElement('div');
  el.className = 'login';
  el.innerHTML = `<form><div class="logo"><span class="logo-mark"></span><span>cav<b>1</b>ar</span></div>
    <p class="refused"></p></form>`;
  el.querySelector('.refused').textContent = msg || "cav1ar won't answer to this address.";
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
    if (editing.includes(app.page)) live(); // keep the draft's inputs as they are
    else render();
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
  showTip(null);
  app.stale = false;
  menu(false);
  if (editing.includes(app.page)) await loadSettings();
  if (app.page === 'history') loadHistory(true);
  if (app.page === 'stats') loadStats();
  render();
  window.scrollTo(0, 0);
}

async function loadSettings() {
  try {
    app.view = await api('GET', '/api/settings');
    app.draft = clone(app.view.settings);
    app.stale = false;
  } catch (e) {
    toast(e.message, true);
  }
}

async function save() {
  app.saving = true;
  app.formError = '';
  renderSavebar();
  try {
    await api('PUT', '/api/settings', app.draft, { 'If-Match': app.view.version });
    await loadSettings();
    toast('Saved');
  } catch (e) {
    app.formError = e.message;
    app.stale = e.status === 409;
  }
  app.saving = false;
  render();
}

function menu(open) {
  document.body.classList.toggle('menu-open', open);
}

// --- Rendering -----------------------------------------------------------------

let liveQueued = false;

// live brings the page up to date with the daemon, in place.
function live() {
  if (liveQueued) return;
  liveQueued = true;
  requestAnimationFrame(() => {
    liveQueued = false;
    renderNav();
    renderBanner();
    renderToolbar();
    renderTitle();
    if (app.page === 'queue' || app.page === 'system') renderPage(false);
    if (app.page === 'encoders') updateBusy();
    if (app.detail !== null && !app.dialog) renderOverlay(false);
  });
}

// render draws everything afresh, as after a change of page or a save.
function render() {
  renderNav();
  renderBanner();
  renderToolbar();
  renderTitle();
  renderPage(true);
  renderOverlay(true);
}

function queueCounts() {
  const c = { running: 0, queued: 0, failed: 0, finished: 0 };
  for (const j of app.jobs.values()) {
    if (j.state === 'running') c.running++;
    else if (j.state === 'queued') c.queued++;
    else if (j.state === 'failed') c.failed++;
    else c.finished++;
  }
  return c;
}

function renderTitle() {
  const c = queueCounts();
  const n = c.running + c.queued;
  document.title = n ? `(${n}) cav1ar` : 'cav1ar';
}

function renderNav() {
  const c = queueCounts();
  morph($('#nav'), pages.map((p) => {
    let count = '';
    if (p.id === 'queue' && c.failed) count = `<span class="count err" title="${c.failed} failed">${c.failed}</span>`;
    if (p.id === 'queue' && c.running + c.queued) count += `<span class="count" title="${c.running} running, ${c.queued} queued">${c.running + c.queued}</span>`;
    return `<a href="#/${p.id}" class="${app.page === p.id ? 'active' : ''}" ${app.page === p.id ? 'aria-current="page"' : ''}>${icon(p.icon)}<span>${p.label}</span>${count}</a>`;
  }).join(''));
}

function renderBanner() {
  const b = $('#banner');
  const open = app.status && !app.status.password_set;
  b.hidden = !open;
  if (open) morph(b, 'No password is set, so anyone who can reach this page can control cav1ar. <a href="#/settings">Set one in Settings</a>.');
}

function tool(act, ic, label, opts = {}) {
  return `<button class="tool ${opts.on ? 'on' : ''}" data-act="${act}" data-key="${act}" ${opts.disabled ? 'disabled' : ''} title="${esc(opts.title || '')}">${icon(ic)}<span>${label}</span></button>`;
}

function renderToolbar() {
  const s = app.status || {};
  let tools = `<button class="tool menu-button" data-act="menu" data-key="menu" aria-label="Menu">${icon('menu')}<span>Menu</span></button>`;
  tools += `<div class="title">${pages.find((p) => p.id === app.page).label}</div>`;
  switch (app.page) {
    case 'queue': {
      tools += tool('add', 'plus', 'Add', { title: 'Queue a file, or every video in a folder' });
      tools += s.paused
        ? tool('resume', 'play', 'Resume', { on: true, title: 'Carry on where the encodes stopped' })
        : tool('pause', 'pause', 'Pause', { title: 'Stop the running encodes where they are' });
      tools += tool('hold', 'hold', s.holding ? 'Holding' : 'Hold', { on: s.holding, title: 'Start no new jobs; running ones finish' });
      const failed = queueCounts().failed;
      if (failed) tools += tool('retry-failed', 'retry', 'Retry failed', { title: `Put the ${failed} failed job${failed === 1 ? '' : 's'} back in the queue` });
      break;
    }
    case 'history':
      tools += tool('refresh-history', 'refresh', 'Refresh');
      break;
    case 'stats':
      tools += tool('refresh-stats', 'refresh', 'Refresh', { disabled: app.stats.loading });
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
  morph($('#toolbar'), `${tools}<div class="spacer"></div><div class="chips">${chips.join('')}</div>`);
}

// renderPage draws the page; fresh puts it in afresh, which editing pages
// need so their inputs show the draft.
function renderPage(fresh) {
  const el = $('#page');
  let html = '';
  switch (app.page) {
    case 'queue': html = queuePage(); break;
    case 'history': html = historyPage(); break;
    case 'stats': html = statsPage(); break;
    case 'encoders': html = encodersPage(); break;
    case 'folders': html = foldersPage(); break;
    case 'settings': html = settingsPage(); break;
    case 'system': html = systemPage(); break;
  }
  if (fresh && editing.includes(app.page)) put(el, html);
  else morph(el, html);
}

function savebar() {
  const changed = dirty();
  if (!changed && !app.formError) return '';
  return `<div class="savebar">
    ${app.formError ? `<span class="form-error">${esc(app.formError)}</span>` : '<span class="note">You have unsaved changes</span>'}
    ${app.stale ? `<button class="button" data-act="reload-settings">Load the new settings</button>` : ''}
    <button class="button" data-act="discard" ${app.saving ? 'disabled' : ''}>Discard</button>
    <button class="button primary" data-act="save" ${!changed || app.saving || app.stale ? 'disabled' : ''}>${app.saving ? 'Saving…' : 'Save'}</button>
  </div>`;
}

function renderSavebar() {
  const old = document.querySelector('.savebar');
  const html = savebar();
  if (old) old.outerHTML = html || '<span hidden></span>';
  else if (html) $('#page').insertAdjacentHTML('beforeend', html);
  $('#page')._html = null; // what's shown is no longer what was put there
  renderToolbar();
}

// --- Queue ------------------------------------------------------------------

const actionLabels = { encode: 'Encoding', remux: 'Remuxing', retag: 'Fixing tags', rename: 'Renaming', none: 'Nothing to do' };
const doneLabels = { encode: 'Encoded', remux: 'Remuxed', retag: 'Tags fixed', rename: 'Renamed', none: 'Nothing to do' };

function stateIcon(state) {
  switch (state) {
    case 'running': return `<span class="state running" title="Running">${icon('spinner', 'spin')}</span>`;
    case 'done': return `<span class="state done" title="Done">${icon('check')}</span>`;
    case 'failed': return `<span class="state failed" title="Failed">${icon('alert')}</span>`;
    case 'canceled': return `<span class="state" title="Canceled">${icon('x')}</span>`;
    default: return `<span class="state" title="Queued">${icon('clock')}</span>`;
  }
}

const matches = (j, q) => !q || (j.input + '\n' + (j.output || '')).toLowerCase().includes(q);

// queueETA guesses how long the queue has left, from how long jobs have
// taken so far and how many run at once.
function queueETA(running, queued) {
  const s = app.status;
  const slots = (s.devices || []).reduce((n, d) => n + (s.slots[d.id] || 0), 0);
  if (!slots || s.paused) return 0;
  const all = [...app.jobs.values()];
  let per = median(all.filter((j) => j.state === 'done' && j.action === 'encode' && j.started && j.finished).map((j) => j.finished - j.started));
  if (!per) per = median(running.filter((j) => j.progress > 0.05 && j.elapsed).map((j) => j.elapsed / j.progress));
  if (!per) return 0;
  const left = running.reduce((n, j) => n + (j.eta > 0 ? j.eta : per / 2), 0) + (s.holding ? 0 : queued.length * per);
  return left / Math.min(slots, Math.max(1, running.length + (s.holding ? 0 : queued.length)));
}

// activity shows what each device is doing: its jobs, and how fast.
function activity(running) {
  const s = app.status;
  const tiles = (s.devices || []).filter((d) => s.slots[d.id] > 0 || running.some((j) => j.device === d.name)).map((d) => {
    const mine = running.filter((j) => j.device === d.name);
    const slots = Math.max(s.slots[d.id] || 0, mine.length);
    const fps = mine.reduce((n, j) => n + (j.fps || 0), 0);
    const dots = Array.from({ length: Math.min(slots, 8) }, (_, i) => `<i class="${i < mine.length ? 'on' : ''}"></i>`).join('');
    const name = d.name.replace(/ \([^)]*\)$/, '');
    return `<a class="device-tile ${mine.length ? 'busy' : ''}" href="#/encoders" data-key="dev-${esc(d.id)}" title="${esc(d.name)}: ${mine.length} of ${slots} running">
      <div class="dt-name">${esc(name)}${d.worker ? ` <span class="tag">${esc(d.worker)}</span>` : ''}</div>
      <div class="dt-row"><span class="slots">${dots}</span><span class="dt-fps">${!mine.length ? 'idle' : fps ? `${Math.round(fps)} fps` : 'starting'}</span></div></a>`;
  });
  return tiles.length ? `<div class="activity">${tiles.join('')}</div>` : '';
}

function queuePage() {
  if (!app.status) return '';
  const all = [...app.jobs.values()];
  const q = app.queue.search.trim().toLowerCase();
  const running = all.filter((j) => j.state === 'running').sort((a, b) => a.started - b.started || a.id - b.id);
  const queued = all.filter((j) => j.state === 'queued')
    .sort((a, b) => b.priority - a.priority || (b.next ? 1 : 0) - (a.next ? 1 : 0) || a.id - b.id);
  const failed = all.filter((j) => j.state === 'failed').sort((a, b) => b.finished - a.finished);
  const finished = all.filter((j) => j.state === 'done' || j.state === 'canceled').sort((a, b) => b.finished - a.finished || b.id - a.id);
  const tab = app.queue.tab;

  const head = `<div class="page-head">
    <div class="segmented" role="tablist">
      <button data-act="queue-tab" data-tab="queue" data-key="t-queue" role="tab" aria-selected="${tab === 'queue'}" class="${tab === 'queue' ? 'active' : ''}">Queue <span class="n">${running.length + queued.length + failed.length}</span></button>
      <button data-act="queue-tab" data-tab="finished" data-key="t-finished" role="tab" aria-selected="${tab === 'finished'}" class="${tab === 'finished' ? 'active' : ''}">Finished <span class="n">${finished.length}</span></button>
    </div>
    <label class="search">${icon('search')}<input type="search" data-search="queue" value="${esc(app.queue.search)}" placeholder="Filter by name" aria-label="Filter by name"></label>
  </div>`;

  if (tab === 'finished') {
    const rows = finished.filter((j) => matches(j, q));
    let body;
    if (!finished.length) body = `<div class="empty">${icon('clock')}<div>Nothing has finished since cav1ar started.</div><div>Older jobs are in the <a href="#/history">history</a>.</div></div>`;
    else if (!rows.length) body = `<div class="empty">${icon('search')}<div>No finished job matches.</div></div>`;
    else {
      body = `<div class="table-wrap"><table>
        <thead><tr><th></th><th>Name</th><th class="hide-small">What</th><th class="hide-small">Device</th>
          <th class="num">Size</th><th class="num hide-small">Took</th><th class="num">Finished</th><th></th></tr></thead>
        <tbody>${rows.map(finishedRow).join('')}</tbody></table></div>
        <p class="summary foot">Since cav1ar started. Older jobs are in the <a href="#/history">history</a>.</p>`;
    }
    return activity(running) + head + body;
  }

  const rows = [...running, ...queued, ...failed].filter((j) => matches(j, q));
  if (!running.length && !queued.length && !failed.length) {
    return activity(running) + head + `<div class="empty">${icon('check')}<div>Nothing to encode.</div>
      <div>New videos in watched folders show up here as they arrive, or <a href="#" data-act="add">add some</a>.</div></div>`;
  }
  const parts = [`${running.length} running`, `${queued.length} queued`];
  if (failed.length) parts.push(`<span class="err-text-inline">${failed.length} failed</span>`);
  const eta = queueETA(running, queued);
  if (eta) parts.push(`about ${fmtDur(eta)} left`);
  const body = rows.length
    ? `<div class="table-wrap"><table>
      <thead><tr><th></th><th>Name</th><th class="hide-small">Device</th><th class="hide-small">Status</th><th class="hide-small">Progress</th>
        <th class="num hide-small">Speed</th><th class="num hide-small">Time left</th><th></th></tr></thead>
      <tbody>${rows.map(queueRow).join('')}</tbody></table></div>`
    : `<div class="empty">${icon('search')}<div>No job matches.</div></div>`;
  return activity(running) + head + `<p class="summary">${parts.join(' · ')}</p>` + body;
}

function queueRow(j) {
  const name = base(j.input);
  const out = j.output && base(j.output) !== name ? base(j.output) : '';
  let status = 'Queued';
  if (j.state === 'running') status = j.step ? j.step[0].toUpperCase() + j.step.slice(1) : actionLabels[j.action] || 'Checking';
  else if (j.state === 'failed') status = 'Failed';
  const tags = [];
  if (j.state === 'queued' && j.next) tags.push('<span class="tag accent">next</span>');
  if (j.state === 'queued' && j.priority) tags.push(`<span class="tag" title="Folder priority">${j.priority > 0 ? '+' : ''}${j.priority}</span>`);
  const pct = j.state === 'running' && j.progress > 0 ? j.progress * 100 : 0;
  const progress = pct
    ? `<div class="progress" role="progressbar" aria-valuenow="${Math.floor(pct)}" aria-valuemin="0" aria-valuemax="100"><div class="bar"><i style="width:${pct.toFixed(1)}%"></i></div><span>${Math.floor(pct)}%</span></div>` : '';
  const speed = j.state === 'running' && j.speed > 0
    ? `<span title="${j.fps ? Math.round(j.fps) + ' fps' : ''}">${fmtSpeed(j.speed)}</span>` : '';
  let actions;
  if (j.state === 'queued') {
    actions = (j.next ? '' : `<button class="icon-button" data-act="next" data-id="${j.id}" data-key="next" title="Run next" aria-label="Run next">${icon('next')}</button>`)
      + `<button class="icon-button danger" data-act="cancel" data-id="${j.id}" data-key="cancel" title="Remove from the queue" aria-label="Remove from the queue">${icon('x')}</button>`;
  } else if (j.state === 'running') {
    actions = `<button class="icon-button danger" data-act="cancel" data-id="${j.id}" data-key="cancel" title="Cancel" aria-label="Cancel">${icon('x')}</button>`;
  } else {
    actions = `<button class="icon-button" data-act="retry" data-id="${j.id}" data-key="retry" title="Retry" aria-label="Retry">${icon('retry')}</button>`;
  }
  // On a phone the status, progress and time left go under the name.
  const small = [status, pct ? `${Math.floor(pct)}%` : '', j.state === 'running' && j.eta > 0 ? `${fmtDur(j.eta)} left` : ''].filter(Boolean).join(' · ');
  return `<tr class="click" data-job="${j.id}" data-key="job-${j.id}">
    <td class="icon">${stateIcon(j.state)}</td>
    <td class="name"><div class="main-line" title="${esc(j.input)}">${esc(name)} ${tags.join(' ')}</div>
      ${out ? `<div class="sub">→ ${esc(out)}</div>` : ''}${j.error ? `<div class="err-text">${esc(j.error)}</div>` : ''}
      <div class="sub show-small">${esc(small)}</div></td>
    <td class="muted hide-small">${esc(j.device || '')}</td>
    <td class="muted hide-small">${esc(status)}</td>
    <td class="hide-small">${progress}</td>
    <td class="num muted hide-small">${speed}</td>
    <td class="num muted hide-small">${j.state === 'running' && j.eta > 0 ? fmtDur(j.eta) : ''}</td>
    <td class="actions">${actions}</td></tr>`;
}

function finishedRow(j) {
  const name = base(j.output && j.state === 'done' ? j.output : j.input);
  const size = j.state === 'done' && j.output !== j.input && (j.action === 'encode' || j.action === 'remux')
    ? `${fmtSize(j.out_size)} ${sizeChange(j.in_size, j.out_size)}` : '';
  const took = j.started && j.finished ? fmtDur(j.finished - j.started) : '';
  return `<tr class="click" data-job="${j.id}" data-key="job-${j.id}">
    <td class="icon">${stateIcon(j.state)}</td>
    <td class="name"><div class="main-line" title="${esc(j.input)}">${esc(name)}</div>${j.note ? `<div class="sub">${esc(j.note)}</div>` : ''}</td>
    <td class="muted hide-small">${esc(j.state === 'done' ? doneLabels[j.action] || '' : 'Canceled')}</td>
    <td class="muted hide-small">${esc(j.device || '')}</td>
    <td class="num muted">${size}</td>
    <td class="num muted hide-small">${took}</td>
    <td class="num muted" title="${j.finished ? new Date(j.finished * 1000).toLocaleString() : ''}">${fmtAgo(j.finished)}</td>
    <td class="actions">${j.state === 'canceled' ? `<button class="icon-button" data-act="retry" data-id="${j.id}" data-key="retry" title="Retry" aria-label="Retry">${icon('retry')}</button>` : ''}</td></tr>`;
}

// --- History ----------------------------------------------------------------

async function loadHistory(reset) {
  const h = app.history;
  if (h.loading && !reset) return;
  if (reset) Object.assign(h, { entries: [], end: false, open: null, error: '' });
  const seq = ++h.seq; // a newer load makes this one's answer moot
  h.loading = true;
  if (app.page === 'history') renderPage(false);
  const before = h.entries.length ? h.entries[h.entries.length - 1].id : 0;
  try {
    const d = await api('GET', `/api/history?before=${before}&result=${h.filter}&q=${encodeURIComponent(h.search.trim())}`);
    if (seq !== h.seq) return;
    const entries = d.entries || [];
    h.entries.push(...entries);
    h.end = entries.length < 50;
  } catch (e) {
    if (seq !== h.seq) return;
    h.error = e.message;
  }
  h.loading = false;
  if (app.page === 'history') renderPage(false);
}

function historyPage() {
  const h = app.history;
  const filters = [['', 'All'], ['done', 'Done'], ['failed', 'Failed'], ['canceled', 'Canceled']];
  let body;
  if (h.error) body = `<div class="empty">${icon('alert')}<div>${esc(h.error)}</div></div>`;
  else if (!h.entries.length) {
    body = h.loading ? '<div class="empty">Loading…</div>'
      : `<div class="empty">${icon(h.search ? 'search' : 'clock')}<div>${h.search || h.filter ? 'Nothing matches.' : 'No finished jobs yet.'}</div></div>`;
  } else {
    body = `<div class="table-wrap"><table>
      <thead><tr><th></th><th>Name</th><th class="hide-small">What</th><th class="hide-small">Device</th>
        <th class="num">Size</th><th class="num hide-small">Took</th><th class="num">Finished</th></tr></thead>
      <tbody>${h.entries.map(historyRow).join('')}</tbody></table></div>
      ${h.end ? '' : `<p class="more"><button class="button" data-act="more-history" ${h.loading ? 'disabled' : ''}>${h.loading ? 'Loading…' : 'Load more'}</button></p>`}`;
  }
  return `<div class="page-head"><div class="segmented" role="tablist">${filters.map(([v, l]) =>
    `<button data-act="history-filter" data-filter="${v}" data-key="f-${v || 'all'}" role="tab" aria-selected="${h.filter === v}" class="${h.filter === v ? 'active' : ''}">${l}</button>`).join('')}</div>
    <label class="search">${icon('search')}<input type="search" data-search="history" value="${esc(h.search)}" placeholder="Search names" aria-label="Search names"></label></div>${body}`;
}

function historyRow(e) {
  const started = unix(e.started), finished = unix(e.finished);
  let size = '';
  if (e.in_size && e.out_size && e.output !== e.input && (e.action === 'encode' || e.action === 'remux')) {
    size = `${fmtSize(e.in_size)} → ${fmtSize(e.out_size)} ${sizeChange(e.in_size, e.out_size)}`;
  }
  const open = app.history.open === e.id;
  const speed = e.duration && started && finished > started && e.action === 'encode' ? ` · ${fmtSpeed(e.duration / (finished - started))} realtime` : '';
  const detail = open ? `<tr data-key="h-detail-${e.id}"><td></td><td colspan="6" class="muted detail-cell">
      <div>From ${esc(e.input)}</div>${e.output && e.output !== e.input ? `<div>To ${esc(e.output)}</div>` : ''}
      ${e.duration ? `<div>${fmtDur(e.duration)} of video${speed}</div>` : ''}
      ${finished ? `<div>Finished ${esc(new Date(finished * 1000).toLocaleString())}</div>` : ''}</td></tr>` : '';
  return `<tr class="click" data-history="${e.id}" data-key="h-${e.id}" aria-expanded="${open}">
    <td class="icon">${stateIcon(e.result)}</td>
    <td class="name"><div class="main-line" title="${esc(e.input)}">${esc(base(e.output && e.result === 'done' ? e.output : e.input))}</div>
      ${e.error ? `<div class="err-text">${esc(e.error)}</div>` : ''}</td>
    <td class="muted hide-small">${esc((e.result === 'done' ? doneLabels : actionLabels)[e.action] || e.action || '')}</td>
    <td class="muted hide-small">${esc(e.device || '')}</td>
    <td class="num muted">${size}</td>
    <td class="num muted hide-small">${started && finished ? fmtDur(finished - started) : ''}</td>
    <td class="num muted" title="${finished ? new Date(finished * 1000).toLocaleString() : ''}">${fmtAgo(finished)}</td></tr>${detail}`;
}

// --- Stats ------------------------------------------------------------------

async function loadStats() {
  const s = app.stats;
  s.loading = true;
  s.error = '';
  const days = s.days;
  try {
    const d = await api('GET', `/api/stats?days=${days}&tz=${-new Date().getTimezoneOffset()}`);
    if (days !== s.days) return;
    s.data = d;
  } catch (e) {
    s.error = e.message;
  }
  s.loading = false;
  if (app.page === 'stats') { renderPage(false); renderToolbar(); }
}

const dayKey = (d) => `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;

// buckets spreads the days' tallies over every day in the range, or over
// weeks when the range is long, so quiet days show as gaps.
function buckets(days, range) {
  const byDay = new Map(days.map((d) => [d.day, d]));
  const today = new Date();
  let first = new Date(today.getFullYear(), today.getMonth(), today.getDate() - range + 1);
  if (!range) {
    if (!days.length) return [];
    const [y, m, d] = days[0].day.split('-').map(Number);
    first = new Date(y, m - 1, d);
  }
  const span = Math.round((today - first) / 864e5) + 1;
  const step = span > 120 ? 7 : 1;
  const out = [];
  for (let at = new Date(first); at <= today; at.setDate(at.getDate() + step)) {
    const b = { start: new Date(at), days: step, done: 0, failed: 0, saved: 0, in_size: 0, out_size: 0 };
    for (let i = 0; i < step; i++) {
      const d = new Date(at);
      d.setDate(d.getDate() + i);
      const t = byDay.get(dayKey(d));
      if (!t) continue;
      b.done += t.done;
      b.failed += t.failed;
      b.in_size += t.in_size;
      b.out_size += t.out_size;
      b.saved += t.in_size - t.out_size;
    }
    out.push(b);
  }
  return out;
}

const fmtDay = (d, opts = { month: 'short', day: 'numeric' }) => d.toLocaleDateString(undefined, opts);

function bucketLabel(b) {
  if (b.days === 1) return fmtDay(b.start, { weekday: 'short', month: 'short', day: 'numeric' });
  const end = new Date(b.start);
  end.setDate(end.getDate() + b.days - 1);
  return `${fmtDay(b.start)} – ${fmtDay(end)}`;
}

// niceMax rounds a chart's top up to a clean number, with the step between
// its gridlines.
function niceMax(max) {
  if (max <= 0) return { top: 4, step: 1 };
  const raw = max / 4;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw);
  return { top: step * Math.ceil(max / step), step };
}

// barChart draws one series as columns: one per day or week, growing from
// the baseline, with a tooltip on each. The axis steps are clean numbers of
// unit, such as a GB for sizes.
function barChart(bs, value, fmt, unit = 1) {
  const W = Math.max(300, Math.round(($('#page')?.clientWidth || 800) - 66)), H = 200, left = 52, right = 8, top = 10, bottom = 26;
  const plotW = W - left - right, plotH = H - top - bottom;
  const max = Math.max(0, ...bs.map(value));
  const nice = niceMax(max / unit), yTop = nice.top * unit, step = nice.step * unit;
  const slot = plotW / Math.max(bs.length, 1);
  const bw = Math.max(2, Math.min(24, slot - 2));
  const y = (v) => top + plotH - (v / yTop) * plotH;
  let grid = '';
  for (let v = 0; v <= yTop + step / 2; v += step) {
    grid += `<line x1="${left}" x2="${W - right}" y1="${y(v).toFixed(1)}" y2="${y(v).toFixed(1)}" class="${v === 0 ? 'base' : 'grid'}"/>`;
    grid += `<text x="${left - 8}" y="${(y(v) + 4).toFixed(1)}" text-anchor="end">${esc(fmt(v, true))}</text>`;
  }
  let bars = '';
  bs.forEach((b, i) => {
    const v = value(b);
    const x = left + i * slot + (slot - bw) / 2;
    if (v > 0) {
      const h = Math.max(2, (v / yTop) * plotH), y0 = top + plotH, r = Math.min(4, bw / 2, h);
      bars += `<path class="bar-mark" d="M${x.toFixed(1)} ${y0}V${(y0 - h + r).toFixed(1)}q0 -${r} ${r} -${r}h${(bw - 2 * r).toFixed(1)}q${r} 0 ${r} ${r}V${y0}z"/>`;
    }
    bars += `<rect class="hit" x="${(left + i * slot).toFixed(1)}" y="${top}" width="${slot.toFixed(1)}" height="${plotH}" tabindex="0"
      data-tip="${esc(bucketLabel(b))}" data-tip-value="${esc(fmt(v, false))}"/>`;
  });
  const labels = new Set([0, Math.floor((bs.length - 1) / 2), bs.length - 1]);
  let xs = '';
  for (const i of labels) {
    if (!bs[i]) continue;
    const anchor = i === 0 ? 'start' : i === bs.length - 1 ? 'end' : 'middle';
    const x = i === 0 ? left : i === bs.length - 1 ? W - right : left + (i + 0.5) * slot;
    xs += `<text x="${x.toFixed(1)}" y="${H - 6}" text-anchor="${anchor}">${esc(fmtDay(bs[i].start))}</text>`;
  }
  return `<svg class="chart" viewBox="0 0 ${W} ${H}" role="img" aria-label="Per ${bs[0]?.days === 7 ? 'week' : 'day'}">${grid}${bars}${xs}</svg>`;
}

function statsPage() {
  const s = app.stats;
  const ranges = [[7, '7 days'], [30, '30 days'], [90, '90 days'], [0, 'All time']];
  const head = `<div class="page-head"><div class="segmented" role="tablist">${ranges.map(([v, l]) =>
    `<button data-act="stats-range" data-days="${v}" data-key="r-${v}" role="tab" aria-selected="${s.days === v}" class="${s.days === v ? 'active' : ''}">${l}</button>`).join('')}</div></div>`;
  if (s.error) return head + `<div class="empty">${icon('alert')}<div>${esc(s.error)}</div></div>`;
  const d = s.data;
  if (!d) return head + '<div class="empty">Loading…</div>';
  const t = d.totals;
  if (!t.done && !t.failed && !t.canceled) {
    return head + `<div class="empty">${icon('chart')}<div>Nothing finished ${s.days ? `in the last ${s.days} days` : 'yet'}.</div>
      <div>Stats come from the history of watched folders.</div></div>`;
  }
  const encoded = d.actions?.encode || 0;
  const saved = t.in_size - t.out_size;
  const smaller = t.in_size ? Math.round((1 - t.out_size / t.in_size) * 100) : 0;
  const speed = t.video_busy ? t.video / t.video_busy : 0;
  const finished = t.done + t.failed;
  const stat = (label, value, sub = '') => `<div class="stat"><div class="label">${label}</div><div class="value">${value}</div>${sub ? `<div class="sub">${sub}</div>` : ''}</div>`;
  const tiles = `<div class="stats">
    ${stat('Files encoded', fmtCount(encoded), t.done > encoded ? `${fmtCount(t.done - encoded)} more remuxed, retagged or renamed` : '')}
    ${stat('Space saved', fmtSize(saved) || '0 B', smaller ? `${smaller}% smaller than the sources` : '')}
    ${stat('Video encoded', t.video ? fmtDur(t.video) : '–', t.video ? '' : 'Counted for encodes from this version on')}
    ${stat('Speed', speed ? `${fmtSpeed(speed)}` : '–', speed ? 'Realtime, whole job, per encode' : 'Counted for encodes from this version on')}
    ${stat('Failed', fmtCount(t.failed), finished ? `${(t.failed / finished * 100).toFixed(t.failed && t.failed / finished < 0.1 ? 1 : 0)}% of jobs` : '')}
    ${stat('Time busy', fmtDur(t.busy), t.canceled ? `${fmtCount(t.canceled)} canceled` : '')}
  </div>`;

  const bs = buckets(d.days || [], s.days);
  const per = bs[0]?.days === 7 ? 'week' : 'day';
  // Sizes step in whole units of the biggest day's: GB, or TB.
  const maxSaved = Math.max(0, ...bs.map((b) => b.saved));
  const sizeUnit = 1024 ** Math.max(0, Math.floor(Math.log(Math.max(maxSaved, 1)) / Math.log(1024)));
  const axisSize = (v) => {
    if (!v) return '0';
    const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'], n = v / sizeUnit;
    return `${Number.isInteger(n) ? n : n.toFixed(1)} ${units[Math.round(Math.log(sizeUnit) / Math.log(1024))]}`;
  };
  const metrics = {
    files: ['Files', (b) => b.done, (v) => fmtCount(v), 1],
    saved: ['Space saved', (b) => Math.max(0, b.saved), (v, axis) => (axis ? axisSize(v) : fmtSize(v) || '0 B'), sizeUnit],
  };
  const [mLabel, mValue, mFmt, mUnit] = metrics[s.metric];
  const table = s.table ? `<div class="table-wrap inset"><table><thead><tr><th>${per === 'week' ? 'Week' : 'Day'}</th><th class="num">Done</th><th class="num">Failed</th><th class="num">Space saved</th></tr></thead>
    <tbody>${bs.filter((b) => b.done || b.failed).reverse().map((b) => `<tr><td>${esc(bucketLabel(b))}</td><td class="num">${fmtCount(b.done)}</td>
      <td class="num">${fmtCount(b.failed)}</td><td class="num">${fmtSize(Math.max(0, b.saved))}</td></tr>`).join('')}</tbody></table></div>` : '';
  const chart = `<div class="card"><h2>${mLabel} per ${per}
      <div class="segmented small">${Object.entries(metrics).map(([k, [l]]) =>
        `<button data-act="stats-metric" data-metric="${k}" data-key="m-${k}" class="${s.metric === k ? 'active' : ''}">${l}</button>`).join('')}</div></h2>
    <div class="chart-wrap">${barChart(bs, mValue, mFmt, mUnit)}</div>
    <div class="card-foot"><button class="link" data-act="stats-table">${s.table ? 'Hide the table' : 'Show as a table'}</button></div>${table}</div>`;

  const devices = (d.devices || []).filter((x) => x.done || x.failed);
  const maxDone = Math.max(1, ...devices.map((x) => x.done));
  const deviceCard = devices.length ? `<div class="card"><h2>Encoders</h2><div class="table-wrap flat"><table>
    <thead><tr><th>Device</th><th>Jobs done</th><th class="num">Failed</th><th class="num">Speed</th><th class="num hide-small">Size change</th><th class="num hide-small">Space saved</th></tr></thead>
    <tbody>${devices.map((x) => `<tr><td>${esc(x.device || 'Before devices were recorded')}</td>
      <td><div class="meter"><div class="bar"><i style="width:${(x.done / maxDone * 100).toFixed(1)}%"></i></div><span>${fmtCount(x.done)}</span></div></td>
      <td class="num muted">${x.failed ? fmtCount(x.failed) : ''}</td>
      <td class="num muted">${x.video_busy ? fmtSpeed(x.video / x.video_busy) : ''}</td>
      <td class="num muted hide-small">${sizeChange(x.in_size, x.out_size)}</td>
      <td class="num muted hide-small">${fmtSize(x.in_size - x.out_size)}</td></tr>`).join('')}</tbody></table></div></div>` : '';

  const actions = Object.entries(d.actions || {}).sort((a, b) => b[1] - a[1]);
  const actionCard = actions.length ? `<div class="card"><h2>What got done</h2><dl class="info">${actions.map(([a, n]) =>
    `<dt>${esc(doneLabels[a] || a || 'Other')}</dt><dd>${fmtCount(n)}</dd>`).join('')}</dl></div>` : '';
  const errorCard = (d.errors || []).length ? `<div class="card"><h2>Commonest failures</h2><div class="table-wrap flat"><table>
    <thead><tr><th>Error</th><th class="num">Times</th><th class="num">Last</th></tr></thead>
    <tbody>${d.errors.map((e) => `<tr><td class="err-cell">${esc(e.error || 'No reason given')}</td><td class="num">${fmtCount(e.count)}</td>
      <td class="num muted">${fmtAgo(unix(e.last))}</td></tr>`).join('')}</tbody></table></div></div>` : '';
  return head + tiles + chart + `<div class="two-col">${deviceCard}${actionCard}</div>` + errorCard;
}

// The chart's tooltip follows the pointer, or the focused column.
function showTip(target) {
  let tip = $('#tip');
  if (!target) {
    if (tip) tip.hidden = true;
    document.querySelectorAll('.chart .hit.on').forEach((h) => h.classList.remove('on'));
    return;
  }
  if (!tip) {
    tip = document.createElement('div');
    tip.id = 'tip';
    tip.setAttribute('role', 'tooltip');
    document.body.append(tip);
  }
  tip.replaceChildren();
  const v = document.createElement('strong');
  v.textContent = target.dataset.tipValue;
  const l = document.createElement('span');
  l.textContent = target.dataset.tip;
  tip.append(v, l);
  tip.hidden = false;
  const r = target.getBoundingClientRect(), tr = tip.getBoundingClientRect();
  const x = Math.min(Math.max(8, r.left + r.width / 2 - tr.width / 2), window.innerWidth - tr.width - 8);
  tip.style.left = `${x}px`;
  tip.style.top = `${Math.max(8, r.top + window.scrollY - tr.height - 8)}px`;
  document.querySelectorAll('.chart .hit.on').forEach((h) => h !== target && h.classList.remove('on'));
  target.classList.add('on');
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

// effectiveSlots is near enough how many at once the draft gives d: the
// daemon gives a CPU more than 1, but only whether a device is on matters
// here.
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
        <div class="controls">${speed}<select class="small" data-slots="${esc(d.id)}" aria-label="How many at once">${opts.map(([v, l]) =>
          `<option value="${v}" ${String(v) === String(shown) ? 'selected' : ''}>${l}</option>`).join('')}</select></div></div>`;
    }
    html += '</div>';
  }
  return html + '</div>' + savebar();
}

function busyText(d) {
  let n = 0, fps = 0;
  for (const j of app.jobs.values()) {
    if (j.state === 'running' && j.device === d.name) {
      n++;
      fps += j.fps || 0;
    }
  }
  return n ? `${n} running${fps ? ` · ${Math.round(fps)} fps` : ''}` : 'idle';
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
  return `<select class="small" data-key="${key}" data-type="num" aria-label="Speed">${opts.map(([o, l]) =>
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
      <div>Watched folders' videos are kept encoded, including new ones as they arrive. <a href="#" data-act="add-folder">Add one</a>.</div></div>`;
  } else {
    body = `<div class="table-wrap"><table>
      <thead><tr><th>Folder</th><th>Names come from</th><th>Priority</th><th></th></tr></thead>
      <tbody>${watch.map((f, i) => `<tr>
        <td class="name"><div class="main-line" title="${esc(f.path)}">${esc(f.path)}</div></td>
        <td><select class="small" data-watch="${i}" data-field="kind" aria-label="Names come from">
          ${[['', 'each file'], ['movies', 'movie folders'], ['shows', 'show folders']].map(([v, l]) =>
            `<option value="${v}" ${(f.kind || '') === v ? 'selected' : ''}>${l}</option>`).join('')}</select></td>
        <td><input type="number" class="small" data-watch="${i}" data-field="priority" value="${f.priority || 0}" min="-9" max="9" aria-label="Priority"></td>
        <td class="actions"><button class="icon-button danger" data-act="remove-folder" data-index="${i}" title="Stop watching" aria-label="Stop watching">${icon('x')}</button></td>
      </tr>`).join('')}</tbody></table></div>
      <p class="summary foot">Videos in folders with a higher priority are encoded first.</p>`;
  }
  return body + `<div class="card settings" style="margin-top:18px">${field('Videos already in a folder when it is first watched', select('watch_existing', [[true, 'handle them too'], [false, 'leave alone']]),
    'Whether the videos already in a newly watched folder are encoded, or only new ones.')}</div>` + savebar();
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
    ${field('Password', `<div class="with-button"><span class="password-state">${v.password_set ? 'Set' : 'Not set'}</span>
      <button class="button" data-act="password">${v.password_set ? 'Change' : 'Set password'}</button>
      ${v.password_set ? '<button class="button danger" data-act="remove-password">Remove</button>' : ''}</div>`,
      'Asked for before anything here can be seen or changed. Saved as soon as it is set, apart from the other settings.')}
  </div>`;
  return html + '</div>' + savebar();
}

// --- System -----------------------------------------------------------------

function applyTheme() {
  const t = store.get('theme') || 'auto';
  if (t === 'auto') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = t;
}

function systemPage() {
  const s = app.status;
  if (!s) return '';
  const stat = (label, value) => `<div class="stat"><div class="label">${label}</div><div class="value">${value}</div></div>`;
  const devices = s.devices || [];
  const theme = store.get('theme') || 'auto';
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
      <dt>More</dt><dd><a href="#/stats">Stats</a> · <a href="https://github.com/haydenkz/cav1ar" target="_blank" rel="noopener">github.com/haydenkz/cav1ar</a></dd>
    </dl></div>
    <div class="card settings"><h2>This browser</h2>
      ${field('Theme', `<div class="segmented flush">${[['auto', 'Match the system'], ['dark', 'Dark'], ['light', 'Light']].map(([v, l]) =>
        `<button data-act="theme" data-theme="${v}" data-key="th-${v}" class="${theme === v ? 'active' : ''}">${l}</button>`).join('')}</div>`, 'Kept in this browser only.')}
    </div>`;
}

// --- Job details --------------------------------------------------------------

let logTimer = null;

async function openDetail(id) {
  app.detail = id;
  app.full = false;
  app.lines = [];
  renderOverlay(true);
  await refreshLog();
  clearInterval(logTimer);
  if (app.detail !== id) return; // closed while the log loaded
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
      if (!app.dialog) renderOverlay(false);
    }
  } catch { /* shown as no output */ }
}

function closeOverlay() {
  app.detail = null;
  app.dialog = null;
  clearInterval(logTimer);
  renderOverlay(true);
}

function detailHTML() {
  const j = app.jobs.get(app.detail);
  if (!j) return '';
  const own = app.lines.filter((l) => l.startsWith('cav1ar: ') && !l.startsWith('cav1ar: ffmpeg '));
  const steps = own.map((l) => {
    const msg = l.slice(8);
    return msg.startsWith('▸ ') ? `<li class="stage">${esc(msg.slice(2))}</li>` : `<li>${esc(msg)}</li>`;
  }).join('');
  let size = '';
  if (j.in_size) size = `${fmtSize(j.in_size)}${j.out_size ? ` → ${fmtSize(j.out_size)} ${sizeChange(j.in_size, j.out_size)}` : ''}`;
  const rows = [
    ['Source', esc(j.input)],
    j.output && j.output !== j.input ? ['Result', esc(j.output)] : null,
    j.device ? ['Device', esc(j.device)] : null,
    size ? ['Size', size] : null,
    j.duration ? ['Video', fmtDur(j.duration)] : null,
    j.elapsed ? ['Time', `${fmtDur(j.elapsed)}${j.state === 'running' && j.eta > 0 ? `, about ${fmtDur(j.eta)} left` : ''}`] : null,
    j.state === 'running' && j.speed > 0 ? ['Speed', `${fmtSpeed(j.speed)}${j.fps ? ` · ${Math.round(j.fps)} fps` : ''}`] : null,
    j.note ? ['Note', esc(j.note)] : null,
    j.error ? ['Error', `<span class="err-inline">${esc(j.error)}</span>`] : null,
  ].filter(Boolean);
  let act = '';
  if (j.state === 'queued' && !j.next) act += `<button class="button" data-act="next" data-id="${j.id}" data-key="next">Run next</button>`;
  if (j.state === 'running' || j.state === 'queued') act += `<button class="button danger" data-act="cancel" data-id="${j.id}" data-key="cancel">${j.state === 'queued' ? 'Remove' : 'Cancel'}</button>`;
  else if (j.state !== 'done') act += `<button class="button" data-act="retry" data-id="${j.id}" data-key="retry">Retry</button>`;
  return `<div class="shade" data-act="close"></div><aside class="drawer" role="dialog" aria-modal="true" aria-label="${esc(base(j.input))}">
    <header>${stateIcon(j.state)}<h3 title="${esc(j.input)}">${esc(base(j.input))}</h3>${act}
      <button class="icon-button" data-act="close" data-key="close" title="Close" aria-label="Close">${icon('x')}</button></header>
    <div class="body"><dl class="info">${rows.map(([k, v]) => `<dt>${k}</dt><dd>${v}</dd>`).join('')}</dl>
      <div class="section-title">${app.full ? 'Output' : 'Steps'}
        <button class="button" data-act="full-log">${app.full ? 'Steps only' : 'Full output'}</button></div>
      ${app.full ? `<pre class="log" data-key="log">${esc(app.lines.join('\n')) || 'No output kept for this job.'}</pre>`
        : `<ul class="steps" data-key="steps">${steps || '<li>No steps yet.</li>'}</ul>`}
    </div></aside>`;
}

// --- Dialogs ----------------------------------------------------------------

async function browse(mode, path, onPick) {
  try {
    const d = await api('GET', '/api/browse?path=' + encodeURIComponent(path || '/'));
    app.dialog = { kind: 'browse', mode, path: d.path, parent: d.parent, entries: d.entries || [], onPick };
    renderOverlay(true);
  } catch (e) {
    if (path && path !== '/') return browse(mode, '/', onPick);
    toast(e.message, true);
  }
}

function dialogHTML() {
  const g = app.dialog;
  if (g.kind === 'password') {
    return `<div class="shade" data-act="close"></div><form class="dialog" id="password-form" role="dialog" aria-modal="true">
      <header>${app.view?.password_set ? 'Change the password' : 'Set a password'}</header>
      <div class="dialog-body">
        <input type="password" name="password" placeholder="New password, at least 8 characters" autocomplete="new-password" autofocus aria-label="New password">
        <input type="password" name="again" placeholder="The same again" autocomplete="new-password" aria-label="The same again">
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
  return `<div class="shade" data-act="close"></div><div class="dialog" role="dialog" aria-modal="true" aria-label="${titles[g.mode]}">
    <header>${titles[g.mode]}</header><div class="path">${esc(g.path)}</div>
    <ul>${up}${items || '<li class="nothing">Nothing here</li>'}</ul>
    <footer><button class="button" data-act="close">Cancel</button>${pickFolder}</footer></div>`;
}

// renderOverlay draws the open dialog or job details. Live updates change
// the details in place, keeping where the log is scrolled to, and following
// its end if it was at the end.
function renderOverlay(fresh) {
  const el = $('#overlay');
  let html = '';
  if (app.dialog) html = dialogHTML();
  else if (app.detail !== null) html = detailHTML();
  const log = el.querySelector('.log');
  const atEnd = log && log.scrollTop + log.clientHeight >= log.scrollHeight - 4;
  if (fresh) put(el, html);
  else morph(el, html);
  const now = el.querySelector('.log');
  if (now && (atEnd || now !== log)) now.scrollTop = now.scrollHeight;
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
      return renderPage(false);
    }
    return;
  }
  e.preventDefault();
  const id = Number(t.dataset.id);
  try {
    switch (t.dataset.act) {
      case 'menu': menu(!document.body.classList.contains('menu-open')); break;
      case 'menu-close': menu(false); break;
      case 'add': browse('add', '/', async (path) => {
        const r = await api('POST', '/api/add', { path });
        toast(r.added === 1 ? 'Added 1 video' : `Added ${r.added} videos`);
      }); break;
      case 'pause': await api('POST', '/api/pause', { paused: true }); break;
      case 'resume': await api('POST', '/api/pause', { paused: false }); break;
      case 'hold': await api('POST', '/api/hold', { holding: !app.status.holding }); break;
      case 'cancel': {
        const j = app.jobs.get(id);
        if (j?.state === 'running' && !confirm(`Cancel ${base(j.input)}? ${j.progress > 0 ? `It's ${Math.floor(j.progress * 100)}% done, and that` : 'What it has done'} will be lost.`)) break;
        await api('POST', `/api/jobs/${id}/cancel`, {});
        break;
      }
      case 'retry': await api('POST', `/api/jobs/${id}/retry`, {}); break;
      case 'next': await api('POST', `/api/jobs/${id}/next`, {}); toast('Runs next'); break;
      case 'retry-failed': {
        const failed = [...app.jobs.values()].filter((j) => j.state === 'failed');
        for (const j of failed) await api('POST', `/api/jobs/${j.id}/retry`, {});
        toast(failed.length === 1 ? 'Retrying 1 job' : `Retrying ${failed.length} jobs`);
        break;
      }
      case 'queue-tab': app.queue.tab = t.dataset.tab; renderPage(false); break;
      case 'close': closeOverlay(); break;
      case 'full-log': app.full = !app.full; renderOverlay(false); break;
      case 'refresh-history': loadHistory(true); break;
      case 'more-history': loadHistory(false); break;
      case 'history-filter': app.history.filter = t.dataset.filter; loadHistory(true); break;
      case 'stats-range': app.stats.days = Number(t.dataset.days); app.stats.data = null; renderPage(false); loadStats(); break;
      case 'stats-metric': app.stats.metric = t.dataset.metric; store.set('stats-metric', app.stats.metric); renderPage(false); break;
      case 'stats-table': app.stats.table = !app.stats.table; renderPage(false); break;
      case 'refresh-stats': loadStats(); renderToolbar(); break;
      case 'theme': store.set('theme', t.dataset.theme); applyTheme(); renderPage(false); break;
      case 'save': await save(); break;
      case 'discard': app.draft = clone(app.view.settings); app.formError = ''; app.stale = false; render(); break;
      case 'reload-settings':
        if (!dirty() || confirm('Load the settings as they are now? Your changes here will be lost.')) {
          await loadSettings();
          app.formError = '';
          render();
        }
        break;
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
        app.dialog = null;
        renderOverlay(true);
        await pick(t.dataset.path);
        break;
      }
      case 'password': app.dialog = { kind: 'password' }; renderOverlay(true); break;
      case 'remove-password':
        if (confirm('Stop asking for a password? Anyone who can reach this page could then control cav1ar.')) {
          await api('PUT', '/api/password', { password: '' });
          passwordSet(false);
          toast('Password removed');
        }
        break;
      case 'logout': await api('POST', '/api/logout', {}); showLogin(); break;
    }
  } catch (x) {
    toast(x.message, true);
  }
});

// passwordSet notes a change of password without touching the draft, which
// may have unsaved changes.
function passwordSet(set) {
  if (app.view) app.view.password_set = set;
  if (app.status) app.status.password_set = set;
  if (app.page === 'settings') {
    const card = document.querySelector('.password-state')?.closest('.field');
    if (card) card.outerHTML = field('Password', `<div class="with-button"><span class="password-state">${set ? 'Set' : 'Not set'}</span>
      <button class="button" data-act="password">${set ? 'Change' : 'Set password'}</button>
      ${set ? '<button class="button danger" data-act="remove-password">Remove</button>' : ''}</div>`,
      'Asked for before anything here can be seen or changed. Saved as soon as it is set, apart from the other settings.');
    $('#page')._html = null;
  }
  renderBanner();
  renderToolbar();
}

document.addEventListener('submit', async (e) => {
  if (e.target.id !== 'password-form') return;
  e.preventDefault();
  const f = e.target, err = f.querySelector('.form-error');
  if (f.password.value !== f.again.value) return (err.textContent = "The two don't match");
  try {
    await api('PUT', '/api/password', { password: f.password.value });
    app.dialog = null;
    renderOverlay(true);
    passwordSet(true);
    toast('Password saved');
  } catch (x) {
    err.textContent = x.message;
  }
});

let searchTimer = null;

function onEdit(e) {
  const t = e.target;
  if (t.dataset.search) {
    if (t.dataset.search === 'queue') {
      app.queue.search = t.value;
      renderPage(false);
    } else {
      app.history.search = t.value;
      clearTimeout(searchTimer);
      searchTimer = setTimeout(() => loadHistory(true), 300);
    }
    return;
  }
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
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    if (app.dialog || app.detail !== null) closeOverlay();
    else menu(false);
    return;
  }
  // "/" goes to the page's search box, as on many sites.
  const typing = /^(INPUT|SELECT|TEXTAREA)$/.test(document.activeElement?.tagName);
  if (e.key === '/' && !typing && !app.dialog && app.detail === null) {
    const box = document.querySelector('#page [data-search]');
    if (box) {
      e.preventDefault();
      box.focus();
    }
  }
});
for (const ev of ['pointerover', 'focusin']) {
  document.addEventListener(ev, (e) => showTip(e.target.closest?.('.chart .hit') || null));
}
window.addEventListener('scroll', () => showTip(null), { passive: true });
window.addEventListener('hashchange', route);
let resizeTimer = null;
window.addEventListener('resize', () => {
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(() => { if (app.page === 'stats') renderPage(false); }, 150); // the chart fits its width
});
window.addEventListener('beforeunload', (e) => { if (dirty()) e.preventDefault(); });
// The stats page keeps itself fresh while it's open.
setInterval(() => { if (app.page === 'stats' && !document.hidden && !app.stats.loading) loadStats(); }, 60000);
// Times like "5 min ago" move on.
setInterval(() => { if (app.page === 'queue' && app.queue.tab === 'finished') renderPage(false); }, 30000);

app.stats.metric = store.get('stats-metric') === 'saved' ? 'saved' : 'files';
applyTheme();
start();
