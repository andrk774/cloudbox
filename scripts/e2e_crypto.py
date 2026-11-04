"""Сквозной тест серверной стороны E2E-папок: сервер хранит только шифротекст и сам не даёт его раскрыть.

    CB_URL=http://127.0.0.1:8080 python3 scripts/e2e_crypto.py

Само шифрование (WebCrypto в браузере) проверяет scripts/e2e_browser.mjs. Вход — Basic-auth
(лимит входов по паролю в CI расходуют другие тесты).
"""
import base64, json, os, secrets, sys, urllib.error, urllib.parse, urllib.request

B = os.environ.get("CB_URL", "http://127.0.0.1:8080")
fails = 0


def check(cond, msg):
    global fails
    print(("OK   " if cond else "FAIL ") + msg)
    if not cond:
        fails += 1


class User:
    def __init__(self, name, pw):
        self.name, self.pw = name, pw
        self.auth = "Basic " + base64.b64encode(f"{name}:{pw}".encode()).decode()

    def req(self, method, path, body=None, raw=None, headers=None):
        h = {"Authorization": self.auth, **(headers or {})}
        data = None
        if body is not None:
            data, h["Content-Type"] = json.dumps(body).encode(), "application/json"
        if raw is not None:
            data = raw
        r = urllib.request.Request(B + urllib.parse.quote(path, safe="/?=&%:+-_.~"), data=data, method=method, headers=h)
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                return resp.status, resp.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def j(self, method, path, body=None, **kw):
        st, b = self.req(method, path, body, **kw)
        try:
            return st, json.loads(b)
        except Exception:
            return st, b


def obf():
    return secrets.token_hex(16)


def fake_cipher(n=64):
    return b"CBE1" + secrets.token_bytes(8) + secrets.token_bytes(n)


admin = User("andrey", "secret123")
if admin.j("GET", "/api/me")[0] == 401:
    admin.j("POST", "/api/auth/register", {"username": "andrey", "password": "secret123"})
ana, eve = User("crypto_ana", "ana-12345"), User("crypto_eve", "eve-12345")
for u in (ana, eve):
    admin.j("POST", "/api/admin/users", {"username": u.name, "password": u.pw})
ids = {x["username"]: x["id"] for x in admin.j("GET", "/api/admin/users")[1]}

# ---------- ключ ----------
st, k = ana.j("GET", "/api/e2e/keys")
check(st == 200 and k["keys"] is None, "ключа ещё нет")
st, e = ana.j("POST", "/api/e2e/folders", {"parent": "", "name": "Личное"})
check(st == 400 and "ключ" in e.get("error", ""), "без ключа E2E-папку не создать")
wrapped = {"v": 1, "pass": {"kdf": "PBKDF2-SHA256", "iter": 600000, "salt": "AA==", "iv": "AA==", "ct": "AA=="}, "rec": {"iv": "AA==", "ct": "AA=="}}
st, r = ana.j("PUT", "/api/e2e/keys", {"data": wrapped, "version": 0})
check(st == 200 and r["version"] == 1, "обёрнутый ключ сохранён")
st, _ = ana.j("PUT", "/api/e2e/keys", {"data": wrapped, "version": 0})
check(st == 400, "второй ключ поверх первого не создаётся")
st, _ = ana.j("PUT", "/api/e2e/keys", {"data": {**wrapped, "v": 2}, "version": 7})
check(st == 412, "замена ключа со старой версией отклонена")
st, r = ana.j("PUT", "/api/e2e/keys", {"data": wrapped, "version": 1})
check(st == 200 and r["version"] == 2, "смена кодовой фразы (замена с верной версией)")
st, k = ana.j("GET", "/api/e2e/keys")
check(st == 200 and k["keys"]["data"]["pass"]["iter"] == 600000, "ключ возвращается владельцу как есть")
check(eve.j("GET", "/api/e2e/keys")[1]["keys"] is None, "чужой ключ не виден")

# ---------- папка ----------
st, vault = ana.j("POST", "/api/e2e/folders", {"parent": "", "name": "Личное"})
check(st == 201 and vault["e2e"], "E2E-папка создана")
vid = vault["id"]
st, _ = ana.j("POST", "/api/e2e/folders", {"parent": vid, "name": "Вложенная"})
check(st == 400, "E2E-папка внутри E2E-папки создаётся обычной вложенной")

up = lambda u, parent, name, data: u.j("POST", "/api/files/upload?" + urllib.parse.urlencode({"parent": parent, "name": name}), raw=data)
st, e = up(ana, vid, "секрет.txt", "пароль от сейфа: 1234".encode())
check(st == 400 and "зашифрованный в браузере" in e.get("error", ""), "открытый текст с настоящим именем не принимается")
st, _ = up(ana, vid, obf(), "пароль от сейфа: 1234".encode())
check(st == 400, "открытый текст под случайным именем не принимается")
st, _ = up(ana, vid, "секрет.txt", fake_cipher())
check(st == 400, "шифротекст под настоящим именем не принимается (имя утекло бы)")
fname = obf()
st, f = up(ana, vid, fname, fake_cipher(200))
check(st == 201 and f["e2e"] and f["name"] == fname, "шифротекст под случайным именем принят")
st, _ = ana.j("POST", "/api/files/folder", {"parent": vid, "name": "Документы"})
check(st == 400, "вложенная папка с настоящим именем не создаётся")
st, sub = ana.j("POST", "/api/files/folder", {"parent": vid, "name": obf()})
check(st == 201 and sub["e2e"], "вложенная папка наследует шифрование")
st, _ = ana.j("PATCH", f"/api/files/{f['id']}", {"name": "отчёт.txt"})
check(st == 400, "переименование в настоящее имя запрещено")

