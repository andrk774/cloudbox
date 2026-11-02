"""Сквозной тест CloudBox: API, версии, корзина, квоты, ссылки, WebDAV."""
# Запуск на ЧИСТОЙ базе: CB_URL=http://127.0.0.1:8080 python3 scripts/e2e.py
# Сервер должен быть запущен с CB_MAX_VERSIONS=2 CB_DEFAULT_QUOTA_MB=50 CB_MAX_UPLOAD_MB=20
import base64, hashlib, http.cookiejar, json, os, sys, urllib.request, urllib.error, zipfile, io

B = os.environ.get("CB_URL", "http://127.0.0.1:8080")
fails = 0

def check(cond, msg):
    global fails
    print(("OK   " if cond else "FAIL ") + msg)
    if not cond:
        fails += 1

class Client:
    def __init__(self):
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
    def req(self, method, path, body=None, raw=None, headers=None):
        h = dict(headers or {})
        data = None
        if body is not None:
            data = json.dumps(body).encode(); h["Content-Type"] = "application/json"
        if raw is not None:
            data = raw; h.setdefault("Content-Type", "application/octet-stream")
        path = urllib.parse.quote(path, safe="/?=&%:+-_.~")
        if "Destination" in h:
            h["Destination"] = urllib.parse.quote(h["Destination"], safe="/:")
        r = urllib.request.Request(B + path, data=data, method=method, headers=h)
        try:
            resp = self.op.open(r)
            return resp.status, resp.read(), resp.headers
        except urllib.error.HTTPError as e:
            return e.code, e.read(), e.headers
    def j(self, method, path, body=None, **kw):
        st, b, _ = self.req(method, path, body, **kw)
        try: return st, json.loads(b)
        except Exception: return st, b

def up(c, parent, name, data, path=None, conflict=None):
    q = {"name": name}
    if parent: q["parent"] = parent
    if path: q["path"] = path
    if conflict: q["conflict"] = conflict
    return c.j("POST", "/api/files/upload?" + urllib.parse.urlencode(q), raw=data)

import urllib.parse

a = Client()
st, d = a.j("POST", "/api/auth/register", {"username": "andrey", "password": "secret123"})
check(st == 200 and d["user"]["is_admin"], "первый пользователь — админ")
st, d = Client().j("POST", "/api/auth/register", {"username": "bob", "password": "secret123"})
check(st == 403, "повторная регистрация закрыта (403)")
st, d = Client().j("POST", "/api/auth/login", {"username": "andrey", "password": "wrong"})
check(st == 401, "неверный пароль → 401")

# папки
st, docs = a.j("POST", "/api/files/folder", {"parent": "", "name": "Документы"})
check(st == 201 and docs["is_dir"], "создана папка Документы")
st, _ = a.j("POST", "/api/files/folder", {"parent": "", "name": "Документы"})
check(st == 409, "дубликат папки → 409")
st, _ = a.j("POST", "/api/files/folder", {"parent": "", "name": "a/b"})
check(st == 400, "имя со слешем → 400")

# загрузка и версии
st, f1 = up(a, docs["id"], "отчёт.txt", b"version 1")
check(st == 201 and f1["size"] == 9 and f1["mime"].startswith("text/plain"), "загружен отчёт.txt, mime=" + f1.get("mime", ""))
for i in range(2, 5):
    st, f1 = up(a, docs["id"], "отчёт.txt", f"version {i} longer".encode())
st, vs = a.j("GET", f"/api/files/{f1['id']}/versions")
check(len(vs) == 3 and vs[0]["current"], f"хранится текущая + 2 версии (всего {len(vs)})")
st, body, hdr = a.req("GET", f"/api/files/{f1['id']}/content")
check(body == b"version 4 longer", "скачивание текущей версии")
check("attachment" in hdr["Content-Disposition"] and "filename*=UTF-8''" in hdr["Content-Disposition"], "Content-Disposition с UTF-8 именем")
st, body, hdr = a.req("GET", f"/api/files/{f1['id']}/content", headers={"Range": "bytes=0-6"})
check(st == 206 and body == b"version", "Range-запрос → 206")
old = vs[-1]
st, n = a.j("POST", f"/api/files/{f1['id']}/versions/{old['id']}/restore")
st, body, _ = a.req("GET", f"/api/files/{f1['id']}/content")
check(body == b"version 2 longer", "восстановление старой версии")

# загрузка с путём (папка) и переименованием при конфликте
st, f2 = up(a, None, "photo.png", b"\x89PNG\r\n\x1a\n" + b"0" * 100, path="Фото/2026/Таганай")
check(st == 201 and f2["mime"] == "image/png", "загрузка с созданием вложенных папок")
st, f3 = up(a, docs["id"], "отчёт.txt", b"other", conflict="rename")
check(st == 201 and f3["name"] == "отчёт (2).txt", "conflict=rename → " + f3.get("name", ""))

