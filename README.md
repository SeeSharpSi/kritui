# Kritui

Server-rendered, multiuser LLM chat UI built with Go, templ, htmx, and SQLite. It sends conversation history to OpenAI-compatible Responses, Anthropic Messages, or Chat Completions endpoints and persists chats in `data.db`.

## Run

Requires Go 1.25.3. Environment LLM values are optional. Copy the example environment file and generate the required database encryption key once:

```sh
cp .env.example .env
chmod 600 .env
openssl rand -base64 32
```

Paste the generated value after `KRITUI_ENCRYPTION_KEY=` in `.env`, replacing the empty sample assignment. Alternatively, generate and append a fresh assignment instead of pasting; the later value overrides the earlier blank one:

```sh
printf 'KRITUI_ENCRYPTION_KEY=%s\n' "$(openssl rand -base64 32)" >> .env
```

Use only one of these methods. The key must be standard base64 encoding of exactly 32 random bytes. Kritui validates it before opening the database. Store it outside the database in a `0600` environment file or secret manager, and back it up separately. Losing the key makes encrypted data unrecoverable, and changing it does not rekey existing data.

Load the environment:

```sh
set -a; . ./.env; set +a
export KRITUI_SECURE_COOKIES=false
```

The server reads environment variables directly and does not load `.env` itself. When `LLM_KEY` and `LLM_ENDPOINT` are both set they override account settings and model-connection controls are hidden; otherwise sign in and enter the endpoint and key in Settings. `LLM_MODEL` remains a deployment-wide override or fallback.

`LLM_ENDPOINT` must be a full `/responses`, `/chat/messages`, or `/chat/completions` URL. Kritui starts with that protocol for an unknown model. On HTTP 500, it tries the remaining protocols and stores the successful per-model preference for the account. `SEARXNG_URL` configures the server used by the optional `websearch` tool. The SearXNG instance must have the JSON response format enabled.

Kritui shows models.dev input-modality icons beside model names in the chat model picker and the Settings default-model and title-model pickers: text, image, PDF, audio, and video. Icons are right-aligned and gray, with accessible labels and tooltips. Provider `/models` metadata is always checked first; models.dev fills image/PDF fields the provider did not declare. If the provider model list fails or is empty, the pickers use models.dev provider models and show "Using models.dev fallback" beneath the list. Provider-reported usage cost and OpenCode list cost are authoritative; when a provider omits cost, Kritui estimates it from the catalog and shows the amount with a leading `~`. The public catalog is cached in `data.db` and refreshed in the background at most once per 6 hours. Set `MODELS_DEV=off` to disable catalog lookups, or `MODELS_DEV_URL` to use a mirror. Estimated costs are list prices, not negotiated prices or subscription charges.

`KRITUI_SECURE_COOKIES=false` is only for direct local HTTP. Keep it set while launching or browsing at `http://localhost:8080`; use the default `true` whenever the app is served through HTTPS, including a TLS-terminating reverse proxy.

Generate templates and start the server:

```sh
go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate
go run .
```

Open <http://localhost:8080> and sign in or create an account.

## Accounts

Username and password login and open signup are available at `/login` and `/signup`. Each account has its own chats, history, attachments, undo state, settings, stored secrets, MCP servers and tokens, prompt appends, default tools, theme, and model protocol preferences. New accounts start from built-in defaults and never inherit another account's data or secrets. Completions, SSE streams, and private tool results are isolated per account.

On an existing database, legacy data stays attached to the Skrittle account (ID 1). Use the normal signup form once with the username Skrittle to set its password and claim the existing data. Usernames are case-insensitive. If Skrittle already has a password, sign in with it instead. Signing up with any other username creates a separate new account with built-in defaults; it does not receive the legacy data. There are no email flows, password reset, or account recovery yet. Multiple sessions may be active; logout revokes only the current session.

PWA assets remain public before login, and the manifest keeps its identity, start URL, and scope at `/`. Login and signup pages share the manifest and mobile web-app metadata, and installed apps keep their session across relaunches. The service worker does not cache private pages or API responses. Mobile installation requires HTTPS, except for the browser localhost exception.

## Data encryption

Kritui requires `KRITUI_ENCRYPTION_KEY` at startup and encrypts stored content with AES-256-GCM from the Go standard library. Encrypted fields include chat titles, message text for every role, tool arguments and results, provider output, prompt append definitions and snapshots, attachment bytes and filenames, pending and receipted inbox text and attachments, stored LLM keys and endpoints, ntfy credentials, endpoints, and topics, and MCP bearer tokens and URLs. IDs, ownership, usernames, password and session hashes, timestamps, model and tool names, and attachment types and dimensions remain plaintext metadata.

