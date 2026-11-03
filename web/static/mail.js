'use strict';
/* CloudBox — почта: веб-клиент для внешних ящиков (IMAP/SMTP).
   HTML писем чужой: он очищается DOMPurify и показывается в iframe без скриптов и без доступа
   к CloudBox (sandbox без allow-same-origin). Картинки из интернета по умолчанию не грузятся —
   это защищает от пикселей слежения. */

const Mail = { info: null, accounts: [], acc: null, folders: [], folder: 'INBOX', page: null, msg: null, q: '', remote: false };

const SPECIAL_ICON = { inbox: '📥', drafts: '📝', sent: '📤', archive: '🗄️', junk: '🚫', trash: '🗑️' };
const SPECIAL_NAME = { inbox: 'Входящие', drafts: 'Черновики', sent: 'Отправленные', archive: 'Архив', junk: 'Спам', trash: 'Корзина' };

const mailURL = (path, params = {}) => `/api/mail/accounts/${Mail.acc.id}/${path}?` + new URLSearchParams(params);
const folderLabel = f => SPECIAL_NAME[f.special] || f.name.split(f.delim || '/').pop();
const who = list => (list || []).map(a => a.name || a.email).join(', ');
const fullAddr = a => a.name ? `${a.name} <${a.email}>` : a.email;

async function viewMail() {
  setCrumbs([{ label: 'Почта' }]);
  els.content.className = 'content mail-content';
  Mail.info = Mail.info || await api('GET', '/api/mail');
  if (!Mail.info.enabled) {
    els.content.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'big' }, '✉️'), Mail.info.reason));
    return;
  }
  Mail.accounts = await api('GET', '/api/mail/accounts');
  if (!Mail.accounts.length) {
    els.content.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'big' }, '✉️'),
      h('p', null, 'Подключите почтовый ящик — Яндекс, Mail.ru, Gmail или свой сервер — и читайте почту прямо в CloudBox.'),
      h('button', { class: 'btn primary', onclick: addAccountDialog }, '＋ Подключить ящик')));
    return;
  }
  if (!Mail.acc || !Mail.accounts.some(a => a.id === Mail.acc.id)) Mail.acc = Mail.accounts[0];
  Mail.side = h('aside', { class: 'mail-side' });
  Mail.list = h('section', { class: 'mail-list' });
  Mail.reader = h('section', { class: 'mail-reader' });
  Mail.root = h('div', { class: 'mail' }, Mail.side, Mail.list, Mail.reader);
  els.content.replaceChildren(Mail.root);
  Mail.reader.replaceChildren(h('div', { class: 'empty' }, 'Выберите письмо'));
  await loadFolders();
  await loadMessages();
}

async function loadFolders() {
  Mail.side.replaceChildren(h('div', { class: 'hint', style: 'padding:12px' }, 'Загрузка папок…'));
  try { Mail.folders = await api('GET', mailURL('folders')); }
  catch (e) { Mail.folders = []; report(e); }
  if (!Mail.folders.some(f => f.name === Mail.folder)) Mail.folder = (Mail.folders[0] || { name: 'INBOX' }).name;
  drawFolders();
}

function drawFolders() {
  const accSel = Mail.accounts.length > 1 ? h('select', { class: 'mail-acc', onchange: async e => {
    Mail.acc = Mail.accounts.find(a => a.id === +e.target.value); Mail.folder = 'INBOX'; Mail.q = '';
    await loadFolders(); await loadMessages();
  } }, Mail.accounts.map(a => h('option', { value: a.id, selected: a.id === Mail.acc.id }, a.email))) : h('div', { class: 'mail-acc-one', title: Mail.acc.email }, Mail.acc.email);
  Mail.side.replaceChildren(
    h('div', { class: 'mail-side-head' }, h('button', { class: 'btn primary', onclick: () => composeDialog({}) }, '✏️ Написать'),
      h('button', { class: 'icon-btn', title: 'Ящики', onclick: e => accountsMenu(e) }, '⚙️')),
    accSel,
    h('nav', { class: 'mail-folders' }, Mail.folders.map(f => h('a', { href: '#', class: f.name === Mail.folder ? 'active' : '', onclick: async e => {
      e.preventDefault(); Mail.folder = f.name; Mail.q = ''; drawFolders(); await loadMessages();
      Mail.root.classList.remove('has-msg');
    } }, h('span', { class: 'ico' }, SPECIAL_ICON[f.special] || '📁'), h('span', { class: 'grow' }, folderLabel(f)),
      f.unread ? h('span', { class: 'cnt' }, f.unread) : null))));
}

