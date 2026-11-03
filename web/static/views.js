'use strict';
/* CloudBox — разделы: корзина, ссылки, галерея, заметки, календарь, контакты, журнал, настройки, администрирование. */

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

// ---------- мои ссылки ----------
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

// ---------- галерея ----------
async function viewGallery() {
  setCrumbs([{ label: 'Галерея' }]);
  els.content.classList.add('gallery-view');
  let items = [];
  let done = false, loading = false;
  const wrap = h('div', { class: 'gallery' });
  const more = h('div', { class: 'hint', style: 'text-align:center;padding:20px' });
  els.content.replaceChildren(wrap, more);
  let lastMonth = '', grid = null;
  const monthName = d => d.toLocaleDateString('ru-RU', { month: 'long', year: 'numeric' });
  const add = batch => {
    for (const n of batch) {
      const d = new Date(n.taken_at || n.created_at);
      const mk = d.getFullYear() + '-' + d.getMonth();
      if (mk !== lastMonth) {
        lastMonth = mk;
        grid = h('div', { class: 'g-grid' });
        wrap.append(h('h3', { class: 'g-month' }, monthName(d)), grid);
      }
      const idx = items.length;
      items.push(n);
      const tile = h('div', { class: 'g-tile', onclick: () => preview(items[idx], items) });
      if (hasThumb(n)) {
        const img = h('img', { loading: 'lazy', src: `/api/files/${n.id}/thumb?s=256`, alt: n.name });
        img.onerror = () => img.replaceWith(h('div', { class: 'g-ph' }, iconFor(n)));
        tile.append(img);
      } else tile.append(h('div', { class: 'g-ph' }, iconFor(n)));
      if (isVideo(n)) tile.append(h('span', { class: 'play' }, '▶'));
      grid.append(tile);
    }
  };
  const load = async () => {
    if (done || loading) return;
    loading = true;
    more.textContent = 'Загрузка…';
    const last = items[items.length - 1];
    const before = last ? '?before=' + encodeURIComponent(last.taken_at || last.created_at) : '';
    const batch = await api('GET', '/api/gallery' + before);
    if (state.view !== 'gallery') return;
    add(batch);
    done = batch.length < 300;
    more.textContent = items.length ? (done ? `Всего: ${items.length}` : '') : 'Фото и видео пока нет. Загрузите их в любую папку — здесь они соберутся по дате съёмки.';
    loading = false;
  };
  await load();
  const io = new IntersectionObserver(es => { if (es[0].isIntersecting) load().catch(report); });
  io.observe(more);
}

