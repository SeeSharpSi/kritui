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

- `server.go` is the HTTP entrypoint: `GET /` renders the page, `POST /messages` appends user and assistant HTML message fragments, and `GET /static/` serves embedded assets. It registers all user-facing tools once at startup.
- `templ/*.templ` contains source components despite using package name `templates`. Edit these source files, then regenerate.
- `static/` is embedded into the executable. Restart `go run .` after asset changes. htmx is vendored as `static/htmx.min.js`; there is no npm build pipeline.
- `db/schema.sql` defines SQLite `chats` and position-ordered `messages`. Callers provide `*sql.DB`; the project does not yet configure a database driver or connection.
- `db.InsertChat` and `db.InsertMessage` persist chats and `llm.Message` values. Chats store enabled tool names as a JSON string array in `chats.tools` for `tools.Registry.Select`. `db.GetChatTools` / `db.SetChatTools` read and replace that list; `db.GetChats` includes it on each chat. `db.GetMessages` restores conversation order and decodes tool-call fields.
- `llm.Client` is non-streaming and constructor-configured. Its endpoint argument must be the full chat-completions URL; no path is appended. The server reads `LLM_KEY`, `LLM_MODEL`, `LLM_ENDPOINT`, and `SEARXNG_URL` directly and does not load a `.env` file.
- `llm.NewConversation` creates a stateful, non-concurrent conversation around a client and optional `tools.Registry`. Initial messages may be supplied to the constructor; `Send` appends a user message, while `Complete` resumes from existing history.
- Conversations advertise registry definitions using OpenAI-compatible function tools. Assistant `tool_calls` are executed through the registry, appended as `tool` messages with matching `tool_call_id` values, and sent back to the model until it returns a final response. Tool and argument errors are returned to the model as tool output; transport, context, and malformed protocol errors are returned to the caller. Consecutive tool-call chains are limited to 16 rounds.
- Chat tool options come from the startup registry. Each message carries selected names through the pending htmx request, and `tools.Registry.Select` derives the registry exposed to that completion.
- `tools.Registry` validates definitions and that invocation arguments are JSON objects. Each `Tool.Execute` implementation must still validate arguments against its own schema before side effects.
