'use strict';
/* CloudBox — чат и видеозвонки.
   События приходят по Server-Sent Events (/api/talk/events), отправка — обычными POST.
   Звонок — WebRTC «каждый с каждым»: сервер только пересылает сигналы (offer/answer/ice) между вкладками. */

const Talk = {
  es: null, peer: null, started: false,
  rooms: [], calls: {},  // room_id → состояние звонка
  unread: 0, view: null, // открытая комната
  rung: new Set(),       // о каких звонках уже показали входящий вызов
};

// ---------- поток событий ----------
Talk.start = function () {
  if (Talk.started || !window.EventSource) return;
  Talk.started = true;
  const navA = document.querySelector('.nav a[data-view="talk"]');
  if (navA) { Talk.badge = h('span', { class: 'nav-count hidden' }); navA.append(Talk.badge); }
  Talk.connect();
  document.addEventListener('visibilitychange', () => { if (!document.hidden) Talk.markRead(); });
};

Talk.connect = function () {
  const es = new EventSource('/api/talk/events');
  Talk.es = es;
  const on = (type, fn) => es.addEventListener(type, e => { try { fn(JSON.parse(e.data)); } catch (err) { console.error('talk', type, err); } });
  on('hello', d => {
    const reconnected = Talk.peer && Talk.peer !== d.peer;
    Talk.peer = d.peer;
    Talk.setUnread(d.unread);
    Talk.calls = {};
    for (const st of d.calls || []) Talk.calls[st.room] = st;
    // при переподключении сервер уже вывел старую вкладку из звонка
    if (reconnected && Call.room) Call.hangup('Связь с сервером прервалась — звонок завершён');
    Talk.refreshView();
  });
  on('message', m => Talk.onMessage(m));
  on('room', () => Talk.refreshView());
  on('room_removed', d => {
    if (Talk.view && Talk.view.id === d.id) { toast('Вы больше не участник этой комнаты'); location.hash = '#/talk'; }
    if (Call.room === d.id) Call.hangup();
    Talk.refreshView();
  });
  on('read', () => Talk.refreshView(true));
  on('call', st => Talk.onCallState(st));
  on('signal', d => Call.onSignal(d).catch(e => console.error('signal', e)));
};

Talk.setUnread = function (n) {
  Talk.unread = Math.max(0, n | 0);
  if (Talk.badge) { Talk.badge.textContent = Talk.unread > 99 ? '99+' : Talk.unread; Talk.badge.classList.toggle('hidden', !Talk.unread); }
};

// refreshView перечитывает список комнат (и счётчик), если открыт раздел чата.
Talk.refreshView = async function (countOnly) {
  if (state.view !== 'talk') {
    if (countOnly) { try { const d = await api('GET', '/api/me'); Talk.setUnread(d.talk_unread); } catch (_) {} }
    return;
  }
  try {
    Talk.rooms = await api('GET', '/api/talk/rooms');
    Talk.setUnread(Talk.rooms.reduce((s, r) => s + r.unread, 0));
    for (const r of Talk.rooms) if (r.call) Talk.calls[r.id] = r.call;
    Talk.drawRooms();
    if (Talk.view) { const r = Talk.rooms.find(x => x.id === Talk.view.id); if (r) { Talk.view.room = r; Talk.drawHeader(); } }
  } catch (e) { console.error(e); }
};

Talk.onMessage = function (m) {
  const v = Talk.view;
  const mine = m.user_id === state.me.id;
  const visible = v && v.id === m.room_id && state.view === 'talk' && !document.hidden;
  if (v && v.id === m.room_id && state.view === 'talk') Talk.appendMessage(m);
  if (m.kind === 'text' && !mine) {
    if (visible) Talk.markRead();
    else {
      Talk.setUnread(Talk.unread + 1);
      Talk.browserNotify(m);
    }
  }
  // обновить превью в списке комнат
  const r = Talk.rooms.find(x => x.id === m.room_id);
  if (r) {
    r.last = m; r.last_activity = m.created_at;
    if (m.kind === 'text' && !mine && !visible) r.unread++;
    Talk.rooms.sort((a, b) => new Date(b.last_activity) - new Date(a.last_activity));
    Talk.drawRooms();
  } else if (state.view === 'talk') Talk.refreshView();
};

