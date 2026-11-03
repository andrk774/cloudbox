#!/bin/sh
# Проверка cloudbox-sync против запущенного сервера.
#   CB_URL=http://127.0.0.1:8080 CB_USER=andrey CB_PASS=<пароль приложения> SYNC=./cloudbox-sync sh scripts/sync-test.sh
set -u
URL=${CB_URL:-http://127.0.0.1:8080}; U=$CB_USER; P=$CB_PASS; SYNC=${SYNC:-cloudbox-sync}
L=$(mktemp -d); FAILS=0
ok()   { echo "OK   $1"; }
bad()  { echo "FAIL $1"; FAILS=$((FAILS+1)); }
chk()  { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
api()  { curl -s -u "$U:$P" "$@"; }
run()  { CLOUDBOX_PASSWORD=$P $SYNC -server "$URL" -user "$U" -local "$L" -remote "/Синхронизация" 2>&1; }
tree() { api "$URL/api/sync/tree?root=$ROOT" | python3 -c 'import json,sys; print("\n".join(sorted(e["path"] for e in json.load(sys.stdin)["entries"])))' 2>/dev/null; }
nid()  { api "$URL/api/sync/tree?root=$ROOT" | python3 -c "import json,sys; print([e['id'] for e in json.load(sys.stdin)['entries'] if e['path']=='$1'][0])"; }

# 1. первичная выгрузка
mkdir -p "$L/Документы/Отчёты" "$L/Фото"
echo "привет" > "$L/readme.txt"
echo "отчёт 1" > "$L/Документы/Отчёты/q1.txt"
head -c 20000000 /dev/urandom > "$L/Фото/большой.bin"
run > /dev/null
ROOT=$(api "$URL/api/files" | python3 -c 'import json,sys; print([n["id"] for n in json.load(sys.stdin)["items"] if n["name"]=="Синхронизация"][0])')
chk "первая синхронизация: всё загружено" '[ "$(tree | tr "\n" " ")" = "readme.txt Документы Документы/Отчёты Документы/Отчёты/q1.txt Фото Фото/большой.bin " ]'
SUM_L=$(sha256sum "$L/Фото/большой.bin" | cut -c1-64)
SUM_R=$(api "$URL/api/sync/tree?root=$ROOT" | python3 -c 'import json,sys; print([e["sha256"] for e in json.load(sys.stdin)["entries"] if e["path"].endswith("большой.bin")][0])')
chk "большой файл загружен частями без искажений" '[ "$SUM_L" = "$SUM_R" ]'
OUT=$(run)
chk "повторный проход ничего не делает" '[ -z "$OUT" ]'

# 2. изменения на сервере → скачивание
DOCS=$(nid "Документы")
printf 'с сервера' | api -X POST --data-binary @- -H 'Content-Type: application/octet-stream' "$URL/api/files/upload?parent=$DOCS&name=new.txt" > /dev/null
run > /dev/null
chk "новый файл с сервера скачан" '[ "$(cat "$L/Документы/new.txt")" = "с сервера" ]'

# 3. локальное изменение → новая версия на сервере
echo "привет, мир" > "$L/readme.txt"
run > /dev/null
RID=$(nid readme.txt)
chk "локальная правка загружена" '[ "$(api "$URL/api/files/$RID/content")" = "привет, мир" ]'
chk "на сервере появилась версия" '[ "$(api "$URL/api/files/$RID/versions" | python3 -c "import json,sys; print(len(json.load(sys.stdin)))")" = "2" ]'

# 4. удаление локально → корзина на сервере
rm "$L/Документы/new.txt"
run > /dev/null
chk "локальное удаление ушло в корзину сервера" 'api "$URL/api/trash" | grep -q "new.txt"'

# 5. удаление на сервере → локальная корзина
QID=$(nid "Документы/Отчёты/q1.txt")
api -X DELETE "$URL/api/files/$QID" > /dev/null
run > /dev/null
chk "удалённый на сервере файл убран локально" '[ ! -e "$L/Документы/Отчёты/q1.txt" ]'
chk "…и сохранён в .cloudbox-trash" '[ -n "$(find "$L/.cloudbox-trash" -name q1.txt)" ]'

# 6. удаление папки целиком
rm -rf "$L/Документы"
run > /dev/null
chk "удалённая локально папка — в корзине сервера" '! tree | grep -q "^Документы"'

# 7. конфликт: правка с двух сторон
RID=$(nid readme.txt)
printf 'серверная версия' | api -X PUT --data-binary @- "$URL/api/files/$RID/content" > /dev/null
echo "локальная версия" > "$L/readme.txt"
run > /dev/null
chk "при конфликте берётся серверная версия" '[ "$(cat "$L/readme.txt")" = "серверная версия" ]'
CONF=$(ls "$L" | grep "конфликт" | head -1)
chk "локальная версия сохранена как конфликтная копия" '[ -n "$CONF" ] && [ "$(cat "$L/$CONF")" = "локальная версия" ]'
run > /dev/null
chk "конфликтная копия загружена на сервер" 'tree | grep -q "конфликт"'

# 8. dry-run ничего не меняет
echo "x" > "$L/dry.txt"
CLOUDBOX_PASSWORD=$P $SYNC -server "$URL" -user "$U" -local "$L" -remote "/Синхронизация" -dry-run > /dev/null 2>&1
chk "dry-run не загружает" '! tree | grep -q "dry.txt"'

rm -rf "$L"
echo
echo "ИТОГО ошибок: $FAILS"
[ $FAILS -eq 0 ]
