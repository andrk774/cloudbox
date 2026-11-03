-- Чат: личные переписки и групповые комнаты.
CREATE TABLE IF NOT EXISTS talk_rooms (
    id            UUID PRIMARY KEY,
    kind          TEXT NOT NULL CHECK (kind IN ('group', 'direct')),
    name          TEXT NOT NULL DEFAULT '',
    created_by    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    -- для личной переписки: "меньший_id:больший_id" — у пары пользователей только одна комната
    direct_key    TEXT UNIQUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_activity TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS talk_members (
    room_id   UUID NOT NULL REFERENCES talk_rooms(id) ON DELETE CASCADE,
    user_id   BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    last_read BIGINT NOT NULL DEFAULT 0, -- id последнего прочитанного сообщения
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (room_id, user_id)
);
CREATE INDEX IF NOT EXISTS talk_members_user_idx ON talk_members(user_id);

CREATE TABLE IF NOT EXISTS talk_messages (
    id         BIGSERIAL PRIMARY KEY,
    room_id    UUID NOT NULL REFERENCES talk_rooms(id) ON DELETE CASCADE,
    user_id    BIGINT REFERENCES users(id) ON DELETE SET NULL,
    kind       TEXT NOT NULL DEFAULT 'text' CHECK (kind IN ('text', 'system')),
    body       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS talk_messages_room_idx ON talk_messages(room_id, id DESC);