function accountsMenu(e) {
  const b = e.currentTarget.getBoundingClientRect();
  contextMenu(b.left, b.bottom + 4, [
    { icon: '＋', label: 'Подключить ещё ящик', onClick: addAccountDialog },
    { icon: '🔄', label: 'Обновить', onClick: async () => { await loadFolders(); await loadMessages(); } },
    '-',
    { icon: '🔌', label: 'Отключить ' + Mail.acc.email, danger: true, onClick: async () => {
      if (!await confirmDialog('Отключить ящик?', `${Mail.acc.email} пропадёт из CloudBox. Письма на почтовом сервере останутся.`, 'Отключить')) return;
      try { await api('DELETE', '/api/mail/accounts/' + Mail.acc.id); Mail.acc = null; viewMail(); } catch (ex) { report(ex); }
    } },
  ]);
}

async function loadMessages(offset = 0) {
  const append = offset > 0;
  if (!append) Mail.list.replaceChildren(h('div', { class: 'hint', style: 'padding:12px' }, 'Загрузка писем…'));
  let page;
  try { page = await api('GET', mailURL('messages', { folder: Mail.folder, offset, q: Mail.q })); }
  catch (e) { report(e); Mail.list.replaceChildren(h('div', { class: 'empty' }, e.message)); return; }
  if (append) page.messages = [...Mail.page.messages, ...page.messages];
  Mail.page = page;
  drawList();
}

function drawList() {
  const p = Mail.page;
  const search = h('input', { type: 'search', placeholder: 'Поиск в папке…', value: Mail.q });
  search.addEventListener('keydown', e => { if (e.key === 'Enter') { Mail.q = search.value.trim(); loadMessages(); } });
  const f = Mail.folders.find(x => x.name === Mail.folder);
  const rows = p.messages.map(m => h('a', { href: '#', class: 'mail-row' + (m.seen ? '' : ' unread') + (Mail.msg && Mail.msg.uid === m.uid ? ' active' : ''),
    onclick: e => { e.preventDefault(); openMessage(m); } },
    h('div', { class: 'top' }, h('span', { class: 'from' }, (f && (f.special === 'sent' || f.special === 'drafts') ? 'Кому: ' + who(m.to) : who(m.from)) || '(без отправителя)'),
      m.flagged ? h('span', { class: 'star' }, '★') : null, m.attachments ? h('span', null, '📎') : null,
      h('span', { class: 'tm' }, m.date ? timeShort(m.date) : '')),
    h('div', { class: 'subj' }, m.subject || '(без темы)')));
  Mail.list.replaceChildren(
    h('div', { class: 'mail-list-head' },
      h('b', null, f ? folderLabel(f) : Mail.folder), h('span', { class: 'sub' }, p.total ? ` · ${p.total}` : ''), search),
    h('div', { class: 'mail-rows' }, rows.length ? rows : h('div', { class: 'empty' }, Mail.q ? 'Ничего не найдено.' : 'Писем нет.'),
      p.messages.length < p.total ? h('div', { style: 'text-align:center;padding:10px' },
        h('button', { class: 'btn', onclick: () => loadMessages(p.messages.length) }, 'Показать ещё')) : null));
}

async function openMessage(sum) {
  Mail.root.classList.add('has-msg');
  Mail.reader.replaceChildren(h('div', { class: 'hint', style: 'padding:16px' }, 'Открываю письмо…'));
  let m;
  try { m = await api('GET', mailURL('message', { folder: Mail.folder, uid: sum.uid })); }
  catch (e) { report(e); Mail.reader.replaceChildren(h('div', { class: 'empty' }, e.message)); return; }
  Mail.msg = m; Mail.remote = false;
  if (!sum.seen) { sum.seen = true; const f = Mail.folders.find(x => x.name === Mail.folder); if (f && f.unread) { f.unread--; drawFolders(); } }
  drawList();
  drawMessage();
}

