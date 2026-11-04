"""Сквозной тест почты CloudBox (веб-клиент IMAP/SMTP).

Нужен тестовый почтовый сервер и CloudBox с разрешёнными локальными серверами:
    go run ./internal/mail/mailtest/cmd/mailtest -imap 127.0.0.1:1143 -smtp 127.0.0.1:1025 &
    CB_SECRET=... CB_MAIL_ALLOW_PRIVATE=true CB_MAIL_EXTRA_PORTS=1143,1025 ./cloudbox &
    CB_URL=http://127.0.0.1:8080 python3 scripts/e2e_mail.py

Входит через Basic-auth: у входа по паролю лимит 10 попыток за 5 минут с IP, его расходуют другие тесты.
"""
import base64, json, os, sys, uuid, urllib.error, urllib.parse, urllib.request

B = os.environ.get("CB_URL", "http://127.0.0.1:8080")
IMAP = int(os.environ.get("MAILTEST_IMAP", "1143"))
SMTP = int(os.environ.get("MAILTEST_SMTP", "1025"))
fails = 0


def check(cond, msg):
    global fails
    print(("OK   " if cond else "FAIL ") + msg)
    if not cond:
        fails += 1


class User:
    def __init__(self, name, pw):
        self.name, self.auth = name, "Basic " + base64.b64encode(f"{name}:{pw}".encode()).decode()

    def req(self, method, path, body=None, raw=None, ctype=None):
        h = {"Authorization": self.auth}
        data = None
        if body is not None:
            data, h["Content-Type"] = json.dumps(body).encode(), "application/json"
        if raw is not None:
            data, h["Content-Type"] = raw, ctype or "application/octet-stream"
        r = urllib.request.Request(B + path, data=data, method=method, headers=h)
        try:
            with urllib.request.urlopen(r, timeout=30) as resp:
                return resp.status, resp.read(), resp.headers
        except urllib.error.HTTPError as e:
            return e.code, e.read(), e.headers

    def j(self, method, path, body=None, **kw):
        st, b, _ = self.req(method, path, body, **kw)
        try:
            return st, json.loads(b)
        except Exception:
            return st, b

    def send(self, acc, message, files=()):
        """multipart/form-data: поле message (JSON) и файлы file."""
        bnd = uuid.uuid4().hex
        parts = [f'--{bnd}\r\nContent-Disposition: form-data; name="message"\r\n\r\n'.encode() + json.dumps(message).encode() + b"\r\n"]
        for name, data in files:
            parts.append(f'--{bnd}\r\nContent-Disposition: form-data; name="file"; filename="{name}"\r\nContent-Type: text/plain\r\n\r\n'.encode() + data + b"\r\n")
        body = b"".join(parts) + f"--{bnd}--\r\n".encode()
        st, b, _ = self.req("POST", f"/api/mail/accounts/{acc}/send", raw=body, ctype="multipart/form-data; boundary=" + bnd)
        try:
            return st, json.loads(b)
        except Exception:
            return st, b


def q(**kw):
    return "?" + urllib.parse.urlencode(kw)


admin = User("andrey", "secret123")
st, _ = admin.j("GET", "/api/me")
if st == 401:
    admin.j("POST", "/api/auth/register", {"username": "andrey", "password": "secret123"})
ana, bob = User("mail_ana", "ana-12345"), User("mail_bob", "bob-12345")
for u, pw in ((ana, "ana-12345"), (bob, "bob-12345")):
    admin.j("POST", "/api/admin/users", {"username": u.name, "password": pw})

st, info = ana.j("GET", "/api/mail")
check(st == 200 and info["enabled"], "почта включена (CB_SECRET задан)")
st, pre = ana.j("GET", "/api/mail/preset?email=ivan@ya.ru")
check(pre.get("known") and pre["preset"]["imap_host"] == "imap.yandex.ru", "подсказка настроек для Яндекса")

acc_in = {"email": "ana@example.org", "name": "Аня", "login": "username", "password": "password",
          "imap_host": "127.0.0.1", "imap_port": IMAP, "imap_sec": "none", "smtp_host": "127.0.0.1", "smtp_port": SMTP, "smtp_sec": "none"}

# защита от SSRF: адрес задаёт пользователь, но подключаться к служебным адресам нельзя
st, e = ana.j("POST", "/api/mail/accounts", {**acc_in, "imap_host": "169.254.169.254", "imap_port": 993, "imap_sec": "tls"})
check(st == 400 and "публичным" in e.get("error", ""), "метаданные облака (169.254.169.254) недоступны")
st, e = ana.j("POST", "/api/mail/accounts", {**acc_in, "imap_port": 5432})
check(st == 400 and "порт" in e.get("error", ""), "чужой порт (5432, PostgreSQL) недоступен")
st, e = ana.j("POST", "/api/mail/accounts", {**acc_in, "password": "wrong"})
check(st == 400 and "пароль" in e.get("error", ""), "неверный пароль ящика — понятная ошибка, ящик не сохранён")

st, acc = ana.j("POST", "/api/mail/accounts", acc_in)
check(st == 201 and acc["email"] == "ana@example.org" and "password" not in acc, "ящик подключён, пароль в ответе не возвращается")
aid = acc["id"]
st, accs = ana.j("GET", "/api/mail/accounts")
check(st == 200 and len(accs) == 1 and all("password" not in a and "password_enc" not in a for a in accs), "список ящиков без паролей")
st, bacc = bob.j("POST", "/api/mail/accounts", {**acc_in, "email": "bob@example.org"})
check(st == 201, "у второго пользователя свой ящик")

st, _ = bob.j("GET", f"/api/mail/accounts/{aid}/folders")
check(st == 404, "чужой ящик недоступен (404)")
st, _ = bob.j("DELETE", f"/api/mail/accounts/{aid}")
check(st == 404, "чужой ящик нельзя отключить")

