-- Почта: подключённые ящики пользователей на внешних IMAP/SMTP-серверах.
CREATE TABLE IF NOT EXISTS mail_accounts (
    id           BIGSERIAL PRIMARY KEY,
    user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    email        TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    imap_host    TEXT NOT NULL,
    imap_port    INT  NOT NULL,
    imap_sec     TEXT NOT NULL,
    smtp_host    TEXT NOT NULL,
    smtp_port    INT  NOT NULL,
    smtp_sec     TEXT NOT NULL,
    login        TEXT NOT NULL,
    -- пароль ящика, зашифрованный AES-256-GCM ключом из CB_SECRET: nonce || шифротекст
    password_enc BYTEA NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS mail_accounts_user_idx ON mail_accounts(user_id);
