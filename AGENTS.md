# Purpose

- Build Kritui as server-rendered hypermedia. Interactions return templ-rendered HTML fragments, not a client-rendered JSON app.
- Browser dependencies stay limited to htmx, a-h/templ, and basic CSS. Do not add JavaScript or CSS frameworks.
- Only edit files named by the user. Ask before editing any others.

# Workflow

- Go version is pinned to 1.25.3. Templates use `github.com/a-h/templ/cmd/templ@v0.3.1020`.
- Fresh checkouts lack generated template Go files. Run `go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate` before builds/tests and after every `templ/*.templ` edit. Generated `templ/*_templ.go` files are ignored; never edit them manually.
- Run `go run .` locally; it binds `:8080` and opens or creates `data.db` in the current working directory. The app does not load `.env`; source it manually. Tracked `./devrun.sh` starts Air and sources `.env` when present; `.air.toml` regenerates templates, builds the binary, and proxies at `:7331`, but `full_bin` requires `.env`. For isolated work, build from the repository and run the binary from a scratch working directory; use no real credentials.
- `KRITUI_ENCRYPTION_KEY` is mandatory: standard base64 encoding of exactly 32 random bytes, read directly from the environment and validated before the database opens. To create a development key, use `openssl rand -base64 32`; never silently generate one or commit a real key. Reuse the same key across restarts for any database you need to retain.
- Set `KRITUI_SECURE_COOKIES=false` only for direct local HTTP; cookies are secure by default, and HTTPS always sets `Secure`. Signup uses the normal `/signup` form. A fresh database reserves legacy account ID 1 for unclaimed Skrittle; signing up as Skrittle claims its existing data. Other accounts start with defaults, not another user's settings or secrets.
- Focus tests with `go test ./internal/app`, `go test ./llm`, `go test ./tools`, or `go test ./tools/git`. Example single test: `go test ./llm -run '^TestComplete$'`. Full verification: `go test ./...` then `go vet ./...`. PDF page-image tests need Poppler `pdfinfo` and `pdftoppm`; provider HTTP tests use local `httptest` servers, and Git tests need `git`.
- No repository-local browser suite. For requested browser verification, use browse MCP against a running `go run .` instance; an isolated binary run from a scratch working directory is an equivalent option. Do not add or run a local browser test package.
- Dockerfile generates templates, runs full tests and vet, then builds. Keep every imported package directory in explicit `COPY` list. Build and runtime stages need Git and Poppler; runtime copies Alpine-built binaries, libraries, and Poppler data because it has no package manager. Forgejo workflow publishes commit-SHA and `main` images after `main` builds; registry credentials belong in organization Actions settings, never repo files.
- Update this file when documented workflows or invariants change.

# Architecture

- `main.go` embeds `static/` and `db/schema.sql`; HTTP implementation is in `internal/app/`. `templ/*.templ` are source components (package name `templates`).
- There is no npm build pipeline.
- `static/` is embedded in the binary. Bump `staticAssetVersion` in `templ/layout.templ` when static assets change, regenerate templates, and restart the app to load changed assets.
- Preserve browser DOM contracts when editing interactions. htmx 4 completion SSE uses unnamed `<hx-partial>` updates; preserve the composer during out-of-band refreshes and swaps.
- Schema and migrations must evolve together: define schema in `db/schema.sql`, add versioned migrations in `internal/app/server.go:migrateDatabase`, and test migration plus ownership behavior. SQLite rows are account-scoped; request and worker contexts must retain both owner (`db.WithUser`) and storage cipher (`db.WithStorageCipher`). Async work commonly uses `context.WithoutCancel`, which must preserve both values. Production rejects missing encryption context after startup guard.
- Completion workers in `internal/app/inbox_handlers.go` outlive initiating requests and serialize work per chat. `llm.Conversation` checkpoints only at complete assistant/tool-round boundaries; inbox steering and queued input are applied there. `db.CheckpointCompletion` atomically persists each full boundary, and snapshot digests reject stale history. SSE refreshes `#message-history` and `#prompt-inbox` with templ-rendered fragments while preserving the composer. Do not call providers or tools while holding short admission/boundary locks.
- `llm.Client` is non-streaming and expects a full Responses, Messages, or Chat Completions endpoint URL, not a provider base URL. Keep protocol behavior driven by endpoint/provider responses, not model-name assumptions.
- Model providers are per-account rows with encrypted endpoint and key. Form model values encode `providerID/model`; chats, inbox items, and completion state carry the provider ID alongside the raw model. Completion, title, and summary clients must resolve the provider before constructing `llm.Client`.
- Tool-result elision changes only outbound model context; stored tool results remain unchanged. Summaries are separate tool-free requests, cached only within one conversation, and must not be persisted as original results.
- Public shares use separate client-side encryption: the browser generates the key, the URL fragment carries it, and the server stores only ciphertext. Never move the share key into HTTP requests or the database.
- Tool implementations validate invocation arguments against their own schema before side effects. Remote Git/MCP credentials must not appear in tool definitions, arguments, or results.
