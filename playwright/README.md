# Kritui Playwright end-to-end checks

Runs real Chromium against the real Kritui HTTP app and a deterministic local fake LLM provider. No internet access, provider key, workspace database, or `.env` file is used.

## Setup

```sh
cd playwright
npm install --ignore-scripts --no-audit --no-fund
```

The package pins `playwright@1.64.1`. Tests launch `/usr/bin/chromium` through Playwright; set `PLAYWRIGHT_CHROMIUM_EXECUTABLE` to override the Chromium path. Browser downloads are not needed.

## Run

```sh
cd playwright
npm test
```

Or directly:

```sh
node --test --test-concurrency=1 --test-timeout=240000 e2e.test.js
```

The suite builds Kritui in an isolated copy under `$KRITUI_PLAYWRIGHT_TMPDIR`, or `~/.cache/opencode/tmp/opencode` when unset. It runs the generated binary with its working directory in a temporary directory under the same build root, so the workspace `data.db` and `.env` are never opened or modified. Each case gets a fresh database, account, and app process. Port `:8080` is required; the suite refuses to start or stop an unrelated service using that port.

## Harness

- `lib/fake-provider.js` serves `GET /v1/models`, a fixture article route, and gated `POST` completion calls on a dynamic loopback port.
- `lib/build.js` copies source without tests, `.git`, `playwright/`, `data.db*`, or `.env*`; generates templ files; and builds the binary.
- `lib/app.js` starts/stops the app, checks `:8080`, waits for `/healthz`, and supports hard restarts.
- `lib/ui.js` drives signup, composer admission, prompt inbox mutations, completion/SSE waits, and retry.
- `lib/isolated-case.js` serializes cases, starts one fake provider and app per case, and cleans up browser, context, app, provider, and temporary database in `finally`.

Every completion request is released manually after asserting DOM and provider state, so tests are deterministic. Polling has explicit budgets; there are no fixed sleeps around completion timing.

## Cases

1. Steer during a final response starts a second provider call and persists.
2. Queued prompts B and C drain FIFO without browser completion clicks.
3. A tool-call round shows queued input absent from the next continuation while steer input is present; the queue arrives after the final response.
4. Pending prompts survive a reload and support delivery-mode changes and removal.
5. A failed provider parks the queue; Retry recovers and drains it.
6. Idle `Steer now` after a provider failure wakes work, clears stale retry UI, updates the transcript and inbox, and preserves the composer draft.
7. A hard app restart retains the inbox until explicit Retry, then removes stale interrupted-completion UI after success.
8. A composer draft typed after admission survives terminal completion SSE.
9. A steered continuation returns a tool batch that stays in the expanded live list across later tool rounds and reload; pre-steer tools stay collapsed, and the final transcript has no duplicates.
10. The pending inbox sits above the Settings/Model/History toolbar at desktop and mobile widths, retains delivery and model badges, and has no visible heading, counter, or Resume control.

`webfetch` is selected through the form-associated `input[name="tool"][value="webfetch"]` checkbox. Its fixture URL is recorded in the tool call. Kritui's webfetch runtime intentionally rejects loopback/non-default-port URLs, so the test does not depend on the article body being fetched; it verifies the tool round, matching tool result, and next provider request.

## Failure artifacts

On failure Playwright writes:

- `test-results/<timestamp>-<case>/failure.png`
- `test-results/<timestamp>-<case>/page.html`
- `test-results/<timestamp>-<case>/failure.json` with app logs and provider calls
- `test-results/<timestamp>-<case>-trace.zip`

On success the active-queue case writes a representative screenshot to `test-results/screenshots/`, and case 9 writes an active post-steer tool-chain screenshot; case 10 writes a mobile pending-inbox layout screenshot.