// ---------- заметки ----------
async function viewNotes(openId) {
  setCrumbs([{ label: 'Заметки' }]);
  els.content.classList.add('notes-view');
  const d = await api('GET', '/api/notes');
  const items = d.items.sort((a, b) => new Date(b.updated_at) - new Date(a.updated_at));
  const list = h('div', { class: 'notes-list' });
  const pane = h('div', { class: 'note-pane' });
  let current = null, dirty = false, version = null;
  const ta = h('textarea', { class: 'editor', placeholder: 'Пишите в Markdown…' });
  const prev = h('div', { class: 'md-preview hidden' });
  const status = h('span', { class: 'sub' });
  const save = async () => {
    if (!current || !dirty) return;
    try {
      const nn = await api('PUT', `/api/files/${current.id}/content`, new Blob([ta.value], { type: 'text/markdown' }), { headers: { 'If-Match': version || '' } });
      version = nn.version_id; current.version_id = nn.version_id; current.updated_at = nn.updated_at; dirty = false;
      status.textContent = 'сохранено'; drawList2();
    } catch (e) { if (e.status === 412) toast('Заметку изменили на другом устройстве — откройте её заново', true); else report(e); }
  };
  let saveTimer;
  ta.addEventListener('input', () => { dirty = true; status.textContent = '…'; clearTimeout(saveTimer); saveTimer = setTimeout(save, 1200); });
  ta.addEventListener('keydown', e => { if ((e.ctrlKey || e.metaKey) && e.key === 's') { e.preventDefault(); save(); } });
  const toggle = h('button', { class: 'btn', onclick: () => {
    const editing = prev.classList.toggle('hidden');
    ta.classList.toggle('hidden', !editing);
    if (!editing) prev.innerHTML = renderMarkdown(ta.value);
    toggle.textContent = editing ? '👁 Просмотр' : '✏️ Правка';
  } }, '👁 Просмотр');
  const titleOf = n => n.name.replace(/\.(md|markdown|txt)$/i, '');
  const open = async n => {
    await save();
    current = n; version = n.version_id; dirty = false;
    const res = await fetch(`/api/files/${n.id}/content`, { credentials: 'same-origin' });
    ta.value = await res.text();
    prev.innerHTML = renderMarkdown(ta.value);
    status.textContent = 'изменена ' + fmtDate(n.updated_at);
    pane.replaceChildren(h('div', { class: 'editor-bar' },
      h('b', { class: 'note-title' }, titleOf(n)), status, h('span', { style: 'flex:1' }), toggle,
      h('button', { class: 'icon-btn', title: 'Ещё', onclick: e => {
        const r = e.currentTarget.getBoundingClientRect();
        contextMenu(r.left - 160, r.bottom + 4, [
          { icon: '✏️', label: 'Переименовать', onClick: async () => {
            const name = await askText('Название заметки', 'Название', titleOf(n), 'Сохранить');
            if (!name) return;
            try { await api('PATCH', '/api/files/' + n.id, { name: name + '.md' }); viewNotes(n.id); } catch (ex) { report(ex); }
          } },
          { icon: '🔗', label: 'Поделиться', onClick: () => shareDialog(n) },
          { icon: '🕓', label: 'Версии', onClick: () => detailsPanel(n, 'versions') },
          '-', { icon: '🗑️', label: 'Удалить', danger: true, onClick: async () => {
            if (!await confirmDialog('Удалить заметку?', `«${titleOf(n)}» будет перемещена в корзину.`)) return;
            await api('DELETE', '/api/files/' + n.id); viewNotes();
          } },
        ]);
      } }, '⋯')), ta, prev);
    drawList2();
    history.replaceState(null, '', '#/notes/' + n.id);
  };
  const drawList2 = () => list.replaceChildren(
    h('button', { class: 'btn primary', style: 'width:100%;justify-content:center;margin-bottom:10px', onclick: async () => {
      const name = await askText('Новая заметка', 'Название', 'Новая заметка', 'Создать');
      if (!name) return;
      const q = new URLSearchParams({ name: name + '.md', parent: d.folder_id, conflict: 'rename' });
      const n = await api('POST', '/api/files/upload?' + q, new Blob(['# ' + name + '\n\n'], { type: 'text/markdown' }));
      items.unshift(n); open(n);
    } }, '＋ Новая заметка'),
    ...items.map(n => h('div', { class: 'note-item' + (current && current.id === n.id ? ' active' : ''), onclick: () => open(n) },
      h('div', { class: 'nm' }, titleOf(n)), h('div', { class: 'sub' }, fmtDate(n.updated_at)))));
  drawList2();
  pane.append(h('div', { class: 'empty' }, h('div', { class: 'big' }, '🗒️'),
    items.length ? 'Выберите заметку слева' : 'Заметок пока нет. Они хранятся как Markdown-файлы в папке «Заметки» и синхронизируются вместе с остальными файлами.'));
  els.content.replaceChildren(h('div', { class: 'notes' }, list, pane));
  const target = items.find(n => n.id === openId);
  if (target) open(target);
}

