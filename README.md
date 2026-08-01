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
docker build -t kritui .
docker run --rm \
  --name kritui \
  --publish 8080:8080 \
  --env-file .env \
  --volume kritui-data:/data \
  kritui
```

The named volume persists the SQLite database. The image includes a private SearXNG service with JSON responses enabled and rate limiting disabled; the app is preconfigured to use it at `http://127.0.0.1:8081`.

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
- `tools/`: validated tool registry and web-fetch tool
- `db/`: SQLite schema and queries

The server registers `webfetch` and `websearch`. Only tools selected in the chat options are exposed to the LLM for that request.
