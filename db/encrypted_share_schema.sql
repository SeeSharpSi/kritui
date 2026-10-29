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
