'use strict';

// 所有外部文本都通过 DOM 属性写入，避免提示词和接口返回内容成为可执行页面。
const $ = (selector, root = document) => root.querySelector(selector);
const $$ = (selector, root = document) => Array.from(root.querySelectorAll(selector));
const icons = {
  grid: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
  sparkles: '<path d="m12 3 2.5 6.5L21 12l-6.5 2.5L12 21l-2.5-6.5L3 12l6.5-2.5L12 3Z"/><path d="m20 2 .8 2.2L23 5l-2.2.8L20 8l-.8-2.2L17 5l2.2-.8L20 2Z"/>',
  users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2m20 0v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/><circle cx="9" cy="7" r="4"/>',
  history: '<path d="M3 11a9 9 0 1 1 2.9 6.7M3 3v8h8m1-4v5l3 2"/>',
  settings: '<path d="m10 3-.5 2-2 1.2-2-.6-2 3.4 1.5 1.4v2.3l-1.5 1.4 2 3.4 2-.6 2 1.2.5 2h4l.5-2 2-1.2 2 .6 2-3.4-1.5-1.4v-2.3L21 9l-2-3.4-2 .6L15 5l-.5-2H10Z"/><circle cx="12" cy="12" r="3"/>',
  image: '<rect x="3" y="3" width="18" height="18" rx="3"/><circle cx="8.5" cy="8.5" r="1.5"/><path d="m21 15-5-5L5 21"/>',
  database: '<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v14c0 1.7 3.6 3 8 3s8-1.3 8-3V5M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"/>',
  link: '<path d="m10 13 4-4m-7 6-2 2a4 4 0 0 0 6 6l4-4a4 4 0 0 0 0-6m2-4 2-2a4 4 0 0 0-6-6L9 5a4 4 0 0 0 0 6" transform="translate(0 -1)"/>',
  plus: '<path d="M12 5v14M5 12h14"/>',
  copy: '<rect x="8" y="8" width="12" height="13" rx="2"/><path d="M16 8V4a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2v10a2 2 0 0 0 2 2h4"/>',
  menu: '<path d="M4 6h16M4 12h16M4 18h16"/>',
  refresh: '<path d="M20 7a9 9 0 0 0-15-2L2 8m0-6v6h6m-4 9a9 9 0 0 0 15 2l3-3m0 6v-6h-6"/>',
  logout: '<path d="M9 4H5a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h4m6-12 4 4-4 4M9 12h10"/>',
  search: '<circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 5 5"/>',
  close: '<path d="m6 6 12 12M6 18 18 6"/>',
  upload: '<path d="M12 16V3m-5 5 5-5 5 5M4 16v4a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-4"/>',
  download: '<path d="M12 3v13m-5-5 5 5 5-5M4 16v4a1 1 0 0 0 1 1h14a1 1 0 0 0 1-1v-4"/>',
  sliders: '<path d="M5 3v5m0 4v9M12 3v11m0 4v3M19 3v2m0 4v12"/><circle cx="5" cy="10" r="2"/><circle cx="12" cy="16" r="2"/><circle cx="19" cy="7" r="2"/>',
  check: '<path d="m5 12 4 4L19 6"/>',
  'check-circle': '<circle cx="12" cy="12" r="9"/><path d="m8 12 3 3 5-6"/>',
  shield: '<path d="m12 3 8 3v6c0 5-8 9-8 9s-8-4-8-9V6l8-3Z"/><path d="m8 12 3 3 5-6"/>',
  edit: '<path d="m15 5 4 4M4 20l1-5L17 3a2.8 2.8 0 0 1 4 4L9 19l-5 1Z"/>',
  trash: '<path d="M3 6h18M9 6V3h6v3M5 6l1 15h12l1-15M10 10v7m4-7v7"/>',
  eye: '<path d="M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12Z"/><circle cx="12" cy="12" r="3"/>',
  alert: '<path d="m12 3 10 18H2L12 3Z"/><path d="M12 9v5m0 3h.01"/>',
  clock: '<circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/>',
  coins: '<ellipse cx="10" cy="6" rx="7" ry="3"/><path d="M3 6v5c0 1.7 3.1 3 7 3m7-8v4"/><ellipse cx="14" cy="13" rx="7" ry="3"/><path d="M7 13v5c0 1.7 3.1 3 7 3s7-1.3 7-3v-5"/>',
  left: '<path d="m14 6-6 6 6 6"/>',
  right: '<path d="m10 6 6 6-6 6"/>'
};

