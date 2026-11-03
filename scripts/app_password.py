"""Печатает новый пароль приложения: python3 scripts/app_password.py URL ЛОГИН ПАРОЛЬ [НАЗВАНИЕ]"""
import http.cookiejar, json, sys, urllib.request

base, user, password = sys.argv[1:4]
name = sys.argv[4] if len(sys.argv) > 4 else "cli"
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))


def call(method, path, body):
    req = urllib.request.Request(base + path, data=json.dumps(body).encode(), method=method, headers={"Content-Type": "application/json"})
    return json.loads(op.open(req).read())


call("POST", "/api/auth/login", {"username": user, "password": password})
print(call("POST", "/api/me/app-passwords", {"name": name})["password"])