// sanitizeMail — HTML письма → безопасный документ для iframe srcdoc.
function sanitizeMail(m, remote) {
  let html = m.html;
  for (const [cid, uri] of Object.entries(m.inline || {})) html = html.split('cid:' + cid).join(uri);
  const clean = DOMPurify.sanitize(html, { FORBID_TAGS: ['form', 'input', 'button', 'textarea', 'select', 'meta', 'link', 'base'], FORBID_ATTR: ['action', 'formaction'] });
  const img = remote ? 'data: https: http:' : 'data:';
  const csp = `default-src 'none'; img-src ${img}; style-src 'unsafe-inline'; font-src data:`;
  return `<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="${csp}">` +
    `<base target="_blank"><style>body{font:14px/1.5 system-ui,sans-serif;margin:12px;color:#1c2330;background:#fff;overflow-wrap:anywhere}img{max-width:100%;height:auto}blockquote{border-left:3px solid #ccd;margin:0 0 0 4px;padding-left:10px;color:#555}</style>` +
    `</head><body>${clean}</body></html>`;
}

const hasRemote = html => /(src|srcset|background)\s*=\s*["']?\s*(https?:)?\/\//i.test(html) || /url\(\s*["']?\s*(https?:)?\/\//i.test(html);

function drawMessage() {
  const m = Mail.msg; if (!m) return;
  const f = Mail.folders.find(x => x.name === Mail.folder);
  const act = (ico, label, fn, cls = '') => h('button', { class: 'btn ' + cls, title: label, onclick: fn }, ico, h('span', { class: 'lbl' }, label));
  const others = Mail.folders.filter(x => x.name !== Mail.folder);
  const moveSel = h('select', { title: 'Переместить в папку', onchange: async e => {
    const dest = e.target.value; if (!dest) return;
    try { await api('POST', mailURL('move', {}), { folder: Mail.folder, uids: [m.uid], dest }); toast('Перемещено'); afterRemoval(); } catch (ex) { report(ex); }
  } }, h('option', { value: '' }, '📂 Переместить…'), others.map(x => h('option', { value: x.name }, folderLabel(x))));
  let body;
  if (m.html) {
    const frame = h('iframe', { class: 'mail-frame', sandbox: 'allow-popups allow-popups-to-escape-sandbox', referrerpolicy: 'no-referrer' });
    frame.srcdoc = sanitizeMail(m, Mail.remote);
    const banner = !Mail.remote && hasRemote(m.html) ? h('div', { class: 'mail-banner' }, '🛡️ Картинки из интернета скрыты — по ним отправитель может узнать, что вы открыли письмо. ',
      h('a', { href: '#', onclick: e => { e.preventDefault(); Mail.remote = true; drawMessage(); } }, 'Показать')) : null;
    body = [banner, frame];
  } else body = [h('div', { class: 'mail-text' }, linkify(m.text || ''))];
  Mail.reader.replaceChildren(
    h('div', { class: 'mail-actions' },
      h('a', { href: '#', class: 'icon-btn mail-back', onclick: e => { e.preventDefault(); Mail.root.classList.remove('has-msg'); } }, '‹'),
      act('↩️', 'Ответить', () => composeDialog({ reply: m })),
      act('↩️↩️', 'Всем', () => composeDialog({ reply: m, all: true })),
      act('➡️', 'Переслать', () => composeDialog({ forward: m })),
      act(m.flagged ? '★' : '☆', m.flagged ? 'Снять отметку' : 'Важное', async () => {
        try { await api('POST', mailURL('flag', {}), { folder: Mail.folder, uids: [m.uid], flag: 'flagged', on: !m.flagged }); m.flagged = !m.flagged;
          const s = Mail.page.messages.find(x => x.uid === m.uid); if (s) s.flagged = m.flagged; drawMessage(); drawList(); } catch (ex) { report(ex); }
      }),
      act('✉️', 'Непрочитано', async () => {
        try { await api('POST', mailURL('flag', {}), { folder: Mail.folder, uids: [m.uid], flag: 'seen', on: false });
          const s = Mail.page.messages.find(x => x.uid === m.uid); if (s) s.seen = false; if (f) f.unread++; drawFolders(); drawList(); toast('Отмечено непрочитанным'); } catch (ex) { report(ex); }
      }),
      moveSel,
      act('🗑️', f && f.special === 'trash' ? 'Удалить навсегда' : 'Удалить', async () => {
        if (f && f.special === 'trash' && !await confirmDialog('Удалить навсегда?', 'Письмо будет удалено с почтового сервера без возможности восстановления.')) return;
        try { const r = await api('POST', mailURL('delete', {}), { folder: Mail.folder, uids: [m.uid] }); toast(r.permanent ? 'Удалено' : 'Перемещено в корзину'); afterRemoval(); } catch (ex) { report(ex); }
      }, 'danger')),
    h('div', { class: 'mail-head' },
      h('h2', null, m.subject || '(без темы)'),
      h('div', null, h('b', null, (m.from || []).map(fullAddr).join(', ')), h('span', { class: 'tm' }, m.date ? ' · ' + new Date(m.date).toLocaleString('ru-RU') : '')),
      m.to.length ? h('div', { class: 'sub' }, 'Кому: ', m.to.map(fullAddr).join(', ')) : null,
      m.cc.length ? h('div', { class: 'sub' }, 'Копия: ', m.cc.map(fullAddr).join(', ')) : null,
      m.attachments.length ? h('div', { class: 'mail-atts' }, m.attachments.map(a => h('span', { class: 'chip att' },
        h('a', { href: mailURL('attachment', { folder: Mail.folder, uid: m.uid, n: a.index }), download: a.filename, title: 'Скачать' }, '📎 ', a.filename, h('span', { class: 'sub' }, ' ' + fmtSize(a.size))),
        h('button', { class: 'icon-btn', title: 'Сохранить в CloudBox', onclick: () => saveAttachment(m, a) }, '☁️')))) : null),
    ...body);
}

function afterRemoval() {
  const uid = Mail.msg.uid;
  Mail.page.messages = Mail.page.messages.filter(x => x.uid !== uid);
  Mail.page.total = Math.max(0, Mail.page.total - 1);
  Mail.msg = null;
  drawList();
  Mail.reader.replaceChildren(h('div', { class: 'empty' }, 'Выберите письмо'));
  Mail.root.classList.remove('has-msg');
  loadFolders();
}

// Вложения складываются в папку «Вложения из почты» в корне.
async function saveAttachment(m, a) {
  try {
    const name = 'Вложения из почты';
    let dir = (await api('GET', '/api/files')).items.find(x => x.is_dir && x.name === name);
    if (!dir) dir = await api('POST', '/api/files/folder', { parent: '', name });
    const n = await api('POST', mailURL('attachment/save', {}), { folder: Mail.folder, uid: m.uid, n: a.index, parent: dir.id });
    toast(`Сохранено: ${name}/${n.name}`);
    refreshUsage();
  } catch (e) { report(e); }
}

function quote(m) {
  const head = `${m.date ? new Date(m.date).toLocaleString('ru-RU') : ''}, ${(m.from || []).map(fullAddr).join(', ')} пишет:`;
  const text = m.text || new DOMParser().parseFromString(DOMPurify.sanitize(m.html || ''), 'text/html').body.textContent || '';
  return '\n\n' + head + '\n' + text.trim().split('\n').map(l => '> ' + l).join('\n');
}

const withPrefix = (p, s) => new RegExp('^' + p + ':', 'i').test(s || '') ? s : `${p}: ${s || ''}`;

function composeDialog({ reply, all, forward }) {
  const me = Mail.acc.email.toLowerCase();
  let to = '', cc = '', subject = '', text = '', inReplyTo = '', refs = [];
  if (reply) {
    const target = reply.reply_to.length ? reply.reply_to : reply.from;
    to = target.map(fullAddr).join(', ');
    if (all) cc = [...reply.to, ...reply.cc].filter(a => a.email.toLowerCase() !== me && !target.some(t => t.email === a.email)).map(fullAddr).join(', ');
    subject = withPrefix('Re', reply.subject); text = quote(reply);
    inReplyTo = reply.message_id; refs = reply.references || [];
  } else if (forward) {
    subject = withPrefix('Fwd', forward.subject);
    text = '\n\n---------- Пересылаемое письмо ----------\nОт: ' + forward.from.map(fullAddr).join(', ') + '\nДата: ' +
      (forward.date ? new Date(forward.date).toLocaleString('ru-RU') : '') + '\nТема: ' + (forward.subject || '') + '\nКому: ' +
      forward.to.map(fullAddr).join(', ') + '\n\n' + (forward.text || new DOMParser().parseFromString(DOMPurify.sanitize(forward.html || ''), 'text/html').body.textContent || '').trim();
  }
  const fTo = h('input', { type: 'text', value: to, placeholder: 'адрес@example.ru, …' });
  const fCc = h('input', { type: 'text', value: cc });
  const fBcc = h('input', { type: 'text' });
  const fSubj = h('input', { type: 'text', value: subject });
  const fText = h('textarea', { rows: 14, class: 'mail-compose-text' });
  fText.value = text;
  const files = [];
  const fileList = h('div', { class: 'chips' });
  const drawFiles = () => fileList.replaceChildren(...files.map((f, i) => h('span', { class: 'chip' }, '📎 ', f.name, ' ', h('span', { class: 'sub' }, fmtSize(f.size)),
    h('button', { class: 'icon-btn', onclick: () => { files.splice(i, 1); drawFiles(); } }, '×'))));
  const picker = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: e => { files.push(...e.target.files); e.target.value = ''; drawFiles(); } });
  const ccRow = h('div', { class: all || cc ? '' : 'hidden' }, h('label', null, 'Копия'), fCc, h('label', null, 'Скрытая копия'), fBcc);
  const body = [
    h('label', null, 'От'), h('div', { class: 'sub' }, Mail.acc.name ? `${Mail.acc.name} <${Mail.acc.email}>` : Mail.acc.email),
    h('label', null, 'Кому ', h('a', { href: '#', class: 'sub', onclick: e => { e.preventDefault(); ccRow.classList.toggle('hidden'); } }, '(копия)')), fTo, ccRow,
    h('label', null, 'Тема'), fSubj,
    fText,
    h('div', { class: 'row-inline', style: 'margin-top:8px' }, h('button', { class: 'btn', onclick: () => picker.click() }, '📎 Прикрепить файлы'), picker,
      forward && forward.attachments.length ? h('span', { class: 'hint' }, 'Вложения исходного письма не пересылаются — сохраните их в CloudBox и прикрепите.') : null),
    fileList,
  ];
  modal({ title: reply ? 'Ответ' : forward ? 'Пересылка' : 'Новое письмо', wide: true, body, buttons: [{ label: 'Отмена' }, { label: '📤 Отправить', primary: true, onClick: async () => {
    const size = files.reduce((s, f) => s + f.size, 0);
    if (size > 24 * 1024 * 1024) { toast('Вложения больше 24 МБ — большинство серверов такое письмо не примут', true); return false; }
    const fd = new FormData();
    fd.append('message', JSON.stringify({ to: [fTo.value], cc: [fCc.value], bcc: [fBcc.value], subject: fSubj.value, text: fText.value, in_reply_to: inReplyTo, references: refs }));
    for (const f of files) fd.append('file', f, f.name);
    const res = await fetch(`/api/mail/accounts/${Mail.acc.id}/send`, { method: 'POST', body: fd, credentials: 'same-origin' });
    const d = await res.json().catch(() => ({}));
    if (!res.ok) { toast(d.error || 'Ошибка ' + res.status, true); return false; }
    toast('Письмо отправлено');
    if (Mail.folders.find(x => x.name === Mail.folder)?.special === 'sent') loadMessages();
  } }] });
  setTimeout(() => (to ? fText : fTo).focus(), 60);
  if (to) setTimeout(() => fText.setSelectionRange(0, 0), 80);
}

