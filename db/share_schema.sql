CREATE TABLE IF NOT EXISTS shared_responses (
    token_hash TEXT PRIMARY KEY CHECK (token_hash <> ''),
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    chat_id INTEGER NOT NULL,
    message_id INTEGER NOT NULL,
    theme TEXT NOT NULL CHECK (theme <> ''),
    html TEXT NOT NULL CHECK (html <> ''),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
) STRICT;
