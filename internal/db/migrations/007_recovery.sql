ALTER TABLE users ADD COLUMN recovery_email TEXT NOT NULL DEFAULT '';
CREATE TABLE account_tokens (
    token_hash TEXT PRIMARY KEY,
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose TEXT NOT NULL CHECK (purpose IN ('verify', 'reset')),
    email TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    UNIQUE(user_id, purpose)
);
