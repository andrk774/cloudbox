# CloudBox

Self-hosted облачное хранилище в духе **Nextcloud Files**: один бинарник на Go и PostgreSQL. Веб-интерфейс встроен в бинарник, файлы хранятся на диске или в S3 (Garage, SeaweedFS, Yandex Object Storage, Selectel).

## Возможности

**Файлы**
- Папки любой вложенности, загрузка файлов и целых папок, drag & drop из проводника.
- Перемещение перетаскиванием на папку или на «хлебные крошки».
- Переименование, перемещение, копирование. Копия не дублирует данные на диске: блобы общие.
- Скачивание папки ZIP-архивом.
- Просмотр в браузере: изображения, видео, аудио, PDF и текст, листание стрелками.
- Поиск по имени, раздел «Недавние».

**Версии и корзина**
- Каждая перезапись файла сохраняет предыдущую версию. Число хранимых версий задаётся в `CB_MAX_VERSIONS`.
- Любую версию можно скачать или восстановить.
- Корзина: восстановление на прежнее место или в корень, если папки уже нет. Окончательное удаление и автоочистка через `CB_TRASH_DAYS`.

**Публичные ссылки**
- Ссылка на файл или папку с паролем и сроком действия.
- Для папок можно разрешить гостям загрузку. Загрузка гостя никогда не перезаписывает файлы владельца.
- Счётчик скачиваний.

**WebDAV** (`/dav/`)
- Подключается как сетевой диск в Windows, macOS, Linux и Android, работает с rclone.
- Удаление через WebDAV тоже идёт в корзину.

**Пользователи**
- Первый зарегистрированный пользователь становится администратором.
- Администратор задаёт квоты, сбрасывает пароли, создаёт и удаляет пользователей.
- Пароли приложений для WebDAV-клиентов, их можно отозвать.

**Безопасность**
- Пароли хешируются PBKDF2-SHA256 (310 000 итераций).
- Сессии хранятся в БД в виде хеша токена.
- Защита от CSRF через проверку Origin.
- Ограничение попыток входа.
- Файлы отдаются с `CSP: sandbox`, поэтому загруженный HTML не выполнится.
- Строгая изоляция пользователей.

## Быстрый старт

```bash
cp .env.example .env
# в .env обязательно задайте CB_SECRET (openssl rand -hex 32) и DB_PASSWORD
docker compose up -d
```

Откройте `http://IP-сервера:8080` и зарегистрируйте первого пользователя — он станет администратором.

### С HTTPS (нужен для WebDAV в Windows)

Направьте домен на VPS. Затем пропишите в `.env`:

```
DOMAIN=cloud.example.ru
CB_PUBLIC_URL=https://cloud.example.ru
```

И запустите:

```bash
docker compose --profile https up -d
```

Caddy сам получит сертификат Let's Encrypt. Если на сервере уже стоит nginx, возьмите `deploy/nginx.conf.example`. В нём важно отключить буферизацию и лимит тела запроса.

### S3 / Garage

1. Сгенерируйте секрет (`openssl rand -hex 32`) и впишите его в `deploy/garage.toml` вместо `rpc_secret`.
2. Поднимите Garage и настройте его:

```bash
docker compose --profile s3 up -d garage
docker compose exec garage /garage status                 # скопируйте ID ноды
docker compose exec garage /garage layout assign -z dc1 -c 100G <ID>
docker compose exec garage /garage layout apply --version 1
docker compose exec garage /garage bucket create cloudbox
docker compose exec garage /garage key create cloudbox-key   # покажет Key ID и Secret
docker compose exec garage /garage bucket allow --read --write --owner cloudbox --key cloudbox-key
```

3. В `.env` укажите `CB_STORAGE=s3`, `CB_S3_ACCESS_KEY` и `CB_S3_SECRET_KEY`, затем выполните `docker compose --profile s3 up -d`.

Для Yandex Object Storage задайте `CB_S3_ENDPOINT=https://storage.yandexcloud.net` и `CB_S3_REGION=ru-central1`.

> Перенос данных между `local` и `s3` пока не автоматизирован. Выберите хранилище до начала работы.

## Подключение WebDAV

Адрес: `https://cloud.example.ru/dav/`. Логин — ваше имя пользователя, пароль — основной или пароль приложения из настроек.

| Клиент | Как подключить |
|---|---|
| Windows | Проводник → «Подключить сетевой диск» → адрес (только HTTPS) |
| Linux | `davs://cloud.example.ru/dav/` в Nautilus или Dolphin; `davfs2` для монтирования |
| macOS | Finder → ⌘K |
| Android | Solid Explorer, Total Commander + WebDAV, FolderSync |
| rclone | `rclone config` → `webdav` → `vendor: other` |

Для совместимости адрес `/remote.php/webdav/` перенаправляется на `/dav/`.

## Команды

```bash
docker compose exec app cloudbox useradd -admin -quota-mb 0 admin 'StrongPass123'
docker compose exec app cloudbox passwd admin 'NewPass456'
```

## Настройки (переменные окружения)

