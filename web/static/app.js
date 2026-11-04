'use strict';
/* CloudBox — веб-интерфейс: каркас, файлы, общий доступ, загрузка. Остальные разделы — views.js. */

// ---------- утилиты ----------
const $ = (s, el = document) => el.querySelector(s);

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'html') el.innerHTML = v; // только для своих статических строк или санитизированного HTML
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat(Infinity)) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

class ApiError extends Error { constructor(msg, status, data) { super(msg); this.status = status; this.data = data; } }

async function api(method, url, body, extra = {}) {
  const opt = { method, headers: { ...(extra.headers || {}) }, credentials: 'same-origin' };
  if (body !== undefined) {
    if (body instanceof Blob || typeof body === 'string') { opt.body = body; }
    else { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  }
  const res = await fetch(url, opt);
  let data = null;
  try { data = await res.json(); } catch (_) {}
  if (!res.ok) {
    if (res.status === 401 && state.me && !url.startsWith('/api/auth')) { location.reload(); }
    throw new ApiError((data && data.error) || `Ошибка ${res.status}`, res.status, data);
  }
  return data;
}

function fmtSize(b) {
  if (b == null) return '';
  const u = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ'];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return (i === 0 ? b : b.toFixed(b < 10 ? 1 : 0)) + ' ' + u[i];
}

function fmtDate(s) {
  if (!s) return '';
  const d = new Date(s), now = new Date();
  if (d.toDateString() === now.toDateString()) return 'сегодня, ' + d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
  return d.toLocaleDateString('ru-RU', { day: 'numeric', month: 'short', year: d.getFullYear() === now.getFullYear() ? undefined : 'numeric' })
    + ', ' + d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
}

const extOf = n => (n.name.split('.').pop() || '').toLowerCase();

function iconFor(n) {
  if (n.is_dir) return n.e2e ? '🔒' : '📁';
  const m = n.mime || '';
  const ext = extOf(n);
  if (m.startsWith('image/')) return '🖼️';
  if (m.startsWith('video/')) return '🎬';
  if (m.startsWith('audio/')) return '🎵';
  if (m === 'application/pdf') return '📕';
  if (['zip', 'rar', '7z', 'tar', 'gz', 'xz', 'bz2'].includes(ext)) return '🗜️';
  if (['doc', 'docx', 'odt', 'rtf'].includes(ext)) return '📝';
  if (['xls', 'xlsx', 'ods', 'csv'].includes(ext)) return '📊';
  if (['ppt', 'pptx', 'odp'].includes(ext)) return '📽️';
  if (['md', 'markdown'].includes(ext)) return '🗒️';
  if (['js', 'ts', 'go', 'py', 'c', 'cpp', 'h', 'rs', 'java', 'json', 'yml', 'yaml', 'sh', 'html', 'css', 'sql', 'ino'].includes(ext)) return '💻';
  return '📄';
}

const isImage = n => /^image\/(png|jpe?g|gif|webp|bmp|avif)$/.test(n.mime || '');
const isVideo = n => (n.mime || '').startsWith('video/');
const isAudio = n => (n.mime || '').startsWith('audio/');
const isPDF = n => n.mime === 'application/pdf';
const isText = n => (n.mime || '').startsWith('text/') || /\.(md|markdown|json|ya?ml|go|py|js|ts|sh|ino|c|h|cpp|rs|sql|conf|ini|log|toml|env|csv)$/i.test(n.name);
const isMarkdown = n => /\.(md|markdown)$/i.test(n.name);
const canPreview = n => !n.is_dir && (isImage(n) || isVideo(n) || isAudio(n) || isPDF(n) || (isText(n) && n.size < 2 * 1024 * 1024));
const hasThumb = n => !n.is_dir && (isImage(n) || (isVideo(n) && state.usage?.thumbs?.video) || (isPDF(n) && state.usage?.thumbs?.pdf));
const canOffice = n => !n.is_dir && (state.usage?.office_exts || []).includes(extOf(n));
const writable = n => n.can_write !== false;

function toast(msg, err) {
  const t = h('div', { class: 'toast' + (err ? ' err' : '') }, msg);
  $('#toasts').append(t);
  setTimeout(() => t.remove(), err ? 5000 : 2600);
}
const report = e => toast(e.message || String(e), true);

async function copyText(text) {
  try { await navigator.clipboard.writeText(text); toast('Скопировано'); }
  catch (_) { prompt('Скопируйте вручную:', text); }
}

// ---------- модальные окна ----------
function modal({ title, body, buttons = [], wide = false, full = false, onClose }) {
  const close = () => { ov.remove(); document.removeEventListener('keydown', onKey); onClose && onClose(); };
  const onKey = e => { if (e.key === 'Escape' && !document.querySelector('.ctx')) close(); };
  const footer = h('footer', null, buttons.map(b => h('button', {
    class: 'btn' + (b.primary ? ' primary' : '') + (b.danger ? ' danger' : ''),
    onclick: async ev => {
      ev.target.disabled = true;
      try { if ((await (b.onClick ? b.onClick() : true)) !== false) close(); }
      catch (e) { report(e); }
      finally { ev.target.disabled = false; }
    },
  }, b.label)));
  const m = h('div', { class: 'modal' + (wide ? ' wide' : '') + (full ? ' full' : '') },
    h('header', null, h('span', null, title), h('button', { class: 'icon-btn', onclick: close, title: 'Закрыть' }, '×')),
    h('div', { class: 'body' }, body), buttons.length ? footer : null);
  const ov = h('div', { class: 'overlay', onmousedown: e => { if (e.target === ov) close(); } }, m);
  document.body.append(ov);
  document.addEventListener('keydown', onKey);
  setTimeout(() => { const i = m.querySelector('input[type=text],input[type=password],textarea'); if (i && !i.readOnly) { i.focus(); } }, 30);
  return { close, el: m };
}

function askText(title, label, value = '', okLabel = 'OK', type = 'text') {
  return new Promise(resolve => {
    let done = false;
    const inp = h('input', { type, value });
    const finish = v => { done = true; resolve(v); };
    const m = modal({
      title, body: [h('label', null, label), inp],
      buttons: [{ label: 'Отмена' }, { label: okLabel, primary: true, onClick: () => finish(inp.value.trim()) }],
      onClose: () => { if (!done) resolve(null); },
    });
    inp.addEventListener('keydown', e => { if (e.key === 'Enter') { finish(inp.value.trim()); m.close(); } });
    setTimeout(() => { const dot = value.lastIndexOf('.'); if (dot > 0) inp.setSelectionRange(0, dot); else inp.select(); }, 40);
  });
}

function confirmDialog(title, text, okLabel = 'Удалить', danger = true) {
  return new Promise(resolve => {
    let done = false;
    modal({
      title, body: h('p', null, text),
      buttons: [{ label: 'Отмена' }, { label: okLabel, primary: !danger, danger, onClick: () => { done = true; resolve(true); } }],
      onClose: () => { if (!done) resolve(false); },
    });
  });
}

let ctxEl = null;
function contextMenu(x, y, items) {
  closeCtx();
  ctxEl = h('div', { class: 'ctx' }, items.filter(Boolean).map(it => it === '-' ? h('hr') : h('button', {
    class: it.danger ? 'danger' : '', onclick: () => { closeCtx(); it.onClick(); },
  }, h('span', null, it.icon || ''), it.label)));
  document.body.append(ctxEl);
  const r = ctxEl.getBoundingClientRect();
  ctxEl.style.left = Math.max(8, Math.min(x, innerWidth - r.width - 8)) + 'px';
  ctxEl.style.top = Math.max(8, Math.min(y, innerHeight - r.height - 8)) + 'px';
}
function closeCtx() { if (ctxEl) { ctxEl.remove(); ctxEl = null; } }
document.addEventListener('mousedown', e => { if (ctxEl && !ctxEl.contains(e.target)) closeCtx(); });
document.addEventListener('scroll', closeCtx, true);

// ---------- состояние ----------
const state = {
  me: null, usage: null, status: null,
  view: 'files', parent: null, items: [], crumbs: [], canWrite: true, sharedBy: null,
  selected: new Set(), sort: { key: 'name', dir: 1 },
};

// ---------- вход ----------
async function boot() {
  registerSW();
  let st;
  try { st = await api('GET', '/api/auth/status'); } catch (e) { document.body.textContent = 'Сервер недоступен: ' + e.message; return; }
  state.status = st;
  if (st.logged_in) { await loadMe(); renderApp(); } else renderAuth(st);
}

async function loadMe() {
  const d = await api('GET', '/api/me');
  state.me = d.user; state.usage = d;
}

function renderAuth(st) {
  let mode = st.first_user ? 'register' : 'login';
  let ticket = null;
  const root = $('#root');
  const urlErr = new URLSearchParams(location.hash.split('?')[1] || '').get('error');
  const draw = () => {
    const err = h('div', { class: 'error' }, urlErr && mode === 'login' && !ticket ? urlErr : '');
    if (ticket) {
      const code = h('input', { type: 'text', inputmode: 'numeric', autocomplete: 'one-time-code', placeholder: '123456 или код восстановления' });
      const submit = async e => {
        e.preventDefault(); err.textContent = '';
        try { await api('POST', '/api/auth/totp', { ticket, code: code.value.trim() }); history.replaceState(null, '', '/'); await loadMe(); renderApp(); }
        catch (ex) { err.textContent = ex.message; if (ex.status === 401) { ticket = null; setTimeout(draw, 1500); } }
      };
      root.replaceChildren(h('div', { class: 'auth' }, h('form', { class: 'auth-card', onsubmit: submit },
        h('div', { class: 'logo' }, h('div', { class: 'logo-mark' }, '☁'), 'CloudBox'),
        h('h1', null, 'Подтверждение входа'),
        h('div', { class: 'hint' }, 'Введите 6-значный код из приложения-аутентификатора или один из кодов восстановления.'),
        h('label', null, 'Код'), code,
        h('button', { class: 'btn primary', type: 'submit' }, 'Подтвердить'), err,
        h('div', { class: 'auth-switch' }, h('a', { href: '#', onclick: e => { e.preventDefault(); ticket = null; draw(); } }, 'Назад')))));
      code.focus();
      return;
    }
    const user = h('input', { type: 'text', autocomplete: 'username', autocapitalize: 'off' });
    const pass = h('input', { type: 'password', autocomplete: mode === 'login' ? 'current-password' : 'new-password' });
    const submit = async e => {
      e.preventDefault(); err.textContent = '';
      try {
        const r = await api('POST', mode === 'login' ? '/api/auth/login' : '/api/auth/register', { username: user.value.trim(), password: pass.value });
        if (r.totp_required) { ticket = r.ticket; draw(); return; }
        history.replaceState(null, '', '/');
        await loadMe(); renderApp();
      } catch (ex) { err.textContent = ex.message; }
    };
    const pwForm = st.password_login ? [
      h('label', null, 'Имя пользователя'), user,
      h('label', null, 'Пароль'), pass,
      h('button', { class: 'btn primary', type: 'submit' }, mode === 'login' ? 'Войти' : 'Создать учётную запись'),
    ] : [];
    root.replaceChildren(h('div', { class: 'auth' }, h('form', { class: 'auth-card', onsubmit: submit },
      h('div', { class: 'logo' }, h('div', { class: 'logo-mark' }, '☁'), 'CloudBox'),
      h('h1', null, mode === 'login' ? 'Вход' : (st.first_user ? 'Создание администратора' : 'Регистрация')),
      st.first_user ? h('div', { class: 'hint' }, 'Это первый запуск. Первый пользователь станет администратором.') : null,
      pwForm,
      st.oidc && mode === 'login' ? h('a', { class: 'btn' + (st.password_login ? '' : ' primary'), style: 'width:100%;justify-content:center;margin-top:10px;padding:10px', href: '/api/auth/oidc/start' }, '🔐 ' + st.oidc_name) : null,
      err,
      !st.first_user && st.registration_open ? h('div', { class: 'auth-switch' },
        mode === 'login' ? 'Нет аккаунта? ' : 'Уже есть аккаунт? ',
        h('a', { href: '#', onclick: e => { e.preventDefault(); mode = mode === 'login' ? 'register' : 'login'; draw(); } },
          mode === 'login' ? 'Зарегистрироваться' : 'Войти')) : null,
    )));
    if (st.password_login) user.focus();
  };
  draw();
}

// ---------- каркас ----------
let els = {};

const NAV = [
  ['files', '📁', 'Все файлы'], ['recent', '🕘', 'Недавние'], ['favorites', '⭐', 'Избранное'], ['shared', '👥', 'Доступно мне'],
  ['talk', '💬', 'Чат'], ['mail', '✉️', 'Почта'], ['gallery', '🖼️', 'Галерея'], ['notes', '🗒️', 'Заметки'], ['calendar', '📅', 'Календарь'], ['contacts', '📇', 'Контакты'],
  ['shares', '🔗', 'Мои ссылки'], ['activity', '📜', 'Журнал'], ['trash', '🗑️', 'Корзина'], ['settings', '⚙️', 'Настройки'],
];

function renderApp() {
  const navItem = (id, ico, label) => h('a', { href: '#/' + id, 'data-view': id }, h('span', { class: 'ico' }, ico), label);
  els.quota = h('div', { class: 'quota' });
  els.tags = h('div', { class: 'nav-tags' });
  els.nav = h('nav', { class: 'nav' }, NAV.map(n => navItem(...n)), state.me.is_admin ? navItem('admin', '🛠️', 'Администрирование') : null);
  els.crumbs = h('div', { class: 'crumbs' });
  els.search = h('input', { type: 'search', class: 'search', placeholder: 'Поиск по имени и содержимому…' });
  els.search.addEventListener('keydown', e => {
    if (e.key === 'Enter') location.hash = els.search.value.trim() ? '#/search/' + encodeURIComponent(els.search.value.trim()) : '#/files';
  });
  els.fileInput = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: e => { uploadFiles([...e.target.files].map(f => ({ file: f, path: '' }))); e.target.value = ''; } });
  els.dirInput = h('input', { type: 'file', webkitdirectory: true, class: 'hidden', onchange: e => {
    uploadFiles([...e.target.files].map(f => ({ file: f, path: f.webkitRelativePath.split('/').slice(0, -1).join('/') }))); e.target.value = '';
  } });
  els.actions = h('div', { class: 'actions' },
    h('button', { class: 'btn', onclick: newMenu, title: 'Создать' }, '＋', h('span', { class: 'lbl' }, 'Создать')),
    h('button', { class: 'btn', onclick: () => els.dirInput.click(), title: 'Загрузить папку' }, '📂', h('span', { class: 'lbl' }, 'Папку')),
    h('button', { class: 'btn primary', onclick: () => els.fileInput.click() }, '⬆', h('span', { class: 'lbl' }, 'Загрузить')),
    els.fileInput, els.dirInput);
  els.bellCount = h('span', { class: 'bell-count hidden' });
  els.bell = h('button', { class: 'icon-btn bell', title: 'Уведомления', onclick: e => openNotifications(e.currentTarget) }, '🔔', els.bellCount);
  els.content = h('div', { class: 'content' });
  els.app = h('div', { class: 'app' },
    h('aside', { class: 'sidebar' },
      h('div', { class: 'logo' }, h('div', { class: 'logo-mark' }, '☁'), 'CloudBox'),
      els.nav, els.tags, h('div', { class: 'spacer' }), els.quota,
      h('div', { class: 'userbox' }, h('span', null, '👤 ', state.me.username),
        h('button', { class: 'icon-btn', title: 'Выйти', onclick: logout }, '⎋'))),
    h('main', { class: 'main' },
      h('div', { class: 'topbar' },
        h('button', { class: 'icon-btn menu-toggle', onclick: () => els.app.classList.toggle('menu-open') }, '☰'),
        els.crumbs, els.search, els.actions, els.bell),
      els.content));
  $('#root').replaceChildren(els.app);
  els.nav.addEventListener('click', () => els.app.classList.remove('menu-open'));
  setupDrop();
  setupKeys();
  renderQuota();
  renderUnread(state.usage.unread);
  loadSidebarTags();
  setInterval(pollNotifications, 60000);
  addEventListener('hashchange', route);
  route();
  receiveSharedFiles();
  Talk.start();
  Talk.setUnread(state.usage.talk_unread);
}

