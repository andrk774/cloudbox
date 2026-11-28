"""Печатает новый пароль приложения: python3 scripts/app_password.py URL ЛОГИН ПАРОЛЬ [НАЗВАНИЕ]"""
import http.cookiejar, json, sys, time, urllib.error, urllib.request

base, user, password = sys.argv[1:4]
name = sys.argv[4] if len(sys.argv) > 4 else "cli"
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def call(method, path, body):
    req = urllib.request.Request(base + path, data=json.dumps(body).encode(), method=method, headers={"Content-Type": "application/json"})
    return json.loads(op.open(req).read())


# Integration scenarios share the server's per-IP login budget. Wait only for
# 429; wrong credentials and other failures must still stop the suite at once.
deadline = time.monotonic() + 310
while True:
    try:
        call("POST", "/api/auth/login", {"username": user, "password": password})
        break
    except urllib.error.HTTPError as e:
        if e.code != 429 or time.monotonic() >= deadline:
            raise
        time.sleep(5)
print(call("POST", "/api/me/app-passwords", {"name": name})["password"])