# список, хлебные крошки, поиск
st, d = a.j("GET", "/api/files")
check([x["name"] for x in d["items"]] == ["Документы", "Фото"], "корень: папки")
st, tag = a.j("GET", "/api/search?q=таганай")
check(len(tag) == 1 and tag[0]["path"] == "/Фото/2026", "поиск без учёта регистра + путь")
st, d = a.j("GET", "/api/files?parent=" + tag[0]["id"])
check([c["name"] for c in d["breadcrumbs"]] == ["Фото", "2026", "Таганай"], "хлебные крошки")
st, _ = a.j("GET", "/api/search?q=%25")
check(st == 200 and _ == [], "поиск по '%' экранируется")

# перемещение, цикл, копирование
foto = a.j("GET", "/api/files")[1]["items"][1]
st, _ = a.j("PATCH", f"/api/files/{foto['id']}", {"parent": tag[0]["id"]})
check(st == 400, "перемещение папки внутрь себя → 400")
st, mv = a.j("PATCH", f"/api/files/{f3['id']}", {"name": "итог.txt", "parent": None})
check(st == 200 and mv["parent_id"] is None and mv["name"] == "итог.txt", "переименование + перенос в корень")
st, cp = a.j("POST", f"/api/files/{foto['id']}/copy", {"parent": None})
check(st == 201 and cp["name"] == "Фото (2)", "копия папки → " + cp.get("name", ""))
st, s = a.j("GET", "/api/search?q=photo.png")
check(len(s) == 2, "в копии есть файл")

# ZIP
st, body, hdr = a.req("GET", f"/api/files/{foto['id']}/zip")
zf = zipfile.ZipFile(io.BytesIO(body))
check("Фото/2026/Таганай/photo.png" in zf.namelist(), "ZIP папки: " + ", ".join(zf.namelist()))

# корзина
st, _ = a.j("DELETE", f"/api/files/{f1['id']}")  # сначала файл отдельно
st, _ = a.j("DELETE", f"/api/files/{docs['id']}")  # потом папку
st, t = a.j("GET", "/api/trash")
check(len(t["items"]) == 2, "в корзине 2 объекта")
st, _ = a.j("GET", f"/api/files/{f1['id']}")
check(st == 404, "удалённый файл недоступен")
st, r = a.j("POST", f"/api/trash/{docs['id']}/restore")
check(st == 200, "папка восстановлена")
st, d = a.j("GET", "/api/files?parent=" + docs["id"])
check([x["name"] for x in d["items"]] == [], "файл, удалённый отдельно, остался в корзине")
st, r = a.j("POST", f"/api/trash/{f1['id']}/restore")
st, d = a.j("GET", "/api/files?parent=" + docs["id"])
check([x["name"] for x in d["items"]] == ["отчёт.txt"], "файл вернулся на место")
# purge: папка с отдельно удалённым ребёнком
a.j("DELETE", f"/api/files/{f1['id']}")
a.j("DELETE", f"/api/files/{docs['id']}")
st, _ = a.j("DELETE", f"/api/trash/{docs['id']}")
st, t = a.j("GET", "/api/trash")
check(st == 200 and [x["name"] for x in t["items"]] == ["отчёт.txt"], "окончательное удаление папки не тронуло отдельно удалённый файл")
st, r = a.j("POST", f"/api/trash/{f1['id']}/restore")
check(st == 200 and r["parent_id"] is None, "восстановление в корень, если папки больше нет")

# квота (50 МБ) и лимит размера (20 МБ)
st, _ = up(a, None, "big.bin", os.urandom(21 * 1024 * 1024))
check(st == 413, f"файл больше лимита → 413 (получено {st})")
for i in range(3):
    st, _ = up(a, None, f"chunk{i}.bin", os.urandom(19 * 1024 * 1024))
check(st == 507, f"превышение квоты → 507 (получено {st})")
st, me = a.j("GET", "/api/me")
check(me["used_bytes"] < 50 * 1024 * 1024, "used_bytes " + str(me["used_bytes"]))

