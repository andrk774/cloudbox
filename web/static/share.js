'use strict';
/* Публичная страница ссылки /s/{token} */
const token = location.pathname.split('/').filter(Boolean)[1];
const base = '/api/public/' + encodeURIComponent(token);
const root = document.getElementById('root');

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v === true) el.setAttribute(k, '');
    else el.setAttribute(k, v);
  }
  for (const kid of kids.flat()) if (kid != null && kid !== false) el.append(kid instanceof Node ? kid : document.createTextNode(String(kid)));
  return el;
}
function fmtSize(b) { const u = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ']; let i = 0; while (b >= 1024 && i < 4) { b /= 1024; i++; } return (i ? b.toFixed(b < 10 ? 1 : 0) : b) + ' ' + u[i]; }
function fmtDate(s) { return new Date(s).toLocaleString('ru-RU', { day: 'numeric', month: 'short', year: 'numeric', hour: '2-digit', minute: '2-digit' }); }
function toast(msg, err) { const t = h('div', { class: 'toast' + (err ? ' err' : '') }, msg); document.getElementById('toasts').append(t); setTimeout(() => t.remove(), 4000); }
function icon(n) {
  if (n.is_dir) return '📁';
  const m = n.mime || '';
  return m.startsWith('image/') ? '🖼️' : m.startsWith('video/') ? '🎬' : m.startsWith('audio/') ? '🎵' : m === 'application/pdf' ? '📕' : '📄';
}
async function get(url, opt) {
  const r = await fetch(url, opt);
  let d = null; try { d = await r.json(); } catch (_) {}
  if (!r.ok) { const e = new Error((d && d.error) || 'Ошибка ' + r.status); e.status = r.status; throw e; }
  return d;
}

function thumb(n) {
  const el = h('div', { class: 'ficon' + (n.is_dir ? ' dir' : '') }, icon(n));
  if (!n.is_dir && /^(image\/(png|jpe?g|gif|webp|bmp)|video\/|application\/pdf)/.test(n.mime || '')) {
    const img = h('img', { loading: 'lazy', alt: '', src: base + '/thumb?s=128&node=' + n.id });
    img.onerror = () => img.replaceWith(icon(n));
    el.replaceChildren(img);
  }
  return el;
}

const logo = () => h('div', { class: 'logo' }, h('div', { class: 'logo-mark' }, '☁'), 'CloudBox');

async function start() {
  let info;
  try { info = await get(base); }
  catch (e) {
    root.replaceChildren(logo(), h('div', { class: 'empty' }, h('div', { class: 'big' }, '🔗'), e.message));
    return;
  }
  if (!info.authorized) return askPassword(info);
  if (info.is_dir) return showDir(info, null);
  showFile(info);
}

function askPassword(info) {
  const pw = h('input', { type: 'password', autocomplete: 'off' });
  const err = h('div', { class: 'error' });
  root.replaceChildren(h('div', { class: 'auth', style: 'min-height:70vh' }, h('form', { class: 'auth-card', onsubmit: async e => {
    e.preventDefault(); err.textContent = '';
    try { await get(base + '/auth', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: pw.value }) }); start(); }
    catch (ex) { err.textContent = ex.message; }
  } }, logo(), h('h1', null, '🔒 ' + info.name), h('div', { class: 'hint' }, 'Владелец защитил ссылку паролем.'),
    h('label', null, 'Пароль'), pw, h('button', { class: 'btn primary', type: 'submit' }, 'Открыть'), err)));
  pw.focus();
}