Talk.browserNotify = function (m) {
  if (!('Notification' in window) || Notification.permission !== 'granted' || !document.hidden) return;
  const r = Talk.rooms.find(x => x.id === m.room_id);
  const title = r && r.kind === 'group' ? `${m.username} · ${r.name}` : m.username;
  const n = new Notification(title, { body: m.body.slice(0, 200), tag: 'talk-' + m.room_id, icon: '/icon.svg' });
  n.onclick = () => { focus(); location.hash = '#/talk/' + m.room_id; n.close(); };
};

Talk.markRead = function () {
  const v = Talk.view;
  if (!v || state.view !== 'talk' || document.hidden || !v.messages.length) return;
  const last = v.messages[v.messages.length - 1].id;
  if (last <= v.readUpTo) return;
  v.readUpTo = last;
  api('POST', `/api/talk/rooms/${v.id}/read`, { last_id: last }).catch(() => {});
  const r = Talk.rooms.find(x => x.id === v.id);
  if (r && r.unread) { Talk.setUnread(Talk.unread - r.unread); r.unread = 0; Talk.drawRooms(); }
};

// ---------- состояние звонков и входящие вызовы ----------
Talk.onCallState = function (st) {
  const prev = Talk.calls[st.room];
  Talk.calls[st.room] = st;
  if (Call.room === st.room) Call.reconcile(st);
  const meIn = st.participants.some(p => p.peer === Talk.peer);
  const someoneElse = st.participants.some(p => p.user_id !== state.me.id);
  if (st.active && !meIn && someoneElse && !(prev && prev.active) && !Talk.rung.has(st.room + st.started_at)) {
    Talk.rung.add(st.room + st.started_at);
    Talk.ring(st);
  }
  if (!st.active) Talk.dismissRing(st.room);
  if (Talk.view && Talk.view.id === st.room) Talk.drawHeader();
  const r = Talk.rooms.find(x => x.id === st.room);
  if (r) { r.call = st.active ? st : null; Talk.drawRooms(); }
};

Talk.rings = new Map();
Talk.ring = async function (st) {
  let room = Talk.rooms.find(x => x.id === st.room);
  if (!room) { try { room = await api('GET', '/api/talk/rooms/' + st.room); } catch (_) { return; } }
  const who = st.participants.find(p => p.user_id !== state.me.id);
  const title = room.kind === 'group' ? `Звонок в «${room.name}»` : `${who ? who.username : room.name} звонит`;
  const el = h('div', { class: 'ring' },
    h('div', { class: 'ring-ico' }, '📞'),
    h('div', { class: 'grow' }, h('div', { class: 'nm' }, title), room.kind === 'group' && who ? h('div', { class: 'sub' }, 'начал(а) ' + who.username) : null),
    h('button', { class: 'btn primary', onclick: () => { Talk.dismissRing(st.room); location.hash = '#/talk/' + st.room; Call.join(st.room); } }, 'Ответить'),
    h('button', { class: 'icon-btn', title: 'Скрыть', onclick: () => Talk.dismissRing(st.room) }, '×'));
  Talk.dismissRing(st.room);
  Talk.rings.set(st.room, el);
  document.body.append(el);
  if ('Notification' in window && Notification.permission === 'granted' && document.hidden) {
    const n = new Notification(title, { body: 'Нажмите, чтобы ответить', tag: 'call-' + st.room, icon: '/icon.svg', requireInteraction: true });
    n.onclick = () => { focus(); location.hash = '#/talk/' + st.room; n.close(); };
  }
};
Talk.dismissRing = function (room) { const el = Talk.rings.get(room); if (el) { el.remove(); Talk.rings.delete(room); } };

