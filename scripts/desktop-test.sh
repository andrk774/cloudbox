#!/bin/sh
# Проверка графического клиента cloudbox-desktop против запущенного сервера (без окна и трея).
#   CB_URL=http://127.0.0.1:8080 CB_USER=andrey CB_PASS=<пароль приложения> DESKTOP=./cloudbox-desktop sh scripts/desktop-test.sh
set -u
URL=${CB_URL:-http://127.0.0.1:8080}; U=$CB_USER; P=$CB_PASS; DESKTOP=${DESKTOP:-cloudbox-desktop}
D=$(mktemp -d); L="$D/папка"; FAILS=0
ok()  { echo "OK   $1"; }
bad() { echo "FAIL $1"; FAILS=$((FAILS+1)); }
chk() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
js()  { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }

"$DESKTOP" -no-browser -config "$D/desktop.json" > "$D/desktop.log" 2>&1 &
PID=$!
for i in $(seq 50); do [ -s "$D/desktop.lock" ] && break; sleep 0.2; done
LINK=$(cat "$D/desktop.lock" 2>/dev/null)
BASE=${LINK%%/?t=*}
HOST=${BASE#http://}
chk "клиент запущен, адрес окна в lock-файле" '[ -n "$LINK" ]'

C="$D/cookies"
chk "без токена интерфейс закрыт" '[ "$(curl -s -o /dev/null -w "%{http_code}" "$BASE/api/status")" = 403 ]'
chk "вход по ссылке с токеном" '[ "$(curl -s -c "$C" -o /dev/null -w "%{http_code}" "$LINK")" = 302 ]'
chk "чужой Host отклонён (DNS rebinding)" '[ "$(curl -s -b "$C" -H "Host: evil.example" -o /dev/null -w "%{http_code}" "$BASE/api/status")" = 403 ]'
chk "POST с чужого сайта отклонён (CSRF)" '[ "$(curl -s -b "$C" -X POST -H "Origin: https://evil.example" -o /dev/null -w "%{http_code}" "$BASE/api/sync")" = 403 ]'
post() { curl -s -b "$C" -H "Origin: $BASE" -H "Content-Type: application/json" -X POST "$BASE$1" -d "$2"; }
chk "до настройки — состояние setup" '[ "$(curl -s -b "$C" "$BASE/api/status" | js "d[\"state\"]")" = setup ]'

OUT=$(post /api/config "{\"server\":\"$URL\",\"user\":\"$U\",\"password\":\"неверный\",\"local\":\"$L\",\"remote\":\"/Десктоп\"}")
chk "неверный пароль приложения — понятная ошибка" 'echo "$OUT" | grep -q "пароль"'

mkdir -p "$L/Отчёты"
echo "из графического клиента" > "$L/Отчёты/q1.txt"
OUT=$(post /api/config "{\"server\":\"$URL\",\"user\":\"$U\",\"password\":\"$P\",\"local\":\"$L\",\"remote\":\"/Десктоп\",\"interval_sec\":10}")
chk "настройки сохранены" 'echo "$OUT" | grep -q "\"has_password\":true"'
chk "пароль не записан в настройки как есть (Windows: DPAPI; Linux, macOS: base64 в файле с правами 0600)" '! grep -q "$P" "$D/desktop.json"'

waitok() { for i in $(seq 60); do S=$(curl -s -b "$C" "$BASE/api/status"); [ "$(echo "$S" | js "d.get(\"last_ok\") and d[\"state\"]")" = idle ] && [ "$(echo "$S" | js "d[\"last_ok\"]")" != "$1" ] && return 0; sleep 0.5; done; return 1; }
chk "первая синхронизация прошла" 'waitok none'
LAST=$(curl -s -b "$C" "$BASE/api/status" | js 'd["last_ok"]')
ROOT=$(curl -s -u "$U:$P" "$URL/api/files" | js '[n["id"] for n in d["items"] if n["name"]=="Десктоп"][0]')
TREE=$(curl -s -u "$U:$P" "$URL/api/sync/tree?root=$ROOT" | js '" ".join(sorted(e["path"] for e in d["entries"]))')
chk "файл выгружен на сервер" '[ "$TREE" = "Отчёты Отчёты/q1.txt" ]'

# изменение на сервере → скачивается по кнопке «Синхронизировать сейчас»
curl -s -u "$U:$P" --data-binary "с сервера" "$URL/api/files/upload?parent=$ROOT&name=new.txt" > /dev/null
post /api/sync '{}' > /dev/null
chk "«Синхронизировать сейчас» скачал новый файл" 'waitok "$LAST" && [ "$(cat "$L/new.txt" 2>/dev/null)" = "с сервера" ]'

OUT=$(post /api/pause '{"paused":true}')
chk "пауза" 'echo "$OUT" | grep -q "\"state\":\"paused\""'
OUT=$(post /api/pause '{"paused":false}')
chk "продолжить" 'echo "$OUT" | grep -q "\"paused\":false"'

# повторный запуск не поднимает второй клиент
"$DESKTOP" -no-browser -config "$D/desktop.json" > /dev/null 2>&1
chk "второй экземпляр не запускается (одна копия на пользователя)" '[ "$(cat "$D/desktop.lock")" = "$LINK" ] && kill -0 $PID 2>/dev/null'

post /api/quit '{}' > /dev/null
for i in $(seq 25); do kill -0 $PID 2>/dev/null || break; sleep 0.2; done
chk "«Выйти» завершает клиент" '! kill -0 $PID 2>/dev/null'
[ $FAILS -gt 0 ] && { echo "--- журнал клиента"; cat "$D/desktop.log"; }
rm -rf "$D"
echo
echo "ИТОГО ошибок: $FAILS"
exit $FAILS
