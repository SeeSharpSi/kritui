# syntax=docker/dockerfile:1

FROM golang:1.25.3-alpine AS build

LABEL io.github.seesharpsi.kritui="true"

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY *.go ./
COPY db/ ./db/
COPY llm/ ./llm/
COPY markdown/ ./markdown/
COPY static/ ./static/
COPY templ/ ./templ/
COPY tools/ ./tools/

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/kritui .

FROM searxng/searxng:2026.7.31-057a77168

LABEL io.github.seesharpsi.kritui="true" \
    org.opencontainers.image.title="Kritui" \
    org.opencontainers.image.description="Server-rendered LLM chat UI bundled with SearXNG" \
    org.opencontainers.image.source="https://github.com/SeeSharpSi/kritui"

USER root

COPY --from=build --chown=0:0 --chmod=0555 /out/kritui /usr/local/bin/kritui

COPY --chown=0:0 --chmod=0444 <<'EOF' /usr/local/searxng/kritui-settings.yml
use_default_settings:
  engines:
    keep_only:
      - duckduckgo
      - mojeek
      - wikipedia
      - github

search:
  max_page: 0
  ban_time_on_fail: 5
  max_ban_time_on_fail: 60
  suspended_times:
    SearxEngineAccessDenied: 60
    SearxEngineCaptcha: 300
    SearxEngineTooManyRequests: 60
  formats:
    - html
    - json

server:
  limiter: false
  public_instance: false
  method: GET
  image_proxy: false

engines:
  - name: mojeek
    disabled: false

outgoing:
  pool_connections: 20
  pool_maxsize: 10
  request_timeout: 5.0
  enable_http2: false
EOF

COPY --chmod=0555 --chown=0:0 <<'EOF' /usr/local/bin/start-kritui
#!/bin/sh
set -u

searxng_pid=
kritui_pid=

stop() {
    if [ -n "$searxng_pid" ]; then
        kill -TERM "$searxng_pid" 2>/dev/null || true
    fi
    if [ -n "$kritui_pid" ]; then
        kill -TERM "$kritui_pid" 2>/dev/null || true
    fi
    if [ -n "$searxng_pid" ]; then
        wait "$searxng_pid" 2>/dev/null || true
    fi
    if [ -n "$kritui_pid" ]; then
        wait "$kritui_pid" 2>/dev/null || true
    fi
}

shutdown() {
    trap - TERM INT
    stop
    exit 0
}

trap shutdown TERM INT

if [ -z "${SEARXNG_SECRET:-}" ]; then
    SEARXNG_SECRET="$(head -c 32 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | cut -c 1-32)"
    export SEARXNG_SECRET
fi

(cd /usr/local/searxng && exec ./entrypoint.sh) &
searxng_pid=$!
/usr/local/bin/kritui &
kritui_pid=$!

while kill -0 "$searxng_pid" 2>/dev/null && kill -0 "$kritui_pid" 2>/dev/null; do
    sleep 1
done

if ! kill -0 "$searxng_pid" 2>/dev/null; then
    wait "$searxng_pid"
    status=$?
else
    wait "$kritui_pid"
    status=$?
fi

trap - TERM INT
stop

if [ "$status" -eq 0 ]; then
    exit 1
fi
exit "$status"
EOF

RUN mkdir -p /data && chown 977:977 /data

ENV SEARXNG_SETTINGS_PATH=/usr/local/searxng/kritui-settings.yml \
    SEARXNG_URL=http://127.0.0.1:8081 \
    SEARXNG_PORT=8081 \
    SEARXNG_BIND_ADDRESS=127.0.0.1 \
    SEARXNG_LIMITER=false \
    SEARXNG_PUBLIC_INSTANCE=false \
    SEARXNG_METHOD=GET \
    GRANIAN_HOST=127.0.0.1 \
    GRANIAN_PORT=8081

WORKDIR /data
USER 977:977

VOLUME ["/data"]
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8080/ \
        && wget -q -O /dev/null http://127.0.0.1:8081/ \
        || exit 1

ENTRYPOINT ["/usr/local/bin/start-kritui"]
