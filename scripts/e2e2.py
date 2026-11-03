"""Сквозной тест функций CloudBox v2.

Запуск на ЧИСТОЙ базе после scripts/e2e.py не нужен — тест создаёт своих пользователей сам:
    CB_URL=http://127.0.0.1:8080 FIXTURES=/path/with/photo.jpg,clip.mp4 python3 scripts/e2e2.py
Сервер: CB_WOPI_URL указывает на любой сервер, отдающий /hosting/discovery (для проверки WOPI).
"""
import base64, hashlib, hmac, http.cookiejar, io, json, os, re, struct, sys, time, urllib.error, urllib.parse, urllib.request, zipfile

B = os.environ.get("CB_URL", "http://127.0.0.1:8080")
FX = os.environ.get("FIXTURES", "/var/tmp/fx")
fails = 0


def check(cond, msg):
    global fails
    print(("OK   " if cond else "FAIL ") + msg)
    if not cond:
        fails += 1


class Client:
    def __init__(self, basic=None):
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.basic = basic

    def req(self, method, path, body=None, raw=None, headers=None):
        h = dict(headers or {})
        data = None
        if body is not None:
            data = json.dumps(body).encode(); h["Content-Type"] = "application/json"
        if raw is not None:
            data = raw; h.setdefault("Content-Type", "application/octet-stream")
        if self.basic:
            h["Authorization"] = "Basic " + base64.b64encode(self.basic.encode()).decode()
        if "Destination" in h:
            h["Destination"] = urllib.parse.quote(h["Destination"], safe="/:")
        path = urllib.parse.quote(path, safe="/?=&%:+-_.~")
        r = urllib.request.Request(B + path, data=data, method=method, headers=h)
        try:
            resp = self.op.open(r)
            return resp.status, resp.read(), resp.headers
        except urllib.error.HTTPError as e:
            return e.code, e.read(), e.headers

    def j(self, method, path, body=None, **kw):
        st, b, _ = self.req(method, path, body, **kw)
        try:
            return st, json.loads(b)
        except Exception:
            return st, b


def up(c, parent, name, data, **extra):
    q = {"name": name, **extra}
    if parent:
        q["parent"] = parent
    return c.j("POST", "/api/files/upload?" + urllib.parse.urlencode(q), raw=data)