function renderQuota() {
  const u = state.usage; if (!u) return;
  const pct = u.quota_bytes > 0 ? Math.min(100, u.used_bytes / u.quota_bytes * 100) : 0;
  els.quota.replaceChildren(...[
    h('div', null, 'Занято ', fmtSize(u.used_bytes), u.quota_bytes > 0 ? ' из ' + fmtSize(u.quota_bytes) : ''),
    u.quota_bytes > 0 ? h('div', { class: 'bar' + (pct > 90 ? ' warn' : '') }, h('div', { style: `width:${pct}%` })) : null].filter(Boolean));
}

async function refreshUsage() { try { const d = await api('GET', '/api/me'); state.usage = d; renderQuota(); renderUnread(d.unread); } catch (_) {} }

async function logout() {
  await api('POST', '/api/auth/logout').catch(() => {});
  location.hash = ''; location.reload();
}

async function loadSidebarTags() {
  try {
    const tags = await api('GET', '/api/tags');
    els.tags.replaceChildren(...(tags.length ? [h('div', { class: 'nav-title' }, 'Теги')] : []),
      ...tags.map(t => h('a', { href: '#/tag/' + t.id, 'data-view': 'tag-' + t.id }, h('span', { class: 'dot', style: `background:${t.color}` }), t.name,
        t.count ? h('span', { class: 'cnt' }, t.count) : null)));
  } catch (_) {}
}