// ---------- календарь ----------
let calMonth = null;
async function viewCalendar() {
  setCrumbs([{ label: 'Календарь' }]);
  if (!calMonth) { const n = new Date(); calMonth = new Date(n.getFullYear(), n.getMonth(), 1); }
  const first = new Date(calMonth);
  const startDow = (first.getDay() + 6) % 7; // понедельник — первый
  const gridStart = new Date(first); gridStart.setDate(1 - startDow);
  const gridEnd = new Date(gridStart); gridEnd.setDate(gridStart.getDate() + 42);
  const [events, cols, info] = await Promise.all([
    api('GET', `/api/events?from=${gridStart.toISOString()}&to=${gridEnd.toISOString()}`),
    api('GET', '/api/calendars'), api('GET', '/api/pim'),
  ]);
  if (state.view !== 'calendar') return;
  const byDay = {};
  for (const ev of events) {
    const s = new Date(ev.start), e = new Date(ev.end);
    for (let d = new Date(s.getFullYear(), s.getMonth(), s.getDate()); d < e || d.toDateString() === s.toDateString(); d.setDate(d.getDate() + 1)) {
      (byDay[d.toDateString()] = byDay[d.toDateString()] || []).push(ev);
      if (!ev.all_day) break;
    }
  }
  const today = new Date().toDateString();
  const cells = [];
  for (let i = 0; i < 42; i++) {
    const d = new Date(gridStart); d.setDate(gridStart.getDate() + i);
    const evs = byDay[d.toDateString()] || [];
    cells.push(h('div', { class: 'cal-cell' + (d.getMonth() !== first.getMonth() ? ' other' : '') + (d.toDateString() === today ? ' today' : ''),
      ondblclick: () => eventDialog(cols.calendars, d) },
      h('div', { class: 'cal-day' }, d.getDate()),
      evs.slice(0, 4).map(ev => h('div', { class: 'cal-ev', style: `border-left-color:${ev.color}`, title: ev.summary, onclick: () => eventView(ev) },
        ev.all_day ? '' : new Date(ev.start).toLocaleTimeString('ru-RU', { hour: '2-digit', minute: '2-digit' }) + ' ', ev.summary, ev.recurring ? ' ↻' : '')),
      evs.length > 4 ? h('div', { class: 'sub' }, '+ ещё ' + (evs.length - 4)) : null));
  }
  const shift = m => { calMonth = new Date(calMonth.getFullYear(), calMonth.getMonth() + m, 1); viewCalendar().catch(report); };
  els.content.replaceChildren(
    h('div', { class: 'cal-head' },
      h('button', { class: 'btn', onclick: () => shift(-1) }, '‹'),
      h('h2', null, first.toLocaleDateString('ru-RU', { month: 'long', year: 'numeric' })),
      h('button', { class: 'btn', onclick: () => shift(1) }, '›'),
      h('button', { class: 'btn ghost', onclick: () => { calMonth = null; viewCalendar(); } }, 'Сегодня'),
      h('span', { style: 'flex:1' }),
      h('button', { class: 'btn', onclick: () => davInfoDialog(info, cols) }, '📱 Подключить телефон'),
      h('button', { class: 'btn primary', onclick: () => eventDialog(cols.calendars, new Date()) }, '＋ Событие')),
    h('div', { class: 'cal-grid' }, ['Пн', 'Вт', 'Ср', 'Чт', 'Пт', 'Сб', 'Вс'].map(d => h('div', { class: 'cal-dow' }, d)), cells),
    h('div', { class: 'hint', style: 'margin-top:10px' }, 'Двойной щелчок по дню — новое событие. Календари синхронизируются по CalDAV с телефоном и Thunderbird.'));
}

function eventView(ev) {
  const fmt = d => new Date(d).toLocaleString('ru-RU', { day: 'numeric', month: 'long', hour: ev.all_day ? undefined : '2-digit', minute: ev.all_day ? undefined : '2-digit' });
  modal({
    title: ev.summary || 'Событие',
    body: [h('p', null, '🗓 ', fmt(ev.start), ' — ', fmt(ev.all_day ? new Date(new Date(ev.end) - 1) : ev.end), ev.recurring ? ' (повторяется)' : ''),
      ev.location ? h('p', null, '📍 ', ev.location) : null, ev.description ? h('p', { style: 'white-space:pre-wrap' }, ev.description) : null,
      h('p', { class: 'sub' }, 'Календарь: ' + ev.calendar)],
    buttons: [{ label: ev.recurring ? 'Удалить всю серию' : 'Удалить', danger: true, onClick: async () => {
      if (!await confirmDialog('Удалить событие?', ev.summary)) return false;
      await api('DELETE', '/api/events/' + ev.id); viewCalendar();
    } }, { label: 'Закрыть', primary: true }],
  });
}

