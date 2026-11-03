-- Последний принятый шаг TOTP: один и тот же код нельзя использовать повторно (RFC 6238, раздел 5.2).
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_last_step BIGINT NOT NULL DEFAULT 0;