// ---------- маршрутизация ----------
function route() {
  const [hpath] = location.hash.replace(/^#\/?/, '').split('?');
  const parts = hpath.split('/');
  const view = parts[0] || 'files';
  state.view = view; state.selected.clear();
  const navKey = view === 'tag' ? 'tag-' + parts[1] : view;
  for (const a of document.querySelectorAll('.nav a, .nav-tags a')) a.classList.toggle('active', a.dataset.view === navKey);
  els.actions.classList.toggle('hidden', view !== 'files' || !state.canWrite && !!parts[1]);
  if (view !== 'search') els.search.value = '';
  els.content.className = 'content';
  const run = {
    files: () => viewFiles(parts[1] || null),
    file: () => openFileLink(parts[1]),
    recent: viewRecent, favorites: viewFavorites, shared: viewShared,
    tag: () => viewTag(+parts[1]),
    search: () => viewSearch(decodeURIComponent(parts.slice(1).join('/'))),
    talk: () => viewTalk(parts[1] || null), mail: viewMail, e2e: () => viewE2E(parts[1]),
    gallery: viewGallery, notes: () => viewNotes(parts[1]), calendar: viewCalendar, contacts: viewContacts,
    activity: viewActivity, trash: viewTrash, shares: viewShares, settings: viewSettings, admin: viewAdmin,
    receive: () => viewFiles(null),
  }[view] || (() => viewFiles(null));
  run().catch(report);
}

function setCrumbs(list) {
  const kids = [];
  list.forEach((c, i) => {
    if (i) kids.push(h('span', { class: 'sep' }, '›'));
    const last = i === list.length - 1;
    kids.push(c.href && !last ? h('a', { href: c.href, 'data-drop': c.id === undefined ? null : (c.id || 'root') }, c.label) : h('span', { class: last ? 'cur' : '' }, c.label));
  });
  els.crumbs.replaceChildren(...kids);
  for (const a of els.crumbs.querySelectorAll('a[data-drop]')) attachDropTarget(a, a.dataset.drop === 'root' ? null : a.dataset.drop);
}

// ---------- файлы ----------
async function viewFiles(parentId) {
  state.parent = parentId;
  const d = await api('GET', '/api/files' + (parentId ? '?parent=' + parentId : ''));
  if (state.view !== 'files' && state.view !== 'receive' || state.parent !== parentId) return;
  state.items = d.items; state.crumbs = d.breadcrumbs; state.canWrite = d.can_write; state.sharedBy = d.shared_by || null;
  els.actions.classList.toggle('hidden', !d.can_write);
  const head = d.shared_by
    ? [{ label: 'Доступно мне', href: '#/shared' }]
    : [{ label: 'Все файлы', href: '#/files', id: null }];
  setCrumbs([...head, ...d.breadcrumbs.map(c => ({ label: c.name, href: '#/files/' + c.id, id: c.id }))]);
  drawList({
    emptyText: d.can_write ? 'Папка пуста. Перетащите сюда файлы или нажмите «Загрузить».' : 'Папка пуста.',
    banner: d.shared_by ? `Папка пользователя ${d.shared_by}` + (d.can_write ? ' — можно изменять' : ' — только просмотр') : null,
  });
}

async function viewRecent() {
  setCrumbs([{ label: 'Недавние' }]);
  state.items = await api('GET', '/api/recent');
  drawList({ emptyText: 'Пока ничего нет.', noSort: true, showPath: true });
}

async function viewFavorites() {
  setCrumbs([{ label: 'Избранное' }]);
  state.items = await api('GET', '/api/favorites');
  drawList({ emptyText: 'Отметьте файлы или папки звёздочкой ☆ — они появятся здесь.' });
}

async function viewShared() {
  setCrumbs([{ label: 'Доступно мне' }]);
  state.items = await api('GET', '/api/shared-with-me');
  drawList({ emptyText: 'С вами пока никто не поделился файлами.', showOwner: true });
}

async function viewTag(id) {
  const tags = await api('GET', '/api/tags');
  const t = tags.find(x => x.id === id);
  setCrumbs([{ label: 'Тег: ' + (t ? t.name : '?') }]);
  state.items = await api('GET', `/api/tags/${id}/files`);
  drawList({ emptyText: 'Нет файлов с этим тегом.' });
}

async function viewSearch(q) {
  els.search.value = q;
  setCrumbs([{ label: 'Поиск: «' + q + '»' }]);
  els.content.replaceChildren(h('div', { class: 'empty' }, 'Ищу…'));
  state.items = await api('GET', '/api/search?q=' + encodeURIComponent(q));
  drawList({ emptyText: 'Ничего не найдено. Поиск идёт по именам и по тексту документов (TXT, Markdown, PDF, DOCX, XLSX, ODT…).', showPath: true, noSort: true });
}

async function openFileLink(id) {
  // ссылка из уведомления: открыть папку файла и панель свойств
  try {
    const n = await api('GET', '/api/files/' + id);
    history.replaceState(null, '', '#/files/' + (n.parent_id || ''));
    state.view = 'files';
    await viewFiles(n.parent_id || null);
    detailsPanel(n, 'comments');
  } catch (e) { report(e); location.hash = '#/files'; }
}

function sortedItems() {
  const { key, dir } = state.sort;
  return [...state.items].sort((a, b) => {
    if (a.is_dir !== b.is_dir) return a.is_dir ? -1 : 1;
    let r = 0;
    if (key === 'size') r = a.size - b.size;
    else if (key === 'date') r = new Date(a.updated_at) - new Date(b.updated_at);
    else r = a.name.localeCompare(b.name, 'ru', { numeric: true, sensitivity: 'base' });
    return r * dir;
  });
}

let lastListOpts = {};
function drawList(opts = lastListOpts) {
  lastListOpts = opts;
  const items = opts.noSort ? state.items : sortedItems();
  const kids = [];
  if (opts.banner) kids.push(h('div', { class: 'banner' }, '👥 ', opts.banner));
  if (state.selected.size) {
    const sel = items.filter(n => state.selected.has(n.id));
    const canW = sel.every(writable) && (state.view !== 'files' || state.canWrite);
    kids.push(h('div', { class: 'selbar' },
      h('span', { class: 'count' }, 'Выбрано: ' + sel.length),
      canW ? h('button', { class: 'btn', onclick: () => moveDialog(sel, false) }, 'Переместить') : null,
      h('button', { class: 'btn', onclick: () => moveDialog(sel, true) }, 'Копировать к себе'),
      canW ? h('button', { class: 'btn danger', onclick: () => trashNodes(sel) }, 'Удалить') : null,
      h('button', { class: 'btn ghost', onclick: () => { state.selected.clear(); drawList(); } }, 'Снять выделение')));
  }
  if (!items.length) {
    kids.push(h('div', { class: 'empty' }, h('div', { class: 'big' }, '☁'), opts.emptyText));
    els.content.replaceChildren(...kids);
    return;
  }
  const th = (key, label, cls) => h('th', {
    class: cls + (opts.noSort ? '' : ' sortable'),
    onclick: opts.noSort ? null : () => { state.sort = { key, dir: state.sort.key === key ? -state.sort.dir : 1 }; drawList(); },
  }, label, !opts.noSort && state.sort.key === key ? (state.sort.dir > 0 ? ' ↑' : ' ↓') : '');
  const allBox = h('input', { type: 'checkbox', checked: items.length && items.every(n => state.selected.has(n.id)),
    onchange: e => { items.forEach(n => e.target.checked ? state.selected.add(n.id) : state.selected.delete(n.id)); drawList(); } });
  kids.push(h('table', { class: 'files' },
    h('thead', null, h('tr', null, h('th', { class: 'c-check' }, allBox), th('name', 'Имя', ''), th('size', 'Размер', 'c-size'), th('date', 'Изменён', 'c-date'), h('th', { class: 'c-act' }))),
    h('tbody', null, items.map(n => fileRow(n, opts, items)))));
  els.content.replaceChildren(...kids);
}

function thumbIcon(n, size = 128) {
  const icon = h('div', { class: 'ficon' + (n.is_dir ? ' dir' : '') }, iconFor(n));
  if (hasThumb(n)) {
    const img = h('img', { loading: 'lazy', alt: '', src: `/api/files/${n.id}/thumb?s=${size}` });
    img.onerror = () => img.replaceWith(iconFor(n));
    icon.replaceChildren(img);
    if (isVideo(n)) icon.append(h('span', { class: 'play' }, '▶'));
  }
  return icon;
}

function fileRow(n, opts, list) {
  const sub = [];
  if (opts.showPath && n.path) sub.push(n.path);
  if (n.owner) sub.push('владелец: ' + n.owner + (n.can_write ? ' · можно изменять' : ' · только просмотр'));
  if (n.snippet) sub.push(n.snippet);
  const name = h('div', { class: 'fname', onclick: e => { e.stopPropagation(); openNode(n, list); } },
    thumbIcon(n),
    h('div', { style: 'min-width:0' },
      h('div', { class: 'nm', title: n.name }, n.name,
        n.shared ? h('span', { class: 'mini', title: 'Есть общий доступ' }, ' 👥') : null,
        (n.tags || []).map(t => h('span', { class: 'tag', style: `background:${t.color}` }, t.name))),
      sub.map(s => h('div', { class: 'sub' + (n.snippet && s === n.snippet ? ' snippet' : '') }, s))));
  const box = h('input', { type: 'checkbox', checked: state.selected.has(n.id),
    onclick: e => e.stopPropagation(),
    onchange: e => { e.target.checked ? state.selected.add(n.id) : state.selected.delete(n.id); drawList(); } });
  const star = h('button', { class: 'icon-btn star' + (n.favorite ? ' on' : ''), title: n.favorite ? 'Убрать из избранного' : 'В избранное',
    onclick: async e => {
      e.stopPropagation();
      try { await api(n.favorite ? 'DELETE' : 'PUT', `/api/files/${n.id}/favorite`); n.favorite = !n.favorite; drawList(); }
      catch (ex) { report(ex); }
    } }, n.favorite ? '★' : '☆');
  const tr = h('tr', {
    class: state.selected.has(n.id) ? 'selected' : '', draggable: writable(n) ? 'true' : null,
    onclick: e => {
      if (e.ctrlKey || e.metaKey) state.selected.has(n.id) ? state.selected.delete(n.id) : state.selected.add(n.id);
      else { state.selected.clear(); state.selected.add(n.id); }
      drawList();
    },
    oncontextmenu: e => { e.preventDefault(); nodeMenu(n, e.clientX, e.clientY, list); },
    ondragstart: e => {
      const ids = state.selected.has(n.id) ? [...state.selected] : [n.id];
      e.dataTransfer.setData('application/x-cloudbox', JSON.stringify(ids));
      e.dataTransfer.effectAllowed = 'move';
    },
  },
  h('td', { class: 'c-check' }, box),
  h('td', null, name),
  h('td', { class: 'c-size' }, n.is_dir ? '—' : fmtSize(n.size)),
  h('td', { class: 'c-date' }, fmtDate(n.updated_at)),
  h('td', { class: 'c-act' }, star,
    h('button', { class: 'icon-btn', title: 'Поделиться', onclick: e => { e.stopPropagation(); shareDialog(n); } }, '🔗'),
    h('button', { class: 'icon-btn', title: 'Ещё', onclick: e => { e.stopPropagation(); const r = e.currentTarget.getBoundingClientRect(); nodeMenu(n, r.left - 180, r.bottom + 4, list); } }, '⋯')));
  if (n.is_dir && !n.e2e && writable(n)) attachDropTarget(tr, n.id); // в E2E-папку — только через её окно, с шифрованием
  return tr;
}

function attachDropTarget(el, folderId) {
  el.addEventListener('dragover', e => {
    if (!e.dataTransfer.types.includes('application/x-cloudbox')) return;
    e.preventDefault(); e.stopPropagation(); el.classList.add('drop-target');
  });
  el.addEventListener('dragleave', () => el.classList.remove('drop-target'));
  el.addEventListener('drop', async e => {
    const raw = e.dataTransfer.getData('application/x-cloudbox');
    el.classList.remove('drop-target');
    if (!raw) return;
    e.preventDefault(); e.stopPropagation();
    const ids = JSON.parse(raw).filter(id => id !== folderId);
    let ok = 0;
    for (const id of ids) {
      try { await api('PATCH', '/api/files/' + id, { parent: folderId }); ok++; } catch (ex) { report(ex); }
    }
    if (ok) toast(`Перемещено: ${ok}`);
    state.selected.clear(); route();
  });
}

function openNode(n, list) {
  if (n.is_dir && n.e2e) { location.hash = '#/e2e/' + n.id; return; }
  if (n.is_dir) { location.hash = '#/files/' + n.id; return; }
  if (canOffice(n)) { window.open('/office/' + n.id, '_blank'); return; }
  if (isText(n) && n.size < 2 * 1024 * 1024) { editorDialog(n); return; }
  if (canPreview(n)) preview(n, list.filter(canPreview));
  else location.href = `/api/files/${n.id}/content`;
}

function nodeMenu(n, x, y, list) {
  if (n.e2e) { e2eRootMenu(n, x, y); return; } // шаринг, просмотр, теги и т. п. для шифротекста бессмысленны
  const w = writable(n) && (state.view !== 'files' || state.canWrite);
  const items = [];
  if (n.is_dir) items.push({ icon: '📂', label: 'Открыть', onClick: () => openNode(n, list) });
  else {
    if (canOffice(n)) items.push({ icon: '📝', label: 'Открыть в редакторе', onClick: () => window.open('/office/' + n.id, '_blank') });
    if (isText(n) && n.size < 2 * 1024 * 1024) items.push({ icon: '✏️', label: w ? 'Редактировать' : 'Открыть', onClick: () => editorDialog(n) });
    if (canPreview(n)) items.push({ icon: '👁', label: 'Просмотр', onClick: () => preview(n, list.filter(canPreview)) });
    items.push({ icon: '⬇', label: 'Скачать', onClick: () => { location.href = `/api/files/${n.id}/content`; } });
  }
  if (n.is_dir) items.push({ icon: '🗜️', label: 'Скачать ZIP', onClick: () => { location.href = `/api/files/${n.id}/zip`; } });
  if (state.view !== 'files' && n.parent_id !== undefined && !n.owner) items.push({ icon: '📍', label: 'Показать в папке', onClick: () => { location.hash = '#/files/' + (n.parent_id || ''); } });
  items.push('-',
    { icon: 'ℹ️', label: 'Свойства', onClick: () => detailsPanel(n) },
    { icon: '🔗', label: 'Поделиться', onClick: () => shareDialog(n) },
    { icon: '🏷️', label: 'Теги', onClick: () => detailsPanel(n, 'tags') },
    { icon: '💬', label: 'Комментарии', onClick: () => detailsPanel(n, 'comments') },
    { icon: n.favorite ? '★' : '☆', label: n.favorite ? 'Убрать из избранного' : 'В избранное', onClick: async () => {
      try { await api(n.favorite ? 'DELETE' : 'PUT', `/api/files/${n.id}/favorite`); n.favorite = !n.favorite; drawList(); } catch (e) { report(e); }
    } });
  if (w) items.push({ icon: '✏️', label: 'Переименовать', onClick: () => renameNode(n) }, { icon: '➡️', label: 'Переместить', onClick: () => moveDialog([n], false) });
  items.push({ icon: '📋', label: 'Копировать', onClick: () => moveDialog([n], true) });
  if (!n.is_dir) items.push({ icon: '🕓', label: 'Версии', onClick: () => detailsPanel(n, 'versions') });
  if (w) items.push('-', { icon: '🗑️', label: 'Удалить', danger: true, onClick: () => trashNodes([n]) });
  contextMenu(x, y, items);
}

// ---------- действия ----------
function newMenu(e) {
  const r = e.currentTarget.getBoundingClientRect();
  contextMenu(r.left, r.bottom + 4, [
    { icon: '📁', label: 'Папку', onClick: newFolder },
    { icon: '🗒️', label: 'Заметку (Markdown)', onClick: () => newTextFile('Новая заметка.md', '# Новая заметка\n\n') },
    { icon: '📄', label: 'Текстовый файл', onClick: () => newTextFile('Новый файл.txt', '') },
    // E2E-папки — только в своём дереве: ключ владельца не подходит к чужой общей папке
    state.sharedBy ? null : { icon: '🔒', label: 'Зашифрованную папку (E2E)', onClick: () => e2eNewFolder(state.parent) },
  ]);
}

async function newFolder() {
  const name = await askText('Новая папка', 'Название', 'Новая папка', 'Создать');
  if (!name) return;
  try { await api('POST', '/api/files/folder', { parent: state.parent || '', name }); route(); }
  catch (e) { report(e); }
}

async function newTextFile(def, content) {
  const name = await askText('Новый файл', 'Имя файла', def, 'Создать');
  if (!name) return;
  try {
    const q = new URLSearchParams({ name, conflict: 'rename' });
    if (state.parent) q.set('parent', state.parent);
    const n = await api('POST', '/api/files/upload?' + q, new Blob([content], { type: 'text/plain' }));
    route(); editorDialog(n);
  } catch (e) { report(e); }
}

async function renameNode(n) {
  const name = await askText('Переименовать', 'Новое имя', n.name, 'Сохранить');
  if (!name || name === n.name) return;
  try { await api('PATCH', '/api/files/' + n.id, { name }); route(); } catch (e) { report(e); }
}

async function trashNodes(nodes) {
  const text = nodes.length === 1 ? `«${nodes[0].name}» будет перемещён в корзину.` : `Объекты (${nodes.length}) будут перемещены в корзину.`;
  if (!await confirmDialog('Удалить?', text)) return;
  let ok = 0;
  for (const n of nodes) { try { await api('DELETE', '/api/files/' + n.id); ok++; } catch (e) { report(e); } }
  if (ok) toast(`В корзине: ${ok}`);
  state.selected.clear(); route(); refreshUsage();
}

function moveDialog(nodes, copy) {
  const movingIds = new Set(nodes.filter(n => n.is_dir).map(n => n.id));
  let cur = null, curCrumbs = [];
  const list = h('div', { class: 'picker' });
  const where = h('div', { class: 'hint', style: 'margin-bottom:6px' });
  const load = async id => {
    const d = await api('GET', '/api/files' + (id ? '?parent=' + id : ''));
    cur = id; curCrumbs = d.breadcrumbs;
    where.textContent = 'Куда: /' + curCrumbs.map(c => c.name).join('/');
    const rows = [];
    if (id) rows.push(h('div', { class: 'row', onclick: () => load(curCrumbs.length > 1 ? curCrumbs[curCrumbs.length - 2].id : null).catch(report) }, '⬆', '..'));
    for (const n of d.items.filter(n => n.is_dir)) {
      const dis = movingIds.has(n.id);
      rows.push(h('div', { class: 'row' + (dis ? ' disabled' : ''), onclick: dis ? null : () => load(n.id).catch(report) }, '📁', n.name));
    }
    if (!rows.length) rows.push(h('div', { class: 'row disabled' }, 'Нет вложенных папок'));
    list.replaceChildren(...rows);
  };
  modal({
    title: (copy ? 'Копировать' : 'Переместить') + (nodes.length === 1 ? ` «${nodes[0].name}»` : ` (${nodes.length})`),
    body: [where, list],
    buttons: [{ label: 'Отмена' }, {
      label: copy ? 'Копировать сюда' : 'Переместить сюда', primary: true,
      onClick: async () => {
        let ok = 0;
        for (const n of nodes) {
          try {
            if (copy) await api('POST', `/api/files/${n.id}/copy`, { parent: cur });
            else await api('PATCH', '/api/files/' + n.id, { parent: cur });
            ok++;
          } catch (e) { report(e); }
        }
        if (ok) toast((copy ? 'Скопировано: ' : 'Перемещено: ') + ok);
        state.selected.clear(); route(); if (copy) refreshUsage();
      },
    }],
  });
  load(state.view === 'files' && !state.sharedBy ? state.parent : null).catch(report);
}

// ---------- общий доступ ----------
function shareDialog(n) {
  const body = h('div');
  const m = modal({ title: 'Поделиться «' + n.name + '»', wide: true, body });
  const load = async () => {
    const acc = await api('GET', `/api/files/${n.id}/access`);
    if (!acc.owner) {
      body.replaceChildren(h('p', null, `Это ${n.is_dir ? 'папка' : 'файл'} пользователя `, h('b', null, acc.owner_name), '. ',
        acc.can_write ? 'У вас есть право изменять.' : 'У вас доступ только на просмотр.'),
        h('p', { class: 'hint' }, 'Делиться дальше может только владелец.'));
      return;
    }
    // --- пользователи и группы
    const q = h('input', { type: 'text', placeholder: 'Имя пользователя или группы…', autocomplete: 'off' });
    const results = h('div', { class: 'picker hidden', style: 'max-height:180px' });
    const rw = h('input', { type: 'checkbox' });
    let timer;
    q.addEventListener('input', () => {
      clearTimeout(timer);
      timer = setTimeout(async () => {
        if (!q.value.trim()) { results.classList.add('hidden'); return; }
        const list = await api('GET', '/api/directory?q=' + encodeURIComponent(q.value.trim()));
        results.classList.toggle('hidden', !list.length);
        results.replaceChildren(...list.map(e => h('div', { class: 'row', onclick: async () => {
          try {
            await api('POST', '/api/user-shares', { node_id: n.id, [e.kind === 'user' ? 'user_id' : 'group_id']: e.id, can_write: rw.checked });
            q.value = ''; results.classList.add('hidden'); toast('Доступ выдан: ' + e.name); load(); drawList();
          } catch (ex) { report(ex); }
        } }, e.kind === 'user' ? '👤' : '👥', e.name, e.kind === 'group' ? h('span', { class: 'badge' }, 'группа') : null)));
      }, 200);
    });
    const userRows = acc.user_shares.map(us => h('div', { class: 'list-row' },
      h('span', null, us.group_id ? '👥' : '👤'), h('div', { class: 'grow' }, us.target_name, us.group_id ? h('span', { class: 'badge' }, 'группа') : null),
      h('label', { class: 'check', style: 'margin:0' }, h('input', { type: 'checkbox', checked: us.can_write, onchange: async e => {
        try { await api('PATCH', '/api/user-shares/' + us.id, { can_write: e.target.checked }); toast('Права изменены'); } catch (ex) { report(ex); }
      } }), 'изменение'),
      h('button', { class: 'btn danger', onclick: async () => { try { await api('DELETE', '/api/user-shares/' + us.id); load(); drawList(); } catch (e) { report(e); } } }, 'Закрыть доступ')));
    // --- публичные ссылки
    const pw = h('input', { type: 'text', placeholder: 'без пароля', autocomplete: 'off' });
    const exp = h('input', { type: 'datetime-local' });
    const up = h('input', { type: 'checkbox' });
    body.replaceChildren(
      h('h4', { class: 'sect' }, '👥 Пользователи и группы'),
      h('div', { class: 'row-inline' }, h('div', { style: 'flex:1;position:relative' }, q, results),
        h('label', { class: 'check', style: 'margin:0;white-space:nowrap' }, rw, 'может изменять')),
      userRows.length ? h('div', null, userRows) : h('div', { class: 'hint', style: 'margin-top:6px' }, 'Пока ни с кем не поделились.'),
      h('h4', { class: 'sect' }, '🔗 Публичные ссылки'),
      acc.links.length ? h('div', null, acc.links.map(sh => shareRow(sh, load))) : h('div', { class: 'hint' }, 'Ссылок нет.'),
      h('div', { class: 'card', style: 'padding:12px;margin:8px 0 0;max-width:none' },
        h('div', { class: 'row' },
          h('div', null, h('label', null, 'Пароль'), pw),
          h('div', null, h('label', null, 'Действует до'), exp)),
        n.is_dir ? h('label', { class: 'check', style: 'margin-top:12px' }, up, 'Разрешить гостям загружать файлы в эту папку') : null,
        h('div', { style: 'margin-top:12px' }, h('button', { class: 'btn primary', onclick: async () => {
          try {
            await api('POST', '/api/shares', { node_id: n.id, password: pw.value, allow_upload: up.checked, expires_at: exp.value ? new Date(exp.value).toISOString() : null });
            await load(); drawList();
          } catch (e) { report(e); }
        } }, 'Создать ссылку'))));
  };
  load().catch(e => { report(e); m.close(); });
}

function shareRow(sh, reload) {
  const inp = h('input', { type: 'text', readonly: true, value: sh.url });
  const meta = [sh.has_password ? '🔒 с паролем' : 'без пароля',
    sh.expires_at ? 'до ' + fmtDate(sh.expires_at) : 'бессрочно',
    sh.allow_upload ? 'загрузка разрешена' : null,
    'скачиваний: ' + sh.downloads].filter(Boolean).join(' · ');
  return h('div', { class: 'list-row', style: 'flex-direction:column;align-items:stretch' },
    reload.showName ? h('div', null, (sh.is_dir ? '📁 ' : '📄 ') + sh.node_name) : null,
    h('div', { class: 'linkbox' }, inp,
      h('button', { class: 'btn', onclick: () => copyText(sh.url) }, 'Копировать'),
      h('button', { class: 'btn', title: 'QR-код', onclick: () => qrDialog(sh.url) }, '▦'),
      h('button', { class: 'btn danger', onclick: async () => {
        if (!await confirmDialog('Удалить ссылку?', 'Ссылка перестанет работать для всех, кому вы её отправили.')) return;
        try { await api('DELETE', '/api/shares/' + sh.id); await reload(); } catch (e) { report(e); }
      } }, 'Удалить')),
    h('div', { class: 'sub' }, meta));
}

function qrSVG(text, cell = 5) {
  const qr = qrcode(0, 'M'); qr.addData(unescape(encodeURIComponent(text))); qr.make();
  const div = h('div', { class: 'qr', html: qr.createSvgTag({ cellSize: cell, margin: 2, scalable: true }) });
  return div;
}

function qrDialog(text) {
  modal({ title: 'QR-код', body: [qrSVG(text, 6), h('div', { class: 'hint', style: 'text-align:center;word-break:break-all' }, text)] });
}

// ---------- панель свойств ----------
async function detailsPanel(n, tab = 'info') {
  const tabs = [['info', 'Сведения'], ['tags', 'Теги'], ['comments', 'Комментарии'], ...(n.is_dir ? [] : [['versions', 'Версии']]), ['activity', 'Журнал']];
  const nav = h('div', { class: 'tabs' });
  const pane = h('div', { class: 'tab-pane' });
  const m = modal({ title: n.name, wide: true, body: [nav, pane] });
  const show = async t => {
    tab = t;
    nav.replaceChildren(...tabs.map(([id, label]) => h('button', { class: 'tab' + (id === t ? ' active' : ''), onclick: () => show(id) }, label)));
    pane.replaceChildren(h('div', { class: 'hint' }, 'Загрузка…'));
    try { await ({ info: detInfo, tags: detTags, comments: detComments, versions: detVersions, activity: detActivity })[t](n, pane, m); }
    catch (e) { pane.replaceChildren(h('div', { class: 'error' }, e.message)); }
  };
  show(tab);
}

async function detInfo(n, pane) {
  const full = await api('GET', '/api/files/' + n.id);
  const rows = [
    ['Тип', full.is_dir ? 'Папка' : (full.mime || '—')],
    !full.is_dir && ['Размер', fmtSize(full.size) + ` (${full.size.toLocaleString('ru-RU')} байт)`],
    full.path && ['Путь', full.path],
    ['Создан', fmtDate(full.created_at)],
    ['Изменён', fmtDate(full.updated_at)],
    full.taken_at && ['Дата съёмки', fmtDate(full.taken_at)],
    ['Права', full.can_write ? 'чтение и изменение' : 'только просмотр'],
  ].filter(Boolean);
  pane.replaceChildren(
    hasThumb(full) ? h('div', { class: 'det-thumb' }, h('img', { src: `/api/files/${full.id}/thumb?s=512`, alt: '' })) : null,
    h('table', { class: 'kv' }, rows.map(([k, v]) => h('tr', null, h('th', null, k), h('td', null, v)))),
    h('div', { class: 'row-inline', style: 'margin-top:14px' },
      !full.is_dir ? h('a', { class: 'btn', href: `/api/files/${full.id}/content` }, '⬇ Скачать') : h('a', { class: 'btn', href: `/api/files/${full.id}/zip` }, '🗜️ ZIP'),
      h('button', { class: 'btn', onclick: () => shareDialog(full) }, '🔗 Поделиться'),
      canOffice(full) ? h('a', { class: 'btn', href: '/office/' + full.id, target: '_blank' }, '📝 Открыть в редакторе') : null));
}

async function detTags(n, pane) {
  const [all, full] = await Promise.all([api('GET', '/api/tags'), api('GET', '/api/files/' + n.id)]);
  const mine = new Set((full.tags || []).map(t => t.id));
  const save = async () => {
    try { await api('PUT', `/api/files/${n.id}/tags`, { tag_ids: [...mine] }); n.tags = all.filter(t => mine.has(t.id)); drawList(); loadSidebarTags(); }
    catch (e) { report(e); }
  };
  const name = h('input', { type: 'text', placeholder: 'Новый тег' });
  const color = h('input', { type: 'color', value: '#2f6fed' });
  pane.replaceChildren(
    h('div', { class: 'tag-cloud' }, all.length ? all.map(t => h('label', { class: 'tag-pick' },
      h('input', { type: 'checkbox', checked: mine.has(t.id), onchange: e => { e.target.checked ? mine.add(t.id) : mine.delete(t.id); save(); } }),
      h('span', { class: 'tag', style: `background:${t.color}` }, t.name),
      h('button', { class: 'icon-btn', title: 'Удалить тег везде', onclick: async e => {
        e.preventDefault();
        if (!await confirmDialog('Удалить тег?', `Тег «${t.name}» будет снят со всех файлов.`)) return;
        try { await api('DELETE', '/api/tags/' + t.id); loadSidebarTags(); detTags(n, pane); drawList(); } catch (ex) { report(ex); }
      } }, '×'))) : h('div', { class: 'hint' }, 'Тегов пока нет — создайте первый.')),
    h('div', { class: 'row-inline', style: 'margin-top:14px' }, name, color,
      h('button', { class: 'btn primary', onclick: async () => {
        try {
          let t;
          try { t = await api('POST', '/api/tags', { name: name.value, color: color.value }); }
          catch (e) {
            // такой тег уже есть — просто прикрепляем его
            t = e.status === 409 && all.find(x => x.name.toLowerCase() === name.value.trim().toLowerCase());
            if (!t) throw e;
          }
          mine.add(t.id); await save(); detTags(n, pane);
        } catch (e) { report(e); }
      } }, 'Добавить')),
    h('div', { class: 'hint', style: 'margin-top:8px' }, 'Теги личные: другие пользователи их не видят.'));
}

async function detComments(n, pane) {
  const list = await api('GET', `/api/files/${n.id}/comments`);
  const ta = h('textarea', { rows: 3, placeholder: 'Написать комментарий… (Ctrl+Enter — отправить)' });
  const send = async () => {
    if (!ta.value.trim()) return;
    try { await api('POST', `/api/files/${n.id}/comments`, { body: ta.value }); detComments(n, pane); } catch (e) { report(e); }
  };
  ta.addEventListener('keydown', e => { if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) send(); });
  pane.replaceChildren(
    h('div', { class: 'comments' }, list.length ? list.map(c => h('div', { class: 'comment' },
      h('div', { class: 'c-head' }, h('b', null, c.username), h('span', { class: 'sub' }, ' · ' + fmtDate(c.created_at)),
        c.user_id === state.me.id || !n.owner ? h('button', { class: 'icon-btn', title: 'Удалить', onclick: async () => {
          try { await api('DELETE', '/api/comments/' + c.id); detComments(n, pane); } catch (e) { report(e); }
        } }, '×') : null),
      h('div', { class: 'c-body' }, c.body))) : h('div', { class: 'hint' }, 'Комментариев нет. Их видят все, у кого есть доступ к файлу.')),
    ta, h('div', { style: 'text-align:right;margin-top:8px' }, h('button', { class: 'btn primary', onclick: send }, 'Отправить')));
  setTimeout(() => ta.focus(), 30);
}

