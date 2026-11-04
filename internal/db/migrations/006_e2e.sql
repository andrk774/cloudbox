-- Сквозное шифрование: E2E-папки, содержимое и имена в которых шифрует клиент.
-- Сервер хранит только шифротекст, обёрнутый ключ пользователя и зашифрованные метаданные папок.

-- e2e — узел внутри E2E-папки (или сама папка). Флаг наследуется при создании.
ALTER TABLE nodes ADD COLUMN IF NOT EXISTS e2e BOOLEAN NOT NULL DEFAULT FALSE;

-- Ключ хранилища пользователя, обёрнутый ключом из кодовой фразы и ключом восстановления.
-- Сам ключ и кодовую фразу сервер не видит никогда.
CREATE TABLE IF NOT EXISTS e2e_keys (
    user_id    BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    data       JSONB  NOT NULL,
    version    INT    NOT NULL DEFAULT 1,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Зашифрованные метаданные E2E-папки: настоящие имена, ключи файлов. version — защита от потери
-- правок при одновременной записи с двух устройств (запись только при совпадении версии).
CREATE TABLE IF NOT EXISTS e2e_meta (
    node_id    UUID PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
    version    INT  NOT NULL DEFAULT 0,
    data       TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