function icon(name) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  node.setAttribute('viewBox', '0 0 24 24');
  node.setAttribute('aria-hidden', 'true');
  // 图标内容仅来自本文件中的固定字典。
  node.innerHTML = icons[name] || icons.image;
  return node;
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value == null) continue;
    if (key === 'class') node.className = value;
    else if (key === 'text') node.textContent = String(value);
    else if (key.startsWith('on') && typeof value === 'function') node.addEventListener(key.slice(2), value);
    else if (key === 'disabled' || key === 'hidden') node[key] = value;
    else node.setAttribute(key, String(value));
  }
  for (const child of children.flat()) {
    if (child == null) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function mountIcons(root = document) {
  $$('[data-icon]', root).forEach(node => {
    const glyph = icon(node.dataset.icon);
    node.classList.contains('nav-item') ? node.prepend(glyph) : node.append(glyph);
    delete node.dataset.icon;
  });
}

const state = {
  authenticated: false, initialized: true, view: 'overview', accounts: [], models: [], settings: null,
  stats: null, logsPage: 1, logsTotal: 0, logLimit: 20, accountEdit: null, accountMode: 'cookie', accountSubmitting: false,
  references: [], generating: false, generationController: null, generationTimer: null,
  settingsDirty: false, logsRequest: 0, accountsRequest: 0, drawerRequest: 0
};
const titles = { overview: '概览', studio: '生图工作台', accounts: '账号池', logs: '生成记录', settings: '设置' };

// 仅允许普通网页地址和站内路径，图片链接不会接受脚本或任意本地文件地址。
function safeURL(value) {
  if (!value || typeof value !== 'string') return '';
  try {
    const url = new URL(value, location.origin);
    return ['http:', 'https:'].includes(url.protocol) ? url.href : '';
  } catch { return ''; }
}

function count(value, fraction = 0) {
  if (value == null || !Number.isFinite(Number(value))) return '—';
  return Number(value).toLocaleString('zh-CN', { maximumFractionDigits: fraction });
}
function bytes(value) {
  if (value == null || !Number.isFinite(Number(value))) return '—';
  const amount = Number(value);
  if (amount < 1024) return `${count(amount)} B`;
  if (amount < 1024 ** 2) return `${count(amount / 1024, 1)} KB`;
  if (amount < 1024 ** 3) return `${count(amount / 1024 ** 2, 1)} MB`;
  return `${count(amount / 1024 ** 3, 2)} GB`;
}
function date(value, compact = false) {
  if (!value || String(value).startsWith('0001-')) return '—';
  const time = new Date(value);
  if (Number.isNaN(time.getTime())) return '—';
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', ...(compact ? {} : { year: 'numeric', second: '2-digit' }), hour12: false }).format(time);
}
function duration(value) {
  if (value == null) return '—';
  const seconds = Number(value) / 1000;
  return seconds >= 60 ? `${Math.floor(seconds / 60)} 分 ${Math.round(seconds % 60)} 秒` : `${count(seconds, 1)} 秒`;
}
function statusBadge(value) {
  const key = String(value || 'unknown').toLowerCase();
  const labels = { success: '已完成', failed: '失败', error: '异常', active: '正常', ready: '正常', healthy: '正常', valid: '正常', ok: '正常', expired: '已过期', refreshing: '刷新中', running: '生成中', pending: '等待续期', unverified: '待验证', disabled: '已停用', unknown: '未验证', missing: '待登录', missing_credentials: '待登录', cooldown: '稍后重试' };
  const cls = ['active', 'ready', 'healthy', 'valid', 'ok'].includes(key) ? 'success' : ['pending', 'unverified', 'cooldown', 'missing', 'missing_credentials', 'unknown'].includes(key) ? 'warning' : key;
  return el('span', { class: `badge ${['success', 'failed', 'error', 'expired', 'refreshing', 'running', 'disabled', 'warning'].includes(cls) ? cls : ''}`, text: labels[key] || key });
}
function errorMessage(error) { return error instanceof Error ? error.message : String(error || '操作失败'); }
function toast(message, isError = false) {
  const item = el('div', { class: `toast${isError ? ' error' : ''}` }, icon(isError ? 'alert' : 'check-circle'), el('span', { text: message }));
  $('#toast-container').append(item);
  setTimeout(() => item.remove(), isError ? 6000 : 3500);
}
function empty(target, title, description, iconName = 'image', action) {
  const box = el('div', { class: 'empty-state' }, el('span', { class: 'soft-icon' }, icon(iconName)), el('h3', { text: title }), description ? el('p', { text: description }) : null, action);
  target.replaceChildren(box);
}
function loading(target, message = '正在加载') { target.replaceChildren(el('div', { class: 'loading-state' }, el('span', { class: 'spinner' }), message)); }
function showError(target, error, retry) {
  empty(target, '暂时无法加载', errorMessage(error), 'alert', retry ? el('button', { class: 'button secondary tiny', onclick: retry }, '重试') : null);
}
function busy(button, active, text) {
  if (active) {
    button.dataset.original = button.textContent;
    button.disabled = true;
    button.replaceChildren(el('span', { class: 'spinner' }), text || '处理中');
  } else {
    button.disabled = false;
    button.textContent = button.dataset.original || text || '确认';
    delete button.dataset.original;
  }
}

async function api(path, options = {}) {
  const init = { credentials: 'same-origin', ...options, headers: { ...(options.headers || {}) } };
  if (options.body && !(options.body instanceof FormData)) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(options.body);
  }
  let response;
  try { response = await fetch(path, init); }
  catch (error) {
    if (error.name === 'AbortError') throw error;
    throw new Error('无法连接服务，请检查网络后重试。');
  }
  const text = await response.text();
  let data;
  try { data = text ? JSON.parse(text) : {}; } catch { data = {}; }
  if (!response.ok) {
    if (response.status === 401 && state.authenticated && !path.includes('/auth/')) showAuth(true);
    const message = typeof data.error === 'string' ? data.error : data.error?.message || data.message;
    throw new Error(message || `请求失败（${response.status}）`);
  }
  return data;
}

function showAuth(initialized, username = '') {
  state.authenticated = false;
  state.initialized = initialized;
  state.generationController?.abort();
  closeDrawer();
  $$('dialog[open]').forEach(dialog => dialog.close());
  $('#boot').hidden = true;
  $('#app').hidden = true;
  $('#auth-view').hidden = false;
  $('#auth-title').textContent = initialized ? '欢迎回来' : '创建你的工作空间';
  $('#auth-description').textContent = initialized ? '登录，继续你的创作。' : '设置管理员账号，开启创作。';
  $('#auth-submit').textContent = initialized ? '登录' : '创建并进入';
  $('#auth-username').value = username;
  $('#auth-password').value = '';
  $('#auth-password').autocomplete = initialized ? 'current-password' : 'new-password';
  $('#auth-password').minLength = initialized ? 1 : 10;
  $('#auth-error').hidden = true;
}
async function showApp(username) {
  state.authenticated = true;
  $('#boot').hidden = true;
  $('#auth-view').hidden = true;
  $('#app').hidden = false;
  $('#user-name').textContent = username || '管理员';
  $('#user-initial').textContent = (username || 'A').slice(0, 1).toUpperCase();
  // 重新登录后重新取数，避免沿用上一个账号的数据。
  state.accounts = [];
  state.models = [];
  state.settings = null;
  state.settingsDirty = false;
  clearReferences();
  await Promise.allSettled([loadModels(), loadSettings(false)]);
  if (!state.authenticated) return;
  navigate(location.hash.slice(1) in titles ? location.hash.slice(1) : 'overview');
}
async function navigate(view, refresh = false) {
  if (!state.authenticated || !(view in titles)) return;
  state.view = view;
  $$('.view').forEach(node => { node.hidden = node.id !== `view-${view}`; });
  $$('.nav-item').forEach(node => node.classList.toggle('active', node.dataset.view === view));
  $('#breadcrumb-title').textContent = titles[view];
  document.title = `${titles[view]} · Adobe 2API`;
  if (location.hash !== `#${view}`) history.replaceState(null, '', `#${view}`);
  closeSidebar();
  if (view === 'overview') await loadOverview();
  if (view === 'accounts') await loadAccounts();
  if (view === 'logs') await loadLogs();
  if (view === 'settings' && (!state.settings || refresh) && !state.settingsDirty) await loadSettings();
  if (view === 'studio' && (!state.models.length || refresh)) await loadModels();
}
function closeSidebar() { $('#sidebar').classList.remove('open'); $('#sidebar-scrim').hidden = true; }