async function detVersions(n, pane) {
  const vs = await api('GET', `/api/files/${n.id}/versions`);
  pane.replaceChildren(h('div', { class: 'hint' }, 'При каждой перезаписи файла сохраняется предыдущая версия.'),
    ...vs.map(v => h('div', { class: 'list-row' },
      h('div', { class: 'grow' }, fmtDate(v.created_at), v.current ? h('span', { class: 'badge cur' }, 'текущая') : null,
        h('div', { class: 'sub' }, fmtSize(v.size) + ' · sha256 ' + v.sha256.slice(0, 12))),
      h('a', { class: 'btn', href: `/api/files/${n.id}/content?version=${v.id}` }, 'Скачать'),
      v.current || !writable(n) ? null : h('button', { class: 'btn', onclick: async () => {
        try { await api('POST', `/api/files/${n.id}/versions/${v.id}/restore`); toast('Версия восстановлена'); detVersions(n, pane); route(); }
        catch (e) { report(e); }
      } }, 'Восстановить'))));
}

const ACTIONS = {
  upload: 'загрузил(а)', mkdir: 'создал(а) папку', rename: 'переименовал(а)', move: 'переместил(а)', copy: 'скопировал(а)',
  trash: 'удалил(а) в корзину', restore: 'восстановил(а)', purge: 'удалил(а) навсегда', edit: 'изменил(а)',
  restore_version: 'восстановил(а) версию', share_link: 'создал(а) ссылку на', share_user: 'поделился(ась)',
  comment: 'прокомментировал(а)', public_download: 'скачано по ссылке:', public_upload: 'гость загрузил',
};

