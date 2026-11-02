CREATE TABLE IF NOT EXISTS users (
    id            BIGSERIAL PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
    quota_bytes   BIGINT NOT NULL DEFAULT 0, -- 0 = без ограничения
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    user_agent TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions(user_id);

-- Пароли приложений для WebDAV-клиентов (основной пароль в WebDAV тоже работает)
CREATE TABLE IF NOT EXISTS app_passwords (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at  TIMESTAMPTZ
);

-- Узел дерева: файл или папка. parent_id IS NULL — лежит в корне пользователя.
CREATE TABLE IF NOT EXISTS nodes (
    id                 UUID PRIMARY KEY,
    owner_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id          UUID REFERENCES nodes(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    is_dir             BOOLEAN NOT NULL,
    size               BIGINT NOT NULL DEFAULT 0,
    mime               TEXT NOT NULL DEFAULT '',
    current_version_id UUID,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at         TIMESTAMPTZ,          -- в корзине
    trash_root         BOOLEAN NOT NULL DEFAULT FALSE, -- именно этот узел удалили (а не его родителя)
    trash_parent_id    UUID                  -- куда восстанавливать
);
CREATE UNIQUE INDEX IF NOT EXISTS nodes_unique_name
    ON nodes(owner_id, COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), name)
    WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS nodes_parent_idx ON nodes(owner_id, parent_id) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS nodes_trash_idx ON nodes(owner_id, deleted_at) WHERE trash_root;

CREATE TABLE IF NOT EXISTS versions (
    id         UUID PRIMARY KEY,
    node_id    UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    owner_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    blob_key   TEXT NOT NULL,
    size       BIGINT NOT NULL,
    sha256     TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS versions_node_idx ON versions(node_id, created_at DESC);
CREATE INDEX IF NOT EXISTS versions_owner_idx ON versions(owner_id);

ALTER TABLE nodes DROP CONSTRAINT IF EXISTS nodes_current_version_fk;
ALTER TABLE nodes ADD CONSTRAINT nodes_current_version_fk
    FOREIGN KEY (current_version_id) REFERENCES versions(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED;

-- Ключи блобов, которые нужно удалить из хранилища (удаляются фоновой задачей)
CREATE TABLE IF NOT EXISTS blob_gc (
    blob_key   TEXT PRIMARY KEY,
    queued_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS shares (
    id            UUID PRIMARY KEY,
    token         TEXT NOT NULL UNIQUE,
    node_id       UUID NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    owner_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    password_hash TEXT,
    expires_at    TIMESTAMPTZ,
    allow_upload  BOOLEAN NOT NULL DEFAULT FALSE,
    downloads     BIGINT NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS shares_owner_idx ON shares(owner_id);
