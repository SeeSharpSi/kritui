PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS settings (
    name TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS chats (
    id INTEGER PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    tools TEXT NOT NULL DEFAULT '[]',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    CHECK (
        CASE
            WHEN json_valid(tools) THEN json_type(tools) = 'array'
            ELSE 0
        END
    )
) STRICT;

CREATE TABLE IF NOT EXISTS messages (
    id INTEGER PRIMARY KEY,
    chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    role TEXT NOT NULL CHECK (role IN ('system', 'developer', 'user', 'assistant', 'tool')),
    content TEXT NOT NULL DEFAULT '',
    model TEXT,
    total_tokens INTEGER,
    cost REAL,
    tool_calls TEXT,
    tool_call_id TEXT,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (chat_id, position),
    CHECK (tool_calls IS NULL OR role = 'assistant'),
    CHECK (
        tool_calls IS NULL OR
        CASE
            WHEN json_valid(tool_calls) THEN json_type(tool_calls) = 'array'
            ELSE 0
        END
    ),
    CHECK (
        (role = 'tool' AND tool_call_id IS NOT NULL) OR
        (role <> 'tool' AND tool_call_id IS NULL)
    )
) STRICT;

CREATE INDEX IF NOT EXISTS messages_chat_created_at_idx
    ON messages (chat_id, created_at);

CREATE TRIGGER IF NOT EXISTS messages_touch_chat_after_insert
AFTER INSERT ON messages
BEGIN
    UPDATE chats
    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = NEW.chat_id;
END;

CREATE TRIGGER IF NOT EXISTS messages_touch_chat_after_update
AFTER UPDATE ON messages
BEGIN
    UPDATE chats
    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id IN (OLD.chat_id, NEW.chat_id);
END;

CREATE TRIGGER IF NOT EXISTS messages_touch_chat_after_delete
AFTER DELETE ON messages
BEGIN
    UPDATE chats
    SET updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
    WHERE id = OLD.chat_id;
END;