// ---------- раздел «Чат» ----------
async function viewTalk(roomId) {
  setCrumbs([{ label: 'Чат', href: '#/talk' }]);
  els.content.className = 'content talk-content';
  Talk.list = h('div', { class: 'talk-rooms' });
  Talk.pane = h('section', { class: 'talk-room' });
  const notifyBtn = 'Notification' in window && Notification.permission === 'default'
    ? h('button', { class: 'btn', title: 'Показывать уведомления о сообщениях и звонках, когда вкладка свёрнута', onclick: async e => {
      await Notification.requestPermission(); e.target.remove();
    } }, '🔔') : null;
  els.content.replaceChildren(h('div', { class: 'talk' + (roomId ? ' has-room' : '') },
    h('aside', { class: 'talk-side' },
      h('div', { class: 'talk-side-head' }, h('button', { class: 'btn primary', onclick: newChatDialog }, '＋ Новый чат'), notifyBtn),
      Talk.list),
    Talk.pane));
  Talk.view = null;
  Talk.rooms = await api('GET', '/api/talk/rooms');
  Talk.setUnread(Talk.rooms.reduce((s, r) => s + r.unread, 0));
  for (const r of Talk.rooms) if (r.call) Talk.calls[r.id] = r.call;
  Talk.drawRooms();
  if (roomId) await Talk.openRoom(roomId);
  else Talk.pane.replaceChildren(h('div', { class: 'empty' }, h('div', { class: 'big' }, '💬'),
    Talk.rooms.length ? 'Выберите переписку слева.' : 'Переписок пока нет. Нажмите «Новый чат», чтобы написать коллеге или создать комнату.'));
}

const avatarOf = (name, kind) => h('div', { class: 'avatar' + (kind === 'group' ? ' group' : '') }, kind === 'group' ? '👥' : (name || '?').slice(0, 1).toUpperCase());

function timeShort(s) {
  const d = new Date(s), now = new Date();
  if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' });
  return d.toLocaleDateString('ru-RU', { day: 'numeric', month: 'short' });
}

Talk.drawRooms = function () {
  if (!Talk.list || !Talk.list.isConnected) return;
  Talk.list.replaceChildren(...Talk.rooms.map(r => {
    const last = r.last ? (r.last.kind === 'system' ? r.last.body : (r.last.user_id === state.me.id ? 'Вы: ' : (r.kind === 'group' ? r.last.username + ': ' : '')) + r.last.body) : 'Нет сообщений';
    return h('a', { class: 'talk-item' + (Talk.view && Talk.view.id === r.id ? ' active' : ''), href: '#/talk/' + r.id },
      avatarOf(r.name, r.kind),
      h('div', { class: 'grow' },
        h('div', { class: 'top' }, h('span', { class: 'nm' }, r.name), h('span', { class: 'tm' }, r.last ? timeShort(r.last.created_at) : '')),
        h('div', { class: 'bottom' }, h('span', { class: 'prev' }, last.split('\n')[0]),
          r.call ? h('span', { class: 'call-dot', title: 'Идёт звонок' }, '📞') : null,
          r.unread ? h('span', { class: 'nav-count' }, r.unread > 99 ? '99+' : r.unread) : null)));
  }));
};

Talk.openRoom = async function (id) {
  let room;
  try { room = await api('GET', '/api/talk/rooms/' + id); }
  catch (e) { report(e); location.hash = '#/talk'; return; }
  const box = h('div', { class: 'talk-msgs' });
  const input = h('textarea', { rows: 1, placeholder: 'Сообщение…', maxlength: 4000 });
  const v = Talk.view = { id, room, box, input, messages: [], ids: new Set(), readUpTo: room.last_read, head: h('header', { class: 'talk-head' }) };
  Talk.drawRooms();
  const send = async () => {
    const body = input.value.trim();
    if (!body) return;
    input.value = ''; autosize();
    try { const m = await api('POST', `/api/talk/rooms/${id}/messages`, { body }); Talk.appendMessage(m); }
    catch (e) { input.value = body; autosize(); report(e); }
  };
  const autosize = () => { input.style.height = 'auto'; input.style.height = Math.min(input.scrollHeight, 160) + 'px'; };
  input.addEventListener('input', autosize);
  input.addEventListener('keydown', e => { if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); } });
  Talk.pane.replaceChildren(v.head, box,
    h('footer', { class: 'talk-compose' }, input, h('button', { class: 'btn primary', onclick: send, title: 'Отправить (Enter)' }, '➤')));
  setCrumbs([{ label: 'Чат', href: '#/talk' }, { label: room.name }]);
  Talk.drawHeader();
  const msgs = await api('GET', `/api/talk/rooms/${id}/messages?limit=50`);
  if (Talk.view !== v) return;
  v.more = msgs.length === 50;
  for (const m of msgs) Talk.appendMessage(m, true);
  Talk.drawMessages();
  box.scrollTop = box.scrollHeight;
  Talk.markRead();
  if (matchMedia('(pointer: fine)').matches) input.focus();
};

