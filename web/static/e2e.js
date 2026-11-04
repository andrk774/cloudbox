'use strict';
/* CloudBox — зашифрованные (E2E) папки. Всё шифрование — здесь, в браузере (WebCrypto); сервер видит
   только шифротекст под случайными именами, обёрнутый ключ и зашифрованные метаданные.

   Ключи:
     ключ хранилища VK — 256 бит, создаётся в браузере; на сервере лежит только обёрнутым (AES-256-GCM):
       • ключом из кодовой фразы: PBKDF2-SHA256, 600 000 итераций, соль 16 байт;
       • ключом восстановления — 256 бит, показывается пользователю один раз.
   Метаданные папки (настоящие имена, ключи файлов) — JSON, AES-256-GCM ключом VK; AAD = id папки,
     поэтому сервер не может подсунуть метаданные одной папки в другую.
   Файл — свой случайный ключ (256 бит, хранится в метаданных):
     "CBE1" + префикс nonce (8 байт) + сегменты по 64 КиБ открытого текста, каждый AES-256-GCM
     с nonce = префикс ‖ номер сегмента (4 байта); AAD = флаг последнего сегмента — обрезка и
     перестановка сегментов обнаруживаются при расшифровке. */

const E2E = { keys: null, vk: null, metas: new Map() };

const E2E_ITER = 600000, E2E_SEG = 65536, E2E_CHUNK_SEGS = 128; // 128 × 64 КиБ = 8 МиБ на запрос
const E2E_MAGIC = new TextEncoder().encode('CBE1');
const VAULT_AAD = new TextEncoder().encode('cloudbox-e2e-vault-v1');
const te = new TextEncoder(), td = new TextDecoder();
const rand = n => crypto.getRandomValues(new Uint8Array(n));
const toB64 = u8 => { let s = ''; for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode(...u8.subarray(i, i + 0x8000)); return btoa(s); };
const fromB64 = s => Uint8Array.from(atob(s), c => c.charCodeAt(0));
const hex = u8 => [...u8].map(b => b.toString(16).padStart(2, '0')).join('');

