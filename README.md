# Kritui

Server-rendered LLM chat UI built with Go, templ, htmx, and SQLite. It sends conversation history to an OpenAI-compatible chat-completions endpoint and persists chats in `data.db`.

## Run

Requires Go 1.25.3. Configure the LLM connection:

```sh
cp .env.example .env
set -a; . ./.env; set +a
```

`LLM_ENDPOINT` must be the full chat-completions URL. `SEARXNG_URL` configures the server used by the optional `websearch` tool. The SearXNG instance must have the JSON response format enabled.

Generate templates and start the server:

```sh
go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate
go run .
```

Open <http://localhost:8080>.

## Docker

Configure the LLM connection in `.env`, then build and run the image:

```sh
docker build --tag kritui:latest .
docker run --rm \
  --name kritui \
  --publish 8080:8080 \
  --env-file .env \
  --volume kritui-data:/data \
  kritui:latest
```

The named volume persists the SQLite database. The image includes a private SearXNG service with JSON responses enabled and rate limiting disabled; the app is preconfigured to use it at `http://127.0.0.1:8081`. It also includes isolated Git runtime files required by the optional Git tool capability.

## Container Publishing

Forgejo Actions builds and publishes the image after every push to `main`:

```text
git.skrittle.net/the_rebel_alliance/kritui:<full-commit-sha>
git.skrittle.net/the_rebel_alliance/kritui:main
```

The immutable commit tag is pushed before the mutable `main` channel. The build generates templ sources and requires `go test ./...` and `go vet ./...` to pass before either tag is published. Registry credentials come from organization variable `REGISTRY_USERNAME` and secret `REGISTRY_TOKEN`; they must never be committed.

Repository setup requires Actions and Packages units enabled, organization runner `docker` visible under `Settings -> Actions -> Runners`, and organization-scoped publisher settings. `REGISTRY_USERNAME` names restricted package publisher account; `REGISTRY_TOKEN` is limited to package writes. Workflow receives no production host or application credential.

Rebuilding the same tag can leave the previous image dangling. Kritui images carry a project label, so future dangling builds can be reviewed and removed without pruning images from other projects:

```sh
docker image ls --filter dangling=true --filter "label=io.github.seesharpsi.kritui=true"
docker image prune --filter "label=io.github.seesharpsi.kritui=true"
```

Images built before this label was added need a one-time unscoped cleanup. Review them before pruning:

```sh
docker image ls --filter dangling=true
docker image prune
```

BuildKit cache is separate from images. It speeds up rebuilds, but old cache can be inspected and pruned independently:

```sh
docker buildx du
docker builder prune --filter "until=24h"
```

## Development

With [Air](https://github.com/air-verse/air) installed and `.env` configured:

```sh
air
```

Air regenerates templates, rebuilds on Go, templ, or CSS changes, and serves its proxy at <http://localhost:7331>.

## Verify

```sh
go test ./...
go vet ./...
```

## Structure

- `server.go`, `*_handlers.go`, `tool_stream.go`: HTTP server, chat flow, and persistence wiring
- `templ/`, `static/`: server-rendered components and embedded browser assets
- `llm/`: non-streaming API client and tool-capable conversation loop
- `commands/`: slash-command registry and built-ins
- `tools/`: validated model-tool registry and web-tool implementations
- `tools/git/`: read-only public Git repository inspection tools
- `db/`: SQLite schema and queries

The server registers `webfetch` and `websearch`. Only tools selected in the chat options are exposed to the LLM for that request.