async function loadModels() {
  try {
    const data = await api('/api/admin/models');
    state.models = Array.isArray(data.items) ? data.items : [];
    const select = $('#generate-model');
    const old = select.value;
    select.replaceChildren(...state.models.map(model => el('option', { value: model.id, text: model.name || model.id })));
    if (state.models.some(model => model.id === old)) select.value = old;
    else {
      const banana = state.models.find(model => /banana|香蕉/i.test(`${model.id} ${model.name}`));
      if (banana) select.value = banana.id;
    }
    if (!state.models.length) select.append(el('option', { value: '', text: '暂无可用模型' }));
    $('#generate-submit').disabled = !state.models.length || state.generating;
    $('#model-count').textContent = `${count(state.models.length)} 个可用模型`;
    updateModelOptions();
  } catch (error) {
    $('#generate-model').replaceChildren(el('option', { value: '', text: '模型加载失败' }));
    $('#generate-submit').disabled = true;
    $('#model-count').textContent = '模型暂时不可用';
    toast(errorMessage(error), true);
  }
}
function updateModelOptions() {
  const model = state.models.find(item => item.id === $('#generate-model').value);
  const resolutions = model?.resolutions?.length ? model.resolutions : ['1K', '2K', '4K'];
  const select = $('#generate-resolution');
  const previous = select.value;
  select.replaceChildren(...resolutions.map(value => el('option', { value, text: value })));
  if (resolutions.includes(previous)) select.value = previous;
  updateRatioOptions();
}
function updateRatioOptions() {
  const model = state.models.find(item => item.id === $('#generate-model').value);
  let ratios = model?.ratios?.length ? model.ratios : ['1:1', '16:9', '9:16', '4:3', '3:4'];
  // 部分模型的大图请求必须指定比例，按模型能力联动选项，不提交无效的自动比例。
  if (model?.high_resolution_requires_ratio && ['2K', '4K'].includes($('#generate-resolution').value)) ratios = ratios.filter(value => value !== 'auto');
  const select = $('#generate-ratio');
  const previous = select.value;
  select.replaceChildren(...ratios.map(value => el('option', { value, text: value === 'auto' ? '自动' : value })));
  if (ratios.includes(previous)) select.value = previous;
  else if (previous === 'auto' && ratios.includes('1:1')) select.value = '1:1';
}

function renderStats(stats) {
  const cards = [
    { title: '累计生成请求', value: count(stats.total_requests ?? stats.requests), icon: 'sparkles', foot: `今日 ${count(stats.today_requests)} 次` },
    { title: '可用账号', value: count(stats.enabled_accounts), icon: 'users', foot: `共 ${count(stats.accounts)} 个账号`, positive: true },
    { title: '已记录积分', value: count(stats.credits_used, 2), icon: 'coins', foot: `${count(stats.success_requests)} 次成功生成` },
    { title: '已存储图片', value: count(stats.image_count), icon: 'image', foot: `占用 ${bytes(stats.storage_bytes)}` }
  ];
  $('#stats-grid').replaceChildren(...cards.map(card => el('div', { class: 'card stat-card' }, el('div', { class: 'stat-top' }, el('span', { text: card.title }), el('span', { class: 'soft-icon' }, icon(card.icon))), el('div', { class: 'stat-value', text: card.value }), el('div', { class: 'stat-foot' }, card.positive ? el('span', { class: 'status-dot' }) : null, el('span', { class: card.positive ? 'positive' : '', text: card.foot })))));
  $('#storage-summary').textContent = `${count(stats.image_count)} 张图片 · 占用 ${bytes(stats.storage_bytes)}`;
  updateStorage();
}
function updateStorage() {
  const max = Number(state.settings?.max_storage_mb || 0) * 1024 ** 2;
  const used = Number(state.stats?.storage_bytes || 0);
  $('#storage-progress-bar').style.width = `${max ? Math.min(100, used / max * 100) : 0}%`;
  $('#storage-capacity').textContent = max ? `${bytes(used)} / ${bytes(max)}` : `${bytes(used)} · 未设置容量上限`;
}
async function loadOverview() {
  if (!state.stats) {
    $('#stats-grid').replaceChildren(...Array.from({ length: 4 }, () => el('div', { class: 'card stat-card' }, el('div', { class: 'skeleton' }), el('div', { class: 'stat-value' }, el('span', { class: 'skeleton' })), el('div', { class: 'skeleton' }))));
  }
  loading($('#recent-logs'));
  const results = await Promise.allSettled([api('/api/admin/stats'), api('/api/admin/logs?page=1&limit=5')]);
  if (results[0].status === 'fulfilled') {
    state.stats = results[0].value;
    renderStats(state.stats);
  } else {
    toast(errorMessage(results[0].reason), true);
    if (!state.stats) $('#stats-grid').replaceChildren(el('div', { class: 'card stat-card muted', text: '统计暂时不可用' }));
  }
  if (results[1].status === 'fulfilled') {
    const items = results[1].value.items || [];
    if (!items.length) empty($('#recent-logs'), '还没有生成记录', '开始创作你的第一张图片。', 'sparkles', el('button', { class: 'button secondary tiny', onclick: () => navigate('studio') }, '开始创作'));
    else $('#recent-logs').replaceChildren(...items.map(log => {
      const row = el('div', { class: 'recent-row', role: 'button', tabindex: '0', onclick: () => openLog(log.id), onkeydown: event => { if (event.key === 'Enter' || event.key === ' ') { event.preventDefault(); openLog(log.id); } } }, thumbnail(log.images?.[0]), el('div', { class: 'recent-copy' }, el('div', { class: 'recent-prompt', text: log.prompt || '图片创作' }), el('div', { class: 'recent-info' }, el('span', { text: modelName(log.model) }), el('span', { text: duration(log.duration_ms) }), el('span', { text: date(log.created_at, true) }))), statusBadge(log.status));
      return row;
    }));
  } else showError($('#recent-logs'), results[1].reason, loadOverview);
  updateEndpoints();
}
function modelName(id) { return state.models.find(model => model.id === id)?.name || id || '—'; }
function updateEndpoints() {
  const base = (state.settings?.public_url || location.origin).replace(/\/+$/, '');
  $('#openai-endpoint').textContent = `${base}/v1`;
  $('#gemini-endpoint').textContent = `${base}/v1beta`;
}
function thumbnail(image) {
  const url = safeURL(image?.url);
  if (!url || image.deleted) return el('span', { class: 'image-thumb' }, icon('image'));
  return el('img', { class: 'image-thumb', src: url, alt: '生成图片', loading: 'lazy', decoding: 'async', onerror: event => event.target.replaceWith(el('span', { class: 'image-thumb' }, icon('image'))) });
}

