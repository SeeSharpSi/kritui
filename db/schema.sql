PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS settings (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    default_model TEXT,
    max_tool_rounds INTEGER CHECK (max_tool_rounds IS NULL OR max_tool_rounds BETWEEN 1 AND 100),
    default_tools_configured INTEGER NOT NULL DEFAULT 0 CHECK (default_tools_configured IN (0, 1)),
    prompt_appends_configured INTEGER NOT NULL DEFAULT 0 CHECK (prompt_appends_configured IN (0, 1)),
    ntfy_endpoint TEXT,
    ntfy_topic TEXT,
    ntfy_api_key TEXT,
    theme TEXT CHECK (theme IS NULL OR theme IN ('rose-pine', 'rose-pine-dark', 'nord', 'tokyo-night', 'og'))
) STRICT;

INSERT OR IGNORE INTO settings (id) VALUES (1);

CREATE TABLE IF NOT EXISTS default_tools (
    position INTEGER PRIMARY KEY CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> '')
) STRICT;

CREATE TABLE IF NOT EXISTS prompt_appends (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    position INTEGER NOT NULL UNIQUE CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    text TEXT NOT NULL CHECK (text <> ''),
    enabled_by_default INTEGER NOT NULL DEFAULT 0 CHECK (enabled_by_default IN (0, 1))
) STRICT;

CREATE TABLE IF NOT EXISTS mcp_servers (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    position INTEGER NOT NULL UNIQUE CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    url TEXT NOT NULL CHECK (url <> ''),
    authorization_token TEXT
) STRICT;

CREATE TABLE IF NOT EXISTS model_endpoint_preferences (
    model TEXT PRIMARY KEY CHECK (trim(model) <> ''),
    endpoint_type TEXT NOT NULL CHECK (endpoint_type IN ('responses', 'messages', 'chat_completions'))
) STRICT;

CREATE TABLE IF NOT EXISTS chats (
    id INTEGER PRIMARY KEY,
    title TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE TABLE IF NOT EXISTS chat_tools (
    chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    PRIMARY KEY (chat_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS chat_prompt_appends (
    chat_id INTEGER NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    prompt_append_id TEXT NOT NULL CHECK (prompt_append_id <> ''),
    PRIMARY KEY (chat_id, position)
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
    tool_call_id TEXT,
    undo_sequence INTEGER CHECK (undo_sequence IS NULL OR undo_sequence > 0),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    UNIQUE (chat_id, position),
    UNIQUE (id, role),
    CHECK (
        (role = 'tool' AND tool_call_id IS NOT NULL) OR
        (role <> 'tool' AND tool_call_id IS NULL)
    )
) STRICT;

CREATE TABLE IF NOT EXISTS message_prompt_appends (
    message_id INTEGER NOT NULL,
    message_role TEXT NOT NULL DEFAULT 'user' CHECK (message_role = 'user'),
    position INTEGER NOT NULL CHECK (position >= 0),
    text TEXT NOT NULL,
    PRIMARY KEY (message_id, position),
    FOREIGN KEY (message_id, message_role) REFERENCES messages(id, role) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS message_user_images (
    message_id INTEGER NOT NULL,
    message_role TEXT NOT NULL DEFAULT 'user' CHECK (message_role = 'user'),
    position INTEGER NOT NULL CHECK (position >= 0),
    filename TEXT NOT NULL,
    media_type TEXT NOT NULL CHECK (media_type <> ''),
    width INTEGER NOT NULL CHECK (width >= 0),
    height INTEGER NOT NULL CHECK (height >= 0),
    data BLOB NOT NULL CHECK (length(data) > 0),
    PRIMARY KEY (message_id, position),
    FOREIGN KEY (message_id, message_role) REFERENCES messages(id, role) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS message_tool_calls (
    message_id INTEGER NOT NULL,
    message_role TEXT NOT NULL DEFAULT 'assistant' CHECK (message_role = 'assistant'),
    position INTEGER NOT NULL CHECK (position >= 0),
    call_id TEXT NOT NULL CHECK (call_id <> ''),
    call_type TEXT NOT NULL CHECK (call_type <> ''),
    function_name TEXT NOT NULL CHECK (function_name <> ''),
    arguments TEXT NOT NULL,
    PRIMARY KEY (message_id, position),
    UNIQUE (message_id, call_id),
    FOREIGN KEY (message_id, message_role) REFERENCES messages(id, role) ON DELETE CASCADE
) STRICT;

CREATE TABLE IF NOT EXISTS message_provider_outputs (
    message_id INTEGER NOT NULL,
    message_role TEXT NOT NULL DEFAULT 'assistant' CHECK (message_role = 'assistant'),
    position INTEGER NOT NULL CHECK (position >= 0),
    payload TEXT NOT NULL CHECK (
        CASE
            WHEN json_valid(payload) THEN json_type(payload) = 'object'
            ELSE 0
        END
    ),
    PRIMARY KEY (message_id, position),
    FOREIGN KEY (message_id, message_role) REFERENCES messages(id, role) ON DELETE CASCADE
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

PRAGMA user_version = 16;