Talk.drawHeader = function () {
  const v = Talk.view; if (!v) return;
  const r = v.room, st = Talk.calls[r.id];
  const inThis = Call.room === r.id;
  const callBtn = inThis
    ? h('button', { class: 'btn primary', onclick: () => Call.show() }, '📹 Вернуться в звонок')
    : st && st.active
      ? h('button', { class: 'btn primary pulse', onclick: () => Call.join(r.id) }, `📞 Присоединиться (${st.participants.length})`)
      : h('button', { class: 'btn', onclick: () => Call.join(r.id), title: 'Начать видеозвонок' }, '📹 Звонок');
  const sub = r.kind === 'group' ? `${r.members.length} ${plural(r.members.length, 'участник', 'участника', 'участников')}` : 'личная переписка';
  v.head.replaceChildren(
    h('a', { class: 'icon-btn talk-back', href: '#/talk', title: 'К списку' }, '‹'),
    avatarOf(r.name, r.kind),
    h('div', { class: 'grow' }, h('div', { class: 'nm' }, r.name), h('div', { class: 'sub' }, sub)),
    callBtn,
    h('button', { class: 'icon-btn', title: 'Ещё', onclick: e => roomMenu(r, e) }, '⋯'));
};

function plural(n, one, few, many) {
  const m10 = n % 10, m100 = n % 100;
  return m10 === 1 && m100 !== 11 ? one : m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14) ? few : many;
}

Talk.appendMessage = function (m, batch) {
  const v = Talk.view;
  if (!v || v.id !== m.room_id || v.ids.has(m.id)) return;
  v.ids.add(m.id);
  v.messages.push(m);
  if (batch) return;
  const stick = v.box.scrollHeight - v.box.scrollTop - v.box.clientHeight < 80;
  Talk.drawMessages();
  if (stick || m.user_id === state.me.id) v.box.scrollTop = v.box.scrollHeight;
};

// linkify — текст сообщения с кликабельными ссылками http(s); всё остальное — только текстовые узлы.
function linkify(text) {
  const out = [];
  const re = /https?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)\]]/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    out.push(h('a', { href: m[0], target: '_blank', rel: 'noopener noreferrer' }, m[0]));
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