async function loadAccounts() {
  const request = ++state.accountsRequest;
  $('#accounts-body').replaceChildren();
  loading($('#accounts-empty'));
  try {
    const data = await api('/api/admin/accounts');
    if (request !== state.accountsRequest) return;
    state.accounts = data.items || [];
    renderAccounts();
  } catch (error) { if (request === state.accountsRequest) showError($('#accounts-empty'), error, loadAccounts); }
}
function renderAccounts() {
  const query = $('#account-search').value.trim().toLowerCase();
  const items = state.accounts.filter(account => `${account.name || ''} ${account.email || ''}`.toLowerCase().includes(query));
  $('#accounts-count').textContent = `${count(state.accounts.length)} 个账号`;
  $('#accounts-empty').replaceChildren();
  if (!items.length) {
    $('#accounts-body').replaceChildren();
    empty($('#accounts-empty'), query ? '没有找到匹配的账号' : '添加你的第一个账号', query ? '试试其他关键词。' : '导入 Cookie 或自动获取登录账号。', 'users', query ? null : el('button', { class: 'button secondary tiny', onclick: () => openAccount() }, '添加账号'));
    return;
  }
  $('#accounts-body').replaceChildren(...items.map(account => {
    const toggle = el('button', { class: 'switch', role: 'switch', 'aria-checked': Boolean(account.enabled), 'aria-label': `${account.enabled ? '停用' : '启用'}${account.name || '账号'}`, onclick: async () => {
      toggle.disabled = true;
      try { await api(`/api/admin/accounts/${encodeURIComponent(account.id)}`, { method: 'PUT', body: { enabled: !account.enabled } }); account.enabled = !account.enabled; renderAccounts(); toast(account.enabled ? '账号已启用' : '账号已停用'); }
      catch (error) { toast(errorMessage(error), true); toggle.disabled = false; }
    } });
    const refresh = el('button', { class: 'icon-button', title: '刷新登录', 'aria-label': '刷新登录', onclick: () => refreshAccount(account, refresh) }, icon('refresh'));
    return el('tr', {}, el('td', {}, el('div', { class: 'account-identity' }, el('span', { class: 'avatar', text: (account.name || account.email || 'A').slice(0, 1).toUpperCase() }), el('div', {}, el('div', { class: 'account-name', text: account.name || '未命名账号' }), el('div', { class: 'account-email', text: account.email || '—' })))), el('td', { title: account.last_error || '' }, statusBadge(account.enabled ? account.status : 'disabled')), el('td', { text: count(account.credits, 2) }), el('td', { text: date(account.last_refresh, true) }), el('td', {}, toggle), el('td', {}, el('div', { class: 'table-actions' }, refresh, el('button', { class: 'icon-button', title: '编辑账号', 'aria-label': '编辑账号', onclick: () => openAccount(account) }, icon('edit')), el('button', { class: 'icon-button danger', title: '删除账号', 'aria-label': '删除账号', onclick: () => deleteAccount(account) }, icon('trash')))));
  }));
}
async function refreshAccount(account, button) {
  button.disabled = true;
  button.replaceChildren(el('span', { class: 'spinner' }));
  try { await api(`/api/admin/accounts/${encodeURIComponent(account.id)}/refresh`, { method: 'POST' }); toast('账号登录已刷新'); await loadAccounts(); }
  catch (error) { toast(errorMessage(error), true); button.disabled = false; button.replaceChildren(icon('refresh')); await loadAccounts(); }
}
function setAccountMode(mode) {
  if (state.accountSubmitting) return;
  state.accountMode = mode;
  $('#account-cookie').hidden = mode !== 'cookie';
  $('#account-browser').hidden = mode !== 'browser';
  $$('[data-account-mode]').forEach(button => button.classList.toggle('selected', button.dataset.accountMode === mode));
  const form = $('#account-form');
  form.elements.cookie.required = mode === 'cookie' && !state.accountEdit;
  form.elements.name.required = Boolean(state.accountEdit);
  $('#account-submit').textContent = state.accountEdit ? '保存账号' : mode === 'browser' ? '获取账号' : '导入账号';
  $('#account-form-error').hidden = true;
}
function openAccount(account = null) {
  if (state.accountSubmitting) return;
  state.accountEdit = account;
  $('#account-form').reset();
  $('#account-form-error').hidden = true;
  $('#account-dialog-title').textContent = account ? '编辑账号' : '添加账号';
  $('#account-tabs').hidden = Boolean(account);
  setAccountMode('cookie');
  const form = $('#account-form');
  if (account) {
    form.elements.name.value = account.name || '';
  }
  form.elements.name.placeholder = account ? '账号名称' : '留空自动命名';
  form.elements.cookie.placeholder = account ? '留空保留当前 Cookie' : '粘贴已登录账号的 Cookie';
  $('#account-dialog').showModal();
}
async function confirmAction(title, message, label = '确认') {
  const dialog = $('#confirm-dialog');
  $('#confirm-title').textContent = title;
  $('#confirm-message').textContent = message;
  $('#confirm-submit').textContent = label;
  dialog.showModal();
  return new Promise(resolve => dialog.addEventListener('close', () => resolve(dialog.returnValue === 'confirm'), { once: true }));
}
async function deleteAccount(account) {
  if (!await confirmAction('删除账号', `确认删除“${account.name || account.email || '此账号'}”？`, '删除账号')) return;
  try { await api(`/api/admin/accounts/${encodeURIComponent(account.id)}`, { method: 'DELETE' }); toast('账号已删除'); await loadAccounts(); }
  catch (error) { toast(errorMessage(error), true); }
}

