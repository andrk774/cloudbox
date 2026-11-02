'use strict';
/* CloudBox — веб-интерфейс. Без фреймворков и сборки. */

// ---------- утилиты ----------
const $ = (s, el = document) => el.querySelector(s);

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'html') el.innerHTML = v; // только для своих статических строк
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat()) {
    if (kid == null || kid === false) continue;
    el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  }
  return el;
}

class ApiError extends Error { constructor(msg, status) { super(msg); this.status = status; } }

async function api(method, url, body) {
  const opt = { method, headers: {}, credentials: 'same-origin' };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  const res = await fetch(url, opt);
  let data = null;
  try { data = await res.json(); } catch (_) {}
  if (!res.ok) {
    if (res.status === 401 && state.me && !url.startsWith('/api/auth')) { location.reload(); }
    throw new ApiError((data && data.error) || `Ошибка ${res.status}`, res.status);
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
  const sameDay = d.toDateString() === now.toDateString();
  if (sameDay) return 'сегодня, ' + d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
  return d.toLocaleDateString('ru-RU', { day: 'numeric', month: 'short', year: d.getFullYear() === now.getFullYear() ? undefined : 'numeric' })
    + ', ' + d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
}

function iconFor(n) {
  if (n.is_dir) return '📁';
  const m = n.mime || '';
  const ext = (n.name.split('.').pop() || '').toLowerCase();
  if (m.startsWith('image/')) return '🖼️';
  if (m.startsWith('video/')) return '🎬';
  if (m.startsWith('audio/')) return '🎵';
  if (m === 'application/pdf') return '📕';
  if (['zip', 'rar', '7z', 'tar', 'gz', 'xz', 'bz2'].includes(ext)) return '🗜️';
  if (['doc', 'docx', 'odt', 'rtf'].includes(ext)) return '📝';
  if (['xls', 'xlsx', 'ods', 'csv'].includes(ext)) return '📊';
  if (['ppt', 'pptx', 'odp'].includes(ext)) return '📽️';
  if (['js', 'ts', 'go', 'py', 'c', 'cpp', 'h', 'rs', 'java', 'json', 'yml', 'yaml', 'sh', 'html', 'css', 'sql', 'ino'].includes(ext)) return '💻';
  if (m.startsWith('text/')) return '📄';
  return '📄';
}

const isImage = n => /^image\/(png|jpe?g|gif|webp|bmp|avif)$/.test(n.mime || '');
const isVideo = n => (n.mime || '').startsWith('video/');
const isAudio = n => (n.mime || '').startsWith('audio/');
const isPDF = n => n.mime === 'application/pdf';
const isText = n => (n.mime || '').startsWith('text/') || /\.(md|json|ya?ml|go|py|js|ts|sh|ino|c|h|cpp|rs|sql|conf|ini|log|toml|env)$/i.test(n.name);
const canPreview = n => !n.is_dir && (isImage(n) || isVideo(n) || isAudio(n) || isPDF(n) || (isText(n) && n.size < 2 * 1024 * 1024));

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
function modal({ title, body, buttons = [], wide = false, onClose }) {
  const close = () => { ov.remove(); document.removeEventListener('keydown', onKey); onClose && onClose(); };
  const onKey = e => { if (e.key === 'Escape') close(); };
  const footer = h('footer', null, buttons.map(b => h('button', {
    class: 'btn' + (b.primary ? ' primary' : '') + (b.danger ? ' danger' : ''),
    onclick: async ev => {
      ev.target.disabled = true;
      try { if ((await (b.onClick ? b.onClick() : true)) !== false) close(); }
      catch (e) { report(e); }
      finally { ev.target.disabled = false; }
    },
  }, b.label)));
  const m = h('div', { class: 'modal' + (wide ? ' wide' : '') },
    h('header', null, h('span', null, title), h('button', { class: 'icon-btn', onclick: close, title: 'Закрыть' }, '×')),
    h('div', { class: 'body' }, body), buttons.length ? footer : null);
  const ov = h('div', { class: 'overlay', onmousedown: e => { if (e.target === ov) close(); } }, m);
  document.body.append(ov);
  document.addEventListener('keydown', onKey);
  setTimeout(() => { const i = m.querySelector('input'); if (i) { i.focus(); i.select && i.select(); } }, 30);
  return { close, el: m };
}

function askText(title, label, value = '', okLabel = 'OK') {
  return new Promise(resolve => {
    let done = false;
    const inp = h('input', { type: 'text', value });
    const finish = v => { done = true; resolve(v); };
    const m = modal({
      title, body: [h('label', null, label), inp],
      buttons: [{ label: 'Отмена' }, { label: okLabel, primary: true, onClick: () => finish(inp.value.trim()) }],
      onClose: () => { if (!done) resolve(null); },
    });
    inp.addEventListener('keydown', e => { if (e.key === 'Enter') { finish(inp.value.trim()); m.close(); } });
    // выделяем имя без расширения
    setTimeout(() => { const dot = value.lastIndexOf('.'); if (dot > 0) inp.setSelectionRange(0, dot); }, 40);
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
  ctxEl = h('div', { class: 'ctx' }, items.map(it => it === '-' ? h('hr') : h('button', {
    class: it.danger ? 'danger' : '', onclick: () => { closeCtx(); it.onClick(); },
  }, h('span', null, it.icon || ''), it.label)));
  document.body.append(ctxEl);
  const r = ctxEl.getBoundingClientRect();
  ctxEl.style.left = Math.min(x, innerWidth - r.width - 8) + 'px';
  ctxEl.style.top = Math.min(y, innerHeight - r.height - 8) + 'px';
}
function closeCtx() { if (ctxEl) { ctxEl.remove(); ctxEl = null; } }
document.addEventListener('mousedown', e => { if (ctxEl && !ctxEl.contains(e.target)) closeCtx(); });
document.addEventListener('scroll', closeCtx, true);

// ---------- состояние ----------
const state = {
  me: null, usage: null,
  view: 'files', parent: null, items: [], crumbs: [],
  selected: new Set(), sort: { key: 'name', dir: 1 },
};

// ---------- вход / регистрация ----------
async function boot() {
  let st;
  try { st = await api('GET', '/api/auth/status'); } catch (e) { document.body.textContent = 'Сервер недоступен: ' + e.message; return; }
  if (st.logged_in) { await loadMe(); renderApp(); } else renderAuth(st);
}

async function loadMe() {
  const d = await api('GET', '/api/me');
  state.me = d.user; state.usage = d;
}

function renderAuth(st) {
  let mode = st.first_user ? 'register' : 'login';
  const root = $('#root');
  const draw = () => {
    const user = h('input', { type: 'text', autocomplete: 'username', autocapitalize: 'off' });
    const pass = h('input', { type: 'password', autocomplete: mode === 'login' ? 'current-password' : 'new-password' });
    const err = h('div', { class: 'error' });
    const submit = async e => {
      e.preventDefault(); err.textContent = '';
      try {
        await api('POST', mode === 'login' ? '/api/auth/login' : '/api/auth/register', { username: user.value.trim(), password: pass.value });
        await loadMe(); renderApp();
      } catch (ex) { err.textContent = ex.message; }
    };
    root.replaceChildren(h('div', { class: 'auth' }, h('form', { class: 'auth-card', onsubmit: submit },
      h('div', { class: 'logo' }, h('div', { class: 'logo-mark' }, '☁'), 'CloudBox'),
      h('h1', null, mode === 'login' ? 'Вход' : (st.first_user ? 'Создание администратора' : 'Регистрация')),
      st.first_user ? h('div', { class: 'hint' }, 'Это первый запуск. Первый пользователь станет администратором.') : null,
      h('label', null, 'Имя пользователя'), user,
      h('label', null, 'Пароль'), pass,
      h('button', { class: 'btn primary', type: 'submit' }, mode === 'login' ? 'Войти' : 'Создать учётную запись'),
      err,
      !st.first_user && st.registration_open ? h('div', { class: 'auth-switch' },
        mode === 'login' ? 'Нет аккаунта? ' : 'Уже есть аккаунт? ',
        h('a', { href: '#', onclick: e => { e.preventDefault(); mode = mode === 'login' ? 'register' : 'login'; draw(); } },
          mode === 'login' ? 'Зарегистрироваться' : 'Войти')) : null,
    )));
    user.focus();
  };
  draw();
}

// ---------- каркас приложения ----------
let els = {};

function renderApp() {
  const navItem = (id, ico, label) => h('a', { href: '#/' + id, 'data-view': id }, h('span', { class: 'ico' }, ico), label);
  els.quota = h('div', { class: 'quota' });
  els.nav = h('nav', { class: 'nav' },
    navItem('files', '📁', 'Все файлы'),
    navItem('recent', '🕘', 'Недавние'),
    navItem('shares', '🔗', 'Мои ссылки'),
    navItem('trash', '🗑️', 'Корзина'),
    navItem('settings', '⚙️', 'Настройки'),
    state.me.is_admin ? navItem('admin', '👥', 'Пользователи') : null);
  els.crumbs = h('div', { class: 'crumbs' });
  els.search = h('input', { type: 'search', class: 'search', placeholder: 'Поиск по имени…' });
  els.search.addEventListener('keydown', e => {
    if (e.key === 'Enter') location.hash = els.search.value.trim() ? '#/search/' + encodeURIComponent(els.search.value.trim()) : '#/files';
  });
  els.fileInput = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: e => { uploadFiles([...e.target.files].map(f => ({ file: f, path: '' }))); e.target.value = ''; } });
  els.dirInput = h('input', { type: 'file', webkitdirectory: true, class: 'hidden', onchange: e => {
    uploadFiles([...e.target.files].map(f => ({ file: f, path: f.webkitRelativePath.split('/').slice(0, -1).join('/') }))); e.target.value = '';
  } });
  els.actions = h('div', { class: 'actions' },
    h('button', { class: 'btn', onclick: newFolder, title: 'Новая папка' }, '＋', h('span', { class: 'lbl' }, 'Папка')),
    h('button', { class: 'btn', onclick: () => els.dirInput.click(), title: 'Загрузить папку' }, '📂', h('span', { class: 'lbl' }, 'Загрузить папку')),
    h('button', { class: 'btn primary', onclick: () => els.fileInput.click() }, '⬆', h('span', { class: 'lbl' }, 'Загрузить')),
    els.fileInput, els.dirInput);
  els.content = h('div', { class: 'content' });
  els.app = h('div', { class: 'app' },
    h('aside', { class: 'sidebar' },
      h('div', { class: 'logo' }, h('div', { class: 'logo-mark' }, '☁'), 'CloudBox'),
      els.nav, h('div', { class: 'spacer' }), els.quota,
      h('div', { class: 'userbox' }, h('span', null, '👤 ', state.me.username),
        h('button', { class: 'icon-btn', title: 'Выйти', onclick: logout }, '⎋'))),
    h('main', { class: 'main' },
      h('div', { class: 'topbar' },
        h('button', { class: 'icon-btn menu-toggle', onclick: () => els.app.classList.toggle('menu-open') }, '☰'),
        els.crumbs, els.search, els.actions),
      els.content));
  $('#root').replaceChildren(els.app);
  els.nav.addEventListener('click', () => els.app.classList.remove('menu-open'));
  setupDrop();
  setupKeys();
  renderQuota();
  addEventListener('hashchange', route);
  route();
}

function renderQuota() {
  const u = state.usage; if (!u) return;
  const pct = u.quota_bytes > 0 ? Math.min(100, u.used_bytes / u.quota_bytes * 100) : 0;
  els.quota.replaceChildren(
    h('div', null, 'Занято ', fmtSize(u.used_bytes), u.quota_bytes > 0 ? ' из ' + fmtSize(u.quota_bytes) : ''),
    u.quota_bytes > 0 ? h('div', { class: 'bar' + (pct > 90 ? ' warn' : '') }, h('div', { style: `width:${pct}%` })) : null);
}

async function refreshUsage() { try { const d = await api('GET', '/api/me'); state.usage = d; renderQuota(); } catch (_) {} }

async function logout() {
  await api('POST', '/api/auth/logout').catch(() => {});
  location.hash = ''; location.reload();
}

// ---------- маршрутизация ----------
function route() {
  const parts = location.hash.replace(/^#\/?/, '').split('/');
  const view = parts[0] || 'files';
  state.view = view; state.selected.clear();
  for (const a of els.nav.querySelectorAll('a')) a.classList.toggle('active', a.dataset.view === (view === 'search' ? '' : view));
  els.actions.classList.toggle('hidden', view !== 'files');
  if (view !== 'search') els.search.value = '';
  const run = {
    files: () => viewFiles(parts[1] || null),
    recent: viewRecent,
    search: () => viewSearch(decodeURIComponent(parts.slice(1).join('/'))),
    trash: viewTrash, shares: viewShares, settings: viewSettings,
    admin: viewAdmin,
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
  // перетаскивание файлов на «крошку» — перемещение в эту папку
  for (const a of els.crumbs.querySelectorAll('a[data-drop]')) attachDropTarget(a, a.dataset.drop === 'root' ? null : a.dataset.drop);
}

// ---------- список файлов ----------
async function viewFiles(parentId) {
  state.parent = parentId;
  const d = await api('GET', '/api/files' + (parentId ? '?parent=' + parentId : ''));
  if (state.view !== 'files' || state.parent !== parentId) return;
  state.items = d.items; state.crumbs = d.breadcrumbs;
  setCrumbs([{ label: 'Все файлы', href: '#/files', id: null }, ...d.breadcrumbs.map(c => ({ label: c.name, href: '#/files/' + c.id, id: c.id }))]);
  drawList({ emptyText: 'Папка пуста. Перетащите сюда файлы или нажмите «Загрузить».' });
}

async function viewRecent() {
  setCrumbs([{ label: 'Недавние' }]);
  state.items = await api('GET', '/api/recent');
  drawList({ emptyText: 'Пока ничего нет.', noSort: true, showPath: false });
}

async function viewSearch(q) {
  els.search.value = q;
  setCrumbs([{ label: 'Поиск: «' + q + '»' }]);
  state.items = await api('GET', '/api/search?q=' + encodeURIComponent(q));
  drawList({ emptyText: 'Ничего не найдено.', showPath: true });
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
  if (state.selected.size) {
    const sel = items.filter(n => state.selected.has(n.id));
    kids.push(h('div', { class: 'selbar' },
      h('span', { class: 'count' }, 'Выбрано: ' + sel.length),
      h('button', { class: 'btn', onclick: () => moveDialog(sel, false) }, 'Переместить'),
      h('button', { class: 'btn', onclick: () => moveDialog(sel, true) }, 'Копировать'),
      h('button', { class: 'btn danger', onclick: () => trashNodes(sel) }, 'Удалить'),
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
  const tbody = h('tbody', null, items.map(n => fileRow(n, opts, items)));
  kids.push(h('table', { class: 'files' },
    h('thead', null, h('tr', null, h('th', { class: 'c-check' }, allBox), th('name', 'Имя', ''), th('size', 'Размер', 'c-size'), th('date', 'Изменён', 'c-date'), h('th', { class: 'c-act' }))),
    tbody));
  els.content.replaceChildren(...kids);
}

function fileRow(n, opts, list) {
  const icon = h('div', { class: 'ficon' + (n.is_dir ? ' dir' : '') }, iconFor(n));
  if (isImage(n) && n.size < 15 * 1024 * 1024) {
    const img = h('img', { loading: 'lazy', alt: '', src: `/api/files/${n.id}/content?inline=1` });
    img.onerror = () => img.replaceWith(iconFor(n));
    icon.replaceChildren(img);
  }
  const name = h('div', { class: 'fname', onclick: e => { e.stopPropagation(); openNode(n, list); } },
    icon, h('div', { style: 'min-width:0' }, h('div', { class: 'nm', title: n.name }, n.name),
      opts.showPath && n.path ? h('div', { class: 'sub' }, n.path) : null));
  const box = h('input', { type: 'checkbox', checked: state.selected.has(n.id),
    onclick: e => e.stopPropagation(),
    onchange: e => { e.target.checked ? state.selected.add(n.id) : state.selected.delete(n.id); drawList(); } });
  const tr = h('tr', {
    class: state.selected.has(n.id) ? 'selected' : '', draggable: 'true',
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
  h('td', { class: 'c-act' },
    !n.is_dir ? h('a', { class: 'icon-btn', title: 'Скачать', href: `/api/files/${n.id}/content`, onclick: e => e.stopPropagation() }, '⬇') : null,
    h('button', { class: 'icon-btn', title: 'Поделиться', onclick: e => { e.stopPropagation(); shareDialog(n); } }, '🔗'),
    h('button', { class: 'icon-btn', title: 'Ещё', onclick: e => { e.stopPropagation(); const r = e.target.getBoundingClientRect(); nodeMenu(n, r.left - 160, r.bottom + 4, list); } }, '⋯')));
  if (n.is_dir) attachDropTarget(tr, n.id);
  return tr;
}

// Перетаскивание строк на папку — перемещение
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
  if (n.is_dir) { location.hash = '#/files/' + n.id; return; }
  if (canPreview(n)) preview(n, list.filter(canPreview));
  else location.href = `/api/files/${n.id}/content`;
}

function nodeMenu(n, x, y, list) {
  const items = [];
  if (n.is_dir) items.push({ icon: '📂', label: 'Открыть', onClick: () => openNode(n, list) });
  else {
    if (canPreview(n)) items.push({ icon: '👁', label: 'Просмотр', onClick: () => openNode(n, list) });
    items.push({ icon: '⬇', label: 'Скачать', onClick: () => { location.href = `/api/files/${n.id}/content`; } });
  }
  if (n.is_dir) items.push({ icon: '🗜️', label: 'Скачать ZIP', onClick: () => { location.href = `/api/files/${n.id}/zip`; } });
  if (state.view !== 'files' && n.parent_id !== undefined) items.push({ icon: '📍', label: 'Показать в папке', onClick: () => { location.hash = '#/files/' + (n.parent_id || ''); } });
  items.push('-',
    { icon: '🔗', label: 'Поделиться', onClick: () => shareDialog(n) },
    { icon: '✏️', label: 'Переименовать', onClick: () => renameNode(n) },
    { icon: '➡️', label: 'Переместить', onClick: () => moveDialog([n], false) },
    { icon: '📋', label: 'Копировать', onClick: () => moveDialog([n], true) });
  if (!n.is_dir) items.push({ icon: '🕓', label: 'Версии', onClick: () => versionsDialog(n) });
  items.push('-', { icon: '🗑️', label: 'Удалить', danger: true, onClick: () => trashNodes([n]) });
  contextMenu(x, y, items);
}

// ---------- действия ----------
async function newFolder() {
  const name = await askText('Новая папка', 'Название', 'Новая папка', 'Создать');
  if (!name) return;
  try { await api('POST', '/api/files/folder', { parent: state.parent || '', name }); route(); }
  catch (e) { report(e); }
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
  toast(`В корзине: ${ok}`);
  state.selected.clear(); route(); refreshUsage();
}

// Выбор папки (для перемещения и копирования)
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
  load(state.view === 'files' ? state.parent : null).catch(report);
}

// ---------- публичные ссылки ----------
function shareDialog(n) {
  const listEl = h('div');
  const pw = h('input', { type: 'text', placeholder: 'без пароля', autocomplete: 'off' });
  const exp = h('input', { type: 'datetime-local' });
  const up = h('input', { type: 'checkbox' });
  const load = async () => {
    const shares = await api('GET', '/api/shares?node=' + n.id);
    listEl.replaceChildren(...(shares.length ? shares.map(sh => shareRow(sh, load)) : [h('div', { class: 'hint' }, 'Ссылок пока нет.')]));
  };
  modal({
    title: 'Поделиться «' + n.name + '»', wide: true,
    body: [listEl,
      h('h4', { style: 'margin:18px 0 0' }, 'Новая ссылка'),
      h('div', { class: 'card', style: 'padding:12px;margin:8px 0 0' },
        h('div', { class: 'row' },
          h('div', null, h('label', null, 'Пароль'), pw),
          h('div', null, h('label', null, 'Действует до'), exp)),
        n.is_dir ? h('label', { class: 'check', style: 'margin-top:12px' }, up, 'Разрешить загрузку файлов в эту папку') : null,
        h('div', { style: 'margin-top:12px' }, h('button', { class: 'btn primary', onclick: async () => {
          try {
            await api('POST', '/api/shares', { node_id: n.id, password: pw.value, allow_upload: up.checked, expires_at: exp.value ? new Date(exp.value).toISOString() : null });
            pw.value = ''; exp.value = ''; up.checked = false; await load();
          } catch (e) { report(e); }
        } }, 'Создать ссылку'))),
    ],
  });
  load().catch(report);
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
      h('button', { class: 'btn danger', onclick: async () => {
        if (!await confirmDialog('Удалить ссылку?', 'Ссылка перестанет работать для всех, кому вы её отправили.')) return;
        try { await api('DELETE', '/api/shares/' + sh.id); await reload(); } catch (e) { report(e); }
      } }, 'Удалить')),
    h('div', { class: 'sub' }, meta));
}

async function viewShares() {
  setCrumbs([{ label: 'Мои ссылки' }]);
  const load = async () => {
    const shares = await api('GET', '/api/shares');
    els.content.replaceChildren(shares.length
      ? h('div', { class: 'card', style: 'max-width:900px' }, shares.map(sh => shareRow(sh, load)))
      : h('div', { class: 'empty' }, h('div', { class: 'big' }, '🔗'), 'Публичных ссылок нет. Создайте ссылку через меню файла → «Поделиться».'));
  };
  load.showName = true;
  await load();
}

// ---------- версии ----------
function versionsDialog(n) {
  const listEl = h('div');
  const load = async () => {
    const vs = await api('GET', `/api/files/${n.id}/versions`);
    listEl.replaceChildren(...vs.map(v => h('div', { class: 'list-row' },
      h('div', { class: 'grow' }, fmtDate(v.created_at), v.current ? h('span', { class: 'badge cur' }, 'текущая') : null,
        h('div', { class: 'sub' }, fmtSize(v.size) + ' · sha256 ' + v.sha256.slice(0, 12))),
      h('a', { class: 'btn', href: `/api/files/${n.id}/content?version=${v.id}` }, 'Скачать'),
      v.current ? null : h('button', { class: 'btn', onclick: async () => {
        try { await api('POST', `/api/files/${n.id}/versions/${v.id}/restore`); toast('Версия восстановлена'); await load(); route(); }
        catch (e) { report(e); }
      } }, 'Восстановить'))));
  };
  modal({ title: 'Версии «' + n.name + '»', body: [h('div', { class: 'hint' }, 'При каждой перезаписи файла сохраняется предыдущая версия.'), listEl], wide: true });
  load().catch(report);
}

// ---------- просмотр ----------
function preview(n, list) {
  let idx = Math.max(0, list.findIndex(x => x.id === n.id));
  const stage = h('div', { class: 'stage' });
  const title = h('div', { class: 'nm' });
  const dl = h('a', { class: 'icon-btn', title: 'Скачать' }, '⬇');
  const close = () => { ov.remove(); document.removeEventListener('keydown', onKey); };
  const show = async () => {
    const cur = list[idx];
    title.textContent = cur.name;
    dl.href = `/api/files/${cur.id}/content`;
    const src = `/api/files/${cur.id}/content?inline=1`;
    if (isImage(cur)) stage.replaceChildren(h('img', { src, alt: cur.name }));
    else if (isVideo(cur)) stage.replaceChildren(h('video', { src, controls: true, autoplay: true }));
    else if (isAudio(cur)) stage.replaceChildren(h('audio', { src, controls: true, autoplay: true }));
    else if (isPDF(cur)) stage.replaceChildren(h('iframe', { src }));
    else {
      stage.replaceChildren(h('div', { style: 'color:#ccc' }, 'Загрузка…'));
      try { const t = await (await fetch(src)).text(); stage.replaceChildren(h('pre', null, t)); }
      catch (e) { stage.replaceChildren(h('div', null, e.message)); }
    }
  };
  const step = d => { if (list.length > 1) { idx = (idx + d + list.length) % list.length; show(); } };
  const onKey = e => { if (e.key === 'Escape') close(); if (e.key === 'ArrowRight') step(1); if (e.key === 'ArrowLeft') step(-1); };
  const ov = h('div', { class: 'preview' },
    h('header', null, title, dl, h('button', { class: 'icon-btn', onclick: close }, '×')),
    stage,
    list.length > 1 ? h('button', { class: 'nav-arrow prev', onclick: () => step(-1) }, '‹') : null,
    list.length > 1 ? h('button', { class: 'nav-arrow next', onclick: () => step(1) }, '›') : null);
  document.body.append(ov);
  document.addEventListener('keydown', onKey);
  show();
}

// ---------- загрузка файлов ----------
const uploads = { queue: [], active: 0, panel: null, list: null, total: 0, done: 0 };

function uploadFiles(entries, parent = state.parent) {
  if (!entries.length) return;
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
    uploadOne(job).finally(() => {
      uploads.active--; uploads.done++; updateUploadTitle();
      if (!uploads.queue.length && !uploads.active) {
        refreshUsage(); if (state.view === 'files') route();
        // если всё загрузилось без ошибок — прячем панель через несколько секунд
        const p = uploads.panel;
        if (p && !p.querySelector('.up-item.fail')) setTimeout(() => {
          if (uploads.panel === p && !uploads.active && !uploads.queue.length) { p.remove(); uploads.panel = null; uploads.total = uploads.done = 0; }
        }, 4000);
      }
      pumpUploads();
    });
  }
}

function uploadOne(job) {
  return new Promise(resolve => {
    const q = new URLSearchParams({ name: job.file.name });
    if (job.parent) q.set('parent', job.parent);
    if (job.path) q.set('path', job.path);
    const xhr = new XMLHttpRequest();
    xhr.open('POST', '/api/files/upload?' + q);
    xhr.setRequestHeader('Content-Type', 'application/octet-stream');
    xhr.upload.onprogress = e => { if (e.lengthComputable) { const p = Math.round(e.loaded / e.total * 100); job.bar.style.width = p + '%'; job.pct.textContent = p + '%'; } };
    xhr.onload = () => {
      if (xhr.status < 300) { job.row.classList.add('done'); job.bar.style.width = '100%'; job.pct.textContent = '✓'; }
      else { job.row.classList.add('fail'); job.bar.style.width = '100%'; let m = 'ошибка'; try { m = JSON.parse(xhr.responseText).error; } catch (_) {} job.pct.textContent = m; }
      resolve();
    };
    xhr.onerror = () => { job.row.classList.add('fail'); job.pct.textContent = 'сеть'; resolve(); };
    xhr.send(job.file);
  });
}

// Перетаскивание файлов и папок из проводника
function setupDrop() {
  let depth = 0, zone = null;
  const isFiles = e => e.dataTransfer && [...e.dataTransfer.types].includes('Files');
  addEventListener('dragenter', e => {
    if (!isFiles(e) || state.view !== 'files') return;
    depth++;
    if (!zone) { zone = h('div', { class: 'dropzone' }, 'Отпустите, чтобы загрузить сюда'); document.body.append(zone); }
  });
  addEventListener('dragleave', e => { if (!isFiles(e)) return; if (--depth <= 0 && zone) { zone.remove(); zone = null; depth = 0; } });
  addEventListener('dragover', e => { if (isFiles(e) && state.view === 'files') e.preventDefault(); });
  addEventListener('drop', async e => {
    if (!isFiles(e)) return;
    e.preventDefault(); depth = 0; if (zone) { zone.remove(); zone = null; }
    if (state.view !== 'files') return;
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
    if (e.key === 'Delete' && sel.length && ['files', 'recent', 'search'].includes(state.view)) trashNodes(sel);
    if (e.key === 'F2' && sel.length === 1) { e.preventDefault(); renameNode(sel[0]); }
    if ((e.ctrlKey || e.metaKey) && e.key === 'a' && state.view === 'files') { e.preventDefault(); state.items.forEach(n => state.selected.add(n.id)); drawList(); }
    if (e.key === 'Escape' && state.selected.size) { state.selected.clear(); drawList(); }
  });
}

// ---------- корзина ----------
async function viewTrash() {
  setCrumbs([{ label: 'Корзина' }]);
  const d = await api('GET', '/api/trash');
  const head = h('div', { class: 'selbar', style: 'background:transparent;padding:0 0 10px' },
    h('span', { class: 'count hint', style: 'font-weight:400' }, `Объекты удаляются навсегда через ${d.retention_days} дн.`),
    d.items.length ? h('button', { class: 'btn danger', onclick: async () => {
      if (!await confirmDialog('Очистить корзину?', 'Все объекты будут удалены без возможности восстановления.', 'Очистить')) return;
      try { await api('DELETE', '/api/trash'); toast('Корзина очищена'); viewTrash(); refreshUsage(); } catch (e) { report(e); }
    } }, 'Очистить корзину') : null);
  if (!d.items.length) {
    els.content.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'big' }, '🗑️'), 'Корзина пуста.'));
    return;
  }
  els.content.replaceChildren(head, h('table', { class: 'files' },
    h('thead', null, h('tr', null, h('th', null, 'Имя'), h('th', { class: 'c-size' }, 'Размер'), h('th', { class: 'c-date' }, 'Удалён'), h('th', { class: 'c-act' }))),
    h('tbody', null, d.items.map(n => h('tr', null,
      h('td', null, h('div', { class: 'fname', style: 'cursor:default' }, h('div', { class: 'ficon' + (n.is_dir ? ' dir' : '') }, iconFor(n)),
        h('div', { style: 'min-width:0' }, h('div', { class: 'nm' }, n.name), h('div', { class: 'sub' }, 'было в ' + n.orig_path)))),
      h('td', { class: 'c-size' }, n.is_dir ? '—' : fmtSize(n.size)),
      h('td', { class: 'c-date' }, fmtDate(n.deleted_at)),
      h('td', { class: 'c-act' },
        h('button', { class: 'btn', onclick: async () => { try { await api('POST', `/api/trash/${n.id}/restore`); toast('Восстановлено'); viewTrash(); } catch (e) { report(e); } } }, 'Восстановить'), ' ',
        h('button', { class: 'btn danger', onclick: async () => {
          if (!await confirmDialog('Удалить навсегда?', `«${n.name}» будет удалён без возможности восстановления.`)) return;
          try { await api('DELETE', '/api/trash/' + n.id); viewTrash(); refreshUsage(); } catch (e) { report(e); }
        } }, 'Удалить')))))));
}