# ---------- метаданные ----------
st, m = ana.j("GET", f"/api/e2e/meta/{vid}")
check(st == 200 and m["version"] == 0 and m["data"] == "", "метаданных ещё нет")
st, r = ana.j("PUT", f"/api/e2e/meta/{vid}", {"data": "aXY=.Y3Q=", "version": 0})
check(st == 200 and r["version"] == 1, "метаданные записаны")
st, _ = ana.j("PUT", f"/api/e2e/meta/{vid}", {"data": "eA==.eA==", "version": 0})
check(st == 412, "одновременная запись со старой версией отклонена (412)")
st, r = ana.j("PUT", f"/api/e2e/meta/{vid}", {"data": "eQ==.eQ==", "version": 1})
check(st == 200 and r["version"] == 2, "запись с актуальной версией")
st, _ = eve.j("GET", f"/api/e2e/meta/{vid}")
check(st == 404, "чужие метаданные недоступны")
st, _ = ana.j("GET", f"/api/e2e/meta/{f['id']}")
check(st == 404, "метаданные есть только у E2E-папок")

# ---------- сервер не раскрывает шифротекст ----------
st, plain = up(ana, "", "обычный.txt", "обычный".encode())
st, _ = ana.j("GET", f"/api/files/{f['id']}/thumb")
check(st == 404, "миниатюры нет")
st, _ = ana.j("POST", "/api/shares", {"node_id": vid})
check(st == 400, "публичная ссылка на E2E-папку запрещена")
st, _ = ana.j("POST", "/api/user-shares", {"node_id": vid, "user_id": ids["crypto_eve"], "can_write": False})
check(st == 400, "общий доступ к E2E-папке запрещён")
st, _ = ana.req("GET", f"/api/files/{vid}/zip")
check(st == 400, "ZIP E2E-папки не собирается")
# /office/{id} принимает только cookie сессии, а тест входит через Basic (лимит входов по паролю в CI
# расходуют другие сценарии) — запрет онлайн-офиса для E2E (officePage) здесь не проверяется.
st, rec = ana.j("GET", "/api/recent")
check(st == 200 and all(not x.get("e2e") for x in rec), "в «Недавних» нет шифротекста")
st, tree = ana.j("GET", "/api/sync/tree")
check(st == 200 and any(x.get("e2e") and x["id"] == f["id"] for x in tree["entries"]), "в дереве синхронизации E2E помечены")

# ---------- граница E2E ----------
st, _ = ana.j("PATCH", f"/api/files/{f['id']}", {"parent": None})
check(st == 400, "E2E-файл нельзя вынести в обычную папку")
st, _ = ana.j("PATCH", f"/api/files/{plain['id']}", {"parent": vid, "name": obf()})
check(st == 400, "обычный файл нельзя внести в E2E-папку")
st, _ = ana.j("POST", f"/api/files/{f['id']}/copy", {"parent": None})
check(st == 400, "E2E-файл нельзя скопировать сервером")
st, nf = ana.j("POST", "/api/files/folder", {"parent": "", "name": "Архив"})
st, mv = ana.j("PATCH", f"/api/files/{vid}", {"parent": nf["id"]})
check(st == 200 and mv["e2e"], "E2E-папку целиком можно перенести в другую обычную папку")
st, other = ana.j("POST", "/api/files/folder", {"parent": "", "name": "Прочее"})  # без вложений: иначе сработала бы проверка цикла
st, e = ana.j("PATCH", f"/api/files/{other['id']}", {"parent": sub["id"], "name": obf()})
check(st == 400 and "между зашифрованной" in e.get("error", ""), "обычную папку нельзя положить внутрь E2E")

# ---------- WebDAV ----------
st, body = ana.req("PROPFIND", "/dav/Архив/", headers={"Depth": "1"})
check(st == 207 and "Личное".encode() not in body, "WebDAV не показывает E2E-папку")
st, _ = ana.req("PROPFIND", "/dav/Архив/Личное/", headers={"Depth": "0"})
check(st == 404, "WebDAV не открывает E2E-папку по прямому пути")

st, e = ana.j("DELETE", "/api/e2e/keys")
check(st == 400 and "есть зашифрованные" in e.get("error", ""), "ключ нельзя удалить, пока есть E2E-папки")

print("\nИТОГО ошибок:", fails)
sys.exit(1 if fails else 0)
