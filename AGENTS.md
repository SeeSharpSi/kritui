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

# Architecture

- `server.go` is the HTTP entrypoint: `GET /` renders the page, `POST /messages` appends an HTML message fragment, and `GET /static/` serves embedded assets. The message handler currently echoes user input; `llm/` and `tools/` are not wired into the server yet.
- `templ/*.templ` contains source components despite using package name `templates`. Edit these source files, then regenerate.
- `static/` is embedded into the executable. Restart `go run .` after asset changes. htmx is vendored as `static/htmx.min.js`; there is no npm build pipeline.
- `llm.Client` is non-streaming and constructor-configured. Its endpoint argument must be the full chat-completions URL; no path is appended. The server currently reads no environment variables or `.env` file.
- `tools.Registry` validates definitions and that invocation arguments are JSON objects. Each `Tool.Execute` implementation must still validate arguments against its own schema before side effects.