function showFile(info) {
  const n = info.node;
  const src = base + '/content?inline=1';
  let media = null;
  const m = n.mime || '';
  if (/^image\/(png|jpe?g|gif|webp|bmp|avif)$/.test(m)) { media = h('img', { src: base + '/thumb?s=1024', alt: n.name }); media.onerror = () => { media.src = src; }; }
  else if (m.startsWith('video/')) media = h('video', { src, controls: true });
  else if (m.startsWith('audio/')) media = h('audio', { src, controls: true });
  root.replaceChildren(
    h('div', { class: 'pub-head' }, logo()),
    h('div', { class: 'card pub-file', style: 'max-width:none' },
      h('div', { class: 'big' }, icon(n)),
      h('h1', null, n.name),
      h('div', { class: 'hint' }, fmtSize(n.size) + ' · изменён ' + fmtDate(n.updated_at) + (info.expires_at ? ' · ссылка действует до ' + fmtDate(info.expires_at) : '')),
      h('div', { style: 'margin-top:18px' }, h('a', { class: 'btn primary', href: base + '/content' }, '⬇ Скачать')),
      media ? h('div', { class: 'media' }, media) : null));
}

async function showDir(info, dirId) {
  let d;
  try { d = await get(base + '/list' + (dirId ? '?dir=' + dirId : '')); }
  catch (e) { if (e.status === 401) return askPassword(info); toast(e.message, true); return; }
  const cur = d.breadcrumbs[d.breadcrumbs.length - 1];
  const crumbs = h('div', { class: 'crumbs', style: 'margin-bottom:12px' });
  d.breadcrumbs.forEach((c, i) => {
    if (i) crumbs.append(h('span', { class: 'sep' }, '›'));
    const last = i === d.breadcrumbs.length - 1;
    crumbs.append(last ? h('span', { class: 'cur' }, c.name) : h('a', { href: '#', onclick: e => { e.preventDefault(); showDir(info, i === 0 ? null : c.id); } }, c.name));
  });

  let uploadBox = null;
  if (info.allow_upload) {
    const inp = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: async e => {
      const filesList = [...e.target.files]; e.target.value = '';
      for (const f of filesList) {
        toast('Загрузка: ' + f.name);
        const q = new URLSearchParams({ name: f.name });
        if (dirId) q.set('dir', dirId);
        try { await get(base + '/upload?' + q, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: f }); }
        catch (ex) { toast(f.name + ': ' + ex.message, true); }
      }
      showDir(info, dirId);
    } });
    uploadBox = h('span', null, inp, h('button', { class: 'btn', onclick: () => inp.click() }, '⬆ Загрузить сюда'));
  }

  const rows = d.items.map(n => h('tr', null,
    h('td', null, h('div', { class: 'fname', onclick: () => n.is_dir ? showDir(info, n.id) : (location.href = base + '/content?node=' + n.id) },
      thumb(n), h('div', { class: 'nm' }, n.name))),
    h('td', { class: 'c-size' }, n.is_dir ? '—' : fmtSize(n.size)),
    h('td', { class: 'c-date' }, fmtDate(n.updated_at)),
    h('td', { class: 'c-act' }, h('a', { class: 'icon-btn', title: 'Скачать', href: n.is_dir ? base + '/zip?node=' + n.id : base + '/content?node=' + n.id }, '⬇'))));

  root.replaceChildren(
    h('div', { class: 'pub-head' }, logo(), h('div', { class: 'actions' }, uploadBox,
      h('a', { class: 'btn primary', href: base + '/zip' + (dirId ? '?node=' + dirId : '') }, '🗜️ Скачать всё (ZIP)'))),
    crumbs,
    d.items.length ? h('table', { class: 'files' },
      h('thead', null, h('tr', null, h('th', null, 'Имя'), h('th', { class: 'c-size' }, 'Размер'), h('th', { class: 'c-date' }, 'Изменён'), h('th', { class: 'c-act' }))),
      h('tbody', null, rows))
      : h('div', { class: 'empty' }, h('div', { class: 'big' }, '📂'), 'Папка пуста'),
    info.expires_at ? h('p', { class: 'hint' }, 'Ссылка действует до ' + fmtDate(info.expires_at)) : null);
  document.title = (cur ? cur.name : info.name) + ' — CloudBox';
}

start();