function activityRow(a) {
  return h('div', { class: 'list-row act' },
    h('div', { class: 'grow' },
      h('b', null, a.actor || 'система'), ' ', ACTIONS[a.action] || a.action, ' ',
      a.node_name ? h('span', { class: 'act-node' }, '«' + a.node_name + '»') : null,
      a.details ? h('span', { class: 'sub' }, ' — ' + a.details) : null),
    h('span', { class: 'sub', style: 'white-space:nowrap' }, fmtDate(a.at)));
}

async function detActivity(n, pane) {
  const list = await api('GET', '/api/activity?node=' + n.id);
  pane.replaceChildren(...(list.length ? list.map(activityRow) : [h('div', { class: 'hint' }, 'Событий нет.')]));
}

// ---------- редактор текстов и заметок ----------
async function editorDialog(n, opts = {}) {
  const res = await fetch(`/api/files/${n.id}/content`, { credentials: 'same-origin' });
  if (!res.ok) { report(new Error('Не удалось открыть файл')); return; }
  let text = await res.text();
  let version = n.version_id;
  const canW = writable(n) && !(state.view === 'files' && !state.canWrite);
  const ta = h('textarea', { class: 'editor', spellcheck: isMarkdown(n) ? 'true' : 'false', readonly: !canW });
  ta.value = text;
  const prev = h('div', { class: 'md-preview hidden' });
  const status = h('span', { class: 'sub' }, canW ? 'Ctrl+S — сохранить' : 'Только просмотр');
  let dirty = false;
  ta.addEventListener('input', () => { dirty = true; status.textContent = 'не сохранено'; });
  const save = async () => {
    if (!canW) return;
    try {
      const nn = await api('PUT', `/api/files/${n.id}/content`, new Blob([ta.value], { type: 'text/plain' }), { headers: { 'If-Match': version || '' } });
      version = nn.version_id; n.version_id = nn.version_id; n.size = nn.size; dirty = false;
      status.textContent = 'сохранено в ' + new Date().toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
      opts.onSave && opts.onSave(nn);
    } catch (e) {
      if (e.status === 412) toast('Файл изменён кем-то ещё. Скопируйте свои правки и откройте файл заново.', true);
      else report(e);
    }
  };
  ta.addEventListener('keydown', e => {
    if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); save(); }
    if (e.key === 'Tab') { e.preventDefault(); const s = ta.selectionStart; ta.setRangeText('  ', s, ta.selectionEnd, 'end'); }
  });
  const toggle = h('button', { class: 'btn', onclick: () => {
    const on = prev.classList.toggle('hidden');
    ta.classList.toggle('hidden', !on);
    if (!on) prev.innerHTML = renderMarkdown(ta.value);
    toggle.textContent = on ? '👁 Просмотр' : '✏️ Правка';
  } }, '👁 Просмотр');
  const m = modal({
    title: n.name, full: true,
    body: [h('div', { class: 'editor-bar' }, isMarkdown(n) ? toggle : null, status, h('span', { style: 'flex:1' }),
      canW ? h('button', { class: 'btn primary', onclick: save }, 'Сохранить') : null), ta, prev],
    onClose: () => { if (dirty && canW && confirm('Сохранить изменения?')) save(); route(); },
  });
  if (isMarkdown(n) && !canW) toggle.click();
  return m;
}

