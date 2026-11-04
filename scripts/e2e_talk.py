"""Сквозной тест чата и сигнализации звонков CloudBox.

    CB_URL=http://127.0.0.1:8080 python3 scripts/e2e_talk.py

Работает и на чистой базе, и после e2e.py/e2e2.py. Входит через Basic-auth, а не /api/auth/login:
у входа по паролю лимит 10 попыток за 5 минут с IP, и в CI его уже расходуют другие сценарии.
"""
import base64, json, os, queue, sys, threading, time, urllib.error, urllib.request

B = os.environ.get("CB_URL", "http://127.0.0.1:8080")
fails = 0


def check(cond, msg):
    global fails
    print(("OK   " if cond else "FAIL ") + msg)
    if not cond:
        fails += 1


class User:
    def __init__(self, name, password):
        self.name, self.auth = name, "Basic " + base64.b64encode(f"{name}:{password}".encode()).decode()
        self.id = None

    def req(self, method, path, body=None):
        data = json.dumps(body).encode() if body is not None else None
        h = {"Authorization": self.auth}
        if data is not None:
            h["Content-Type"] = "application/json"
        r = urllib.request.Request(B + path, data=data, method=method, headers=h)
        try:
            with urllib.request.urlopen(r, timeout=15) as resp:
                raw = resp.read()
                return resp.status, json.loads(raw) if raw else None
        except urllib.error.HTTPError as e:
            raw = e.read()
            try:
                return e.code, json.loads(raw)
            except Exception:
                return e.code, raw


class Events:
    """Поток Server-Sent Events в отдельном потоке: события складываются в очередь."""

    def __init__(self, user):
        self.q = queue.Queue()
        self.backlog = []
        self.user = user
        req = urllib.request.Request(B + "/api/talk/events", headers={"Authorization": user.auth})
        self.resp = urllib.request.urlopen(req, timeout=60)
        self.ctype = self.resp.headers.get("Content-Type", "")
        threading.Thread(target=self._read, daemon=True).start()
        self.hello = self.wait("hello")
        self.peer = self.hello["peer"] if self.hello else None

    def _read(self):
        ev, data = None, []
        try:
            for raw in self.resp:
                line = raw.decode().rstrip("\n").rstrip("\r")
                if line.startswith("event: "):
                    ev = line[7:]
                elif line.startswith("data: "):
                    data.append(line[6:])
                elif line == "" and ev:
                    self.q.put((ev, json.loads("\n".join(data))))
                    ev, data = None, []
        except Exception:
            pass

    def wait(self, typ, cond=lambda d: True, timeout=5):
        """Ждёт событие typ, подходящее под cond. Неподходящие события не теряются, а копятся
        в backlog: сервер может прислать их в любом порядке (например, сообщение раньше состояния звонка)."""
        for i, (ev, d) in enumerate(self.backlog):
            if ev == typ and cond(d):
                del self.backlog[i]
                return d
        end = time.time() + timeout
        while time.time() < end:
            try:
                ev, d = self.q.get(timeout=max(0.05, end - time.time()))
            except queue.Empty:
                break
            if ev == typ and cond(d):
                return d
            self.backlog.append((ev, d))
        return None

    def drain(self):
        """Забыть всё, что пришло до этого момента: следующая проверка увидит только новые события."""
        time.sleep(0.3)
        self.backlog.clear()
        while not self.q.empty():
            self.q.get_nowait()

    def none(self, typ, cond=lambda d: True, timeout=1.0):
        return self.wait(typ, cond, timeout) is None

    def close(self):
        self.resp.close()


# ---------- пользователи ----------
admin = User("andrey", "secret123")
st, me = admin.req("GET", "/api/me")
if st == 401:  # чистая база — регистрируем администратора (регистрация не входит в лимит входов)
    st, _ = admin.req("POST", "/api/auth/register", {"username": "andrey", "password": "secret123"})
    st, me = admin.req("GET", "/api/me")
check(st == 200 and me["user"]["is_admin"], "администратор (Basic-auth)")
admin.id = me["user"]["id"]

ana, bob, eve = User("talk_ana", "ana-12345"), User("talk_bob", "bob-12345"), User("talk_eve", "eve-12345")
for u, pw in ((ana, "ana-12345"), (bob, "bob-12345"), (eve, "eve-12345")):
    admin.req("POST", "/api/admin/users", {"username": u.name, "password": pw})
ids = {x["username"]: x["id"] for x in admin.req("GET", "/api/admin/users")[1]}
for u in (ana, bob, eve):
    u.id = ids.get(u.name)
check(all(u.id for u in (ana, bob, eve)), "созданы участники чата")

st, _ = User("talk_ana", "wrong").req("GET", "/api/talk/rooms")
check(st == 401, "без авторизации чат недоступен")

ea, eb, ee = Events(ana), Events(bob), Events(eve)
check("text/event-stream" in ea.ctype and ea.peer and eb.peer and ea.peer != eb.peer, "поток событий: hello с id вкладки")