function parameters(log) {
  if (log.parameters && typeof log.parameters === 'object') return log.parameters;
  try { return JSON.parse(log.parameters || '{}'); } catch { return {}; }
}
async function loadLogs() {
  const request = ++state.logsRequest;
  $('#logs-body').replaceChildren();
  $('#logs-pagination').replaceChildren();
  loading($('#logs-empty'));
  const query = new URLSearchParams({ page: String(state.logsPage), limit: String(state.logLimit), q: $('#log-search').value.trim(), status: $('#log-status').value });
  try {
    const data = await api(`/api/admin/logs?${query}`);
    if (request !== state.logsRequest) return;
    const items = data.items || [];
    state.logsTotal = Number(data.total || 0);
    $('#logs-count').textContent = `${count(state.logsTotal)} 条记录`;
    $('#logs-empty').replaceChildren();
    if (!items.length) empty($('#logs-empty'), '暂无生成记录', $('#log-search').value || $('#log-status').value ? '试试其他搜索条件。' : '生成图片后，记录将出现在这里。', 'history');
    else $('#logs-body').replaceChildren(...items.map(log => {
      const params = parameters(log);
      const row = el('tr', { tabindex: '0', 'aria-label': `查看生成详情：${log.prompt || log.id}`, onclick: () => openLog(log.id), onkeydown: event => { if (event.key === 'Enter') openLog(log.id); } }, el('td', {}, el('div', { class: 'log-creation' }, thumbnail(log.images?.[0]), el('div', { class: 'log-copy' }, el('div', { class: 'log-prompt', text: log.prompt || '图片创作', title: log.prompt || '' }), el('div', { class: 'log-account', text: log.account_name || '—' })))), el('td', {}, el('div', { class: 'cell-main', text: modelName(log.model) }), el('div', { class: 'cell-sub', text: log.protocol || '—' })), el('td', {}, el('div', { class: 'cell-main', text: params.aspect_ratio || '自动' }), el('div', { class: 'cell-sub', text: params.resolution || params.size || '—' })), el('td', {}, el('div', { class: 'cell-main', text: duration(log.duration_ms) }), el('div', { class: 'cell-sub', text: `${count(log.credits, 2)} 积分` })), el('td', {}, statusBadge(log.status)), el('td', { class: 'cell-sub', text: date(log.created_at, true) }), el('td', {}, icon('right')));
      return row;
    }));
    renderPagination();
  } catch (error) { if (request === state.logsRequest) showError($('#logs-empty'), error, loadLogs); }
}
function renderPagination() {
  const pages = Math.max(1, Math.ceil(state.logsTotal / state.logLimit));
  const from = state.logsTotal ? (state.logsPage - 1) * state.logLimit + 1 : 0;
  const to = Math.min(state.logsPage * state.logLimit, state.logsTotal);
  $('#logs-pagination').replaceChildren(el('span', { text: `${from}–${to} / 共 ${count(state.logsTotal)} 条` }), el('div', { class: 'pagination-buttons' }, el('button', { class: 'icon-button', disabled: state.logsPage <= 1, 'aria-label': '上一页', onclick: () => { state.logsPage--; loadLogs(); } }, icon('left')), el('span', { class: 'page-number', text: state.logsPage }), el('span', { class: 'muted', text: `/ ${pages}` }), el('button', { class: 'icon-button', disabled: state.logsPage >= pages, 'aria-label': '下一页', onclick: () => { state.logsPage++; loadLogs(); } }, icon('right'))));
}
let drawerOpener = null;
function closeDrawer() {
  ++state.drawerRequest;
  $('#log-drawer').hidden = true;
  $('#drawer-scrim').hidden = true;
  // 关闭详情后释放图片与诊断文本，不让隐藏抽屉继续保留整条记录。
  $('#log-detail').replaceChildren();
  document.body.style.overflow = '';
  if (drawerOpener instanceof HTMLElement && drawerOpener.isConnected) drawerOpener.focus({ preventScroll: true });
  drawerOpener = null;
}
async function openLog(id) {
  drawerOpener = document.activeElement;
  const request = ++state.drawerRequest;
  $('#log-drawer').hidden = false;
  $('#drawer-scrim').hidden = false;
  document.body.style.overflow = 'hidden';
  $('#drawer-close').focus();
  loading($('#log-detail'));
  try {
    const log = await api(`/api/admin/logs/${encodeURIComponent(id)}`);
    if (request !== state.drawerRequest) return;
    renderLogDetail(log);
  } catch (error) { if (request === state.drawerRequest) showError($('#log-detail'), error, () => openLog(id)); }
}
function fact(label, value) { return el('div', { class: 'detail-fact' }, el('label', { text: label }), el('span', { text: value == null || value === '' ? '—' : value })); }
function imageTile(image, label = '生成图片') {
  const url = safeURL(image.url);
  if (image.deleted || !url) return el('div', { class: 'detail-image deleted-image' }, icon('image'), '图片已清理');
  const img = el('img', { src: url, alt: label, loading: 'lazy', decoding: 'async', onerror: () => img.replaceWith(el('div', { class: 'deleted-image' }, icon('image'), '图片不可用')) });
  return el('button', { class: 'detail-image', 'aria-label': `查看${label}`, onclick: () => openImage(image) }, img);
}
// 请求与响应按需渲染，关闭折叠项后释放内容节点，避免长记录占用过多页面内存。
function upstreamDetail(label, headers, body) {
  const hasHeaders = headers && typeof headers === 'object' && !Array.isArray(headers) && Object.keys(headers).length;
  const hasBody = typeof body === 'string' && body.trim();
  if (!hasHeaders && !hasBody) return null;
  const details = el('details', { class: 'upstream-details' }, el('summary', { text: label }));
  details.addEventListener('toggle', () => {
    details.querySelector('.upstream-content')?.remove();
    if (!details.open) return;
    // 诊断内容只能作为文本展示，即使包含 HTML 也不会执行。
    details.append(el('div', { class: 'upstream-content' },
      hasHeaders ? el('h4', { text: label === '请求内容' ? '请求头' : '响应头' }) : null,
      hasHeaders ? el('pre', { class: 'detail-params', text: JSON.stringify(headers, null, 2) }) : null,
      hasBody ? el('h4', { text: label === '请求内容' ? '请求参数' : '响应正文' }) : null,
      hasBody ? el('pre', { class: 'detail-params', text: body }) : null));
  });
  return details;
}
function renderLogDetail(log) {
  const params = parameters(log);
  const children = [el('div', { class: 'detail-status' }, statusBadge(log.status), el('span', { class: 'small muted', text: date(log.created_at) }))];
  if (log.images?.length) children.push(el('div', { class: `detail-images${log.images.length === 1 ? ' single' : ''}` }, ...log.images.map(image => imageTile(image))));
  if (log.error) children.push(el('p', { class: 'form-error', text: log.error }));
  children.push(el('div', { class: 'detail-section' }, el('h3', { text: '提示词' }), el('div', { class: 'detail-prompt', text: log.prompt || '—' })));
  children.push(el('div', { class: 'detail-facts' }, fact('模型', modelName(log.model)), fact('账号', log.account_name), fact('耗时', duration(log.duration_ms)), fact('消耗积分', count(log.credits, 2)), fact('画面比例', params.aspect_ratio || '自动'), fact('分辨率', params.resolution || params.size), fact('接口协议', log.protocol), fact('请求状态', log.http_status), fact('请求地址', log.endpoint), fact('记录编号', log.id)));
  if (log.references?.length) children.push(el('div', { class: 'detail-section' }, el('h3', { text: `参考图 · ${log.references.length} 张` }), el('div', { class: 'detail-images detail-reference' }, ...log.references.map(image => imageTile(image, '参考图')))));
  children.push(el('div', { class: 'detail-section' }, el('h3', { text: '请求参数' }), el('pre', { class: 'detail-params', text: JSON.stringify(params, null, 2) })));
  const calls = (log.upstream || []).map(call => el('div', { class: 'upstream-item' }, el('div', { class: 'upstream-top' }, el('strong', { text: call.method || 'GET' }), el('span', { text: duration(call.duration_ms) }), el('span', { class: `badge ${call.status >= 200 && call.status < 400 ? 'success' : 'failed'}`, text: call.status || '—' })), el('div', { class: 'upstream-url', text: call.url }), call.request_id ? el('div', { class: 'upstream-url', text: `请求编号：${call.request_id}` }) : null,
    upstreamDetail('请求内容', call.request_headers, call.request_body),
    upstreamDetail('响应内容', call.response_headers, call.response_body)));
  children.push(el('div', { class: 'detail-section' }, el('h3', { text: `接口记录 · ${calls.length} 次` }), ...calls, calls.length ? null : el('p', { class: 'small muted', text: '暂无接口调用记录' })));
  $('#log-detail').replaceChildren(...children);
}
function openImage(image) {
  const url = safeURL(image.url);
  if (!url || image.deleted) return;
  $('#image-full').src = url;
  $('#image-full').alt = `图片 ${image.width || ''} × ${image.height || ''}`;
  $('#image-download').href = url;
  $('#image-download').download = `${image.id || 'image'}.${image.mime_type === 'image/jpeg' ? 'jpg' : image.mime_type === 'image/webp' ? 'webp' : 'png'}`;
  $('#image-dialog').showModal();
}