// ---------- примитивы ----------
const aesKey = (raw, usages) => crypto.subtle.importKey('raw', raw, 'AES-GCM', false, usages);
async function gcmEnc(key, data, aad) {
  const iv = rand(12);
  return { iv, ct: new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv, additionalData: aad }, key, data)) };
}
async function gcmDec(key, iv, ct, aad) {
  return new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv, additionalData: aad }, key, ct));
}
async function kekFromPass(pass, salt, iter) {
  const base = await crypto.subtle.importKey('raw', te.encode(pass), 'PBKDF2', false, ['deriveKey']);
  return crypto.subtle.deriveKey({ name: 'PBKDF2', salt, iterations: iter, hash: 'SHA-256' }, base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
}

// Ключ восстановления — base32 без похожих символов (I, O, 0, 1), группами по 4.
const B32 = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789';
function b32enc(u8) {
  let bits = 0, val = 0, out = '';
  for (const b of u8) { val = (val << 8) | b; bits += 8; while (bits >= 5) { out += B32[(val >>> (bits - 5)) & 31]; bits -= 5; } }
  if (bits) out += B32[(val << (5 - bits)) & 31];
  return out.match(/.{1,4}/g).join('-');
}
function b32dec(s) {
  // только верхний регистр и без разделителей; похожие символы не «исправляем»: I, O, 0, 1 в алфавите нет,
  // а L есть — замена L→1 ломала ключи, где встречается L
  const clean = s.toUpperCase().replace(/[^A-Z0-9]/g, '');
  let bits = 0, val = 0; const out = [];
  for (const c of clean) {
    const i = B32.indexOf(c);
    if (i < 0) throw new Error('В ключе восстановления недопустимый символ «' + c + '»');
    val = (val << 5) | i; bits += 5;
    if (bits >= 8) { out.push((val >>> (bits - 8)) & 255); bits -= 8; }
  }
  return new Uint8Array(out.slice(0, 32));
}

// ---------- ключ хранилища ----------
async function e2eLoadKeys() { E2E.keys = (await api('GET', '/api/e2e/keys')).keys; return E2E.keys; }

async function wrapPass(vkRaw, pass) {
  const salt = rand(16);
  const w = await gcmEnc(await kekFromPass(pass, salt, E2E_ITER), vkRaw, VAULT_AAD);
  return { kdf: 'PBKDF2-SHA256', iter: E2E_ITER, salt: toB64(salt), iv: toB64(w.iv), ct: toB64(w.ct) };
}

async function e2eSetup(pass) {
  const vkRaw = rand(32), recRaw = rand(32);
  const rec = await gcmEnc(await aesKey(recRaw, ['encrypt']), vkRaw, VAULT_AAD);
  const data = { v: 1, pass: await wrapPass(vkRaw, pass), rec: { iv: toB64(rec.iv), ct: toB64(rec.ct) } };
  await api('PUT', '/api/e2e/keys', { data, version: 0 });
  E2E.vk = await aesKey(vkRaw, ['encrypt', 'decrypt']);
  const recovery = b32enc(recRaw);
  vkRaw.fill(0); recRaw.fill(0);
  await e2eLoadKeys();
  return recovery;
}

// Разблокировка кодовой фразой или ключом восстановления. Ключ хранилища живёт только в памяти вкладки.
async function e2eUnlock(secret, isRecovery) {
  const d = E2E.keys.data;
  let raw;
  try {
    if (isRecovery) raw = await gcmDec(await aesKey(b32dec(secret), ['decrypt']), fromB64(d.rec.iv), fromB64(d.rec.ct), VAULT_AAD);
    else raw = await gcmDec(await kekFromPass(secret, fromB64(d.pass.salt), d.pass.iter), fromB64(d.pass.iv), fromB64(d.pass.ct), VAULT_AAD);
  } catch (_) { throw new Error(isRecovery ? 'Неверный ключ восстановления' : 'Неверная кодовая фраза'); }
  E2E.vk = await aesKey(raw, ['encrypt', 'decrypt']);
  return raw; // вызывающий обнуляет после использования
}

async function e2eChangePass(vkRaw, pass) {
  const d = { ...E2E.keys.data, pass: await wrapPass(vkRaw, pass) };
  await api('PUT', '/api/e2e/keys', { data: d, version: E2E.keys.version });
  await e2eLoadKeys();
}

// ensureUnlocked — показать мастер настройки или окно ввода кодовой фразы. false — пользователь отказался.
async function e2eEnsureUnlocked() {
  if (E2E.vk) return true;
  if (!E2E.keys) await e2eLoadKeys();
  return E2E.keys ? unlockDialog() : setupDialog();
}

function passQuality(p) { return p.length >= 12 ? '' : 'Не короче 12 символов — лучше несколько случайных слов.'; }

function setupDialog() {
  return new Promise(resolve => {
    let done = false;
    const p1 = h('input', { type: 'password', autocomplete: 'new-password' });
    const p2 = h('input', { type: 'password', autocomplete: 'new-password' });
    const err = h('div', { class: 'error' });
    modal({ title: '🔒 Сквозное шифрование', body: [
      h('p', null, 'Файлы в зашифрованных папках шифруются прямо в браузере. Сервер, его администратор и любой, кто получит доступ к дискам, увидят только бессмысленные данные.'),
      h('p', { class: 'hint' }, 'Придумайте кодовую фразу — она нужна, чтобы открывать зашифрованные папки, и не совпадает с паролем входа. CloudBox её не хранит и восстановить не сможет.'),
      h('label', null, 'Кодовая фраза'), p1, h('label', null, 'Ещё раз'), p2, err,
    ], buttons: [{ label: 'Отмена' }, { label: 'Создать ключ', primary: true, onClick: async () => {
      err.textContent = passQuality(p1.value) || (p1.value !== p2.value ? 'Фразы не совпадают' : '');
      if (err.textContent) return false;
      toast('Создаю ключ…');
      const recovery = await e2eSetup(p1.value);
      done = true;
      await recoveryDialog(recovery);
      resolve(true);
    } }], onClose: () => { if (!done) resolve(false); } });
  });
}

function recoveryDialog(code) {
  return new Promise(resolve => {
    const ok = h('input', { type: 'checkbox' });
    const field = h('input', { type: 'text', readonly: true, value: code, class: 'e2e-recovery' });
    modal({ title: 'Ключ восстановления', body: [
      h('p', null, 'Если забудете кодовую фразу, зашифрованные файлы можно будет открыть только этим ключом. ', h('b', null, 'Он показывается один раз.')),
      field,
      h('div', { class: 'row-inline', style: 'margin-top:8px' }, h('button', { class: 'btn', onclick: () => copyText(code) }, '📋 Скопировать')),
      h('p', { class: 'hint' }, 'Сохраните его в менеджере паролей или запишите на бумаге и храните отдельно от компьютера. Без фразы и без этого ключа файлы не восстановит никто — даже администратор сервера.'),
      h('label', { class: 'check' }, ok, 'Я сохранил ключ восстановления'),
    ], buttons: [{ label: 'Готово', primary: true, onClick: () => { if (!ok.checked) { toast('Отметьте, что ключ сохранён', true); return false; } resolve(); } }],
    onClose: resolve });
    setTimeout(() => field.select(), 50);
  });
}

function unlockDialog() {
  return new Promise(resolve => {
    let done = false, rec = false;
    const inp = h('input', { type: 'password', autocomplete: 'current-password' });
    const lbl = h('label', null, 'Кодовая фраза');
    const err = h('div', { class: 'error' });
    const toggle = h('a', { href: '#', onclick: e => { e.preventDefault(); rec = !rec; lbl.textContent = rec ? 'Ключ восстановления' : 'Кодовая фраза';
      inp.type = rec ? 'text' : 'password'; toggle.textContent = rec ? 'Ввести кодовую фразу' : 'Забыли фразу? Войти ключом восстановления'; inp.focus(); } }, 'Забыли фразу? Войти ключом восстановления');
    const m = modal({ title: '🔒 Открыть зашифрованные папки', body: [lbl, inp, err, h('div', { style: 'margin-top:8px' }, toggle)],
      buttons: [{ label: 'Отмена' }, { label: 'Открыть', primary: true, onClick: async () => {
        err.textContent = '';
        try {
          const raw = await e2eUnlock(inp.value.trim(), rec);
          done = true;
          if (rec) { // вошли ключом восстановления — сразу предлагаем новую фразу
            const p = await askText('Новая кодовая фраза', 'Придумайте новую фразу (не короче 12 символов)', '', 'Сохранить', 'password');
            if (p && !passQuality(p)) { await e2eChangePass(raw, p); toast('Кодовая фраза изменена'); }
          }
          raw.fill(0);
          resolve(true);
        } catch (e) { err.textContent = e.message; return false; }
      } }], onClose: () => { if (!done) resolve(false); } });
    inp.addEventListener('keydown', e => { if (e.key === 'Enter') m.el.querySelector('footer .primary').click(); });
  });
}

// ---------- метаданные папок ----------
const metaAAD = id => te.encode('cloudbox-e2e-meta-v1:' + id);
const emptyMeta = () => ({ v: 1, files: {}, dirs: {} });

async function e2eLoadMeta(id) {
  const r = await api('GET', '/api/e2e/meta/' + id);
  let meta = emptyMeta();
  if (r.version) {
    const [iv, ct] = r.data.split('.');
    try { meta = JSON.parse(td.decode(await gcmDec(E2E.vk, fromB64(iv), fromB64(ct), metaAAD(id)))); }
    catch (_) { throw new Error('Метаданные папки не расшифровываются: они повреждены или подменены'); }
  }
  E2E.metas.set(id, { meta, version: r.version });
  return { meta, version: r.version };
}

// e2eUpdateMeta применяет fn к свежим метаданным и записывает их; при одновременной правке с другого
// устройства сервер отвечает 412 — перечитываем и применяем изменение заново.
async function e2eUpdateMeta(id, fn) {
  for (let i = 0; i < 6; i++) {
    const { meta, version } = await e2eLoadMeta(id);
    fn(meta);
    const { iv, ct } = await gcmEnc(E2E.vk, te.encode(JSON.stringify(meta)), metaAAD(id));
    try {
      const r = await api('PUT', '/api/e2e/meta/' + id, { data: toB64(iv) + '.' + toB64(ct), version });
      E2E.metas.set(id, { meta, version: r.version });
      return meta;
    } catch (e) { if (e.status !== 412) throw e; }
  }
  throw new Error('Не удалось сохранить изменения: папку одновременно меняют с другого устройства');
}

// ---------- шифрование файлов ----------
const e2eSegs = n => Math.max(1, Math.ceil(n / E2E_SEG));
const e2eCipherSize = n => 12 + n + e2eSegs(n) * 16;
function segNonce(prefix, i) { const n = new Uint8Array(12); n.set(prefix); new DataView(n.buffer).setUint32(8, i); return n; }
const segAAD = last => new Uint8Array([last ? 1 : 0]);

async function e2eUpload(file, parentId, onProgress) {
  const dekRaw = rand(32), prefix = rand(8);
  const key = await aesKey(dekRaw, ['encrypt']);
  const obf = hex(rand(16));
  const total = e2eCipherSize(file.size), segs = e2eSegs(file.size);
  const up = await api('POST', '/api/uploads', { parent: parentId, name: obf, size: total });
  let off = 0;
  for (let s = 0; s < segs; s += E2E_CHUNK_SEGS) {
    const parts = s === 0 ? [E2E_MAGIC, prefix] : [];
    for (let i = s; i < Math.min(s + E2E_CHUNK_SEGS, segs); i++) {
      const plain = await file.slice(i * E2E_SEG, Math.min((i + 1) * E2E_SEG, file.size)).arrayBuffer();
      parts.push(new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: segNonce(prefix, i), additionalData: segAAD(i === segs - 1) }, key, plain)));
    }
    const body = new Blob(parts);
    const res = await fetch(`/api/uploads/${up.id}?offset=${off}`, { method: 'PUT', body, credentials: 'same-origin' });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || 'Ошибка загрузки ' + res.status);
    off += body.size;
    onProgress && onProgress(off / total);
  }
  const node = await api('POST', `/api/uploads/${up.id}/complete`);
  const entry = { name: file.name, size: file.size, mime: file.type || '', mtime: file.lastModified || Date.now(), key: toB64(dekRaw) };
  dekRaw.fill(0);
  await e2eUpdateMeta(parentId, m => { m.files[obf] = entry; });
  return node;
}