# ---------- личная переписка ----------
st, dm = ana.req("POST", "/api/talk/direct", {"user_id": bob.id})
check(st == 200 and dm["kind"] == "direct" and dm["name"] == "talk_bob", "личная переписка создана")
st, dm2 = bob.req("POST", "/api/talk/direct", {"user_id": ana.id})
check(st == 200 and dm2["id"] == dm["id"] and dm2["name"] == "talk_ana", "у пары пользователей одна переписка")
st, _ = ana.req("POST", "/api/talk/direct", {"user_id": ana.id})
check(st == 400, "написать самому себе нельзя")

st, m1 = ana.req("POST", f"/api/talk/rooms/{dm['id']}/messages", {"body": "  Привет, Боб! <b>не жирный</b>  "})
check(st == 201 and m1["body"] == "Привет, Боб! <b>не жирный</b>" and m1["username"] == "talk_ana", "сообщение отправлено (пробелы обрезаны, HTML как текст)")
got = eb.wait("message", lambda d: d["id"] == m1["id"])
check(got is not None and got["body"] == m1["body"], "получатель получил сообщение в реальном времени")
check(ee.none("message", lambda d: d["room_id"] == dm["id"]), "посторонний не получает чужие сообщения")

st, rooms = bob.req("GET", "/api/talk/rooms")
r = next((x for x in rooms if x["id"] == dm["id"]), None)
check(r is not None and r["unread"] == 1 and r["last"]["id"] == m1["id"], "у получателя 1 непрочитанное")
check(bob.req("GET", "/api/me")[1]["talk_unread"] == 1, "счётчик непрочитанного в /api/me")
st, rooms = ana.req("GET", "/api/talk/rooms")
check(next(x for x in rooms if x["id"] == dm["id"])["unread"] == 0, "своё сообщение не считается непрочитанным")
st, _ = bob.req("POST", f"/api/talk/rooms/{dm['id']}/read", {"last_id": m1["id"]})
check(st == 200 and bob.req("GET", "/api/me")[1]["talk_unread"] == 0, "прочитано — счётчик обнулился")

for path in (f"/api/talk/rooms/{dm['id']}", f"/api/talk/rooms/{dm['id']}/messages"):
    st, _ = eve.req("GET", path)
    check(st == 404, f"посторонний: {path.split('/')[-1]} → 404")
st, _ = eve.req("POST", f"/api/talk/rooms/{dm['id']}/messages", {"body": "влез"})
check(st == 404, "посторонний не может писать в чужую комнату")
st, _ = ana.req("POST", f"/api/talk/rooms/{dm['id']}/messages", {"body": "   "})
check(st == 400, "пустое сообщение → 400")
st, _ = ana.req("POST", f"/api/talk/rooms/{dm['id']}/messages", {"body": "я" * 4001})
check(st == 400, "сообщение длиннее 4000 символов → 400")

# история с пагинацией
for i in range(5):
    ana.req("POST", f"/api/talk/rooms/{dm['id']}/messages", {"body": f"сообщение {i}"})
st, page = bob.req("GET", f"/api/talk/rooms/{dm['id']}/messages?limit=3")
check(st == 200 and [m["body"] for m in page] == ["сообщение 2", "сообщение 3", "сообщение 4"], "последняя страница истории по порядку")
st, older = bob.req("GET", f"/api/talk/rooms/{dm['id']}/messages?limit=3&before={page[0]['id']}")
check(st == 200 and older[-1]["body"] == "сообщение 1" and older[0]["id"] == m1["id"], "более ранние сообщения")

# ---------- групповая комната ----------
st, g = ana.req("POST", "/api/talk/rooms", {"name": "Поход на Таганай", "user_ids": [bob.id]})
check(st == 201 and g["kind"] == "group" and {m["id"] for m in g["members"]} == {ana.id, bob.id}, "комната создана с участниками")
check(eb.wait("room", lambda d: d["id"] == g["id"]) is not None, "участник получил событие о новой комнате")
st, _ = ana.req("POST", "/api/talk/rooms", {"name": "  ", "user_ids": []})
check(st == 400, "комната без названия → 400")
st, gm = bob.req("POST", f"/api/talk/rooms/{g['id']}/members", {"user_ids": [eve.id]})
check(st == 200 and eve.id in {m["id"] for m in gm["members"]}, "участник пригласил ещё одного")
sysmsg = ee.wait("message", lambda d: d["room_id"] == g["id"] and d["kind"] == "system")
check(sysmsg is not None and "добавил" in sysmsg["body"], "системное сообщение о добавлении")
st, _ = bob.req("DELETE", f"/api/talk/rooms/{g['id']}/members/{eve.id}")
check(st == 403, "исключать может только создатель")
st, _ = ana.req("PATCH", f"/api/talk/rooms/{g['id']}", {"name": "Таганай-2026"})
check(st == 200 and bob.req("GET", f"/api/talk/rooms/{g['id']}")[1]["name"] == "Таганай-2026", "переименование")
st, _ = ana.req("POST", f"/api/talk/rooms/{dm['id']}/members", {"user_ids": [eve.id]})
check(st == 400, "в личную переписку участников не добавить")