async function loadSettings(displayError = true) {
  try {
    const data = await api('/api/admin/settings');
    state.settings = data;
    state.settingsDirty = false;
    const form = $('#settings-form');
    for (const field of ['public_url', 'proxy_url', 'admin_username', 'refresh_minutes', 'retention_hours', 'max_storage_mb', 'max_concurrent', 'request_timeout_seconds', 'log_retention_days']) form.elements[field].value = data[field] ?? '';
    form.elements.password.value = '';
    $('#api-key-list').replaceChildren();
    (data.api_keys || []).forEach(key => addKey(key));
    $('#settings-save-note').textContent = '设置将在保存后生效';
    updateEndpoints();
    updateStorage();
  } catch (error) { if (displayError) toast(errorMessage(error), true); }
}
function addKey(value = '') {
  const input = el('input', { type: 'password', value, autocomplete: 'off', 'aria-label': 'API 密钥', placeholder: '输入密钥', minlength: '16', maxlength: '256', required: 'required' });
  const row = el('div', { class: 'key-row' }, input, el('button', { class: 'icon-button', type: 'button', title: '显示或隐藏密钥', 'aria-label': '显示或隐藏密钥', onclick: () => { input.type = input.type === 'password' ? 'text' : 'password'; } }, icon('eye')), el('button', { class: 'icon-button', type: 'button', title: '复制密钥', 'aria-label': '复制密钥', onclick: () => copyText(input.value) }, icon('copy')), el('button', { class: 'icon-button danger', type: 'button', title: '移除密钥', 'aria-label': '移除密钥', onclick: () => { row.remove(); markSettingsDirty(); } }, icon('trash')));
  $('#api-key-list').append(row);
  if (!value) { markSettingsDirty(); input.focus(); }
}
function markSettingsDirty() { state.settingsDirty = true; $('#settings-save-note').textContent = '有尚未保存的更改'; }
async function copyText(text) {
  if (!text) { toast('暂无可复制的内容', true); return; }
  try {
    if (navigator.clipboard?.writeText) await navigator.clipboard.writeText(text);
    else {
      const input = el('textarea', { 'aria-hidden': 'true' });
      input.value = text;
      input.style.position = 'fixed'; input.style.opacity = '0';
      document.body.append(input); input.select();
      const copied = document.execCommand('copy'); input.remove();
      if (!copied) throw new Error('复制不可用');
    }
    toast('已复制');
  } catch { toast('复制失败，请手动复制。', true); }
}

// 参考图上传完成后释放本地预览地址，仅保存文件索引，防止反复创作时积累图片内存。
function clearReferences() {
  state.references.forEach(reference => { if (reference.objectURL) URL.revokeObjectURL(reference.objectURL); reference.controller?.abort(); });
  state.references = [];
  renderReferences();
}
function renderReferences() {
  $('#reference-count').textContent = `${state.references.length} / 6`;
  $('#reference-upload').hidden = state.references.length >= 6;
  $('#references').replaceChildren(...state.references.map(reference => el('div', { class: 'reference-item' }, el('img', { src: reference.objectURL || safeURL(reference.image?.url), alt: reference.name || '参考图', decoding: 'async' }), reference.uploading ? el('span', { class: 'spinner' }) : null, el('button', { type: 'button', 'aria-label': '移除参考图', disabled: state.generating, onclick: () => {
    reference.controller?.abort();
    if (reference.objectURL) URL.revokeObjectURL(reference.objectURL);
    state.references = state.references.filter(item => item !== reference);
    renderReferences();
  } }, icon('close')))));
}
async function uploadReferences(files) {
  if (state.generating) return;
  const available = 6 - state.references.length;
  if (files.length > available) toast('最多添加 6 张参考图', true);
  for (const file of Array.from(files).slice(0, available)) {
    if (!['image/png', 'image/jpeg', 'image/webp'].includes(file.type)) { toast('请选择 PNG、JPG 或 WebP 图片', true); continue; }
    if (file.size > 20 * 1024 ** 2) { toast('单张参考图不能超过 20 MB', true); continue; }
    const reference = { name: file.name, objectURL: URL.createObjectURL(file), uploading: true, controller: new AbortController() };
    state.references.push(reference);
    renderReferences();
    const form = new FormData(); form.append('file', file);
    try {
      reference.image = await api('/api/admin/uploads', { method: 'POST', body: form, signal: reference.controller.signal });
      reference.uploading = false;
      if (state.references.includes(reference)) {
        URL.revokeObjectURL(reference.objectURL); reference.objectURL = null;
        renderReferences();
      }
    } catch (error) {
      if (reference.objectURL) URL.revokeObjectURL(reference.objectURL);
      state.references = state.references.filter(item => item !== reference);
      renderReferences();
      if (error.name !== 'AbortError') toast(`参考图上传失败：${errorMessage(error)}`, true);
    }
  }
}
function setGenerating(active) {
  state.generating = active;
  $$('#generate-form input, #generate-form select, #generate-form textarea').forEach(input => { input.disabled = active; });
  $('#generate-cancel').hidden = !active;
  $('#reference-upload').classList.toggle('disabled', active);
  if (active) busy($('#generate-submit'), true, '正在生成');
  else { busy($('#generate-submit'), false); $('#generate-submit').replaceChildren(icon('sparkles'), '生成图片'); $('#generate-submit').disabled = !state.models.length; }
  renderReferences();
}
function renderGeneration(images) {
  if (!images?.length) { empty($('#generation-output'), '没有返回图片', '请查看生成记录了解详情。', 'image'); return; }
  $('#generation-output').replaceChildren(el('div', { class: `result-grid${images.length === 1 ? ' single' : ''}` }, ...images.map((image, index) => {
    const url = safeURL(image.url);
    const img = url ? el('img', { src: url, alt: `生成图片 ${index + 1}`, decoding: 'async', loading: 'lazy', onerror: () => img.replaceWith(el('div', { class: 'deleted-image' }, icon('image'), '图片不可用')) }) : el('div', { class: 'deleted-image' }, icon('image'), '图片不可用');
    return el('div', { class: 'result-image' }, el('button', { type: 'button', 'aria-label': `查看图片 ${index + 1}`, onclick: () => openImage(image) }, img), el('div', { class: 'result-caption' }, el('span', { text: image.width && image.height ? `${image.width} × ${image.height}` : `图片 ${index + 1}` }), url ? el('a', { class: 'icon-button', href: url, download: `${image.id || 'image'}.png`, target: '_blank', rel: 'noopener', title: '下载图片', 'aria-label': '下载图片' }, icon('download')) : null));
  })));
}