// ---------- настройки ----------
async function viewSettings() {
  setCrumbs([{ label: 'Настройки' }]);
  const u = state.usage;
  const oldPw = h('input', { type: 'password', autocomplete: 'current-password' });
  const newPw = h('input', { type: 'password', autocomplete: 'new-password' });
  const apList = h('div');
  const apName = h('input', { type: 'text', placeholder: 'например: Ноутбук, Телефон, rclone' });
  const loadAP = async () => {
    const list = await api('GET', '/api/me/app-passwords');
    apList.replaceChildren(...(list.length ? list.map(ap => h('div', { class: 'list-row' },
      h('div', { class: 'grow' }, ap.name, h('div', { class: 'sub' }, 'создан ' + fmtDate(ap.created_at) + (ap.last_used_at ? ' · использовался ' + fmtDate(ap.last_used_at) : ' · не использовался'))),
      h('button', { class: 'btn danger', onclick: async () => { try { await api('DELETE', '/api/me/app-passwords/' + ap.id); loadAP(); } catch (e) { report(e); } } }, 'Отозвать')))
      : [h('div', { class: 'hint' }, 'Паролей приложений нет.')]));
  };
  const davUrl = u.webdav_url;
  els.content.replaceChildren(
    h('div', { class: 'card' }, h('h2', null, 'Подключение как сетевой диск (WebDAV)'),
      h('div', { class: 'linkbox' }, h('input', { type: 'text', readonly: true, value: davUrl }), h('button', { class: 'btn', onclick: () => copyText(davUrl) }, 'Копировать')),
      h('ul', { class: 'hint', style: 'padding-left:18px;line-height:1.8' },
        h('li', null, 'Windows: Проводник → «Этот компьютер» → «Подключить сетевой диск» → вставьте адрес. Нужен HTTPS.'),
        h('li', null, 'Linux (GNOME/KDE): адрес ', h('code', { class: 'inline' }, davUrl.replace(/^http/, 'dav')), ' в файловом менеджере.'),
        h('li', null, 'macOS: Finder → «Подключение к серверу» (⌘K).'),
        h('li', null, 'Android: Solid Explorer, Total Commander (WebDAV-плагин), FolderSync.'),
        h('li', null, 'rclone: ', h('code', { class: 'inline' }, 'rclone config → webdav → vendor: other')))),
    h('div', { class: 'card' }, h('h2', null, 'Пароли приложений'),
      h('div', { class: 'hint' }, 'Для WebDAV-клиентов лучше использовать отдельные пароли — их можно отозвать, не меняя основной.'),
      apList,
      h('div', { class: 'row', style: 'margin-top:10px' }, h('div', null, apName),
        h('button', { class: 'btn primary', onclick: async () => {
          try {
            const r = await api('POST', '/api/me/app-passwords', { name: apName.value });
            apName.value = '';
            modal({ title: 'Пароль приложения', body: [h('div', null, 'Логин: ', h('b', null, state.me.username)), h('div', { class: 'secret' }, r.password),
              h('div', { class: 'hint' }, 'Сохраните пароль — больше он показан не будет.')], buttons: [{ label: 'Скопировать и закрыть', primary: true, onClick: () => copyText(r.password) }] });
            loadAP();
          } catch (e) { report(e); }
        } }, 'Создать'))),
    h('div', { class: 'card' }, h('h2', null, 'Смена пароля'),
      h('div', { class: 'row' }, h('div', null, h('label', null, 'Текущий пароль'), oldPw), h('div', null, h('label', null, 'Новый пароль'), newPw),
        h('button', { class: 'btn primary', onclick: async () => {
          try { await api('POST', '/api/me/password', { old_password: oldPw.value, new_password: newPw.value }); oldPw.value = newPw.value = ''; toast('Пароль изменён, другие сеансы завершены'); }
          catch (e) { report(e); }
        } }, 'Сменить'))),
    h('div', { class: 'card' }, h('h2', null, 'Хранилище'),
      h('div', null, 'Занято: ', fmtSize(u.used_bytes), u.quota_bytes > 0 ? ' из ' + fmtSize(u.quota_bytes) : ' (без ограничения)'),
      h('div', { class: 'hint' }, 'Учитываются все версии файлов и корзина. Максимальный размер одного файла: ' + fmtSize(u.max_upload_bytes) + '.')));
  loadAP().catch(report);
}