// ---------- просмотр ----------
function preview(n, list) {
  let idx = Math.max(0, list.findIndex(x => x.id === n.id));
  const stage = h('div', { class: 'stage' });
  const title = h('div', { class: 'nm' });
  const dl = h('a', { class: 'icon-btn', title: 'Скачать' }, '⬇');
  const info = h('button', { class: 'icon-btn', title: 'Свойства', onclick: () => { close(); detailsPanel(list[idx]); } }, 'ℹ️');
  const close = () => { ov.remove(); document.removeEventListener('keydown', onKey); };
  const base = n._public ? n._public : '/api/files/';
  const show = async () => {
    const cur = list[idx];
    title.textContent = cur.name;
    dl.href = `/api/files/${cur.id}/content`;
    const src = `/api/files/${cur.id}/content?inline=1`;
    if (isImage(cur)) {
      // сначала быстрая миниатюра 1024, потом оригинал
      const img = h('img', { src: `/api/files/${cur.id}/thumb?s=1024`, alt: cur.name });
      img.onerror = () => { img.src = src; };
      const full = new Image(); full.onload = () => { if (list[idx] === cur) img.src = src; }; full.src = src;
      stage.replaceChildren(img);
    } else if (isVideo(cur)) stage.replaceChildren(h('video', { src, controls: true, autoplay: true, playsinline: true }));
    else if (isAudio(cur)) stage.replaceChildren(h('audio', { src, controls: true, autoplay: true }));
    else if (isPDF(cur)) stage.replaceChildren(h('iframe', { src }));
    else {
      stage.replaceChildren(h('div', { style: 'color:#ccc' }, 'Загрузка…'));
      try { const t = await (await fetch(src)).text(); stage.replaceChildren(isMarkdown(cur) ? h('div', { class: 'md-preview light', html: renderMarkdown(t) }) : h('pre', null, t)); }
      catch (e) { stage.replaceChildren(h('div', null, e.message)); }
    }
  };
  const step = d => { if (list.length > 1) { idx = (idx + d + list.length) % list.length; show(); } };
  const onKey = e => { if (e.key === 'Escape') close(); if (e.key === 'ArrowRight') step(1); if (e.key === 'ArrowLeft') step(-1); };
  let tx = null;
  const ov = h('div', { class: 'preview',
    ontouchstart: e => { tx = e.touches[0].clientX; },
    ontouchend: e => { if (tx != null) { const dx = e.changedTouches[0].clientX - tx; if (Math.abs(dx) > 60) step(dx < 0 ? 1 : -1); tx = null; } } },
    h('header', null, title, info, dl, h('button', { class: 'icon-btn', onclick: close }, '×')),
    stage,
    list.length > 1 ? h('button', { class: 'nav-arrow prev', onclick: () => step(-1) }, '‹') : null,
    list.length > 1 ? h('button', { class: 'nav-arrow next', onclick: () => step(1) }, '›') : null);
  document.body.append(ov);
  document.addEventListener('keydown', onKey);
  show();
}