// 登录状态使用服务端会话，浏览器不持久保存密码、Cookie 或访问令牌。
mountIcons();
$('#auth-form').addEventListener('submit', async event => {
  event.preventDefault();
  $('#auth-error').hidden = true;
  const button = $('#auth-submit');
  const username = $('#auth-username').value.trim();
  const password = $('#auth-password').value;
  if (!username) { $('#auth-username').focus(); return; }
  busy(button, true, state.initialized ? '正在登录' : '正在创建');
  try {
    const data = await api(state.initialized ? '/api/auth/login' : '/api/auth/setup', { method: 'POST', body: { username, password } });
    $('#auth-password').value = '';
    await showApp(data.username || username);
  } catch (error) { $('#auth-error').textContent = errorMessage(error); $('#auth-error').hidden = false; }
  finally { busy(button, false); }
});
$('#logout').addEventListener('click', async () => {
  try { await api('/api/auth/logout', { method: 'POST' }); showAuth(true); }
  catch (error) { toast(errorMessage(error), true); }
});
$$('[data-view]').forEach(button => button.addEventListener('click', () => navigate(button.dataset.view)));
$$('[data-navigate]').forEach(button => button.addEventListener('click', () => navigate(button.dataset.navigate)));
$('#page-refresh').addEventListener('click', async () => {
  const button = $('#page-refresh'); button.disabled = true;
  try { await navigate(state.view, true); } finally { button.disabled = false; }
});
$('#menu-toggle').addEventListener('click', () => { $('#sidebar').classList.toggle('open'); $('#sidebar-scrim').hidden = !$('#sidebar').classList.contains('open'); });
$('#sidebar-scrim').addEventListener('click', closeSidebar);
$$('[data-copy-target]').forEach(button => button.addEventListener('click', () => copyText($(`#${button.dataset.copyTarget}`).textContent)));
$$('[data-close-dialog]').forEach(button => button.addEventListener('click', () => $(`#${button.dataset.closeDialog}`).close()));
$('#image-dialog').addEventListener('close', () => { $('#image-full').removeAttribute('src'); $('#image-download').removeAttribute('href'); });
$('#drawer-close').addEventListener('click', closeDrawer);
$('#drawer-scrim').addEventListener('click', closeDrawer);
document.addEventListener('keydown', event => {
  if (event.key === 'Escape' && !$('#log-drawer').hidden && !$$('dialog[open]').length) closeDrawer();
  if (event.key === 'Tab' && !$('#log-drawer').hidden && !$$('dialog[open]').length) {
    const nodes = $$('button, a[href], summary, [tabindex="0"]', $('#log-drawer')).filter(node => !node.hidden && !node.disabled && node.getClientRects().length);
    const first = nodes[0], last = nodes[nodes.length - 1];
    if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last?.focus(); }
    else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first?.focus(); }
  }
});
window.addEventListener('hashchange', () => { const view = location.hash.slice(1); if (view in titles && view !== state.view) navigate(view); });
$('#add-account').addEventListener('click', () => openAccount());
$('#account-search').addEventListener('input', renderAccounts);
$$('[data-account-mode]').forEach(button => button.addEventListener('click', () => setAccountMode(button.dataset.accountMode)));
// 保存期间锁定表单，避免重复导入以及切换模式后误用正在执行的请求。
$('#account-dialog').addEventListener('cancel', event => { if (state.accountSubmitting) event.preventDefault(); });
$('#account-dialog').addEventListener('close', () => {
  $('#account-form').reset();
  state.accountEdit = null;
});
$('#account-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (state.accountSubmitting) return;
  const form = event.currentTarget;
  const body = { name: form.elements.name.value.trim() };
  if (state.accountEdit && !body.name) { form.elements.name.focus(); return; }
  if (state.accountMode === 'cookie') {
    const cookie = form.elements.cookie.value.trim();
    // 编辑时忽略空 Cookie，服务器会保留已有登录信息。
    if (cookie) body.cookie = cookie;
    else if (!state.accountEdit) { form.elements.cookie.focus(); return; }
  }
  const browserMode = state.accountMode === 'browser';
  const editing = Boolean(state.accountEdit);
  const path = editing ? `/api/admin/accounts/${encodeURIComponent(state.accountEdit.id)}` : browserMode ? '/api/admin/accounts/browser' : '/api/admin/accounts/import';
  const button = $('#account-submit');
  state.accountSubmitting = true;
  $$('#account-form input, #account-form textarea, #account-form [data-account-mode], #account-form [data-close-dialog]').forEach(input => { input.disabled = true; });
  busy(button, true, browserMode ? '正在获取' : '正在保存'); $('#account-form-error').hidden = true;
  try {
    const result = await api(path, { method: editing ? 'PUT' : 'POST', body });
    $('#account-dialog').close();
    toast(browserMode ? result.created ? '账号已导入' : '账号登录已更新' : editing ? '账号已保存' : '账号已导入');
    await loadAccounts();
  } catch (error) { $('#account-form-error').textContent = errorMessage(error); $('#account-form-error').hidden = false; }
  finally {
    state.accountSubmitting = false;
    $$('#account-form input, #account-form textarea, #account-form [data-account-mode], #account-form [data-close-dialog]').forEach(input => { input.disabled = false; });
    busy(button, false);
  }
});
let logSearchTimer;
$('#log-search').addEventListener('input', () => { clearTimeout(logSearchTimer); logSearchTimer = setTimeout(() => { state.logsPage = 1; loadLogs(); }, 350); });
$('#log-status').addEventListener('change', () => { state.logsPage = 1; loadLogs(); });
$('#settings-form').addEventListener('input', markSettingsDirty);
$('#add-key').addEventListener('click', () => addKey());
$('#settings-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = event.currentTarget;
  const body = {};
  for (const key of ['public_url', 'proxy_url', 'admin_username']) body[key] = form.elements[key].value.trim();
  for (const key of ['refresh_minutes', 'retention_hours', 'max_storage_mb', 'max_concurrent', 'request_timeout_seconds', 'log_retention_days']) body[key] = Number(form.elements[key].value);
  body.api_keys = [...new Set($$('#api-key-list input').map(input => input.value.trim()).filter(Boolean))];
  if (form.elements.password.value) body.password = form.elements.password.value;
  const button = $('#settings-save-top'); busy(button, true, '正在保存');
  try {
    await api('/api/admin/settings', { method: 'PUT', body });
    form.elements.password.value = '';
    state.settings = { ...state.settings, ...body }; delete state.settings.password;
    state.settingsDirty = false;
    $('#settings-save-note').textContent = '设置已保存';
    $('#user-name').textContent = body.admin_username;
    $('#user-initial').textContent = (body.admin_username || 'A').slice(0, 1).toUpperCase();
    updateEndpoints(); updateStorage(); toast('设置已保存');
  } catch (error) { toast(errorMessage(error), true); }
  finally { busy(button, false); button.replaceChildren(icon('check'), '保存设置'); }
});
$('#cleanup').addEventListener('click', async () => {
  if (!await confirmAction('清理图片', '按已保存的保留时间和容量上限清理图片，清理后无法恢复。', '开始清理')) return;
  const button = $('#cleanup'); busy(button, true, '正在清理');
  try { await api('/api/admin/cleanup', { method: 'POST' }); toast('图片清理已完成'); state.stats = await api('/api/admin/stats'); renderStats(state.stats); }
  catch (error) { toast(errorMessage(error), true); }
  finally { busy(button, false); }
});
$('#generate-model').addEventListener('change', updateModelOptions);
$('#generate-resolution').addEventListener('change', updateRatioOptions);
$('#reference-input').addEventListener('change', event => { const files = Array.from(event.target.files || []); event.target.value = ''; uploadReferences(files); });
$('#reference-upload').addEventListener('dragover', event => { event.preventDefault(); });
$('#reference-upload').addEventListener('drop', event => { event.preventDefault(); if (event.dataTransfer?.files) uploadReferences(Array.from(event.dataTransfer.files)); });
$('#generate-cancel').addEventListener('click', () => state.generationController?.abort());
$('#generate-form').addEventListener('submit', async event => {
  event.preventDefault();
  if (state.generating) return;
  if (state.references.some(reference => reference.uploading)) { toast('参考图仍在上传，请稍候。', true); return; }
  const prompt = $('#generate-prompt').value.trim();
  if (!prompt) { $('#generate-prompt').focus(); return; }
  const body = { model: $('#generate-model').value, prompt, n: Number($('#generate-count').value), aspect_ratio: $('#generate-ratio').value, resolution: $('#generate-resolution').value, quality: $('#generate-quality').value, references: state.references.filter(reference => reference.image).map(reference => reference.image) };
  $('#generate-error').hidden = true;
  setGenerating(true);
  state.generationController = new AbortController();
  const started = Date.now();
  const elapsed = el('p', { text: '已等待 0 秒' });
  $('#generation-output').replaceChildren(el('div', { class: 'generation-wait' }, el('span', { class: 'spinner' }), el('h2', { text: '正在创作你的画面' }), elapsed));
  $('#generation-meta').textContent = '';
  state.generationTimer = setInterval(() => { elapsed.textContent = `已等待 ${Math.floor((Date.now() - started) / 1000)} 秒`; }, 1000);
  try {
    const data = await api('/api/admin/generate', { method: 'POST', body, signal: state.generationController.signal });
    renderGeneration(data.images);
    $('#generation-meta').textContent = `${duration(data.duration_ms ?? Date.now() - started)} · ${count(data.credits, 2)} 积分`;
    toast('图片已生成');
  } catch (error) {
    if (error.name === 'AbortError') { empty($('#generation-output'), '生成已停止', '调整创作设置，随时重新开始。', 'image'); }
    else { $('#generate-error').textContent = errorMessage(error); $('#generate-error').hidden = false; empty($('#generation-output'), '本次生成未完成', '请检查账号状态或稍后重试。', 'alert'); }
  } finally {
    clearInterval(state.generationTimer); state.generationTimer = null; state.generationController = null; setGenerating(false);
  }
});
window.addEventListener('beforeunload', event => {
  if (state.settingsDirty) { event.preventDefault(); event.returnValue = ''; }
});
window.addEventListener('pagehide', () => {
  clearInterval(state.generationTimer);
  state.generationController?.abort();
  state.references.forEach(reference => { if (reference.objectURL) URL.revokeObjectURL(reference.objectURL); reference.controller?.abort(); });
});

(async function initialize() {
  try {
    const data = await api('/api/auth/status');
    if (data.authenticated) await showApp(data.username);
    else showAuth(Boolean(data.initialized), data.username || '');
  } catch (error) {
    $('#boot').replaceChildren(el('span', { class: 'brand-mark', text: 'A' }), el('div', {}, el('p', { text: '暂时无法连接服务' }), el('p', { class: 'small muted', text: errorMessage(error) }), el('button', { class: 'button secondary tiny', onclick: () => location.reload() }, '重新加载')));
  }
})();