// ---------- администрирование ----------
async function viewAdmin() {
  setCrumbs([{ label: 'Пользователи' }]);
  const users = await api('GET', '/api/admin/users');
  const name = h('input', { type: 'text', autocomplete: 'off' });
  const pw = h('input', { type: 'text', autocomplete: 'off' });
  const quota = h('input', { type: 'number', min: '0', placeholder: 'по умолчанию' });
  const adm = h('input', { type: 'checkbox' });
  const gb = b => b > 0 ? (b / 1073741824).toFixed(b % 1073741824 ? 1 : 0) + ' ГБ' : '∞';
  els.content.replaceChildren(
    h('div', { class: 'card', style: 'max-width:900px' }, h('h2', null, 'Новый пользователь'),
      h('div', { class: 'row' },
        h('div', null, h('label', null, 'Имя'), name), h('div', null, h('label', null, 'Пароль'), pw),
        h('div', null, h('label', null, 'Квота, ГБ (0 — без ограничения)'), quota)),
      h('div', { style: 'display:flex;gap:16px;align-items:center;margin-top:12px' },
        h('label', { class: 'check', style: 'margin:0' }, adm, 'Администратор'),
        h('button', { class: 'btn primary', onclick: async () => {
          try {
            const body = { username: name.value.trim(), password: pw.value, is_admin: adm.checked };
            if (quota.value !== '') body.quota_bytes = Math.round(parseFloat(quota.value) * 1073741824);
            await api('POST', '/api/admin/users', body); toast('Пользователь создан'); viewAdmin();
          } catch (e) { report(e); }
        } }, 'Создать'))),
    h('table', { class: 'files', style: 'max-width:900px' },
      h('thead', null, h('tr', null, h('th', null, 'Пользователь'), h('th', { class: 'c-size' }, 'Занято'), h('th', { class: 'c-size' }, 'Квота'), h('th', { class: 'c-date' }, 'Создан'), h('th', { class: 'c-act' }))),
      h('tbody', null, users.map(u => h('tr', null,
        h('td', null, u.username, u.is_admin ? h('span', { class: 'badge' }, 'админ') : null, u.id === state.me.id ? h('span', { class: 'badge' }, 'вы') : null),
        h('td', { class: 'c-size' }, fmtSize(u.used_bytes)),
        h('td', { class: 'c-size' }, gb(u.quota_bytes)),
        h('td', { class: 'c-date' }, fmtDate(u.created_at)),
        h('td', { class: 'c-act' }, h('button', { class: 'icon-btn', onclick: e => {
          const r = e.target.getBoundingClientRect();
          contextMenu(r.left - 170, r.bottom + 4, [
            { icon: '💾', label: 'Изменить квоту', onClick: async () => {
              const v = await askText('Квота для ' + u.username, 'Гигабайт (0 — без ограничения)', u.quota_bytes ? String(u.quota_bytes / 1073741824) : '0', 'Сохранить');
              if (v === null) return;
              try { await api('PATCH', '/api/admin/users/' + u.id, { quota_bytes: Math.round(parseFloat(v.replace(',', '.')) * 1073741824) || 0 }); viewAdmin(); } catch (ex) { report(ex); }
            } },
            { icon: '🔑', label: 'Сбросить пароль', onClick: async () => {
              const v = await askText('Новый пароль для ' + u.username, 'Не короче 8 символов', '', 'Сохранить');
              if (!v) return;
              try { await api('PATCH', '/api/admin/users/' + u.id, { password: v }); toast('Пароль изменён'); } catch (ex) { report(ex); }
            } },
            { icon: '👑', label: u.is_admin ? 'Снять права админа' : 'Сделать админом', onClick: async () => {
              try { await api('PATCH', '/api/admin/users/' + u.id, { is_admin: !u.is_admin }); viewAdmin(); } catch (ex) { report(ex); }
            } },
            '-',
            { icon: '🗑️', label: 'Удалить пользователя', danger: true, onClick: async () => {
              if (!await confirmDialog('Удалить ' + u.username + '?', 'Все файлы пользователя будут удалены безвозвратно.')) return;
              try { await api('DELETE', '/api/admin/users/' + u.id); viewAdmin(); } catch (ex) { report(ex); }
            } },
          ]);
        } }, '⋯')))))));
}

boot();