function addAccountDialog() {
  const email = h('input', { type: 'text', placeholder: 'ivan@yandex.ru', autocomplete: 'off' });
  const pass = h('input', { type: 'password', autocomplete: 'new-password' });
  const name = h('input', { type: 'text', placeholder: 'Иван Петров', value: state.me.username });
  const hint = h('div', { class: 'hint' });
  const f = { imap_host: h('input', { type: 'text' }), imap_port: h('input', { type: 'number', value: 993 }), smtp_host: h('input', { type: 'text' }), smtp_port: h('input', { type: 'number', value: 465 }), login: h('input', { type: 'text', placeholder: 'обычно совпадает с адресом' }) };
  const sec = v => h('select', null, ['tls', 'starttls', 'none'].map(x => h('option', { value: x, selected: x === v }, { tls: 'SSL/TLS', starttls: 'STARTTLS', none: 'без шифрования (только локальная сеть)' }[x])));
  f.imap_sec = sec('tls'); f.smtp_sec = sec('tls');
  const adv = h('div', { class: 'hidden mail-adv' },
    h('label', null, 'Сервер IMAP'), h('div', { class: 'row-inline' }, f.imap_host, f.imap_port, f.imap_sec),
    h('label', null, 'Сервер SMTP'), h('div', { class: 'row-inline' }, f.smtp_host, f.smtp_port, f.smtp_sec),
    h('label', null, 'Логин'), f.login);
  email.addEventListener('change', async () => {
    const d = await api('GET', '/api/mail/preset?email=' + encodeURIComponent(email.value.trim())).catch(() => ({}));
    if (d.known) {
      const p = d.preset;
      f.imap_host.value = p.imap_host; f.imap_port.value = p.imap_port; f.imap_sec.value = p.imap_sec;
      f.smtp_host.value = p.smtp_host; f.smtp_port.value = p.smtp_port; f.smtp_sec.value = p.smtp_sec;
      hint.textContent = '💡 ' + (p.hint || '');
    } else {
      const dom = email.value.split('@')[1] || '';
      if (dom && !f.imap_host.value) { f.imap_host.value = 'imap.' + dom; f.smtp_host.value = 'smtp.' + dom; }
      hint.textContent = 'Сервис не из списка известных — проверьте адреса серверов в расширенных настройках.';
      adv.classList.remove('hidden');
    }
  });
  modal({ title: 'Подключить почтовый ящик', body: [
    h('label', null, 'Адрес почты'), email,
    h('label', null, 'Пароль'), pass, hint,
    h('label', null, 'Имя отправителя'), name,
    h('div', { style: 'margin-top:8px' }, h('a', { href: '#', onclick: e => { e.preventDefault(); adv.classList.toggle('hidden'); } }, '⚙️ Расширенные настройки')),
    adv,
    h('p', { class: 'hint' }, 'Пароль хранится на сервере CloudBox в зашифрованном виде и нужен, чтобы забирать и отправлять почту. Лучше использовать пароль приложения, а не основной.'),
  ], buttons: [{ label: 'Отмена' }, { label: 'Подключить', primary: true, onClick: async () => {
    toast('Проверяю подключение…');
    const a = await api('POST', '/api/mail/accounts', {
      email: email.value.trim(), password: pass.value, name: name.value.trim(), login: f.login.value.trim(),
      imap_host: f.imap_host.value.trim(), imap_port: +f.imap_port.value, imap_sec: f.imap_sec.value,
      smtp_host: f.smtp_host.value.trim(), smtp_port: +f.smtp_port.value, smtp_sec: f.smtp_sec.value });
    toast('Ящик подключён: ' + a.email);
    Mail.acc = a; Mail.folder = 'INBOX';
    if (state.view === 'mail') viewMail(); else location.hash = '#/mail';
  } }] });
}