async function e2eDecrypt(buf, entry) {
  const u8 = new Uint8Array(buf);
  if (u8.length < 12 || td.decode(u8.subarray(0, 4)) !== 'CBE1') throw new Error('Это не зашифрованный файл CloudBox');
  const prefix = u8.slice(4, 12), body = u8.subarray(12);
  const segs = e2eSegs(entry.size);
  if (body.length !== entry.size + segs * 16) throw new Error('Размер файла не совпадает — он повреждён или подменён');
  const key = await aesKey(fromB64(entry.key), ['decrypt']);
  const parts = [];
  for (let i = 0, p = 0; i < segs; i++) {
    const len = Math.min(E2E_SEG, entry.size - i * E2E_SEG) + 16;
    try { parts.push(new Uint8Array(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: segNonce(prefix, i), additionalData: segAAD(i === segs - 1) }, key, body.subarray(p, p + len)))); }
    catch (_) { throw new Error('Файл не расшифровывается: он повреждён или подменён'); }
    p += len;
  }
  return new Blob(parts, { type: entry.mime || 'application/octet-stream' });
}

async function e2eDownload(node, entry) {
  const res = await fetch(`/api/files/${node.id}/content`, { credentials: 'same-origin' });
  if (!res.ok) throw new Error('Не удалось скачать: ' + res.status);
  const blob = await e2eDecrypt(await res.arrayBuffer(), entry);
  const a = h('a', { href: URL.createObjectURL(blob), download: entry.name });
  document.body.append(a); a.click(); a.remove();
  setTimeout(() => URL.revokeObjectURL(a.href), 60000);
}