// ---------- загрузка файлов (с докачкой больших) ----------
const uploads = { queue: [], active: 0, panel: null, list: null, total: 0, done: 0 };

function uploadFiles(entries, parent = state.parent) {
  if (!entries.length) return;
  if (state.view === 'files' && !state.canWrite) { toast('В эту папку нельзя загружать', true); return; }
  if (!uploads.panel) {
    uploads.list = h('div', { class: 'items' });
    uploads.title = h('span');
    uploads.panel = h('div', { class: 'uploads' }, h('header', null, uploads.title,
      h('button', { class: 'icon-btn', onclick: () => { uploads.panel.remove(); uploads.panel = null; uploads.total = uploads.done = 0; } }, '×')), uploads.list);
    document.body.append(uploads.panel);
  }
  const max = state.usage ? state.usage.max_upload_bytes : 0;
  for (const e of entries) {
    const bar = h('div');
    const pct = h('span', null, '0%');
    const row = h('div', { class: 'up-item' }, h('div', { class: 'nm' }, h('span', null, (e.path ? e.path + '/' : '') + e.file.name), pct), h('div', { class: 'bar' }, bar));
    uploads.list.prepend(row);
    uploads.total++;
    if (max && e.file.size > max) { row.classList.add('fail'); pct.textContent = 'слишком большой'; uploads.done++; continue; }
    uploads.queue.push({ ...e, parent, bar, pct, row });
  }
  updateUploadTitle();
  pumpUploads();
}

function updateUploadTitle() {
  if (uploads.title) uploads.title.textContent = uploads.done < uploads.total ? `Загрузка: ${uploads.done} из ${uploads.total}` : `Загружено: ${uploads.total}`;
}

function pumpUploads() {
  while (uploads.active < 3 && uploads.queue.length) {
    const job = uploads.queue.shift();
    uploads.active++;
    const big = job.file.size > (state.usage?.chunk_size || 8 << 20) * 2;
    (big ? uploadChunked(job) : uploadOne(job)).finally(() => {
      uploads.active--; uploads.done++; updateUploadTitle();
      if (!uploads.queue.length && !uploads.active) {
        refreshUsage(); if (state.view === 'files') route();
        const p = uploads.panel;
        if (p && !p.querySelector('.up-item.fail')) setTimeout(() => {
          if (uploads.panel === p && !uploads.active && !uploads.queue.length) { p.remove(); uploads.panel = null; uploads.total = uploads.done = 0; }
        }, 4000);
      }
      pumpUploads();
    });
  }
}

function setProgress(job, done, total) {
  const p = total ? Math.round(done / total * 100) : 100;
  job.bar.style.width = p + '%'; job.pct.textContent = p + '%';
}
function finishJob(job, ok, msg) {
  job.row.classList.add(ok ? 'done' : 'fail'); job.bar.style.width = '100%'; job.pct.textContent = ok ? '✓' : msg;
}