function eventDialog(calendars, day) {
  const pad = n => String(n).padStart(2, '0');
  const dstr = d => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const summary = h('input', { type: 'text', placeholder: 'Название' });
  const allDay = h('input', { type: 'checkbox' });
  const date = h('input', { type: 'date', value: dstr(day) });
  const t1 = h('input', { type: 'time', value: '10:00' });
  const t2 = h('input', { type: 'time', value: '11:00' });
  const date2 = h('input', { type: 'date', value: dstr(day) });
  const loc = h('input', { type: 'text', placeholder: 'Место' });
  const desc = h('textarea', { rows: 3, placeholder: 'Описание' });
  const rr = h('select', null, h('option', { value: '' }, 'не повторять'), h('option', { value: 'FREQ=DAILY' }, 'каждый день'),
    h('option', { value: 'FREQ=WEEKLY' }, 'каждую неделю'), h('option', { value: 'FREQ=MONTHLY' }, 'каждый месяц'), h('option', { value: 'FREQ=YEARLY' }, 'каждый год'));
  const cal = h('select', null, calendars.map(c => h('option', { value: c.id }, c.name)));
  const times = h('div', { class: 'row' }, h('div', null, h('label', null, 'Начало'), t1), h('div', null, h('label', null, 'Конец'), t2));
  allDay.addEventListener('change', () => times.classList.toggle('hidden', allDay.checked));
  modal({
    title: 'Новое событие',
    body: [summary,
      h('div', { class: 'row' }, h('div', null, h('label', null, 'Дата'), date), h('div', null, h('label', null, 'По (включительно)'), date2)),
      h('label', { class: 'check', style: 'margin-top:10px' }, allDay, 'Весь день'), times,
      h('div', { class: 'row' }, h('div', null, h('label', null, 'Повтор'), rr), h('div', null, h('label', null, 'Календарь'), cal)),
      h('label', null, 'Место'), loc, h('label', null, 'Описание'), desc],
    buttons: [{ label: 'Отмена' }, { label: 'Создать', primary: true, onClick: async () => {
      let start, end;
      if (allDay.checked) {
        start = new Date(date.value + 'T00:00:00Z');
        end = new Date(date2.value + 'T00:00:00Z'); end.setUTCDate(end.getUTCDate() + 1);
      } else {
        start = new Date(date.value + 'T' + t1.value); end = new Date(date2.value + 'T' + t2.value);
      }
      await api('POST', '/api/events', { calendar_id: +cal.value, summary: summary.value, location: loc.value, description: desc.value,
        start: start.toISOString(), end: end.toISOString(), all_day: allDay.checked, rrule: rr.value });
      toast('Событие создано'); viewCalendar();
    } }],
  });
}

function davInfoDialog(info, cols) {
  const row = (label, v) => h('div', null, h('label', null, label), h('div', { class: 'linkbox' }, h('input', { type: 'text', readonly: true, value: v }), h('button', { class: 'btn', onclick: () => copyText(v) }, 'Копировать')));
  modal({
    title: 'Синхронизация календарей и контактов', wide: true,
    body: [
      h('p', null, h('b', null, 'Android (DAVx⁵): '), 'добавьте аккаунт «Вход с URL и именем пользователя», адрес сервера — ', h('code', { class: 'inline' }, info.server),
        '. DAVx⁵ сам найдёт календари и контакты.'),
      h('p', null, h('b', null, 'iPhone / macOS: '), 'Настройки → Календарь → Учётные записи → Другое → CalDAV; для контактов — CardDAV. Сервер: адрес ниже.'),
      h('p', null, h('b', null, 'Thunderbird: '), 'Новый календарь → В сети → адрес CalDAV.'),
      row('CalDAV', info.caldav), row('CardDAV', info.carddav),
      h('p', { class: 'hint' }, 'Логин — ваше имя пользователя, пароль — пароль приложения из «Настроек» (обязательно, если включена 2FA).'),
      h('h4', { class: 'sect' }, 'Календари'), ...cols.calendars.map(c => row(c.name, c.url)),
      h('h4', { class: 'sect' }, 'Адресные книги'), ...cols.addressbooks.map(c => row(c.name, c.url)),
      h('div', { class: 'row-inline', style: 'margin-top:12px' }, h('button', { class: 'btn', onclick: async () => {
        const name = await askText('Новый календарь', 'Название', 'Работа', 'Создать');
        if (name) { await api('POST', '/api/calendars', { kind: 'cal', name }); toast('Календарь создан'); }
      } }, '＋ Календарь'), h('button', { class: 'btn', onclick: async () => {
        const name = await askText('Новая адресная книга', 'Название', 'Работа', 'Создать');
        if (name) { await api('POST', '/api/calendars', { kind: 'card', name }); toast('Адресная книга создана'); }
      } }, '＋ Адресная книга')),
    ],
  });
}