// ---------- интерфейс ----------
async function e2eNewFolder(parent) {
  if (!await e2eEnsureUnlocked()) return;
  const name = await askText('Новая зашифрованная папка', 'Название (видно на сервере; всё внутри — зашифровано)', 'Личное', 'Создать');
  if (!name) return;
  try { const n = await api('POST', '/api/e2e/folders', { parent: parent || '', name }); location.hash = '#/e2e/' + n.id; }
  catch (e) { report(e); }
}

// e2eName — настоящее имя узла внутри E2E-папки (по метаданным родителя).
async function e2eName(node) {
  if (!node.parent_id) return node.name;
  const c = E2E.metas.get(node.parent_id) || await e2eLoadMeta(node.parent_id).catch(() => null);
  const m = c && c.meta;
  return (m && (m.dirs[node.name] || m.files[node.name]) || {}).name;
}

async function viewE2E(id) {
  setCrumbs([{ label: 'Все файлы', href: '#/files' }, { label: '🔒 …' }]);
  els.content.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'big' }, '🔒'), 'Зашифрованная папка'));
  if (!await e2eEnsureUnlocked()) { history.back(); return; }
  const [d, { meta }] = await Promise.all([api('GET', '/api/files?parent=' + id), e2eLoadMeta(id)]);
  // хлебные крошки: обычные папки — как есть, внутри E2E — настоящие имена из метаданных
  const crumbs = [{ label: 'Все файлы', href: '#/files' }];
  let prevE2E = false;
  for (const c of d.breadcrumbs) {
    // корень E2E-папки лежит в обычной папке — его имя открытое; глубже — из метаданных родителя
    const real = c.e2e && prevE2E ? (await e2eName(c)) : c.name;
    prevE2E = !!c.e2e;
    const label = (c.e2e ? '🔒 ' : '') + (real || '???');
    crumbs.push({ label, href: c.e2e ? '#/e2e/' + c.id : '#/files/' + c.id });
  }
  setCrumbs(crumbs);
  const picker = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: async e => {
    const list = [...e.target.files]; e.target.value = '';
    for (const f of list) {
      // своё сообщение о ходе загрузки: обычный toast исчезает раньше, чем загрузится большой файл
      const t = h('div', { class: 'toast' }, `🔒 Шифрую и загружаю «${f.name}»…`);
      $('#toasts').append(t);
      try { await e2eUpload(f, id, p => { t.textContent = `🔒 «${f.name}» — ${Math.round(p * 100)}%`; }); }
      catch (ex) { report(ex); }
      finally { t.remove(); }
    }
    refreshUsage(); viewE2E(id);
  } });
  const rows = [];
  const items = d.items.slice().sort((a, b) => (b.is_dir - a.is_dir));
  const entries = items.map(n => ({ n, e: n.is_dir ? meta.dirs[n.name] : meta.files[n.name] }))
    .sort((a, b) => (b.n.is_dir - a.n.is_dir) || (a.e ? a.e.name : '~').localeCompare(b.e ? b.e.name : '~', 'ru'));
  for (const { n, e } of entries) {
    if (!e) {
      rows.push(h('div', { class: 'e2e-row unknown' }, h('span', { class: 'ico' }, '⚠️'),
        h('div', { class: 'grow' }, 'Неизвестный объект', h('div', { class: 'sub' }, 'Нет в метаданных папки (загружен не через CloudBox или метаданные откатились)')),
        h('button', { class: 'btn danger', onclick: async () => { if (await confirmDialog('Удалить?', 'Объект без метаданных расшифровать нельзя.')) { await api('DELETE', '/api/files/' + n.id); viewE2E(id); } } }, 'Удалить')));
      continue;
    }
    const rename = async () => {
      const nn = await askText('Переименовать', 'Новое имя', e.name, 'Сохранить');
      if (!nn || nn === e.name) return;
      try { await e2eUpdateMeta(id, m => { (n.is_dir ? m.dirs : m.files)[n.name].name = nn; }); viewE2E(id); } catch (ex) { report(ex); }
    };
    const del = async () => {
      if (!await confirmDialog('Удалить «' + e.name + '»?', 'Объект переместится в корзину. Метаданные сохраняются, так что после восстановления из корзины он снова откроется.')) return;
      try { await api('DELETE', '/api/files/' + n.id); viewE2E(id); } catch (ex) { report(ex); }
    };
    rows.push(h('div', { class: 'e2e-row' },
      h('span', { class: 'ico' }, n.is_dir ? '📁' : iconFor({ name: e.name, mime: e.mime, is_dir: false })),
      h('a', { href: n.is_dir ? '#/e2e/' + n.id : '#', class: 'grow', onclick: n.is_dir ? null : async ev => {
        ev.preventDefault();
        try { toast('🔓 Расшифровываю «' + e.name + '»…'); await e2eDownload(n, e); } catch (ex) { report(ex); }
      } }, e.name, n.is_dir ? null : h('div', { class: 'sub' }, fmtSize(e.size) + ' · ' + fmtDate(n.updated_at))),
      h('button', { class: 'icon-btn', title: 'Переименовать', onclick: rename }, '✏️'),
      h('button', { class: 'icon-btn', title: 'Удалить', onclick: del }, '🗑️')));
  }
  els.content.replaceChildren(
    h('div', { class: 'e2e-head' },
      h('span', { class: 'e2e-badge' }, '🔒 Сквозное шифрование'),
      h('span', { class: 'hint' }, 'Имена и содержимое видны только вам — на сервере они зашифрованы.'),
      h('span', { style: 'flex:1' }),
      h('button', { class: 'btn', onclick: async () => {
        const name = await askText('Новая папка', 'Название', 'Новая папка', 'Создать');
        if (!name) return;
        try {
          const obf = hex(rand(16));
          const n = await api('POST', '/api/files/folder', { parent: id, name: obf });
          await e2eUpdateMeta(id, m => { m.dirs[obf] = { name }; });
          location.hash = '#/e2e/' + n.id;
        } catch (ex) { report(ex); }
      } }, '📁 Папку'),
      h('button', { class: 'btn primary', onclick: () => picker.click() }, '⬆ Загрузить'), picker,
      h('button', { class: 'icon-btn', title: 'Ещё', onclick: ev => {
        const r = ev.currentTarget.getBoundingClientRect();
        contextMenu(r.right - 240, r.bottom + 4, [
          { icon: '🔑', label: 'Сменить кодовую фразу', onClick: changePassDialog },
          { icon: '🔒', label: 'Заблокировать (забыть ключ во вкладке)', onClick: () => { E2E.vk = null; E2E.metas.clear(); location.hash = '#/files'; } },
        ]);
      } }, '⋯')),
    rows.length ? h('div', { class: 'e2e-list' }, rows) : h('div', { class: 'empty' }, h('div', { class: 'big' }, '🔒'), 'Папка пуста. Загрузите файлы — они зашифруются ещё до отправки на сервер.'));
}

async function changePassDialog() {
  const cur = await askText('Сменить кодовую фразу', 'Текущая кодовая фраза', '', 'Далее', 'password');
  if (!cur) return;
  let raw;
  try { raw = await e2eUnlock(cur, false); } catch (e) { report(e); return; }
  const p = await askText('Новая кодовая фраза', 'Не короче 12 символов', '', 'Сохранить', 'password');
  if (!p) { raw.fill(0); return; }
  if (passQuality(p)) { toast(passQuality(p), true); raw.fill(0); return; }
  try { await e2eChangePass(raw, p); toast('Кодовая фраза изменена'); } catch (e) { report(e); }
  raw.fill(0);
}

// Меню корня E2E-папки в обычном списке файлов: только то, что не требует ключа.
function e2eRootMenu(n, x, y) {
  contextMenu(x, y, [
    { icon: '🔓', label: 'Открыть', onClick: () => { location.hash = '#/e2e/' + n.id; } },
    { icon: '✏️', label: 'Переименовать', onClick: () => renameNode(n) },
    { icon: '➡️', label: 'Переместить', onClick: () => moveDialog([n], false) },
    '-',
    { icon: '🗑️', label: 'Удалить', danger: true, onClick: () => trashNodes([n]) },
  ]);
}
