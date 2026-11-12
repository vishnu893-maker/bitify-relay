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
-- Two more switches, added the same way. Read receipts say whether this
-- listener's "seen" is visible to others; activity status says whether their
-- typing and online state is. Both default to on, so an existing listener is
-- not quietly made private by an upgrade.
ALTER TABLE social_profiles ADD COLUMN IF NOT EXISTS show_read_receipts BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE social_profiles ADD COLUMN IF NOT EXISTS show_activity BOOLEAN NOT NULL DEFAULT TRUE;
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
    -- Where a forwarded message came from. The reference points at the
    -- original message so a forward can be traced back without copying it.
    forwarded_from BIGINT NOT NULL DEFAULT 0,
    -- The message this one answers, if any. Kept as a plain column rather
    -- than a foreign key: a reply must survive the message it answers being
    -- deleted, and then read as "Original message deleted".
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    edited_at  TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS chat_messages_conv_idx ON chat_messages (conv_id, id DESC);
-- A database that already carries messages gains the forward reference the
-- same way the profile columns are gained above.
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS forwarded_from BIGINT NOT NULL DEFAULT 0;

-- A reaction is one per person per message. The table keeps its original
-- primary key so existing reactions survive, and the server enforces the
-- one-per-person rule by clearing the person's other rows before writing
-- the new one.
CREATE TABLE IF NOT EXISTS chat_reactions (
    message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    emoji      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id, emoji)
);
CREATE INDEX IF NOT EXISTS chat_reactions_message_idx ON chat_reactions (message_id);
-- A database that already carries reactions gains the timestamp the same way
-- the profile columns are gained above: the table is created only when it is
-- missing, so a new column has to be added on its own.
ALTER TABLE chat_reactions ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- Who has received what, and who has read it.
--
-- The row is written by the server when the message is sent (delivered), and
-- again when the recipient opens the conversation and the message becomes
-- visible (read). The client never tells the server that it read anything:
-- it asks the server to mark a conversation read up to a point, and the
-- server decides whose messages that covers.
CREATE TABLE IF NOT EXISTS chat_message_status (
    message_id  BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    user_id     BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    sent_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at TIMESTAMPTZ,
    read_at     TIMESTAMPTZ,
    PRIMARY KEY (message_id, user_id)
);
CREATE INDEX IF NOT EXISTS chat_message_status_user_idx ON chat_message_status (user_id, read_at);

-- A message hidden from one person without being deleted for everyone.
--
-- The row stays; only the reader's view of it goes away.
CREATE TABLE IF NOT EXISTS chat_message_hides (
    message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (message_id, user_id)
);

-- Who is typing, and when they last said so. The row is only ever a hint:
-- it is replaced on the next keystroke and read only to show "typing…".
CREATE TABLE IF NOT EXISTS chat_typing (
    conv_id       BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    user_id       BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    last_updated  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (conv_id, user_id)
);
CREATE INDEX IF NOT EXISTS chat_typing_updated_idx ON chat_typing (last_updated);

-- A mention of someone in a group message.
--
-- The message body keeps the text; this table records who was named so the
-- notification and the unread badge can be pointed at them.
CREATE TABLE IF NOT EXISTS chat_mentions (
    message_id BIGINT NOT NULL REFERENCES chat_messages(id) ON DELETE CASCADE,
    user_id    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    PRIMARY KEY (message_id, user_id)
);
CREATE INDEX IF NOT EXISTS chat_mentions_user_idx ON chat_mentions (user_id);

CREATE TABLE IF NOT EXISTS chat_media (
    id          TEXT PRIMARY KEY,
    uploader    BIGINT NOT NULL REFERENCES app_users(id) ON DELETE CASCADE,
    conv_id     BIGINT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    mime        TEXT NOT NULL,
    size        BIGINT NOT NULL,
    duration_ms BIGINT NOT NULL DEFAULT 0,
    data        BYTEA NOT NULL,
    -- The name the file was uploaded under, so a document can say what it is
    -- instead of only that it exists.
    file_name   TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE chat_media ADD COLUMN IF NOT EXISTS file_name TEXT NOT NULL DEFAULT '';

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