st, folders = ana.j("GET", f"/api/mail/accounts/{aid}/folders")
names = {f["name"]: f.get("special") for f in folders} if st == 200 else {}
check(names.get("INBOX") == "inbox" and names.get("Sent") == "sent" and names.get("Trash") == "trash" and "Проекты" in names,
      "папки, включая служебные и кириллическую")

# отправка с вложением с компьютера и файлом из CloudBox
st, cf = ana.j("POST", "/api/files/upload" + q(name="смета.txt"), raw="Итого: 100 ₽".encode())
check(st == 201, "файл в CloudBox для вложения")
msg = {"to": ["Аня <ana@example.org>"], "cc": [], "bcc": [], "subject": "Тест из CloudBox", "text": "Привет!\nВторая строка",
       "cloud_files": [cf["id"]]}
st, r = ana.send(aid, msg, [("план.txt", "маршрут через перевал".encode())])
check(st == 200, f"письмо отправлено ({st} {r})")
st, r = bob.send(bacc["id"], {**msg, "cloud_files": [cf["id"]]})
check(st == 404, "чужой файл CloudBox прикрепить нельзя")
st, r = ana.send(aid, {**msg, "to": [], "cloud_files": []})
check(st == 400, "без получателей → 400")
st, r = ana.send(aid, {**msg, "subject": "x\r\nBcc: evil@example.org", "cloud_files": []})
check(st == 400, "подстановка заголовка через тему отклонена")

st, page = ana.j("GET", f"/api/mail/accounts/{aid}/messages" + q(folder="INBOX"))
m = next((x for x in page.get("messages", []) if x["subject"] == "Тест из CloudBox"), None) if st == 200 else None
check(m is not None and not m["seen"] and m["attachments"], "письмо пришло во «Входящие», непрочитанное, с вложениями")
st, full = ana.j("GET", f"/api/mail/accounts/{aid}/message" + q(folder="INBOX", uid=m["uid"]))
atts = {a["filename"]: a for a in full.get("attachments", [])} if st == 200 else {}
check(st == 200 and "Вторая строка" in full["text"] and set(atts) == {"план.txt", "смета.txt"}, "письмо открыто: текст и оба вложения")
check(full["from"][0]["email"] == "ana@example.org" and full["from"][0]["name"] == "Аня", "отправитель с именем")
st, page = ana.j("GET", f"/api/mail/accounts/{aid}/messages" + q(folder="INBOX"))
check(next(x for x in page["messages"] if x["uid"] == m["uid"])["seen"], "открытое письмо стало прочитанным")

a = atts["план.txt"]
st, data, hdr = ana.req("GET", f"/api/mail/accounts/{aid}/attachment" + q(folder="INBOX", uid=m["uid"], n=a["index"]))
check(st == 200 and data.decode() == "маршрут через перевал" and "attachment" in hdr.get("Content-Disposition", "")
      and "sandbox" in hdr.get("Content-Security-Policy", ""), "вложение скачивается безопасно (attachment + sandbox)")
st, saved = ana.j("POST", f"/api/mail/accounts/{aid}/attachment/save", {"folder": "INBOX", "uid": m["uid"], "n": atts["смета.txt"]["index"], "parent": ""})
st2, body, _ = ana.req("GET", f"/api/files/{saved['id']}/content") if st == 201 else (0, b"", {})
check(st == 201 and body.decode() == "Итого: 100 ₽", "вложение сохранено в CloudBox")

st, _ = ana.j("POST", f"/api/mail/accounts/{aid}/flag", {"folder": "INBOX", "uids": [m["uid"]], "flag": "flagged", "on": True})
check(st == 200, "отмечено важным")
st, _ = ana.j("POST", f"/api/mail/accounts/{aid}/flag", {"folder": "INBOX", "uids": [m["uid"]], "flag": "deleted", "on": True})
check(st == 400, "произвольный флаг поставить нельзя")
st, _ = ana.j("POST", f"/api/mail/accounts/{aid}/move", {"folder": "INBOX", "uids": [m["uid"]], "dest": "Проекты"})
st2, pp = ana.j("GET", f"/api/mail/accounts/{aid}/messages" + q(folder="Проекты"))
check(st == 200 and any(x["subject"] == "Тест из CloudBox" and x["flagged"] for x in pp["messages"]), "перемещено в «Проекты» с отметкой")
uid2 = next(x["uid"] for x in pp["messages"] if x["subject"] == "Тест из CloudBox")
st, r = ana.j("POST", f"/api/mail/accounts/{aid}/delete", {"folder": "Проекты", "uids": [uid2]})
st2, tp = ana.j("GET", f"/api/mail/accounts/{aid}/messages" + q(folder="Trash"))
check(st == 200 and not r["permanent"] and any(x["subject"] == "Тест из CloudBox" for x in tp["messages"]), "удалённое — в корзине почты")

st, sp = ana.j("GET", f"/api/mail/accounts/{aid}/messages" + q(folder="Sent"))
check(st == 200 and any(x["subject"] == "Тест из CloudBox" for x in sp["messages"]), "копия в «Отправленных»")
st, sr = ana.j("GET", f"/api/mail/accounts/{aid}/messages" + q(folder="Trash", q="Тест"))
check(st == 200 and sr["total"] >= 1, "поиск по письмам на сервере")

st, _ = ana.j("DELETE", f"/api/mail/accounts/{aid}")
st2, _ = ana.j("GET", f"/api/mail/accounts/{aid}/folders")
check(st == 200 and st2 == 404, "ящик отключён")

print("\nИТОГО ошибок:", fails)
sys.exit(1 if fails else 0)
