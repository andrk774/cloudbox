// Юнит-тест клиентской криптографии E2E (web/static/e2e.js) в Node — без браузера и сервера.
//
//   node scripts/e2e_crypto_unit.mjs
//
// Проверяет кодирование ключа восстановления, формат шифротекста и обнаружение подмены.
// Полный путь в настоящем браузере — scripts/e2e_browser.mjs.
import { readFileSync } from 'node:fs';
import vm from 'node:vm';

let fails = 0;
const check = (cond, msg) => { console.log((cond ? 'OK   ' : 'FAIL ') + msg); if (!cond) fails++; };

const src = readFileSync(new URL('../web/static/e2e.js', import.meta.url), 'utf8');
const ctx = vm.createContext({ crypto, TextEncoder, TextDecoder, btoa, atob, Blob, Uint8Array, DataView, Math, JSON, Error, console });
vm.runInContext(src + '\n;globalThis.__t = { b32enc, b32dec, gcmEnc, gcmDec, kekFromPass, aesKey, segNonce, segAAD, e2eDecrypt, e2eCipherSize, E2E_MAGIC, E2E_SEG, VAULT_AAD };', ctx);
const t = ctx.__t;
const eq = (a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)) === 0;

// ключ восстановления: 1000 случайных ключей туда и обратно (раньше ломались ключи с буквой L)
let bad = 0;
for (let i = 0; i < 1000; i++) {
  const raw = crypto.getRandomValues(new Uint8Array(32));
  if (!eq(raw, t.b32dec(t.b32enc(raw)))) bad++;
}
check(bad === 0, `ключ восстановления кодируется и декодируется без потерь (ошибок: ${bad} из 1000)`);
const sample = crypto.getRandomValues(new Uint8Array(32));
const code = t.b32enc(sample);
check(/^([A-Z2-9]{4}-){12}[A-Z2-9]{4}$/.test(code), 'формат: 52 символа группами по 4, без I, O, 0, 1');
check(eq(sample, t.b32dec(code.toLowerCase().replace(/-/g, ' '))), 'ввод в нижнем регистре и с пробелами вместо дефисов');
let threw = false; try { t.b32dec('AAAA-1111'); } catch (_) { threw = true; }
check(threw, 'недопустимый символ в ключе — понятная ошибка');

// обёртка ключа хранилища кодовой фразой (как в e2eSetup / e2eUnlock)
const vk = crypto.getRandomValues(new Uint8Array(32)), salt = crypto.getRandomValues(new Uint8Array(16));
const w = await t.gcmEnc(await t.kekFromPass('кодовая фраза', salt, 1000), vk, t.VAULT_AAD);
check(eq(vk, await t.gcmDec(await t.kekFromPass('кодовая фраза', salt, 1000), w.iv, w.ct, t.VAULT_AAD)), 'ключ хранилища разворачивается верной фразой');
let wrong = false; try { await t.gcmDec(await t.kekFromPass('другая фраза', salt, 1000), w.iv, w.ct, t.VAULT_AAD); } catch (_) { wrong = true; }
check(wrong, 'неверная фраза не разворачивает ключ');

// файл: шифрование ровно как в e2eUpload, расшифровка — e2eDecrypt
for (const size of [0, 1, 65536, 65537, 200000]) {
  const text = new Uint8Array(size); // getRandomValues выдаёт не больше 64 КиБ за вызов
  for (let o = 0; o < size; o += 65536) crypto.getRandomValues(text.subarray(o, Math.min(o + 65536, size)));
  const dek = crypto.getRandomValues(new Uint8Array(32)), prefix = crypto.getRandomValues(new Uint8Array(8));
  const key = await t.aesKey(dek, ['encrypt']);
  const segs = Math.max(1, Math.ceil(size / t.E2E_SEG));
  const parts = [t.E2E_MAGIC, prefix];
  for (let i = 0; i < segs; i++) {
    parts.push(new Uint8Array(await crypto.subtle.encrypt({ name: 'AES-GCM', iv: t.segNonce(prefix, i), additionalData: t.segAAD(i === segs - 1) },
      key, text.slice(i * t.E2E_SEG, (i + 1) * t.E2E_SEG))));
  }
  const ct = new Uint8Array(await new Blob(parts).arrayBuffer());
  const entry = { size, key: btoa(String.fromCharCode(...dek)), mime: '' };
  const out = new Uint8Array(await (await t.e2eDecrypt(ct.buffer.slice(0), entry)).arrayBuffer());
  check(ct.length === t.e2eCipherSize(size) && eq(out, text), `файл ${size} байт: размер шифротекста и расшифровка`);
  if (size > 65536) {
    let caught = 0;
    const mutations = [
      c => { c[12 + 65552 + 2] ^= 1; return c; },                        // байт внутри второго сегмента
      c => c.slice(0, c.length - 40),                                    // обрезан конец
      c => { c.set(c.slice(12, 12 + Math.min(65552, c.length - 12 - 65552)), 12 + 65552); return c; }, // начало сегмента 0 на месте сегмента 1
      c => c.slice(0, 12 + 65552),                                       // отрезан последний сегмент целиком
    ];
    for (const m of mutations) { try { await t.e2eDecrypt(m(ct.slice()).buffer, entry); } catch (_) { caught++; } }
    check(caught === mutations.length, `подмена байта, обрезка, перестановка и потеря последнего сегмента обнаружены (${caught} из ${mutations.length})`);
  }
}

console.log('\nИТОГО ошибок:', fails);
process.exit(fails ? 1 : 0);