// ---------- контакты ----------
async function viewContacts() {
  setCrumbs([{ label: 'Контакты' }]);
  const [list, info, cols] = await Promise.all([api('GET', '/api/contacts'), api('GET', '/api/pim'), api('GET', '/api/calendars')]);
  const q = h('input', { type: 'search', placeholder: 'Найти контакт…' });
  const body = h('div', { class: 'contacts' });
  const draw = () => {
    const s = q.value.trim().toLowerCase();
    const shown = list.filter(c => !s || [c.name, c.org, ...c.emails, ...c.phones].join(' ').toLowerCase().includes(s));
    body.replaceChildren(...(shown.length ? shown.map(c => h('div', { class: 'contact' },
      h('div', { class: 'avatar' }, (c.name || '?').slice(0, 1).toUpperCase()),
      h('div', { class: 'grow' }, h('div', { class: 'nm' }, c.name), c.org ? h('div', { class: 'sub' }, c.org) : null,
        h('div', { class: 'sub' }, [...c.phones.map(p => h('a', { href: 'tel:' + p }, p)), ...c.emails.map(e => h('a', { href: 'mailto:' + e }, e))].flatMap((x, i) => i ? [' · ', x] : [x]))),
      h('button', { class: 'icon-btn', title: 'Удалить', onclick: async () => {
        if (!await confirmDialog('Удалить контакт?', c.name)) return;
        await api('DELETE', '/api/contacts/' + c.id); viewContacts();
      } }, '🗑️'))) : [h('div', { class: 'empty' }, h('div', { class: 'big' }, '📇'), list.length ? 'Ничего не найдено.' : 'Контактов пока нет. Добавьте вручную или подключите телефон по CardDAV.')]));
  };
  q.addEventListener('input', draw);
  draw();
  els.content.replaceChildren(h('div', { class: 'cal-head' }, q, h('span', { style: 'flex:1' }),
    h('button', { class: 'btn', onclick: () => davInfoDialog(info, cols) }, '📱 Подключить телефон'),
    h('button', { class: 'btn primary', onclick: () => {
      const name = h('input', { type: 'text', placeholder: 'Имя и фамилия' });
      const org = h('input', { type: 'text', placeholder: 'Организация' });
      const tel = h('input', { type: 'text', placeholder: '+7 …' });
      const email = h('input', { type: 'text', placeholder: 'email@example.ru' });
      const note = h('textarea', { rows: 2, placeholder: 'Заметка' });
      modal({ title: 'Новый контакт', body: [name, h('label', null, 'Телефон'), tel, h('label', null, 'Email'), email, h('label', null, 'Организация'), org, h('label', null, 'Заметка'), note],
        buttons: [{ label: 'Отмена' }, { label: 'Сохранить', primary: true, onClick: async () => {
          await api('POST', '/api/contacts', { name: name.value, org: org.value, phones: [tel.value], emails: [email.value], note: note.value });
          viewContacts();
        } }] });
    } }, '＋ Контакт')), body);
}

// ---------- журнал ----------
async function viewActivity() {
  setCrumbs([{ label: 'Журнал действий' }]);
  const list = await api('GET', '/api/activity?limit=300');
  const groups = {};
  for (const a of list) { const k = new Date(a.at).toLocaleDateString('ru-RU', { day: 'numeric', month: 'long', year: 'numeric' }); (groups[k] = groups[k] || []).push(a); }
  els.content.replaceChildren(list.length
    ? h('div', { class: 'card', style: 'max-width:900px' }, Object.entries(groups).map(([day, items]) => [h('h4', { class: 'sect' }, day), items.map(activityRow)]))
    : h('div', { class: 'empty' }, h('div', { class: 'big' }, '📜'), 'Событий пока нет.'));
}