# ---------- звонок: сигнализация ----------
st, cfg = ana.req("GET", "/api/talk/config")
check(st == 200 and isinstance(cfg["ice_servers"], list) and cfg["max_call_peers"] == 6, "конфигурация ICE")
st, _ = ana.req("POST", f"/api/talk/rooms/{g['id']}/call/join", {"peer": eb.peer})
check(st == 409, "нельзя войти в звонок чужой вкладкой")
st, j1 = ana.req("POST", f"/api/talk/rooms/{g['id']}/call/join", {"peer": ea.peer})
check(st == 200 and j1["participants"] == [], "первый участник начал звонок")
ring = eb.wait("call", lambda d: d["room"] == g["id"] and d["active"])
check(ring is not None and ring["participants"][0]["username"] == "talk_ana", "участники комнаты видят начатый звонок")
check(eb.wait("message", lambda d: d["room_id"] == g["id"] and "начал(а) звонок" in d["body"]) is not None, "системное сообщение о звонке")
st, j2 = bob.req("POST", f"/api/talk/rooms/{g['id']}/call/join", {"peer": eb.peer})
check(st == 200 and [p["peer"] for p in j2["participants"]] == [ea.peer], "второй участник получил список, кому звонить")

offer = {"type": "offer", "sdp": "v=0 fake"}
st, _ = bob.req("POST", f"/api/talk/rooms/{g['id']}/call/signal", {"peer": eb.peer, "to": ea.peer, "type": "offer", "data": offer})
sig = ea.wait("signal")
check(st == 200 and sig and sig["from"] == eb.peer and sig["data"] == offer and sig["username"] == "talk_bob", "offer доставлен нужной вкладке")
st, _ = ana.req("POST", f"/api/talk/rooms/{g['id']}/call/signal", {"peer": ea.peer, "to": eb.peer, "type": "answer", "data": {"type": "answer"}})
check(st == 200 and eb.wait("signal", lambda d: d["type"] == "answer") is not None, "answer доставлен")
st, _ = eve.req("POST", f"/api/talk/rooms/{g['id']}/call/signal", {"peer": ee.peer, "to": ea.peer, "type": "offer", "data": offer})
check(st == 409 and ea.none("signal"), "не участник звонка не может слать сигналы")
st, _ = eve.req("POST", f"/api/talk/rooms/{g['id']}/call/signal", {"peer": ea.peer, "to": eb.peer, "type": "offer", "data": offer})
check(st == 409, "нельзя слать сигналы от чужой вкладки")
st, _ = bob.req("POST", f"/api/talk/rooms/{g['id']}/call/signal", {"peer": eb.peer, "to": ea.peer, "type": "evil", "data": {}})
check(st == 400, "неизвестный тип сигнала → 400")

# исключённый из комнаты вылетает из звонка
st, _ = eve.req("POST", f"/api/talk/rooms/{g['id']}/call/join", {"peer": ee.peer})
check(st == 200, "третий участник в звонке")
ea.drain()  # в очереди есть старое состояние «ana + bob» — оно совпало бы с ожидаемым после исключения
st, _ = ana.req("DELETE", f"/api/talk/rooms/{g['id']}/members/{eve.id}")
check(st == 200, "создатель исключил участника")
check(ee.wait("room_removed", lambda d: d["id"] == g["id"]) is not None, "исключённый получил room_removed")
st_call = ea.wait("call", lambda d: d["room"] == g["id"] and len(d["participants"]) == 2)
check(st_call is not None and ee.peer not in [p["peer"] for p in st_call["participants"]], "исключённый выведен из звонка")
st, _ = eve.req("GET", f"/api/talk/rooms/{g['id']}/messages")
check(st == 404, "исключённый больше не читает комнату")

# закрытие вкладки = выход из звонка
ea.drain()  # старое состояние «только ana» (начало звонка) не должно засчитаться
eb.close()
left = ea.wait("call", lambda d: d["room"] == g["id"] and len(d["participants"]) == 1, timeout=8)
check(left is not None, "закрытая вкладка выведена из звонка")
st, _ = ana.req("POST", f"/api/talk/rooms/{g['id']}/call/leave", {"peer": ea.peer})
ended = ea.wait("message", lambda d: d["room_id"] == g["id"] and d["body"].startswith("Звонок завершён"))
check(st == 200 and ended is not None, "звонок завершён, длительность в чате: " + (ended or {}).get("body", ""))
check(not ana.req("GET", f"/api/talk/rooms/{g['id']}")[1].get("call"), "звонка в комнате больше нет")

# выход последнего удаляет комнату
st, solo = ana.req("POST", "/api/talk/rooms", {"name": "Одиночка", "user_ids": []})
st, _ = ana.req("DELETE", f"/api/talk/rooms/{solo['id']}/members/me")
st2, _ = ana.req("GET", f"/api/talk/rooms/{solo['id']}")
check(st == 200 and st2 == 404, "комната удалена, когда вышел последний")

for e in (ea, ee):
    e.close()
print("\nИТОГО ошибок:", fails)
sys.exit(1 if fails else 0)