function uploadOne(job) {
  return new Promise(resolve => {
    const q = new URLSearchParams({ name: job.file.name });
    if (job.parent) q.set('parent', job.parent);
    if (job.path) q.set('path', job.path);
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/files/upload?' + q);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = e => { if (e.lengthComputable) setProgress(job, e.loaded, e.total); };
    xhr.onload = () => {
      if (xhr.status < 300) finishJob(job, true);
      else { let m = 'ошибка'; try { m = JSON.parse(xhr.responseText).error; } catch (_) {} finishJob(job, false, m); }
      resolve();
    };
    xhr.onerror = () => { finishJob(job, false, 'сеть'); resolve(); };
    xhr.send(job.file);
  });
}

function putChunk(id, offset, blob, onProgress) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open('PUT', `/api/uploads/${id}?offset=${offset}`);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = e => { if (e.lengthComputable) onProgress(e.loaded); };
    xhr.onload = () => {
      let d = {}; try { d = JSON.parse(xhr.responseText); } catch (_) {}
      if (xhr.status < 300) resolve(d.received);
      else reject(Object.assign(new Error(d.error || 'ошибка ' + xhr.status), { status: xhr.status, received: d.received }));
    };
    xhr.onerror = () => reject(Object.assign(new Error('сеть'), { status: 0 }));
    xhr.send(blob);
  });
}

// Большие файлы — частями по 8 МБ. Сессия запоминается в localStorage: после обновления страницы
// и повторного выбора того же файла загрузка продолжится с места обрыва.
async function uploadChunked(job) {
  const f = job.file;
  const key = 'cb-up:' + [job.parent || '', job.path || '', f.name, f.size, f.lastModified].join('|');
  let id = null, offset = 0;
  try { id = localStorage.getItem(key); } catch (_) {}
  try {
    if (id) {
      try { const s = await api('GET', '/api/uploads/' + id); offset = s.received; job.pct.textContent = 'продолжаю…'; }
      catch (_) { id = null; }
    }
    if (!id) {
      const s = await api('POST', '/api/uploads', { parent: job.parent || '', name: f.name, path: job.path || '', size: f.size });
      id = s.id; offset = 0;
      try { localStorage.setItem(key, id); } catch (_) {}
    }
    const chunk = state.usage?.chunk_size || 8 << 20;
    let retries = 0;
    while (offset < f.size) {
      const end = Math.min(offset + chunk, f.size);
      try {
        offset = await putChunk(id, offset, f.slice(offset, end), loaded => setProgress(job, offset + loaded, f.size));
        retries = 0;
      } catch (e) {
        if (e.received != null) { offset = e.received; }
        if (++retries > 20 || (e.status >= 400 && e.status < 500 && e.status !== 409 && e.status !== 400)) throw e;
        job.pct.textContent = 'повтор через ' + Math.min(30, retries * 2) + ' с…';
        await new Promise(r => setTimeout(r, Math.min(30, retries * 2) * 1000));
        try { offset = (await api('GET', '/api/uploads/' + id)).received; } catch (_) {}
      }
      setProgress(job, offset, f.size);
    }
    await api('POST', `/api/uploads/${id}/complete`);
    try { localStorage.removeItem(key); } catch (_) {}
    finishJob(job, true);
  } catch (e) {
    finishJob(job, false, e.message);
  }
}

function setupDrop() {
  let depth = 0, zone = null;
  const isFiles = e => e.dataTransfer && [...e.dataTransfer.types].includes('Files');
  addEventListener('dragenter', e => {
    if (!isFiles(e) || state.view !== 'files' || !state.canWrite) return;
    depth++;
    if (!zone) { zone = h('div', { class: 'dropzone' }, 'Отпустите, чтобы загрузить сюда'); document.body.append(zone); }
  });
  addEventListener('dragleave', e => { if (!isFiles(e)) return; if (--depth <= 0 && zone) { zone.remove(); zone = null; depth = 0; } });
  addEventListener('dragover', e => { if (isFiles(e) && state.view === 'files') e.preventDefault(); });
  addEventListener('drop', async e => {
    if (!isFiles(e)) return;
    e.preventDefault(); depth = 0; if (zone) { zone.remove(); zone = null; }
    if (state.view !== 'files' || !state.canWrite) return;
    const out = [];
    const items = [...e.dataTransfer.items].map(i => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
    if (!items.length) { uploadFiles([...e.dataTransfer.files].map(f => ({ file: f, path: '' }))); return; }
    const walk = async (entry, dir) => {
      if (entry.isFile) { out.push({ file: await new Promise((res, rej) => entry.file(res, rej)), path: dir }); return; }
      const reader = entry.createReader();
      const sub = dir ? dir + '/' + entry.name : entry.name;
      let batch;
      do { batch = await new Promise((res, rej) => reader.readEntries(res, rej)); for (const c of batch) await walk(c, sub); } while (batch.length);
    };
    try { for (const it of items) await walk(it, ''); } catch (ex) { report(ex); }
    uploadFiles(out);
  });
}

function setupKeys() {
  addEventListener('keydown', e => {
    if (document.querySelector('.overlay, .preview') || /INPUT|TEXTAREA/.test(document.activeElement.tagName)) return;
    const sel = state.items.filter(n => state.selected.has(n.id));
    if (e.key === 'Delete' && sel.length && ['files', 'recent', 'search', 'favorites'].includes(state.view)) trashNodes(sel);
    if (e.key === 'F2' && sel.length === 1) { e.preventDefault(); renameNode(sel[0]); }
    if (e.key === 'i' && sel.length === 1) detailsPanel(sel[0]);
    if ((e.ctrlKey || e.metaKey) && e.key === 'a' && state.view === 'files') { e.preventDefault(); state.items.forEach(n => state.selected.add(n.id)); drawList(); }
    if (e.key === 'Escape' && state.selected.size) { state.selected.clear(); drawList(); }
    if (e.key === '/') { e.preventDefault(); els.search.focus(); }
  });
}

// ---------- уведомления ----------
function renderUnread(n) {
  els.bellCount.textContent = n > 99 ? '99+' : n;
  els.bellCount.classList.toggle('hidden', !n);
}

async function pollNotifications() {
  try { const d = await api('GET', '/api/notifications'); renderUnread(d.unread); } catch (_) {}
}

async function openNotifications(btn) {
  closeCtx();
  const d = await api('GET', '/api/notifications');
  const r = btn.getBoundingClientRect();
  ctxEl = h('div', { class: 'ctx notif' },
    h('div', { class: 'notif-head' }, h('b', null, 'Уведомления'),
      d.items.length ? h('button', { class: 'btn ghost', onclick: async () => { await api('DELETE', '/api/notifications'); closeCtx(); renderUnread(0); } }, 'Очистить') : null),
    d.items.length ? d.items.map(n => h('a', { class: 'notif-item' + (n.read_at ? '' : ' unread'), href: n.link || '#', onclick: () => closeCtx() },
      h('div', { class: 'nt' }, n.title), n.body ? h('div', { class: 'sub' }, n.body) : null, h('div', { class: 'sub' }, fmtDate(n.created_at))))
      : h('div', { class: 'hint', style: 'padding:14px' }, 'Новых уведомлений нет.'));
  document.body.append(ctxEl);
  const w = ctxEl.getBoundingClientRect().width;
  ctxEl.style.left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8)) + 'px';
  ctxEl.style.top = (r.bottom + 6) + 'px';
  if (d.unread) { api('POST', '/api/notifications/read', { ids: [] }).catch(() => {}); renderUnread(0); }
}

// ---------- PWA: установка на телефон и «Поделиться → CloudBox» ----------
function registerSW() {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js', { scope: '/' }).catch(() => {});
}

// Файлы, присланные через системное меню «Поделиться» (Android), service worker кладёт в IndexedDB.
async function receiveSharedFiles() {
  if (!location.hash.startsWith('#/receive') || !('indexedDB' in window)) return;
  const db = await new Promise((res, rej) => { const r = indexedDB.open('cloudbox-share', 1); r.onupgradeneeded = () => r.result.createObjectStore('files'); r.onsuccess = () => res(r.result); r.onerror = () => rej(r.error); });
  const files = await new Promise(res => { const tx = db.transaction('files', 'readonly'); const g = tx.objectStore('files').get('pending'); g.onsuccess = () => res(g.result || []); g.onerror = () => res([]); });
  if (!files.length) { location.hash = '#/files'; return; }
  await new Promise(res => { const tx = db.transaction('files', 'readwrite'); tx.objectStore('files').delete('pending'); tx.oncomplete = res; tx.onerror = res; });
  const folder = 'Загрузки с телефона';
  toast(`Загружаю ${files.length} файл(ов) в «${folder}»`);
  uploadFiles(files.map(f => ({ file: f, path: folder })), null);
  history.replaceState(null, '', '#/files');
}

window.addEventListener('load', boot);