# публичные ссылки
st, sh = a.j("POST", "/api/shares", {"node_id": foto["id"], "password": "pw12345", "allow_upload": True})
check(st == 201 and sh["url"].endswith(sh["token"]), "ссылка создана")
g = Client()
tok = sh["token"]
st, info = g.j("GET", f"/api/public/{tok}")
check(info["needs_password"] and not info["authorized"], "ссылка просит пароль")
st, _ = g.j("GET", f"/api/public/{tok}/list")
check(st == 401, "без пароля список закрыт")
st, _ = g.j("POST", f"/api/public/{tok}/auth", {"password": "bad"})
check(st == 401, "неверный пароль ссылки")
st, _ = g.j("POST", f"/api/public/{tok}/auth", {"password": "pw12345"})
st, l = g.j("GET", f"/api/public/{tok}/list")
check(st == 200 and l["items"][0]["name"] == "2026", "список папки по ссылке")
st, body, _ = g.req("GET", f"/api/public/{tok}/content?node={f2['id']}")
check(st == 200 and body.startswith(b"\x89PNG"), "скачивание файла из расшаренной папки")
st, _ = g.j("GET", f"/api/public/{tok}/content?node={f1['id']}")
check(st == 404, "файл вне расшаренной папки недоступен")
st, upd = g.j("POST", f"/api/public/{tok}/upload?name=photo.png&dir={tag[0]['id']}", raw=b"guest")
check(st == 201 and upd["name"] == "photo (2).png", "гость загрузил файл без перезаписи → " + str(upd.get("name")))
st, body, _ = g.req("GET", f"/api/public/{tok}/zip")
check(st == 200 and body[:2] == b"PK", "ZIP по ссылке")
a.j("DELETE", f"/api/shares/{sh['id']}")
st, _ = g.j("GET", f"/api/public/{tok}")
check(st == 404, "удалённая ссылка не работает")

# CSRF
st, _ = a.j("POST", "/api/files/folder", {"parent": "", "name": "x"}, headers={"Origin": "https://evil.example"})
check(st == 403, "запрос с чужого Origin → 403")

# админ: второй пользователь и изоляция
st, u2 = a.j("POST", "/api/admin/users", {"username": "masha", "password": "masha12345"})
check(st == 201 and not u2["is_admin"], "админ создал пользователя")
m = Client()
m.j("POST", "/api/auth/login", {"username": "masha", "password": "masha12345"})
st, _ = m.j("GET", f"/api/files/{f2['id']}")
check(st == 404, "чужой файл не виден")
st, _ = m.j("GET", "/api/admin/users")
check(st == 403, "не-админ не видит админку")

# WebDAV
auth = "Basic " + base64.b64encode(b"andrey:secret123").decode()
st, _, _ = Client().req("PROPFIND", "/dav/", headers={"Depth": "1"})
check(st == 401, "WebDAV без авторизации → 401")
c = Client()
st, body, _ = c.req("PROPFIND", "/dav/", headers={"Depth": "1", "Authorization": auth})
check(st == 207 and "Фото".encode() in body, "PROPFIND корня")
st, _, _ = c.req("MKCOL", "/dav/Заметки", headers={"Authorization": auth})
check(st == 201, "MKCOL")
st, _, _ = c.req("PUT", "/dav/Заметки/todo.md", raw=b"# TODO\n- dav", headers={"Authorization": auth})
check(st == 201, f"PUT нового файла ({st})")
st, _, _ = c.req("PUT", "/dav/Заметки/todo.md", raw=b"# TODO v2", headers={"Authorization": auth})
check(st in (201, 204), f"PUT перезапись ({st})")
st, body, _ = c.req("GET", "/dav/Заметки/todo.md", headers={"Authorization": auth})
check(body == b"# TODO v2", "GET через WebDAV")
s = a.j("GET", "/api/search?q=todo.md")[1]
vs = a.j("GET", f"/api/files/{s[0]['id']}/versions")[1]
check(len(vs) == 2, "перезапись по WebDAV создаёт версию")
st, _, _ = c.req("MOVE", "/dav/Заметки/todo.md", headers={"Authorization": auth, "Destination": B + "/dav/Заметки/done.md"})
check(st in (201, 204), f"MOVE ({st})")
st, _, _ = c.req("COPY", "/dav/Заметки/done.md", headers={"Authorization": auth, "Destination": B + "/dav/copy.md"})
check(st in (201, 204), f"COPY ({st})")
st, _, _ = c.req("DELETE", "/dav/Заметки", headers={"Authorization": auth})
check(st == 204, "DELETE через WebDAV")
t = a.j("GET", "/api/trash")[1]
check(any(x["name"] == "Заметки" for x in t["items"]), "удалённое через WebDAV — в корзине")

# пароль приложения
st, ap = a.j("POST", "/api/me/app-passwords", {"name": "rclone"})
auth2 = "Basic " + base64.b64encode(("andrey:" + ap["password"]).encode()).decode()
st, _, _ = Client().req("PROPFIND", "/dav/", headers={"Depth": "0", "Authorization": auth2})
check(st == 207, "WebDAV по паролю приложения")
a.j("DELETE", f"/api/me/app-passwords/{ap['app_password']['id']}")
st, _, _ = Client().req("PROPFIND", "/dav/", headers={"Depth": "0", "Authorization": auth2})
check(st == 401, "отозванный пароль приложения не работает")

# публичная страница и SPA
st, body, _ = Client().req("GET", "/s/whatever123")
check(st == 200 and b"share.js" in body, "страница /s/{token}")
st, body, _ = Client().req("GET", "/some/spa/path")
check(st == 200 and b"app.js" in body, "SPA fallback")

print("\nИТОГО ошибок:", fails)
sys.exit(1 if fails else 0)
