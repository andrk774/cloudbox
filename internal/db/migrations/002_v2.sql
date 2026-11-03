-- CloudBox v2: шары пользователям, 2FA/OIDC, журнал, теги, комментарии, поиск,
-- уведомления, миниатюры, докачка, CalDAV/CardDAV, WOPI.

-- ---------- пользователи и безопасность ----------
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS totp_secret    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS totp_enabled   BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS recovery_codes TEXT[] NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS oidc_sub       TEXT,
    ADD COLUMN IF NOT EXISTS email          TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS root_etag      TEXT NOT NULL DEFAULT md5(random()::text);
CREATE UNIQUE INDEX IF NOT EXISTS users_oidc_sub ON users(oidc_sub) WHERE oidc_sub IS NOT NULL;

ALTER TABLE sessions
    ADD COLUMN IF NOT EXISTS id        BIGSERIAL,
    ADD COLUMN IF NOT EXISTS ip        TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_seen TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN IF NOT EXISTS method    TEXT NOT NULL DEFAULT 'password';

CREATE TABLE IF NOT EXISTS login_events (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT REFERENCES users(id) ON DELETE CASCADE,
    username   TEXT NOT NULL,
    ip         TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT '',
    success    BOOLEAN NOT NULL,
    method     TEXT NOT NULL,
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS login_events_user ON login_events(user_id, at DESC);

-- ---------- узлы: etag, дата съёмки ----------
ALTER TABLE nodes
    ADD COLUMN IF NOT EXISTS etag          TEXT NOT NULL DEFAULT md5(random()::text || clock_timestamp()::text),
    ADD COLUMN IF NOT EXISTS taken_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS media_checked BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS nodes_media_idx ON nodes(owner_id, (COALESCE(taken_at, created_at)) DESC)
    WHERE deleted_at IS NULL AND NOT is_dir AND (mime LIKE 'image/%' OR mime LIKE 'video/%');

-- ETag узла меняется при любом изменении, ETag всех предков — при изменении содержимого.
CREATE OR REPLACE FUNCTION cb_node_etag() RETURNS trigger AS $$
BEGIN
    NEW.etag := md5(random()::text || clock_timestamp()::text);
    RETURN NEW;
END $$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION cb_bump_chain(p UUID, o BIGINT) RETURNS void AS $$
BEGIN
    IF p IS NOT NULL THEN
        WITH RECURSIVE a AS (
            SELECT id, parent_id FROM nodes WHERE id = p
            UNION ALL SELECT n.id, n.parent_id FROM nodes n JOIN a ON n.id = a.parent_id
        ) UPDATE nodes SET etag = md5(random()::text || clock_timestamp()::text) WHERE id IN (SELECT id FROM a);
    END IF;
    UPDATE users SET root_etag = md5(random()::text || clock_timestamp()::text) WHERE id = o;
END $$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION cb_nodes_changed() RETURNS trigger AS $$
BEGIN
    IF pg_trigger_depth() > 1 THEN RETURN NULL; END IF;
    IF TG_OP = 'DELETE' THEN
        PERFORM cb_bump_chain(OLD.parent_id, OLD.owner_id);
    ELSE
        PERFORM cb_bump_chain(NEW.parent_id, NEW.owner_id);
        IF TG_OP = 'UPDATE' AND OLD.parent_id IS DISTINCT FROM NEW.parent_id THEN
            PERFORM cb_bump_chain(OLD.parent_id, OLD.owner_id);
        END IF;
    END IF;
    RETURN NULL;
END $$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS nodes_etag_self ON nodes;
CREATE TRIGGER nodes_etag_self BEFORE UPDATE ON nodes FOR EACH ROW
    WHEN (OLD.etag IS NOT DISTINCT FROM NEW.etag) EXECUTE FUNCTION cb_node_etag();
DROP TRIGGER IF EXISTS nodes_etag_chain ON nodes;
CREATE TRIGGER nodes_etag_chain AFTER INSERT OR UPDATE OR DELETE ON nodes FOR EACH ROW EXECUTE FUNCTION cb_nodes_changed();

-- ---------- докачка чанками ----------
CREATE TABLE IF NOT EXISTS uploads (
    id         UUID PRIMARY KEY,
    owner_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- чей это будет файл
    actor_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE, -- кто загружает
    parent_id  UUID,
    name       TEXT NOT NULL,
    rel_path   TEXT NOT NULL DEFAULT '',
    conflict   TEXT NOT NULL DEFAULT '',
    size       BIGINT NOT NULL,
    received   BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- миниатюры ----------
CREATE TABLE IF NOT EXISTS thumbs (
    version_id UUID NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
    size       INT NOT NULL,
    blob_key   TEXT NOT NULL, -- '' — миниатюру сделать нельзя (запоминаем, чтобы не пытаться снова)
    PRIMARY KEY (version_id, size)
);
CREATE OR REPLACE FUNCTION cb_thumb_gc() RETURNS trigger AS $$
BEGIN
    IF OLD.blob_key <> '' THEN
        INSERT INTO blob_gc(blob_key) VALUES (OLD.blob_key) ON CONFLICT DO NOTHING;
    END IF;
    RETURN NULL;
END $$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS thumbs_gc ON thumbs;
CREATE TRIGGER thumbs_gc AFTER DELETE ON thumbs FOR EACH ROW EXECUTE FUNCTION cb_thumb_gc();

-- ---------- группы и общий доступ ----------
CREATE TABLE IF NOT EXISTS groups (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS group_members (
    group_id BIGINT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);
CREATE TABLE IF NOT EXISTS user_shares (
    id         UUID PRIMARY KEY,
    node_id    UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    owner_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_id    BIGINT REFERENCES users(id) ON DELETE CASCADE,
    group_id   BIGINT REFERENCES groups(id) ON DELETE CASCADE,
    can_write  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((user_id IS NULL) <> (group_id IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS user_shares_user ON user_shares(node_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS user_shares_group ON user_shares(node_id, group_id) WHERE group_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS user_shares_target ON user_shares(user_id);

-- ---------- журнал действий ----------
CREATE TABLE IF NOT EXISTS activity (
    id         BIGSERIAL PRIMARY KEY,
    owner_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_id   BIGINT,
    actor_name TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    node_id    UUID,
    node_name  TEXT NOT NULL DEFAULT '',
    details    TEXT NOT NULL DEFAULT '',
    ip         TEXT NOT NULL DEFAULT '',
    at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS activity_owner ON activity(owner_id, at DESC);
CREATE INDEX IF NOT EXISTS activity_node ON activity(node_id, at DESC);

-- ---------- избранное, теги, комментарии ----------
CREATE TABLE IF NOT EXISTS favorites (
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    node_id    UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, node_id)
);
CREATE TABLE IF NOT EXISTS tags (
    id      BIGSERIAL PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    color   TEXT NOT NULL DEFAULT '#2f6fed',
    UNIQUE (user_id, name)
);
CREATE TABLE IF NOT EXISTS node_tags (
    node_id UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    tag_id  BIGINT NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    PRIMARY KEY (node_id, tag_id)
);
CREATE TABLE IF NOT EXISTS comments (
    id         BIGSERIAL PRIMARY KEY,
    node_id    UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    body       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS comments_node ON comments(node_id, created_at);

-- ---------- полнотекстовый поиск ----------
CREATE TABLE IF NOT EXISTS node_text (
    node_id    UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    version_id UUID,
    content    TEXT NOT NULL,
    tsv        tsvector NOT NULL
);
CREATE INDEX IF NOT EXISTS node_text_tsv ON node_text USING GIN (tsv);
CREATE TABLE IF NOT EXISTS index_queue (
    node_id   UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    queued_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------- уведомления ----------
CREATE TABLE IF NOT EXISTS notifications (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    title      TEXT NOT NULL,
    body       TEXT NOT NULL DEFAULT '',
    link       TEXT NOT NULL DEFAULT '',
    dedupe     TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    read_at    TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS notifications_user ON notifications(user_id, created_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS notifications_dedupe ON notifications(user_id, dedupe) WHERE dedupe IS NOT NULL;

-- ---------- календари и адресные книги ----------
CREATE TABLE IF NOT EXISTS dav_collections (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL CHECK (kind IN ('cal', 'card')),
    slug        TEXT NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    color       TEXT NOT NULL DEFAULT '#2f6fed',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, kind, slug)
);
CREATE TABLE IF NOT EXISTS dav_objects (
    id            BIGSERIAL PRIMARY KEY,
    collection_id BIGINT NOT NULL REFERENCES dav_collections(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,       -- имя ресурса, например abc.ics
    uid           TEXT NOT NULL DEFAULT '',
    data          TEXT NOT NULL,
    etag          TEXT NOT NULL,
    modified      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (collection_id, name)
);

-- ---------- блокировки WOPI (онлайн-редактор) ----------
CREATE TABLE IF NOT EXISTS wopi_locks (
    node_id    UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    lock_id    TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL
);