| Переменная | По умолчанию | Описание |
|---|---|---|
| `CB_LISTEN` | `:8080` | адрес сервера |
| `CB_DATABASE_URL` | — | строка подключения PostgreSQL |
| `CB_PUBLIC_URL` | `http://localhost:8080` | внешний адрес для ссылок |
| `CB_SECRET` | случайный | ключ подписи cookie публичных ссылок |
| `CB_ALLOW_REGISTRATION` | `false` | открытая регистрация |
| `CB_DEFAULT_QUOTA_MB` | `10240` | квота новых пользователей (0 — без ограничения) |
| `CB_MAX_UPLOAD_MB` | `4096` | максимальный размер файла |
| `CB_MAX_VERSIONS` | `10` | старых версий на файл |
| `CB_TRASH_DAYS` | `30` | срок хранения в корзине |
| `CB_STORAGE` | `local` | `local` или `s3` |
| `CB_DATA_DIR` | `./data` | каталог блобов для `local` |
| `CB_TMP_DIR` | системный | временные файлы загрузок |
| `CB_S3_*` | — | `ENDPOINT`, `REGION`, `BUCKET`, `ACCESS_KEY`, `SECRET_KEY`, `PATH_STYLE` |

## Устройство

```
cmd/cloudbox        точка входа, фоновые задачи, CLI
internal/config     переменные окружения
internal/db         подключение и миграции (встроены в бинарник)
internal/storage    блобы: local (диск) и S3 (свой клиент SigV4 без зависимостей)
internal/files      ядро: дерево, версии, корзина, квоты, ссылки, сборка мусора
internal/auth       пользователи, сессии, пароли приложений
internal/secure     PBKDF2, токены, HMAC
internal/api        REST API, публичные ссылки, раздача файлов
internal/dav        WebDAV поверх ядра (golang.org/x/net/webdav)
web/static          веб-интерфейс на чистом JS, встраивается через go:embed
```

**Метаданные в PostgreSQL, байты в хранилище.** Таблица `nodes` хранит дерево: файлы и папки. Таблица `versions` хранит версии; каждая ссылается на блоб по ключу. Несколько версий могут ссылаться на один блоб, это используется при копировании и восстановлении версии.

**Удаление блобов.** Блоб удаляется не сразу: ключ попадает в очередь `blob_gc`. Фоновая задача стирает объект, только когда на него не осталось ни одной ссылки.

**Корзина.** При удалении всё поддерево помечается одной меткой времени. По ней восстанавливается ровно то, что было удалено вместе. Объекты, удалённые раньше по отдельности, остаются в корзине.

## REST API (кратко)

Авторизация: cookie сессии или заголовок `Authorization: Bearer <token>`. Токен возвращает `/api/auth/login`.

```
POST   /api/auth/register | /api/auth/login | /api/auth/logout
GET    /api/me                                  профиль и квота
GET    /api/files?parent=ID                     содержимое папки + хлебные крошки
POST   /api/files/folder        {parent,name}
POST   /api/files/upload?parent=&name=&path=&conflict=rename   тело — байты файла
GET    /api/files/{id}/content[?version=&inline=1]             поддерживает Range
GET    /api/files/{id}/zip
PATCH  /api/files/{id}          {name?, parent?}                переименование и перенос
POST   /api/files/{id}/copy     {parent, name?}
DELETE /api/files/{id}                                          в корзину
GET    /api/files/{id}/versions   POST /api/files/{id}/versions/{vid}/restore
GET    /api/search?q=   GET /api/recent
GET    /api/trash   POST /api/trash/{id}/restore   DELETE /api/trash/{id}   DELETE /api/trash
GET    /api/shares   POST /api/shares {node_id,password,expires_at,allow_upload}   DELETE /api/shares/{id}
GET/POST/PATCH/DELETE /api/admin/users...
```

Загрузка из консоли:

```bash
curl -H "Authorization: Bearer $TOKEN" --data-binary @video.mp4 \
  "https://cloud.example.ru/api/files/upload?name=video.mp4&path=Видео/2026"
```

## Тесты

`scripts/e2e.py` — сквозной тест на 62 проверки: API, версии, корзина, квоты, ссылки, CSRF, изоляция пользователей, WebDAV. Тест пройден на обоих бэкендах: локальном диске и S3 (SeaweedFS).

## Сборка без Docker

```bash
go build -o cloudbox ./cmd/cloudbox
CB_DATABASE_URL=postgres://... CB_SECRET=... ./cloudbox
```

В `go.mod` модули `golang.org/x/*` заменены на официальные зеркала на GitHub (`github.com/golang/*`). Это нужно для сборки там, где закрыт `proxy.golang.org`. Если сеть открыта, `replace` можно удалить.

## Что дальше (по аналогии с Nextcloud)

- [ ] Миниатюры на сервере (сейчас превью — это сам файл)
- [ ] Докачка больших файлов чанками (tus или протокол Nextcloud chunking v2)
- [ ] Общие папки между пользователями (не только публичные ссылки)
- [ ] Календарь и контакты: CalDAV/CardDAV
- [ ] Онлайн-редактирование документов через Collabora или OnlyOffice (WOPI)
- [ ] 2FA (TOTP), журнал входов
- [ ] Шифрование на стороне сервера, перенос local ↔ s3
- [ ] Десктопный клиент синхронизации
