package main

// Bitify Messages: the schema. Created on boot with IF NOT EXISTS, next to the
// account tables, so a database that already serves accounts needs no
// migration step. Everything keys off app_users(id): there is no second
// account system.

const messagesSchema = `
CREATE TABLE IF NOT EXISTS social_profiles (
    user_id             BIGINT PRIMARY KEY REFERENCES app_users(id) ON DELETE CASCADE,
    username            TEXT NOT NULL DEFAULT '',
    display_name        TEXT NOT NULL DEFAULT '',
    bio                 TEXT NOT NULL DEFAULT '',
    avatar_url          TEXT NOT NULL DEFAULT '',
    activity_visibility TEXT NOT NULL DEFAULT 'friends',
    show_online         BOOLEAN NOT NULL DEFAULT TRUE,
    notify_messages     BOOLEAN NOT NULL DEFAULT TRUE,
    notify_activity     BOOLEAN NOT NULL DEFAULT TRUE,
    premium_until       TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- A database that already carries profiles gains the two notification columns
-- here rather than by a migration step: the table is created only when it is
-- missing, so the columns have to be added on their own.
ALTER TABLE social_profiles ADD COLUMN IF NOT EXISTS notify_messages BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE social_profiles ADD COLUMN IF NOT EXISTS notify_activity BOOLEAN NOT NULL DEFAULT TRUE;
CREATE UNIQUE INDEX IF NOT EXISTS social_profiles_username_key
    ON social_profiles (lower(username)) WHERE username <> '';

CREATE TABLE IF NOT EXISTS friendships (
    user_lo      BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    user_hi      BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    status       TEXT NOT NULL,
    requested_by BIGINT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_lo, user_hi),
    CHECK (user_lo < user_hi)
);
CREATE INDEX IF NOT EXISTS friendships_hi_idx ON friendships (user_hi);

CREATE TABLE IF NOT EXISTS social_blocks (
    blocker    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    blocked    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (blocker, blocked)
);

CREATE TABLE IF NOT EXISTS social_reports (
    id          BIGSERIAL PRIMARY KEY,
    reporter    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    target_user BIGINT NOT NULL,
    message_id  BIGINT NOT NULL DEFAULT 0,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS conversations (
    id          BIGSERIAL PRIMARY KEY,
    kind        TEXT NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    description TEXT NOT NULL DEFAULT '',
    avatar_url  TEXT NOT NULL DEFAULT '',
    created_by  BIGINT NOT NULL,
    direct_key  TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS conversations_direct_key
    ON conversations (direct_key) WHERE direct_key IS NOT NULL;

CREATE TABLE IF NOT EXISTS conversation_members (
    conv_id      BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id      BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    role         TEXT NOT NULL DEFAULT 'member',
    accepted     BOOLEAN NOT NULL DEFAULT TRUE,
    muted        BOOLEAN NOT NULL DEFAULT FALSE,
    last_read_id BIGINT NOT NULL DEFAULT 0,
    joined_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conv_id, user_id)
);
CREATE INDEX IF NOT EXISTS conversation_members_user_idx ON conversation_members (user_id);

CREATE TABLE IF NOT EXISTS chat_messages (
    id         BIGSERIAL PRIMARY KEY,
    conv_id    BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    sender_id  BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL DEFAULT 'text',
    body       TEXT NOT NULL DEFAULT '',
    payload    TEXT NOT NULL DEFAULT '',
    reply_to   BIGINT NOT NULL DEFAULT 0,
    forwarded  BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at  TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS chat_messages_conv_idx ON chat_messages (conv_id, id DESC);

CREATE TABLE IF NOT EXISTS chat_reactions (
    message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    emoji      TEXT NOT NULL,
    PRIMARY KEY (message_id, user_id, emoji)
);

CREATE TABLE IF NOT EXISTS chat_media (
    id          TEXT PRIMARY KEY,
    uploader    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    conv_id     BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    mime        TEXT NOT NULL,
    size        BIGINT NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    data        BYTEA NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS music_activity (
    id         BIGSERIAL PRIMARY KEY,
    user_id    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL,
    payload    TEXT NOT NULL DEFAULT '',
    hidden     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS music_activity_user_idx ON music_activity (user_id, id DESC);

-- Hiding a post from your own feed is per viewer: the row above belongs to
-- whoever posted it, so it cannot carry "I do not want to see this". This
-- table does, one row per (activity, viewer).
CREATE TABLE IF NOT EXISTS music_activity_hides (
    activity_id BIGINT NOT NULL REFERENCES music_activity(id) ON DELETE CASCADE,
    user_id     BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (activity_id, user_id)
);
CREATE INDEX IF NOT EXISTS music_activity_hides_user_idx ON music_activity_hides (user_id);
`