def totp(secret, t=None):
    key = base64.b32decode(secret + "=" * (-len(secret) % 8))
    counter = int((t or time.time()) // 30)
    h = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    o = h[-1] & 15
    return "%06d" % ((struct.unpack(">I", h[o:o + 4])[0] & 0x7fffffff) % 1000000)


def jpeg_size(b):
    i = 2
    while i < len(b):
        if b[i] != 0xFF:
            return None
        m = b[i + 1]
        ln = struct.unpack(">H", b[i + 2:i + 4])[0]
        if m in (0xC0, 0xC1, 0xC2):
            h, w = struct.unpack(">HH", b[i + 5:i + 9])
            return w, h
        i += 2 + ln
    return None


def with_exif(jpg, date="2024:07:15 10:30:00", orientation=6):
    s = date.encode() + b"\0"
    tiff = b"II*\0" + struct.pack("<I", 8)
    tiff += struct.pack("<H", 2)
    tiff += struct.pack("<HHII", 0x0112, 3, 1, orientation)
    tiff += struct.pack("<HHII", 0x8769, 4, 1, 38)
    tiff += struct.pack("<I", 0)
    tiff += struct.pack("<H", 1) + struct.pack("<HHII", 0x9003, 2, len(s), 56) + struct.pack("<I", 0)
    tiff += s
    app1 = b"Exif\0\0" + tiff
    return jpg[:2] + b"\xFF\xE1" + struct.pack(">H", len(app1) + 2) + app1 + jpg[2:]


def minimal_docx(text):
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as z:
        z.writestr("[Content_Types].xml", '<?xml version="1.0"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"/>')
        z.writestr("word/document.xml", '<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>' + text + '</w:t></w:r></w:p></w:body></w:document>')
    return buf.getvalue()


def minimal_pdf(text):
    stream = ("BT /F1 18 Tf 72 720 Td (%s) Tj ET" % text).encode()
    objs = [b"<< /Type /Catalog /Pages 2 0 R >>", b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
            b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
            b"<< /Length %d >>\nstream\n" % len(stream) + stream + b"\nendstream",
            b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>"]
    out = b"%PDF-1.4\n"
    offs = []
    for i, o in enumerate(objs, 1):
        offs.append(len(out))
        out += b"%d 0 obj\n" % i + o + b"\nendobj\n"
    x = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    for o in offs:
        out += b"%010d 00000 n \n" % o
    out += b"trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n" % (len(objs) + 1, x)
    return out


def wait(cond, timeout=20):
    t = time.time()
    while time.time() - t < timeout:
        if cond():
            return True
        time.sleep(0.5)
    return False


# ---------- пользователи ----------
a = Client()
st, d = a.j("POST", "/api/auth/register", {"username": "owner", "password": "owner12345"})
if st != 200:  # база уже с пользователями (после e2e.py) — входим админом из первого теста
    a = Client()
    st, d = a.j("POST", "/api/auth/login", {"username": "andrey", "password": "secret123"})
check(st == 200, "вход владельца")
me = a.j("GET", "/api/me")[1]
check(me["user"]["is_admin"], "владелец — администратор")
uname = me["user"]["username"]
apw = a.j("POST", "/api/me/app-passwords", {"name": "tests"})[1]["password"]
for name in ("masha", "petya"):
    a.j("POST", "/api/admin/users", {"username": name, "password": name + "12345", "quota_bytes": 0})
users = {u["username"]: u["id"] for u in a.j("GET", "/api/admin/users")[1]}
a.j("PATCH", "/api/admin/users/%d" % me["user"]["id"], {"quota_bytes": 0})
m = Client(); m.j("POST", "/api/auth/login", {"username": "masha", "password": "masha12345"})
p = Client(); p.j("POST", "/api/auth/login", {"username": "petya", "password": "petya12345"})

# ---------- общий доступ ----------
st, proj = a.j("POST", "/api/files/folder", {"parent": "", "name": "Проект Урал"})
st, sub = a.j("POST", "/api/files/folder", {"parent": proj["id"], "name": "Маршруты"})
st, doc = up(a, proj["id"], "план.txt", "Маршрут через перевал Таганай и хребет Откликной".encode())
st, sh = a.j("POST", "/api/user-shares", {"node_id": proj["id"], "user_id": users["masha"], "can_write": True})
check(st == 201 and sh["target_name"] == "masha", "владелец поделился папкой с masha (запись)")
st, dup = a.j("POST", "/api/user-shares", {"node_id": proj["id"], "user_id": users["masha"], "can_write": False})
check(st == 409, "повторная шара → 409")
st, swm = m.j("GET", "/api/shared-with-me")
check(st == 200 and len(swm) == 1 and swm[0]["owner"] == uname and swm[0]["can_write"], "masha видит папку в «Доступно мне»")
st, lst = m.j("GET", "/api/files?parent=" + proj["id"])
check(st == 200 and {x["name"] for x in lst["items"]} == {"Маршруты", "план.txt"} and lst["can_write"], "masha открывает общую папку")
check([c["name"] for c in lst["breadcrumbs"]] == ["Проект Урал"] and lst.get("shared_by") == uname, "крошки обрезаны до общей папки")
st, mf = up(m, sub["id"], "трек.gpx", b"<gpx/>")
check(st == 201, "masha загрузила файл во вложенную папку")
st, own = a.j("GET", "/api/files?parent=" + sub["id"])
check(any(x["name"] == "трек.gpx" for x in own["items"]), "файл появился у владельца")
st, _ = m.j("DELETE", "/api/files/" + proj["id"])
check(st == 403, "masha не может удалить корень общей папки")
st, _ = m.j("PATCH", "/api/files/" + mf["id"], {"name": "трек-1.gpx"})
check(st == 200, "masha переименовала свой файл внутри")
st, _ = m.j("PATCH", "/api/files/" + mf["id"], {"parent": None})
check(st == 400, "перенос из чужой папки в свою → 400 (только копирование)")
st, cp = m.j("POST", "/api/files/" + mf["id"] + "/copy", {"parent": None})
check(st == 201, "masha скопировала файл к себе")
st, _ = p.j("GET", "/api/files?parent=" + proj["id"])
check(st == 404, "petya не видит чужую папку")
st, n = m.j("GET", "/api/notifications")
check(n["unread"] >= 1 and any("поделился" in x["title"] for x in n["items"]), "masha получила уведомление о шаре")
# группа
st, g = a.j("POST", "/api/admin/groups", {"name": "Походники"})
a.j("PUT", "/api/admin/groups/%d/members" % g["id"], {"user_ids": [users["petya"]]})
st, gsh = a.j("POST", "/api/user-shares", {"node_id": doc["id"], "group_id": g["id"], "can_write": False})
check(st == 201, "шара файла на группу")
st, body, _ = p.req("GET", "/api/files/" + doc["id"] + "/content")
check(st == 200 and "Таганай".encode() in body, "petya читает файл через группу")
st, _ = up(p, proj["id"], "x.txt", b"x")
check(st == 404, "petya не может писать в папку без доступа")
st, _ = p.j("PUT", "/api/files/" + doc["id"] + "/content", raw=b"hack")
check(st == 403, "petya не может изменить файл с доступом только на чтение")
st, acc = a.j("GET", "/api/files/" + proj["id"] + "/access")
check(st == 200 and len(acc["user_shares"]) == 1, "владелец видит список доступа")
st, dirl = m.j("GET", "/api/directory?q=pe")
check(any(x["name"] == "petya" for x in dirl), "поиск пользователей для шаринга")

# WebDAV в общей папке
mdav = Client(basic="masha:masha12345")
st, body, _ = mdav.req("PROPFIND", "/dav/", headers={"Depth": "1"})
check(st == 207 and "Доступно мне".encode() in body, "WebDAV: masha видит «Доступно мне»")
st, body, _ = mdav.req("PROPFIND", "/dav/Доступно мне/Проект Урал/", headers={"Depth": "1"})
check(st == 207 and "план.txt".encode() in body, "WebDAV: содержимое общей папки")
st, _, _ = mdav.req("PUT", "/dav/Доступно мне/Проект Урал/заметка.md", raw=b"# from dav")
check(st == 201, "WebDAV: masha пишет в общую папку")
st, body, _ = mdav.req("PROPFIND", "/dav/", headers={"Depth": "0", "Content-Type": "application/xml"},
                       raw=b'<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:quota-used-bytes/><d:quota-available-bytes/></d:prop></d:propfind>')
check(st == 207 and b"quota-available-bytes>" in body and b"quota-used-bytes>" in body, "WebDAV: квота отдаётся")
# отзыв доступа
a.j("DELETE", "/api/user-shares/" + sh["id"])
st, _ = m.j("GET", "/api/files?parent=" + proj["id"])
check(st == 404, "после отзыва доступа masha папку не видит")

# ---------- ETag папок ----------
e1 = a.j("GET", "/api/files/" + proj["id"])[1]["etag"]
up(a, sub["id"], "новый.txt", b"1")
e2 = a.j("GET", "/api/files/" + proj["id"])[1]["etag"]
check(e1 != e2, "ETag родительской папки меняется при изменении внутри")
st, tree = a.j("GET", "/api/sync/tree?root=" + proj["id"])
check(st == 200 and any(x["path"] == "Маршруты/новый.txt" for x in tree["entries"]), "дерево для синхронизации")
st, t2 = a.j("GET", "/api/sync/tree?root=%s&etag=%s" % (proj["id"], tree["etag"]))
check(t2.get("unchanged") is True, "дерево не изменилось — короткий ответ")

# ---------- докачка ----------
data = os.urandom(17 * 1024 * 1024 + 12345)
st, u = a.j("POST", "/api/uploads", {"parent": proj["id"], "name": "большой.bin", "size": len(data)})
check(st == 201 and u["chunk_size"] > 0, "создана сессия докачки")
cs = 8 * 1024 * 1024
st, r1 = a.j("PUT", "/api/uploads/%s?offset=0" % u["id"], raw=data[:cs])
check(st == 200 and r1["received"] == cs, "часть 1 принята")
st, bad = a.j("PUT", "/api/uploads/%s?offset=%d" % (u["id"], 2 * cs), raw=data[2 * cs:3 * cs])
check(st == 409 and bad["received"] == cs, "неверное смещение → 409 с текущим смещением")
st, info = a.j("GET", "/api/uploads/" + u["id"])
check(info["received"] == cs, "после «обрыва» сервер помнит смещение")
off = info["received"]
while off < len(data):
    st, r = a.j("PUT", "/api/uploads/%s?offset=%d" % (u["id"], off), raw=data[off:off + cs])
    off = r["received"]
st, big = a.j("POST", "/api/uploads/%s/complete" % u["id"])
check(st == 201 and big["size"] == len(data), "файл собран из частей")
st, body, _ = a.req("GET", "/api/files/%s/content" % big["id"], headers={"Range": "bytes=10000000-10000099"})
check(st == 206 and body == data[10000000:10000100], "содержимое совпадает")

# ---------- медиа: EXIF, миниатюры, галерея ----------
jpg = with_exif(open(os.path.join(FX, "photo.jpg"), "rb").read())
st, ph = up(a, None, "IMG_0001.jpg", jpg, path="Фото/Камера")
check(st == 201 and (ph.get("taken_at") or "").startswith("2024-07-15T10:30:00"), "дата съёмки из EXIF: %s" % ph.get("taken_at"))
st, body, hdr = a.req("GET", "/api/files/%s/thumb?s=256" % ph["id"])
size = jpeg_size(body) if st == 200 else None
check(st == 200 and size == (192, 256), "миниатюра 256 с учётом ориентации EXIF: %s" % (size,))
st, body2, _ = a.req("GET", "/api/files/%s/thumb?s=256" % ph["id"], headers={"If-None-Match": hdr.get("ETag", "")})
check(st == 304, "повторный запрос миниатюры → 304")
st, vid = up(a, None, "clip.mp4", open(os.path.join(FX, "clip.mp4"), "rb").read(), path="Фото/Камера")
st, body, _ = a.req("GET", "/api/files/%s/thumb?s=256" % vid["id"])
check(st == 200 and body[:2] == b"\xff\xd8", "миниатюра видео (ffmpeg)")
st, pdf = up(a, None, "отчёт.pdf", minimal_pdf("Kvarkush plateau expedition report"))
st, body, _ = a.req("GET", "/api/files/%s/thumb?s=256" % pdf["id"])
check(st == 200 and body[:2] == b"\xff\xd8", "миниатюра PDF (pdftoppm)")
st, txtthumb, _ = a.req("GET", "/api/files/%s/thumb" % doc["id"])
check(st == 404, "для текста миниатюры нет → 404")
st, gal = a.j("GET", "/api/gallery")
names = [x["name"] for x in gal]
check(st == 200 and names[0] == "clip.mp4" and names[-1] == "IMG_0001.jpg", "галерея: новое сверху, фото 2024 года — в конце")

# ---------- полнотекстовый поиск ----------
st, dx = up(a, None, "поход.docx", minimal_docx("Отчёт о восхождении на Иремель в сентябре"))
found = wait(lambda: any(x["name"] == "план.txt" for x in a.j("GET", "/api/search?q=перевалы")[1]))
check(found, "поиск по содержимому с морфологией: «перевалы» → план.txt")
res = a.j("GET", "/api/search?q=перевалы")[1]
hit = [x for x in res if x["name"] == "план.txt"]
check(hit and "«" in hit[0]["snippet"], "сниппет с подсветкой: %s" % (hit[0]["snippet"] if hit else ""))
check(wait(lambda: any(x["name"] == "поход.docx" for x in a.j("GET", "/api/search?q=восхождение Иремель")[1])), "поиск внутри DOCX")
check(wait(lambda: any(x["name"] == "отчёт.pdf" for x in a.j("GET", "/api/search?q=Kvarkush")[1])), "поиск внутри PDF")
st, mres = m.j("GET", "/api/search?q=перевалы")
check(st == 200 and mres == [], "поиск не находит чужие файлы")

# ---------- избранное, теги, комментарии ----------
a.j("PUT", "/api/files/%s/favorite" % doc["id"])
st, fav = a.j("GET", "/api/favorites")
check(len(fav) == 1 and fav[0]["favorite"], "избранное")
st, tg = a.j("POST", "/api/tags", {"name": "важное", "color": "#d33a3a"})
a.j("PUT", "/api/files/%s/tags" % doc["id"], {"tag_ids": [tg["id"]]})
st, lst = a.j("GET", "/api/files?parent=" + proj["id"])
pl = [x for x in lst["items"] if x["name"] == "план.txt"][0]
check(pl["favorite"] and pl["tags"][0]["name"] == "важное", "теги и избранное в списке файлов")
st, tf = a.j("GET", "/api/tags/%d/files" % tg["id"])
check(len(tf) == 1, "файлы по тегу")
st, cm = p.j("POST", "/api/files/%s/comments" % doc["id"], {"body": "Берём палатку?"})
check(st == 201, "petya (группа, чтение) комментирует")
st, cms = a.j("GET", "/api/files/%s/comments" % doc["id"])
check(len(cms) == 1 and cms[0]["username"] == "petya", "владелец видит комментарий")
st, an = a.j("GET", "/api/notifications")
check(any("прокомментировал" in x["title"] for x in an["items"]), "владелец получил уведомление о комментарии")
st, _ = m.j("DELETE", "/api/comments/%d" % cm["id"])
check(st == 404 or st == 403, "чужой комментарий удалить нельзя")
st, _ = a.j("DELETE", "/api/comments/%d" % cm["id"])
check(st == 200, "владелец файла удаляет комментарий")

# ---------- журнал ----------
st, act = a.j("GET", "/api/activity")
acts = {x["action"] for x in act}
check({"upload", "mkdir", "share_user", "rename", "comment"} <= acts, "журнал действий: %s" % sorted(acts))
check(any(x["actor"] == "masha" and x["action"] == "upload" for x in act), "в журнале видно, что файл загрузила masha")

# ---------- редактор и заметки ----------
st, notes = a.j("GET", "/api/notes")
check(st == 200 and notes["folder_id"], "папка «Заметки» создана")
st, note = up(a, notes["folder_id"], "Список снаряжения.md", "# Снаряжение\n- [ ] палатка".encode())
st, saved = a.j("PUT", "/api/files/%s/content" % note["id"], raw="# Снаряжение\n- [x] палатка".encode(), headers={"If-Match": note["version_id"]})
check(st == 200 and saved["version_id"] != note["version_id"], "сохранение из редактора")
st, _ = a.j("PUT", "/api/files/%s/content" % note["id"], raw=b"stale", headers={"If-Match": note["version_id"]})
check(st == 412, "устаревшая версия → 412 (не затираем чужие правки)")

# ---------- 2FA ----------
z = Client()
z.j("POST", "/api/auth/login", {"username": "petya", "password": "petya12345"})
st, setup = z.j("POST", "/api/me/totp/setup")
check(st == 200 and setup["uri"].startswith("otpauth://totp/"), "2FA: секрет и otpauth-ссылка")
st, _ = z.j("POST", "/api/me/totp/enable", {"code": "000000"})
check(st == 400, "2FA: неверный код не включает защиту")
st, en = z.j("POST", "/api/me/totp/enable", {"code": totp(setup["secret"])})
check(st == 200 and len(en["recovery_codes"]) == 10, "2FA включена, выданы коды восстановления")
y = Client()
st, l1 = y.j("POST", "/api/auth/login", {"username": "petya", "password": "petya12345"})
check(st == 200 and l1.get("totp_required") and "user" not in l1, "вход требует второй фактор")
st, _ = y.j("GET", "/api/me")
check(st == 401, "без кода сессии нет")
st, _ = y.j("POST", "/api/auth/totp", {"ticket": l1["ticket"], "code": "123456"})
check(st == 400, "неверный код → 400")
st, l2 = y.j("POST", "/api/auth/totp", {"ticket": l1["ticket"], "code": totp(setup["secret"])})
check(st == 200 and y.j("GET", "/api/me")[0] == 200, "вход по коду TOTP")
rc = en["recovery_codes"][0]
y2 = Client()
t = y2.j("POST", "/api/auth/login", {"username": "petya", "password": "petya12345"})[1]["ticket"]
st, _ = y2.j("POST", "/api/auth/totp", {"ticket": t, "code": rc})
check(st == 200, "вход по коду восстановления")
y3 = Client()
t = y3.j("POST", "/api/auth/login", {"username": "petya", "password": "petya12345"})[1]["ticket"]
st, _ = y3.j("POST", "/api/auth/totp", {"ticket": t, "code": rc})
check(st == 400, "код восстановления одноразовый")
st, _, _ = Client(basic="petya:petya12345").req("PROPFIND", "/dav/", headers={"Depth": "0"})
check(st == 401, "с 2FA основной пароль не работает в WebDAV")
st, ses = y.j("GET", "/api/me/sessions")
check(st == 200 and len(ses) >= 3 and sum(1 for x in ses if x["current"]) == 1, "список сессий (%d)" % len(ses))
y.j("DELETE", "/api/me/sessions")
check(y2.j("GET", "/api/me")[0] == 401 and y.j("GET", "/api/me")[0] == 200, "завершение остальных сессий")
st, logins = y.j("GET", "/api/me/logins")
check(any(not x["success"] for x in logins) and any(x["method"] == "password+totp" for x in logins), "журнал входов")

# ---------- Basic-auth в API (клиент синхронизации) ----------
st, d = Client(basic=uname + ":" + apw).j("GET", "/api/me")
check(st == 200 and d["user"]["username"] == uname, "API по паролю приложения (Basic)")

# ---------- CalDAV / CardDAV ----------
dv = Client(basic=uname + ":" + apw)
st, _, hdr = dv.req("PROPFIND", "/.well-known/caldav")
check(st in (301, 308) and hdr.get("Location", "").endswith("/caldav/%s/" % uname), "well-known caldav → принципал")
st, body, _ = dv.req("PROPFIND", "/caldav/%s/calendars/" % uname, headers={"Depth": "1"})
check(st == 207 and b"personal" in body, "PROPFIND календарей (создан «Личный»)")
ics = ("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//test//EN\r\nBEGIN:VEVENT\r\nUID:ev-1@test\r\nDTSTAMP:20261101T000000Z\r\n"
       "DTSTART:20261110T090000Z\r\nDTEND:20261110T100000Z\r\nSUMMARY:Сборы перед походом\r\nRRULE:FREQ=WEEKLY;COUNT=3\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
st, _, hdr = dv.req("PUT", "/caldav/%s/calendars/personal/ev-1.ics" % uname, raw=ics.encode(), headers={"Content-Type": "text/calendar"})
check(st in (201, 204) and hdr.get("ETag"), "CalDAV PUT события")
st, _, _ = dv.req("PUT", "/caldav/%s/calendars/personal/ev-1.ics" % uname, raw=ics.encode(), headers={"Content-Type": "text/calendar", "If-None-Match": "*"})
check(st == 412, "If-None-Match: * на существующем → 412")
st, body, _ = dv.req("GET", "/caldav/%s/calendars/personal/ev-1.ics" % uname)
check(st == 200 and "Сборы".encode() in body, "CalDAV GET")
st, ev = a.j("GET", "/api/events?from=2026-11-01T00:00:00Z&to=2026-12-01T00:00:00Z")
check(st == 200 and len([e for e in ev if e["summary"] == "Сборы перед походом"]) == 3, "повторяющееся событие развёрнуто (3 раза)")
st, ne = a.j("POST", "/api/events", {"summary": "Выезд на Таганай", "start": "2026-11-20T00:00:00Z", "end": "2026-11-22T00:00:00Z", "all_day": True})
check(st == 201, "событие из веб-интерфейса")
st, body, _ = dv.req("REPORT", "/caldav/%s/calendars/personal/" % uname, headers={"Depth": "1", "Content-Type": "application/xml"},
                     raw=b'<?xml version="1.0"?><c:calendar-query xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:prop><d:getetag/></d:prop><c:filter><c:comp-filter name="VCALENDAR"><c:comp-filter name="VEVENT"/></c:comp-filter></c:filter></c:calendar-query>')
check(st == 207 and body.count(b"<getetag") + body.count(b":getetag>") >= 2, "CalDAV REPORT calendar-query видит оба события")
vcf = "BEGIN:VCARD\r\nVERSION:3.0\r\nUID:c-1\r\nFN:Иван Проводник\r\nTEL:+79001234567\r\nEMAIL:ivan@example.ru\r\nEND:VCARD\r\n"
st, _, _ = dv.req("PUT", "/carddav/%s/contacts/contacts/c-1.vcf" % uname, raw=vcf.encode(), headers={"Content-Type": "text/vcard"})
check(st in (201, 204), "CardDAV PUT контакта")
st, cts = a.j("GET", "/api/contacts")
check(st == 200 and any(c["name"] == "Иван Проводник" and "+79001234567" in c["phones"] for c in cts), "контакт виден в веб-интерфейсе")
st, nc = a.j("POST", "/api/contacts", {"name": "Мария Гид", "phones": ["+79007654321"]})
st, body, _ = dv.req("PROPFIND", "/carddav/%s/contacts/contacts/" % uname, headers={"Depth": "1"})
check(st == 207 and body.count(b".vcf") >= 2, "контакт из веба доступен по CardDAV")
st, _, _ = Client(basic=uname + ":wrong").req("PROPFIND", "/caldav/%s/" % uname)
check(st == 401, "CalDAV без пароля → 401")

# ---------- WOPI (онлайн-редактор) ----------
st, me2 = a.j("GET", "/api/me")
if me2.get("office_exts"):
    check("docx" in me2["office_exts"], "редактор: список форматов из discovery")
    st, body, _ = a.req("GET", "/office/" + dx["id"])
    tok = re.search(rb'name="access_token" value="([^"]+)"', body)
    act = re.search(rb'action="([^"]+)"', body)
    check(st == 200 and tok and act and b"WOPISrc=" in act.group(1), "страница редактора с токеном")
    token = urllib.parse.quote(tok.group(1).decode())
    w = Client()
    st, info = w.j("GET", "/wopi/files/%s?access_token=%s" % (dx["id"], token))
    check(st == 200 and info["BaseFileName"] == "поход.docx" and info["UserCanWrite"], "WOPI CheckFileInfo")
    st, body, _ = w.req("GET", "/wopi/files/%s/contents?access_token=%s" % (dx["id"], token))
    check(st == 200 and body[:2] == b"PK", "WOPI GetFile")
    st, _, _ = w.req("POST", "/wopi/files/%s?access_token=%s" % (dx["id"], token), headers={"X-WOPI-Override": "LOCK", "X-WOPI-Lock": "L1"})
    check(st == 200, "WOPI LOCK")
    st, _, h2 = w.req("POST", "/wopi/files/%s?access_token=%s" % (dx["id"], token), headers={"X-WOPI-Override": "LOCK", "X-WOPI-Lock": "L2"})
    check(st == 409 and h2.get("X-WOPI-Lock") == "L1", "чужая блокировка → 409")
    st, _ = a.j("PUT", "/api/files/%s/content" % dx["id"], raw=b"x")
    check(st == 423, "пока документ открыт в редакторе, перезапись через API запрещена (423)")
    newdoc = minimal_docx("Отредактировано в Collabora")
    st, _, h3 = w.req("POST", "/wopi/files/%s/contents?access_token=%s" % (dx["id"], token), raw=newdoc, headers={"X-WOPI-Override": "PUT", "X-WOPI-Lock": "L1"})
    check(st == 200 and h3.get("X-WOPI-ItemVersion"), "WOPI PutFile с блокировкой")
    st, body, _ = a.req("GET", "/api/files/%s/content" % dx["id"])
    check(body == newdoc, "сохранённое содержимое на месте")
    st, _, _ = w.req("POST", "/wopi/files/%s?access_token=%s" % (dx["id"], token), headers={"X-WOPI-Override": "UNLOCK", "X-WOPI-Lock": "L1"})
    check(st == 200, "WOPI UNLOCK")
    st, _, _ = w.req("GET", "/wopi/files/%s?access_token=%s" % (doc["id"], token))
    check(st == 401, "токен одного файла не подходит к другому")
else:
    print("SKIP WOPI — сервер запущен без CB_WOPI_URL")

# ---------- метрики ----------
st, body, _ = Client().req("GET", "/metrics")
check(st == 200 and b"cloudbox_users" in body and b"cloudbox_http_requests_total" in body, "метрики Prometheus")

print("\nИТОГО ошибок:", fails)
sys.exit(1 if fails else 0)
