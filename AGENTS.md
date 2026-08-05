# Purpose

- Build the LLM chat UI as server-rendered hypermedia. Return templ-rendered HTML fragments for interactions; do not turn it into a client-rendered JSON application.
- Browser-side dependencies are limited to htmx, a-h/templ, and basic CSS. Do not add a JavaScript or CSS framework. Prefer readable modern CSS, including nesting where useful.

# Workflow

- Go is pinned to 1.25.3 in `go.mod`.
- A fresh checkout has no generated template Go files. Run `go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate` before building, and after every `templ/*.templ` change. `templ/*_templ.go` is ignored; never edit it manually.
- Run the app with `go run .`; it listens on fixed address `:8080`.
- Only edit files specified by the user. If you need to edit other files, ask before doing so.
- Full verification: `go test ./...` then `go vet ./...`. Current tests use local `httptest` servers and need no external services.
- Focus a package with `go test ./llm` or `go test ./tools`; focus one test with, for example, `go test ./llm -run '^TestComplete$'`.
- After large changes, update the AGENTS.md file accordingly. Keep it concise.

# Architecture

- `server.go` is the HTTP entrypoint: `GET /` renders the page, `GET /healthz` checks local database readiness, history and chat mutation routes return fragments, `POST /messages` accepts a user message, `POST /messages/complete` returns the final message, `POST /messages/retry` prepares a failed completion retry, `GET /messages/tools` streams tool-call HTML over SSE, `GET /sw.js` serves the service worker for root scope, and `GET /static/` serves embedded assets.
- `templ/*.templ` contains source components despite using package name `templates`. Edit these source files, then regenerate.
- `static/` is embedded into the executable. Restart `go run .` after asset changes. htmx and `htmx-ext-sse` v2.2.4 are vendored there; `static/app.js` contains persistent presentation behavior and completion network-error recovery. PWA support is `static/manifest.json`, generated PNG icons, and `static/sw.js` (registered from `app.js`, served at root scope with a fetch listener only; no caching). There is no npm build pipeline.
- `db/schema.sql` defines SQLite `chats` and position-ordered `messages`. `openDatabase` uses one rollback-journal connection with foreign keys and a busy timeout.
- `db.InsertChat` and `db.InsertMessage` persist chats and `llm.Message` values, including optional usage and provider metadata. `db.GetMessageSnapshot` captures ordered history plus a storage digest; `db.AppendCompletion` obtains a writer lock, rejects changed history, chooses the next stored position, and atomically appends generated messages. `db.AllocateChat` creates a chat preloaded with the default-enabled tools setting; `db.GetDefaultEnabledTools`/`db.SetDefaultEnabledTools` back the settings UI, which also stores default model and max tool rounds. Existing DBs get versioned migrations in `migrateDatabase` on startup.
- `llm.Client` is non-streaming and constructor-configured. Its endpoint argument must be the full chat-completions URL; no path is appended. The server reads `LLM_KEY`, `LLM_MODEL`, `LLM_ENDPOINT`, and `SEARXNG_URL` directly and does not load a `.env` file.
- `llm.NewConversation` creates a stateful, non-concurrent conversation around a client and optional `tools.Registry`. Initial messages may be supplied to the constructor; `Send` appends a user message, while `Complete` resumes from existing history.
- Conversations validate complete assistant responses before mutating history or executing tools. Valid function calls are executed through the registry, appended as `tool` messages with matching `tool_call_id` values, and sent back to the model. Consecutive tool-call chains are limited per conversation via `SetMaxToolRounds`, defaulting to 16 rounds; the limit is configurable in settings (1-100).
- Message bodies are limited to 1 MiB; completion, retry, settings, and rename forms are limited to 16 KiB. Chat titles use the first non-empty line capped at 120 runes. Completion resumes a stored snapshot, rejects stale persistence with a retryable conflict, and is limited to one active request per chat in each process.
- Chat-history links use scoped `hx-boost` navigation: full `GET /?chat=N` responses remain deep-linkable while htmx selects and swaps only `main`; history snapshots are disabled for chat privacy.
- `tools.Registry` validates definitions and that invocation arguments are JSON objects. Each `Tool.Execute` implementation must still validate arguments against its own schema before side effects.