Existing plaintext fields are migrated atomically to encrypted envelopes when the app starts; text values use the `enc:v1:` prefix, while byte fields use a binary envelope. Each value uses a fresh 12-byte nonce, and authenticated data binds the field, owner, and stable row ID. A missing, wrong, or malformed key is rejected before the app mutates data, and the server does not fall back to plaintext. `secure_delete` and `VACUUM` remove logically freed plaintext pages from the database file.

This protects database files and backups if they are stolen. It does not protect a running server: the server, configured LLM provider, and any tool that receives content can read plaintext. Process memory and crash dumps can contain plaintext and the encryption key. AES-GCM detects payload tampering, but it does not detect whole-database rollback or metadata tampering. Physical erasure is not guaranteed on SSDs, copy-on-write filesystems, backups, or snapshots; use full-disk encryption and manage old copies separately. Keep `SHOW_TOOLCALLS=false` in production because tool and model logs can contain plaintext. Use HTTPS and restrict access to the key and environment file.

Public share links use their separate existing client-side encryption scheme; the database key does not affect them. There is no key rotation UI or tool yet. Use the same key for every restart and deployment, and do not treat an environment change as rekeying.

## Attachments

Use the chat's plus button to attach PNG, JPEG, WebP, or PDF files. A message can contain up to four attachments, with a limit of 20 MiB per file and 80 MiB total. Text is optional when attaching files. Remove a selected file with its preview's remove button before sending.

PDFs appear as filename cards in the composer and chat history. Composer cards are static previews. In chat history, select a sent PDF card to open the stored file in a new browser tab. Assistant Markdown links to attachment URLs, including `#page=N` citations, also open in a new tab. Kritui stores the original bytes, serves them only to the signed-in owner, and sends them as native file or document content through Responses, Messages, or Chat Completions when the selected model advertises PDF input. If model metadata says PDFs are unsupported but images are supported, Kritui converts up to 200 pages to JPEG images named `<pdf name> - page N.jpg`, adds the automatic "PDF page images" append snapshot to the user message, and sends those images instead. The append appears under that message's Appends control but is not listed in Settings or the chat append picker. Conversion stops at 80 MiB of generated images. If the model supports neither PDFs nor images, the message is rejected with "This model doesn't support PDFs or images." Local runs need Poppler's `pdfinfo` and `pdftoppm`; the Docker image installs Poppler. Attachments remain available after reloading a chat, editing message text, undoing and redoing a turn, or retrying a completion.

## Docker

Configure the LLM connection and encryption key in `.env`, then build and run the image:

```bash
docker build --tag kritui:latest .
docker run --rm \
  --name kritui \
  --publish 8080:8080 \
  --env-file .env \
  --env KRITUI_SECURE_COOKIES=false \
  --volume kritui-data:/data \
  kritui:latest
```

Keep `--env KRITUI_SECURE_COOKIES=false` for direct `http://localhost:8080`; omit it, or set `KRITUI_SECURE_COOKIES=true` in `.env`, when the app is served through HTTPS or a TLS-terminating reverse proxy. Open the app and create an account. On an existing database, sign up as Skrittle once to claim the legacy data as described above.

The named volume persists the SQLite database. It does not contain `KRITUI_ENCRYPTION_KEY`; keep the key in `.env` or your secret manager and back it up separately. The image includes a private SearXNG service with JSON responses enabled and rate limiting disabled; the app is preconfigured to use it at `http://127.0.0.1:8081`. It also includes isolated Git runtime files required by the optional Git tool capability.

## Container Publishing

Forgejo Actions builds and publishes the image after every push to `main`:

```text
git.skrittle.net/rebel_alliance/kritui:<full-commit-sha>
git.skrittle.net/rebel_alliance/kritui:main
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

With [Air](https://github.com/air-verse/air) installed, copy `.env.example` to `.env`, set `KRITUI_ENCRYPTION_KEY`, and run:

```sh
air
```

Air sources `.env` with shell syntax before launching the app, so comments and shell-quoted values work. Set `KRITUI_SECURE_COOKIES=false` in `.env` for Air's local HTTP proxy at <http://localhost:7331>. Create an account through the normal signup form. On an existing database, sign up as Skrittle once or sign in with its current password. Air regenerates templates, rebuilds on Go, templ, or CSS changes, and serves its proxy at <http://localhost:7331>.

## Verify

```sh
go test ./...
go vet ./...
```

## Structure

- `main.go`: embeds browser assets and the database schema, then launches `internal/app`
- `internal/app/`: HTTP routes, authentication, chat/completion/inbox flow, migrations, and application tests
- `templ/`, `static/`: server-rendered components and embedded browser assets
- `llm/`: non-streaming API client and tool-capable conversation loop
- `commands/`: slash-command registry and built-ins
- `tools/`: validated model-tool registry and web-tool implementations
- `tools/git/`: read-only public Git repository inspection tools
- `db/`: SQLite schema and queries

The server registers `webfetch` and `websearch`. Only tools selected in the chat options are exposed to the LLM for that request.