Talk.drawMessages = function () {
  const v = Talk.view; if (!v) return;
  const kids = [];
  if (v.more) kids.push(h('div', { class: 'talk-more' }, h('button', { class: 'btn', onclick: Talk.loadOlder }, 'Показать более ранние')));
  let prev = null, prevDay = '';
  for (const m of v.messages) {
    const day = new Date(m.created_at).toDateString();
    if (day !== prevDay) {
      kids.push(h('div', { class: 'talk-day' }, new Date(m.created_at).toLocaleDateString('ru-RU', { day: 'numeric', month: 'long', year: 'numeric' })));
      prevDay = day; prev = null;
    }
    if (m.kind === 'system') {
      kids.push(h('div', { class: 'talk-sys' }, m.body, ' · ', timeShort(m.created_at)));
      prev = null; continue;
    }
    const mine = m.user_id === state.me.id;
    const cont = prev && prev.user_id === m.user_id && new Date(m.created_at) - new Date(prev.created_at) < 5 * 60e3;
    kids.push(h('div', { class: 'msg' + (mine ? ' mine' : '') + (cont ? ' cont' : '') },
      !cont && !mine && v.room.kind === 'group' ? h('div', { class: 'who' }, m.username || 'удалённый пользователь') : null,
      h('div', { class: 'bubble' }, h('span', { class: 'txt' }, linkify(m.body)),
        h('span', { class: 'tm', title: new Date(m.created_at).toLocaleString('ru-RU') }, new Date(m.created_at).toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' })))));
    prev = m;
  }
  if (!v.messages.length) kids.push(h('div', { class: 'empty' }, 'Сообщений пока нет. Напишите первым!'));
  v.box.replaceChildren(...kids);
  Talk.markRead();
};

Talk.loadOlder = async function () {
  const v = Talk.view; if (!v || !v.messages.length) return;
  const older = await api('GET', `/api/talk/rooms/${v.id}/messages?limit=50&before=${v.messages[0].id}`);
  if (Talk.view !== v) return;
  const h0 = v.box.scrollHeight;
  v.more = older.length === 50;
  v.messages = [...older.filter(m => !v.ids.has(m.id)), ...v.messages];
  for (const m of older) v.ids.add(m.id);
  Talk.drawMessages();
  v.box.scrollTop = v.box.scrollHeight - h0; // остаёмся на том же месте
};

function roomMenu(r, e) {
  const b = e.currentTarget.getBoundingClientRect();
  const isCreator = r.created_by === state.me.id;
  contextMenu(b.right - 220, b.bottom + 4, [
    { icon: '👥', label: 'Участники', onClick: () => membersDialog(r) },
    r.kind === 'group' ? { icon: '➕', label: 'Добавить участников', onClick: async () => {
      const users = await pickUsers({ title: 'Добавить в «' + r.name + '»', multi: true, exclude: r.members.map(m => m.id), okLabel: 'Добавить' });
      if (users && users.length) { try { await api('POST', `/api/talk/rooms/${r.id}/members`, { user_ids: users.map(u => u.id) }); } catch (ex) { report(ex); } }
    } } : null,
    r.kind === 'group' ? { icon: '✏️', label: 'Переименовать', onClick: async () => {
      const name = await askText('Название комнаты', 'Название', r.name, 'Сохранить');
      if (name && name !== r.name) { try { await api('PATCH', '/api/talk/rooms/' + r.id, { name }); } catch (ex) { report(ex); } }
    } } : null,
    r.kind === 'group' ? '-' : null,
    r.kind === 'group' ? { icon: '🚪', label: 'Покинуть комнату', danger: true, onClick: async () => {
      if (!await confirmDialog('Покинуть комнату?', isCreator && r.members.length > 1
        ? 'Вы создатель комнаты: после выхода исключать участников будет некому.' : 'Вернуться можно, только если вас снова пригласят.', 'Покинуть')) return;
      try { await api('DELETE', `/api/talk/rooms/${r.id}/members/me`); if (Call.room === r.id) Call.hangup(); location.hash = '#/talk'; } catch (ex) { report(ex); }
    } } : null,
  ]);
}

function membersDialog(r) {
  const isCreator = r.created_by === state.me.id;
  const md = modal({ title: 'Участники «' + r.name + '»', body: h('div', null, r.members.map(m => h('div', { class: 'list-row' },
    avatarOf(m.username), h('div', { class: 'grow' }, m.username, m.id === r.created_by && r.kind === 'group' ? h('span', { class: 'badge' }, 'создатель') : null,
      m.id === state.me.id ? h('span', { class: 'badge' }, 'вы') : null),
    m.id !== state.me.id ? h('button', { class: 'btn', onclick: async () => {
      try { const d = await api('POST', '/api/talk/direct', { user_id: m.id }); md.close(); location.hash = '#/talk/' + d.id; } catch (ex) { report(ex); }
    } }, '💬 Написать') : null,
    isCreator && r.kind === 'group' && m.id !== state.me.id ? h('button', { class: 'btn danger', onclick: async ev => {
      if (!await confirmDialog('Исключить ' + m.username + '?', 'Пользователь перестанет видеть переписку и будет отключён от звонка.', 'Исключить')) return;
      try { await api('DELETE', `/api/talk/rooms/${r.id}/members/${m.id}`); ev.target.closest('.list-row').remove(); } catch (ex) { report(ex); }
    } }, 'Исключить') : null))) });
}

// pickUsers — выбор пользователей из каталога. Возвращает [{id, name}] или null.
function pickUsers({ title, multi, exclude = [], okLabel = 'Готово', extra }) {
  return new Promise(resolve => {
    let done = false;
    const chosen = new Map();
    const q = h('input', { type: 'text', placeholder: 'Имя пользователя…', autocomplete: 'off' });
    const results = h('div', { class: 'picker', style: 'max-height:240px' });
    const chips = h('div', { class: 'chips' });
    const drawChips = () => chips.replaceChildren(...[...chosen.values()].map(u => h('span', { class: 'chip' }, u.name,
      h('button', { class: 'icon-btn', onclick: () => { chosen.delete(u.id); drawChips(); } }, '×'))));
    let timer;
    const search = async () => {
      const list = (await api('GET', '/api/directory?q=' + encodeURIComponent(q.value.trim()))).filter(e => e.kind === 'user' && !exclude.includes(e.id));
      results.replaceChildren(...(list.length ? list.map(e => h('div', { class: 'row' + (chosen.has(e.id) ? ' disabled' : ''), onclick: () => {
        if (!multi) { done = true; resolve([e]); m.close(); return; }
        chosen.set(e.id, e); drawChips(); q.value = ''; q.focus(); search();
      } }, '👤', e.name)) : [h('div', { class: 'row disabled' }, 'Никого не найдено')]));
    };
    q.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(() => search().catch(report), 200); });
    const m = modal({ title, body: [...(extra || []), h('label', null, multi ? 'Участники' : 'Кому'), chips, q, results],
      buttons: multi ? [{ label: 'Отмена' }, { label: okLabel, primary: true, onClick: () => { done = true; resolve([...chosen.values()]); } }] : [],
      onClose: () => { if (!done) resolve(null); } });
    search().catch(report);
  });
}

async function newChatDialog() {
  const body = h('div', { class: 'newchat' },
    h('button', { class: 'btn big', onclick: async () => {
      m.close();
      const users = await pickUsers({ title: 'Личное сообщение', multi: false });
      if (!users) return;
      try { const r = await api('POST', '/api/talk/direct', { user_id: users[0].id }); location.hash = '#/talk/' + r.id; } catch (e) { report(e); }
    } }, '👤', h('div', null, h('b', null, 'Личное сообщение'), h('div', { class: 'sub' }, 'Переписка и звонки один на один'))),
    h('button', { class: 'btn big', onclick: async () => {
      m.close();
      const name = h('input', { type: 'text', placeholder: 'Например: Поход на Таганай', maxlength: 80 });
      const users = await pickUsers({ title: 'Новая комната', multi: true, okLabel: 'Создать', extra: [h('label', null, 'Название'), name] });
      if (!users) return;
      try { const r = await api('POST', '/api/talk/rooms', { name: name.value, user_ids: users.map(u => u.id) }); location.hash = '#/talk/' + r.id; } catch (e) { report(e); }
    } }, '👥', h('div', null, h('b', null, 'Комната'), h('div', { class: 'sub' }, 'Групповой чат и звонок до 6 участников'))));
  const m = modal({ title: 'Новый чат', body });
}

// ---------- видеозвонок ----------
const Call = {
  room: null, local: null, screen: null, ice: [], pcs: new Map(), early: new Map(),
  el: null, grid: null, tiles: new Map(), mic: true, cam: true, hidden: false,
};

Call.join = async function (roomId) {
  if (Call.room) {
    if (Call.room === roomId) { Call.show(); return; }
    toast('Сначала завершите текущий звонок', true); return;
  }
  if (!navigator.mediaDevices || !window.RTCPeerConnection) {
    toast('Звонки работают только по HTTPS (или на localhost) в современном браузере', true); return;
  }
  if (!Talk.peer) { toast('Подключение к чату ещё не установлено, попробуйте через пару секунд', true); return; }
  let stream;
  try {
    stream = await navigator.mediaDevices.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true }, video: { width: { ideal: 1280 }, height: { ideal: 720 } } });
  } catch (_) {
    try { stream = await navigator.mediaDevices.getUserMedia({ audio: true }); toast('Камера недоступна — звонок только со звуком'); }
    catch (e) { toast('Нет доступа к микрофону: ' + (e.message || e.name), true); return; }
  }
  let r;
  try { r = await api('POST', `/api/talk/rooms/${roomId}/call/join`, { peer: Talk.peer }); }
  catch (e) { stream.getTracks().forEach(t => t.stop()); report(e); return; }
  Object.assign(Call, { room: roomId, local: stream, ice: r.ice_servers, mic: true, cam: stream.getVideoTracks().length > 0, hidden: false });
  Call.render();
  // новичок сам звонит тем, кто уже в звонке: так у пары всегда ровно один инициатор
  for (const p of r.participants) Call.connect(p, true).catch(e => console.error('connect', e));
  if (Talk.view && Talk.view.id === roomId) Talk.drawHeader();
};