// ---------- настройки ----------
async function viewSettings() {
  setCrumbs([{ label: 'Настройки' }]);
  await refreshUsage();
  const u = state.usage;
  const me = u.user;
  const sec = (title, ...kids) => h('div', { class: 'card' }, h('h2', null, title), ...kids);
  const apList = h('div'), sesList = h('div'), loginList = h('div'), tfa = h('div');

  const loadAP = async () => {
    const list = await api('GET', '/api/me/app-passwords');
    apList.replaceChildren(...(list.length ? list.map(ap => h('div', { class: 'list-row' },
      h('div', { class: 'grow' }, ap.name, h('div', { class: 'sub' }, 'создан ' + fmtDate(ap.created_at) + (ap.last_used_at ? ' · использовался ' + fmtDate(ap.last_used_at) : ' · не использовался'))),
      h('button', { class: 'btn danger', onclick: async () => { try { await api('DELETE', '/api/me/app-passwords/' + ap.id); loadAP(); } catch (e) { report(e); } } }, 'Отозвать')))
      : [h('div', { class: 'hint' }, 'Паролей приложений нет.')]));
  };
  const loadSessions = async () => {
    const list = await api('GET', '/api/me/sessions');
    sesList.replaceChildren(...list.map(s => h('div', { class: 'list-row' },
      h('div', { class: 'grow' }, uaName(s.user_agent), s.current ? h('span', { class: 'badge cur' }, 'это устройство') : null,
        h('div', { class: 'sub' }, `${s.ip || '—'} · вход ${fmtDate(s.created_at)} (${s.method}) · активность ${fmtDate(s.last_seen)}`)),
      s.current ? null : h('button', { class: 'btn danger', onclick: async () => { await api('DELETE', '/api/me/sessions/' + s.id); loadSessions(); } }, 'Завершить'))),
      list.length > 1 ? h('button', { class: 'btn', style: 'margin-top:10px', onclick: async () => { await api('DELETE', '/api/me/sessions'); toast('Остальные сеансы завершены'); loadSessions(); } }, 'Завершить все остальные') : null);
  };
  const loadLogins = async () => {
    const list = await api('GET', '/api/me/logins');
    loginList.replaceChildren(...(list.length ? list.slice(0, 15).map(e => h('div', { class: 'list-row' },
      h('span', null, e.success ? '✅' : '⛔'),
      h('div', { class: 'grow' }, `${e.method} · ${e.ip || '—'}`, h('div', { class: 'sub' }, uaName(e.user_agent))),
      h('span', { class: 'sub' }, fmtDate(e.at)))) : [h('div', { class: 'hint' }, 'Записей нет.')]));
  };
  const drawTFA = () => {
    if (me.totp_enabled) {
      tfa.replaceChildren(h('p', null, '✅ Двухфакторная аутентификация включена. Осталось кодов восстановления: ', h('b', null, u.recovery_left)),
        h('p', { class: 'hint' }, 'Для WebDAV, CalDAV и клиента синхронизации используйте пароли приложений — основной пароль там больше не работает.'),
        h('button', { class: 'btn danger', onclick: async () => {
          const code = await askText('Отключить 2FA', 'Код из приложения или код восстановления', '', 'Отключить');
          if (!code) return;
          try { await api('POST', '/api/me/totp/disable', { code }); toast('2FA отключена'); viewSettings(); } catch (e) { report(e); }
        } }, 'Отключить'));
      return;
    }
    tfa.replaceChildren(h('p', { class: 'hint' }, 'Вход будет требовать код из приложения (Google Authenticator, Яндекс Ключ, Aegis, 2FAS, Bitwarden).'),
      h('button', { class: 'btn primary', onclick: async () => {
        try {
          const s = await api('POST', '/api/me/totp/setup');
          const code = h('input', { type: 'text', inputmode: 'numeric', placeholder: '123456', autocomplete: 'one-time-code' });
          modal({ title: 'Включение 2FA', body: [
            h('p', null, '1. Отсканируйте QR-код приложением-аутентификатором:'), qrSVG(s.uri, 5),
            h('p', { class: 'hint', style: 'text-align:center' }, 'или введите ключ вручную: ', h('code', { class: 'inline' }, s.secret.replace(/(.{4})/g, '$1 ').trim())),
            h('p', null, '2. Введите код из приложения:'), code],
          buttons: [{ label: 'Отмена' }, { label: 'Включить', primary: true, onClick: async () => {
            const r = await api('POST', '/api/me/totp/enable', { code: code.value });
            modal({ title: 'Коды восстановления', body: [
              h('p', null, 'Сохраните эти коды в надёжном месте. Каждый можно использовать один раз, если потеряете телефон:'),
              h('div', { class: 'codes' }, r.recovery_codes.map(c => h('code', null, c)))],
              buttons: [{ label: 'Скопировать', onClick: () => { copyText(r.recovery_codes.join('\n')); return false; } }, { label: 'Я сохранил(а)', primary: true }],
              onClose: () => viewSettings() });
          } }] });
        } catch (e) { report(e); }
      } }, 'Включить 2FA'));
  };

  const oldPw = h('input', { type: 'password', autocomplete: 'current-password' });
  const newPw = h('input', { type: 'password', autocomplete: 'new-password' });
  const apName = h('input', { type: 'text', placeholder: 'например: Ноутбук, Телефон, rclone, cloudbox-sync' });
  const davUrl = u.webdav_url;
  const pimInfo = await api('GET', '/api/pim');
  els.content.replaceChildren(
    sec('🔐 Двухфакторная аутентификация', tfa),
    sec('🔑 Пароли приложений',
      h('div', { class: 'hint' }, 'Для WebDAV, CalDAV/CardDAV и клиента синхронизации. Их можно отозвать, не меняя основной пароль.'),
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
    sec('💻 Сеансы', sesList),
    sec('📋 Журнал входов', loginList),
    sec('🗂 Подключение как сетевой диск (WebDAV)',
      h('div', { class: 'linkbox' }, h('input', { type: 'text', readonly: true, value: davUrl }), h('button', { class: 'btn', onclick: () => copyText(davUrl) }, 'Копировать')),
      h('ul', { class: 'hint', style: 'padding-left:18px;line-height:1.8' },
        h('li', null, 'Windows: Проводник → «Этот компьютер» → «Подключить сетевой диск» → вставьте адрес. Нужен HTTPS.'),
        h('li', null, 'Linux (GNOME/KDE): адрес ', h('code', { class: 'inline' }, davUrl.replace(/^http/, 'dav')), ' в файловом менеджере.'),
        h('li', null, 'macOS: Finder → «Подключение к серверу» (⌘K).'),
        h('li', null, 'Android: Solid Explorer, FolderSync, Total Commander (WebDAV-плагин).'),
        h('li', null, 'Чужие общие папки видны в каталоге «Доступно мне».'))),
    sec('📱 Автозагрузка фото с телефона',
      h('p', { class: 'hint' }, 'Вариант 1 — установите CloudBox как приложение (меню браузера → «Установить приложение» / «На экран Домой»). ',
        'После этого в Android в меню «Поделиться» появится CloudBox: выбранные фото загрузятся в папку «Загрузки с телефона».'),
      h('p', { class: 'hint' }, 'Вариант 2 — фоновая автозагрузка: FolderSync или PhotoSync (Android/iOS) → аккаунт WebDAV с адресом выше и паролем приложения → ',
        'синхронизация папки DCIM/Camera с папкой «Фото/Камера» в режиме «Только загрузка», расписание — по Wi-Fi.')),
    sec('🔄 Синхронизация папки на компьютере',
      h('p', { class: 'hint' }, 'Клиент cloudbox-sync (Windows, Linux, macOS) держит локальную папку в актуальном состоянии в обе стороны:'),
      h('pre', { class: 'code' }, `cloudbox-sync -server ${location.origin} -user ${state.me.username} \\\n  -local ~/CloudBox -remote / -watch 30s`),
      h('p', { class: 'hint' }, 'Пароль приложения передайте через переменную CLOUDBOX_PASSWORD.')),
    sec('📅 Календари и контакты', h('p', { class: 'hint' }, 'CalDAV: ', h('code', { class: 'inline' }, pimInfo.caldav), ' · CardDAV: ', h('code', { class: 'inline' }, pimInfo.carddav)),
      h('p', { class: 'hint' }, 'Для Android подойдёт DAVx⁵: достаточно указать адрес сервера ', h('code', { class: 'inline' }, pimInfo.server), '.')),
    me.oidc ? null : sec('🔒 Смена пароля',
      h('div', { class: 'row' }, h('div', null, h('label', null, 'Текущий пароль'), oldPw), h('div', null, h('label', null, 'Новый пароль'), newPw),
        h('button', { class: 'btn primary', onclick: async () => {
          try { await api('POST', '/api/me/password', { old_password: oldPw.value, new_password: newPw.value }); oldPw.value = newPw.value = ''; toast('Пароль изменён, другие сеансы завершены'); }
          catch (e) { report(e); }
        } }, 'Сменить'))),
    sec('💾 Хранилище',
      h('div', null, 'Занято: ', fmtSize(u.used_bytes), u.quota_bytes > 0 ? ' из ' + fmtSize(u.quota_bytes) : ' (без ограничения)'),
      h('div', { class: 'hint' }, 'Учитываются все версии файлов и корзина. Максимальный размер файла: ' + fmtSize(u.max_upload_bytes) +
        (u.encryption ? '. Файлы на сервере хранятся в зашифрованном виде (AES-256-GCM).' : '.'))));
  drawTFA();
  loadAP().catch(report); loadSessions().catch(report); loadLogins().catch(report);
}

function uaName(ua) {
  if (!ua) return 'Неизвестное устройство';
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS' : /Mac OS/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
  const br = /Edg\//.test(ua) ? 'Edge' : /YaBrowser/.test(ua) ? 'Яндекс Браузер' : /Firefox/.test(ua) ? 'Firefox' : /Chrome/.test(ua) ? 'Chrome' : /Safari/.test(ua) ? 'Safari' :
    /Go-http-client/.test(ua) ? 'cloudbox-sync / API' : ua.split(' ')[0];
  return [br, os].filter(Boolean).join(', ');
}

// ---------- администрирование ----------
async function viewAdmin() {
  setCrumbs([{ label: 'Администрирование' }]);
  const [users, groups] = await Promise.all([api('GET', '/api/admin/users'), api('GET', '/api/admin/groups')]);
  const name = h('input', { type: 'text', autocomplete: 'off' });
  const pw = h('input', { type: 'text', autocomplete: 'off' });
  const quota = h('input', { type: 'number', min: '0', placeholder: 'по умолчанию' });
  const adm = h('input', { type: 'checkbox' });
  const gb = b => b > 0 ? (b / 1073741824).toFixed(b % 1073741824 ? 1 : 0) + ' ГБ' : '∞';
  const gname = h('input', { type: 'text', placeholder: 'Название группы' });
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
    h('table', { class: 'files', style: 'max-width:900px;margin-bottom:16px' },
      h('thead', null, h('tr', null, h('th', null, 'Пользователь'), h('th', { class: 'c-size' }, 'Занято'), h('th', { class: 'c-size' }, 'Квота'), h('th', { class: 'c-date' }, 'Создан'), h('th', { class: 'c-act' }))),
      h('tbody', null, users.map(u => h('tr', null,
        h('td', null, u.username, u.is_admin ? h('span', { class: 'badge' }, 'админ') : null, u.totp_enabled ? h('span', { class: 'badge' }, '2FA') : null,
          u.oidc ? h('span', { class: 'badge' }, 'SSO') : null, u.id === state.me.id ? h('span', { class: 'badge' }, 'вы') : null),
        h('td', { class: 'c-size' }, fmtSize(u.used_bytes)),
        h('td', { class: 'c-size' }, gb(u.quota_bytes)),
        h('td', { class: 'c-date' }, fmtDate(u.created_at)),
        h('td', { class: 'c-act' }, h('button', { class: 'icon-btn', onclick: e => {
          const r = e.currentTarget.getBoundingClientRect();
          contextMenu(r.left - 190, r.bottom + 4, [
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
            u.totp_enabled ? { icon: '📵', label: 'Сбросить 2FA', onClick: async () => {
              if (!await confirmDialog('Сбросить 2FA?', `Пользователь ${u.username} сможет войти только по паролю и заново включить 2FA.`, 'Сбросить')) return;
              try { await api('PATCH', '/api/admin/users/' + u.id, { reset_2fa: true }); viewAdmin(); } catch (ex) { report(ex); }
            } } : null,
            { icon: '👑', label: u.is_admin ? 'Снять права админа' : 'Сделать админом', onClick: async () => {
              try { await api('PATCH', '/api/admin/users/' + u.id, { is_admin: !u.is_admin }); viewAdmin(); } catch (ex) { report(ex); }
            } },
            '-',
            { icon: '🗑️', label: 'Удалить пользователя', danger: true, onClick: async () => {
              if (!await confirmDialog('Удалить ' + u.username + '?', 'Все файлы пользователя будут удалены безвозвратно.')) return;
              try { await api('DELETE', '/api/admin/users/' + u.id); viewAdmin(); } catch (ex) { report(ex); }
            } },
          ]);
        } }, '⋯')))))),
    h('div', { class: 'card', style: 'max-width:900px' }, h('h2', null, 'Группы'),
      h('div', { class: 'hint' }, 'Группам можно давать доступ к папкам так же, как пользователям.'),
      ...groups.map(g => h('div', { class: 'list-row' },
        h('span', null, '👥'), h('div', { class: 'grow' }, h('b', null, g.name), h('div', { class: 'sub' }, g.members.length ? g.members.map(m => m.username).join(', ') : 'нет участников')),
        h('button', { class: 'btn', onclick: () => {
          const boxes = users.map(u => h('label', { class: 'check' }, h('input', { type: 'checkbox', value: u.id, checked: g.members.some(m => m.id === u.id) }), u.username));
          modal({ title: 'Участники «' + g.name + '»', body: boxes, buttons: [{ label: 'Отмена' }, { label: 'Сохранить', primary: true, onClick: async () => {
            await api('PUT', `/api/admin/groups/${g.id}/members`, { user_ids: boxes.map(b => b.firstChild).filter(i => i.checked).map(i => +i.value) });
            viewAdmin();
          } }] });
        } }, 'Участники'),
        h('button', { class: 'btn danger', onclick: async () => {
          if (!await confirmDialog('Удалить группу?', `Доступ, выданный группе «${g.name}», будет закрыт.`)) return;
          await api('DELETE', '/api/admin/groups/' + g.id); viewAdmin();
        } }, 'Удалить'))),
      h('div', { class: 'row-inline', style: 'margin-top:10px' }, gname, h('button', { class: 'btn primary', onclick: async () => {
        try { await api('POST', '/api/admin/groups', { name: gname.value }); viewAdmin(); } catch (e) { report(e); }
      } }, 'Создать группу'))));
}
