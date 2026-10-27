CREATE TABLE IF NOT EXISTS inbox_items (
    id INTEGER PRIMARY KEY,
    chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    submission_id TEXT NOT NULL CHECK (
        trim(submission_id) <> '' AND length(CAST(submission_id AS BLOB)) <= 512
    ),
    delivery TEXT NOT NULL CHECK (delivery IN ('steer', 'queue')),
    model TEXT NOT NULL CHECK (
        length(CAST(model AS BLOB)) <= 512
    ),
    status TEXT NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'delivered', 'cancelled')),
    message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    content TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (chat_id, submission_id),
    CHECK (status <> 'pending' OR message_id IS NULL),
    CHECK (status <> 'cancelled' OR message_id IS NULL)
) STRICT;

CREATE INDEX IF NOT EXISTS inbox_items_chat_status_idx
    ON inbox_items (chat_id, status, id);

CREATE TABLE IF NOT EXISTS inbox_tools (
    inbox_id INTEGER NOT NULL REFERENCES inbox_items(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    PRIMARY KEY (inbox_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS inbox_prompt_appends (
    inbox_id INTEGER NOT NULL REFERENCES inbox_items(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    prompt_append_id TEXT NOT NULL CHECK (prompt_append_id <> ''),
    PRIMARY KEY (inbox_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS inbox_prompt_append_texts (
    inbox_id INTEGER NOT NULL REFERENCES inbox_items(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    text TEXT NOT NULL,
    PRIMARY KEY (inbox_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS inbox_attachments (
    inbox_id INTEGER NOT NULL REFERENCES inbox_items(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    filename TEXT NOT NULL,
    media_type TEXT NOT NULL CHECK (media_type <> ''),
    width INTEGER NOT NULL CHECK (width >= 0),
    height INTEGER NOT NULL CHECK (height >= 0),
    data BLOB NOT NULL CHECK (length(data) > 0),
    PRIMARY KEY (inbox_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS completion_state (
    chat_id INTEGER PRIMARY KEY REFERENCES chats(id) ON DELETE CASCADE,
    pending INTEGER NOT NULL DEFAULT 0 CHECK (pending IN (0, 1)),
    model TEXT NOT NULL DEFAULT '' CHECK (
        length(CAST(model AS BLOB)) <= 512
    ),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE TABLE IF NOT EXISTS completion_state_tools (
    chat_id INTEGER NOT NULL REFERENCES completion_state(chat_id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    PRIMARY KEY (chat_id, position)
) STRICT;