Call.connect = async function (p, initiator) {
  const pc = new RTCPeerConnection({ iceServers: Call.ice });
  const entry = { pc, user: p, pending: Call.early.get(p.peer) || [] };
  Call.early.delete(p.peer);
  Call.pcs.set(p.peer, entry);
  for (const t of Call.local.getTracks()) pc.addTrack(t, Call.local);
  if (Call.screen) { // уже идёт показ экрана — отдаём его вместо камеры
    const s = pc.getSenders().find(x => x.track && x.track.kind === 'video');
    if (s) s.replaceTrack(Call.screen.getVideoTracks()[0]);
  }
  pc.onicecandidate = e => { if (e.candidate) Call.signal(p.peer, 'ice', e.candidate); };
  pc.ontrack = e => { Call.tile(p).video.srcObject = e.streams[0]; };
  pc.onconnectionstatechange = () => {
    const t = Call.tiles.get(p.peer);
    if (t) t.state.textContent = { connecting: 'соединение…', failed: 'нет связи', disconnected: 'связь прервалась' }[pc.connectionState] || '';
  };
  Call.tile(p);
  if (initiator) {
    await pc.setLocalDescription(await pc.createOffer());
    Call.signal(p.peer, 'offer', pc.localDescription);
  }
  return entry;
};

