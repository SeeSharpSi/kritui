PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY,
    username TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
    password_hash TEXT NOT NULL DEFAULT '',
    disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE UNIQUE INDEX IF NOT EXISTS users_username_unique
    ON users (username COLLATE NOCASE)
    WHERE username <> '';

-- ID 1 owns data created before multi-user persistence. It stays disabled with
-- an empty hash until a Skrittle signup claims it and sets the first password.
INSERT OR IGNORE INTO users (id, username, password_hash, disabled)
VALUES (1, 'Skrittle', '', 1);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash TEXT PRIMARY KEY CHECK (token_hash <> ''),
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

CREATE INDEX IF NOT EXISTS sessions_user_id_idx ON sessions (user_id);

CREATE TABLE IF NOT EXISTS settings (
    id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    default_model TEXT,
    max_tool_rounds INTEGER CHECK (max_tool_rounds IS NULL OR max_tool_rounds BETWEEN 1 AND 100),
    default_tools_configured INTEGER NOT NULL DEFAULT 0 CHECK (default_tools_configured IN (0, 1)),
    prompt_appends_configured INTEGER NOT NULL DEFAULT 0 CHECK (prompt_appends_configured IN (0, 1)),
    ntfy_endpoint TEXT,
    ntfy_topic TEXT,
    ntfy_api_key TEXT,
    llm_endpoint TEXT,
    llm_api_key TEXT,
    theme TEXT CHECK (theme IS NULL OR theme IN ('rose-pine', 'rose-pine-dark', 'nord', 'forest-night', 'matte-black', '1975')),
    tool_result_elision_mode TEXT NOT NULL DEFAULT 'none'
        CHECK (tool_result_elision_mode IN ('none', 'current_turn', 'last_user_turns', 'tool_result_budget', 'summarize')),
    tool_result_elision_user_turns INTEGER
        CHECK (tool_result_elision_user_turns IS NULL OR tool_result_elision_user_turns BETWEEN 1 AND 1000),
    tool_result_elision_token_budget INTEGER
        CHECK (tool_result_elision_token_budget IS NULL OR tool_result_elision_token_budget BETWEEN 1 AND 10000000),
    tool_result_elision_summary_model TEXT
        CHECK (tool_result_elision_summary_model IS NULL OR
            (trim(tool_result_elision_summary_model) <> '' AND length(CAST(tool_result_elision_summary_model AS BLOB)) <= 512)),
    title_generation_enabled INTEGER NOT NULL DEFAULT 0
        CHECK (title_generation_enabled IN (0, 1)),
    title_generation_model TEXT
        CHECK (title_generation_model IS NULL OR
            (trim(title_generation_model) <> '' AND length(CAST(title_generation_model AS BLOB)) <= 512))
) STRICT;

INSERT OR IGNORE INTO settings (id) VALUES (1);

CREATE TABLE IF NOT EXISTS default_tools (
    user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
    position INTEGER NOT NULL CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    PRIMARY KEY (user_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS prompt_appends (
    user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id <> ''),
    position INTEGER NOT NULL CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    text TEXT NOT NULL CHECK (text <> ''),
    enabled_by_default INTEGER NOT NULL DEFAULT 0 CHECK (enabled_by_default IN (0, 1)),
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS mcp_servers (
    user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id <> ''),
    position INTEGER NOT NULL CHECK (position >= 0),
    name TEXT NOT NULL CHECK (name <> ''),
    url TEXT NOT NULL CHECK (url <> ''),
    authorization_token TEXT,
    PRIMARY KEY (user_id, id),
    UNIQUE (user_id, position)
) STRICT;

CREATE TABLE IF NOT EXISTS model_endpoint_preferences (
    user_id INTEGER NOT NULL DEFAULT 1 REFERENCES users(id) ON DELETE CASCADE,
    model TEXT NOT NULL CHECK (trim(model) <> ''),
    endpoint_type TEXT NOT NULL CHECK (endpoint_type IN ('responses', 'messages', 'chat_completions')),
    PRIMARY KEY (user_id, model)
) STRICT;

CREATE TABLE IF NOT EXISTS chats (
    id INTEGER PRIMARY KEY,
    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    pinned INTEGER NOT NULL DEFAULT 0 CHECK (pinned IN (0, 1))
) STRICT;

CREATE INDEX IF NOT EXISTS chats_user_pinned_updated_idx
    ON chats (user_id, pinned DESC, updated_at DESC, id DESC);

-- Trusted raw inserts that predate user accounts belong to the legacy owner.
-- Application paths always write an explicit user_id.
CREATE TRIGGER IF NOT EXISTS chats_assign_legacy_owner
AFTER INSERT ON chats
WHEN NEW.user_id IS NULL
BEGIN
    UPDATE chats SET user_id = 1 WHERE id = NEW.id;
END;

CREATE TRIGGER IF NOT EXISTS chats_validate_user_update
BEFORE UPDATE OF user_id ON chats
WHEN NEW.user_id IS NULL OR NOT EXISTS (SELECT 1 FROM users WHERE id = NEW.user_id)
BEGIN
    SELECT RAISE(ABORT, 'chats.user_id must reference an existing user');
END;

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
    result_elided INTEGER NOT NULL DEFAULT 0 CHECK (result_elided IN (0, 1)),
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

CREATE TABLE IF NOT EXISTS shared_responses (
    token_hash TEXT PRIMARY KEY CHECK (token_hash <> ''),
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    format_version INTEGER NOT NULL CHECK (format_version = 1),
    nonce BLOB NOT NULL CHECK (length(nonce) = 12),
    ciphertext BLOB NOT NULL CHECK (
        length(ciphertext) BETWEEN 16 AND 16777216
    ),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;

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

PRAGMA user_version = 32;