Call.signal = function (to, type, data) {
  if (!Call.room) return;
  api('POST', `/api/talk/rooms/${Call.room}/call/signal`, { peer: Talk.peer, to, type, data }).catch(e => console.warn('signal', e.message));
};

Call.onSignal = async function (d) {
  if (d.room !== Call.room) return;
  let e = Call.pcs.get(d.from);
  if (d.type === 'offer') {
    if (!e) e = await Call.connect({ peer: d.from, user_id: d.user_id, username: d.username }, false);
    await e.pc.setRemoteDescription(d.data);
    await e.pc.setLocalDescription(await e.pc.createAnswer());
    Call.signal(d.from, 'answer', e.pc.localDescription);
    await Call.flush(e);
  } else if (d.type === 'answer' && e) {
    await e.pc.setRemoteDescription(d.data);
    await Call.flush(e);
  } else if (d.type === 'ice') {
    // кандидаты могут прийти раньше offer — копим, пока нет удалённого описания
    if (e && e.pc.remoteDescription) await e.pc.addIceCandidate(d.data).catch(() => {});
    else if (e) e.pending.push(d.data);
    else { const q = Call.early.get(d.from) || []; q.push(d.data); Call.early.set(d.from, q); }
  }
};

Call.flush = async function (e) {
  const q = e.pending; e.pending = [];
  for (const c of q) await e.pc.addIceCandidate(c).catch(() => {});
};

// reconcile — сервер прислал состав звонка: убираем ушедших.
Call.reconcile = function (st) {
  const present = new Set(st.participants.map(p => p.peer));
  if (!present.has(Talk.peer)) { Call.hangup('Вы отключены от звонка'); return; }
  for (const [peer, e] of Call.pcs) if (!present.has(peer)) { e.pc.close(); Call.pcs.delete(peer); Call.dropTile(peer); }
  for (const peer of Call.tiles.keys()) if (peer !== 'me' && !present.has(peer)) Call.dropTile(peer);
  Call.drawCount(st.participants.length);
};

Call.render = function () {
  Call.grid = h('div', { class: 'call-grid' });
  Call.count = h('span', { class: 'call-count' });
  const btn = (ico, title, fn, cls = '') => h('button', { class: 'call-btn ' + cls, title, onclick: fn }, ico);
  Call.btnMic = btn('🎙️', 'Микрофон', Call.toggleMic);
  Call.btnCam = btn('📷', 'Камера', Call.toggleCam);
  Call.btnScreen = btn('🖥️', 'Показать экран', Call.toggleScreen);
  if (!Call.cam) { Call.btnCam.disabled = true; Call.btnScreen.disabled = true; Call.btnScreen.title = 'Показ экрана доступен, только если звонок начат с камерой'; }
  if (!navigator.mediaDevices.getDisplayMedia) Call.btnScreen.classList.add('hidden');
  const room = Talk.rooms.find(r => r.id === Call.room) || (Talk.view && Talk.view.room) || { name: 'Звонок' };
  Call.el = h('div', { class: 'call' },
    h('div', { class: 'call-top' }, h('span', { class: 'nm' }, '📞 ', room.name), Call.count, h('span', { style: 'flex:1' }),
      h('button', { class: 'btn', onclick: Call.hide, title: 'Свернуть: звонок продолжится' }, '⤡ Свернуть')),
    Call.grid,
    h('div', { class: 'call-bar' }, Call.btnMic, Call.btnCam, Call.btnScreen, btn('📵', 'Завершить', () => Call.hangup(), 'hang')));
  Call.pill = h('button', { class: 'call-pill hidden', onclick: Call.show }, '📞 Идёт звонок · вернуться');
  document.body.append(Call.el, Call.pill);
  const me = Call.tile({ peer: 'me', username: 'Вы' });
  me.video.srcObject = Call.local; me.video.muted = true; me.el.classList.add('me');
  Call.drawCount((Talk.calls[Call.room] || { participants: [1] }).participants.length);
};

Call.drawCount = function (n) { if (Call.count) Call.count.textContent = ` · ${n} ${plural(n, 'участник', 'участника', 'участников')}`; };

Call.tile = function (p) {
  let t = Call.tiles.get(p.peer);
  if (t) return t;
  const video = h('video', { autoplay: true, playsinline: true });
  const status = h('span', { class: 'st' });
  const el = h('div', { class: 'tile' }, video, h('div', { class: 'label' }, p.username, status));
  t = { el, video, state: status };
  Call.tiles.set(p.peer, t);
  if (p.peer === 'me') Call.grid.prepend(el); else Call.grid.append(el);
  Call.grid.dataset.n = Call.tiles.size;
  return t;
};

Call.dropTile = function (peer) {
  const t = Call.tiles.get(peer);
  if (t) { t.video.srcObject = null; t.el.remove(); Call.tiles.delete(peer); }
  if (Call.grid) Call.grid.dataset.n = Call.tiles.size;
};

Call.toggleMic = function () {
  Call.mic = !Call.mic;
  Call.local.getAudioTracks().forEach(t => { t.enabled = Call.mic; });
  Call.btnMic.classList.toggle('off', !Call.mic);
};

Call.toggleCam = function () {
  Call.cam = !Call.cam;
  Call.local.getVideoTracks().forEach(t => { t.enabled = Call.cam; });
  Call.btnCam.classList.toggle('off', !Call.cam);
};

// Показ экрана подменяет видеодорожку у всех соединений (replaceTrack) — без повторного согласования.
Call.toggleScreen = async function () {
  const cam = Call.local.getVideoTracks()[0];
  const setVideo = track => {
    for (const { pc } of Call.pcs.values()) {
      const s = pc.getSenders().find(x => x.track && x.track.kind === 'video');
      if (s) s.replaceTrack(track);
    }
    const me = Call.tiles.get('me');
    if (me) me.video.srcObject = track === cam ? Call.local : new MediaStream([track]);
  };
  if (Call.screen) {
    Call.screen.getTracks().forEach(t => t.stop());
    Call.screen = null; setVideo(cam); Call.btnScreen.classList.remove('on');
    return;
  }
  try { Call.screen = await navigator.mediaDevices.getDisplayMedia({ video: true }); }
  catch (_) { return; } // пользователь отменил выбор
  const track = Call.screen.getVideoTracks()[0];
  track.onended = () => { if (Call.screen) Call.toggleScreen(); }; // «Прекратить показ» в браузере
  setVideo(track); Call.btnScreen.classList.add('on');
};

Call.hide = function () { Call.hidden = true; Call.el.classList.add('hidden'); Call.pill.classList.remove('hidden'); };
Call.show = function () { if (!Call.el) return; Call.hidden = false; Call.el.classList.remove('hidden'); Call.pill.classList.add('hidden'); };

Call.hangup = function (reason) {
  if (!Call.room) return;
  const room = Call.room;
  for (const { pc } of Call.pcs.values()) pc.close();
  Call.pcs.clear(); Call.early.clear();
  for (const s of [Call.local, Call.screen]) if (s) s.getTracks().forEach(t => t.stop());
  for (const peer of [...Call.tiles.keys()]) Call.dropTile(peer);
  if (Call.el) Call.el.remove();
  if (Call.pill) Call.pill.remove();
  Object.assign(Call, { room: null, local: null, screen: null, el: null, grid: null, pill: null });
  api('POST', `/api/talk/rooms/${room}/call/leave`, { peer: Talk.peer }).catch(() => {});
  if (reason) toast(reason, true);
  if (Talk.view && Talk.view.id === room) Talk.drawHeader();
};

addEventListener('pagehide', () => {
  // сервер и так выведет вкладку из звонка при обрыве потока событий; это лишь ускоряет
  if (Call.room && navigator.sendBeacon) {
    navigator.sendBeacon(`/api/talk/rooms/${Call.room}/call/leave`, new Blob([JSON.stringify({ peer: Talk.peer })], { type: 'application/json' }));
  }
});
