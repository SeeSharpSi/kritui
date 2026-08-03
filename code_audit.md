# Kritui Code Audit

## Purpose

This document records a thorough audit of Kritui's HTTP handlers, database layer, LLM client, tool implementations, templates, browser behavior, htmx integration, deployment configuration, and tests.

The audit focuses on:

- Code that can be simpler.
- Incorrect or fragile logic.
- Confirmed and likely bugs.
- Useful refactor boundaries.
- Places where htmx can replace or improve custom browser code.
- Request handlers that mix too many responsibilities.
- Database read and write functions that should own more of their invariants.

Finding numbering follows the original review. Finding 6 is intentionally omitted from this document.

## Review Scope

Files and areas reviewed include:

- `server.go`
- `chat_handlers.go`
- `message_handlers.go`
- `tool_stream.go`
- `handlers_test.go`
- `db/`
- `llm/`
- `tools/`
- `markdown/`
- `templ/*.templ`
- `static/app.js`
- `static/styles.css`
- `Dockerfile`
- `.air.toml`
- `.gitignore`
- `.dockerignore`
- `.env.example`
- `README.md`
- Relevant repository history for schema evolution
- Current official htmx documentation

Generated `templ/*_templ.go` files were not treated as source because they are generated from `.templ` files.

## Severity Definitions

**High** means the issue can break existing data, corrupt conversation behavior, bypass an important trust boundary, or cause conflicting operations under normal supported use.

**Medium** means the issue causes a meaningful correctness, reliability, usability, accessibility, deployment, or maintainability problem, but usually requires a specific failure condition or concurrency pattern.

**Low** means the issue is real but has limited impact, a narrow trigger, or primarily affects consistency and long-term maintainability.

## Executive Summary

Kritui has a coherent server-rendered architecture. Its strongest design choices are HTML fragment responses, scoped htmx navigation, privacy-conscious history handling, explicit tool selection, transactional message writes, sanitized Markdown, and bounded tool operations.

The most important risks are concentrated in five areas:

1. Database upgrades are not reliable because schema changes have not been represented by complete migrations.
2. `webfetch` can access private network resources and follow redirects into them.
3. Chat IDs and completion locks are managed partly in application memory, creating race conditions and expiry-related conflicts.
4. Responses API provider state is not durable across database reloads.
5. Browser error and refresh behavior can leave stale or broken UI despite otherwise good htmx usage.

The highest-value architectural improvement is a small database-backed `Store` that owns schema versions, chat allocation, message append positions, message encoding, and optimistic conversation consistency. The highest-value frontend improvement is standardizing every htmx error as an HTML fragment, then removing broad custom error swapping.

---

## [x] Finding 1: Existing Databases Can Become Unusable After Upgrades

**Severity:** High

**Locations:** `server.go:86-99`, `db/schema.sql`, `db/get.go:23-26`, `db/get.go:113-117`, `db/put.go:89-92`

### Problem

`migrateDatabase` adds only `messages.total_tokens`, `messages.cost`, and the `settings` table. Current queries also require `messages.model` and `chats.tools`, but no migration adds those columns.

Repository history confirms that `messages.model` and `chats.tools` were introduced as direct edits to `db/schema.sql`. Existing databases created before those commits do not receive the columns because `CREATE TABLE IF NOT EXISTS` does not alter an existing table.

### Failure Scenario

An existing user upgrades from a version created before model persistence. Startup migration succeeds because token and cost columns are added or already exist. The first page load calls `GetMessages`, which executes a query selecting `model`. SQLite returns `no such column: model`, and chat rendering fails.

The same pattern applies to `chats.tools`. `GetChats`, `GetChatsPage`, and `GetChatTools` assume the column exists, so history and home-page rendering can fail after an upgrade.

### Additional Weakness

Migration success depends on checking whether error text contains `duplicate column name`. This is brittle across SQLite drivers and versions. It can also hide an unrelated error if its text happens to contain that phrase.

The migration sequence is not explicitly versioned. There is no durable record of which migrations completed, and no straightforward way to test every supported upgrade path.

### Recommendation

Use SQLite's `PRAGMA user_version` or a dedicated migrations table. Define an ordered migration for every schema transition. Run each version transition in a transaction when SQLite permits it.

At minimum, migrations must account for:

- `messages.model`
- `chats.tools`
- `messages.total_tokens`
- `messages.cost`
- `settings`
- Any new column required to preserve Responses API output items

Schema inspection through `PRAGMA table_info` is preferable to string matching when supporting legacy databases with uncertain version metadata.

### Tests

Create fixture databases representing each historical schema. Open each fixture through the current migration code, then exercise `GetChats`, `GetChatTools`, `GetMessages`, settings access, message append, and application restart.

Add an idempotency test that runs all migrations twice and confirms the second run performs no destructive changes.

Add a failure test proving that an unrelated migration error is returned rather than ignored.

---

## [x] Finding 2: `webfetch` Permits Server-Side Request Forgery

**Severity:** High

**Locations:** `tools/webfetch.go:202-213`, `tools/webfetch.go:246-254`

### Problem

URL validation checks only that the string starts with `http://` or `https://`, parses successfully, and contains a host. It does not restrict the resolved destination address.

The configured `http.Client` follows redirects by default. A public URL can therefore redirect to a private address even if initial URL validation becomes stricter.

### Reachable Targets

Current behavior permits requests to:

- Loopback addresses such as `127.0.0.1` and `::1`.
- Private network ranges.
- Link-local addresses.
- Cloud metadata endpoints.
- Services exposed only inside a container network.
- The Kritui process itself.
- The bundled SearXNG process.

### Impact

The LLM can inspect internal HTTP services unavailable to the user directly. Prompt injection in fetched content can also encourage the model to make follow-up requests to internal resources.

Because fetched content is returned to the model, sensitive internal data can be incorporated into an assistant response or tool-call log.

### Recommendation

Reject loopback, private, link-local, multicast, unspecified, and reserved destination addresses. Validation must occur after DNS resolution and at connection time to reduce DNS rebinding risk.

Use a custom transport with a validating `DialContext`. Install a `CheckRedirect` callback that applies the same URL and destination policy to every redirect target.

Reject URL credentials. Consider rejecting non-default ports unless arbitrary ports are a required feature. Consider an HTTPS-only default with an explicit opt-in for HTTP.

### Tests

Add tests for direct IPv4 and IPv6 private addresses, loopback hosts, DNS names resolving to private addresses, public-to-private redirects, multi-hop redirects, URL credentials, alternate ports, and allowed public destinations.

The redirect tests must verify every hop, not only the final URL.

---

## [x] Finding 3: New Chat Allocation Races

**Severity:** High

**Locations:** `chat_handlers.go:27-46`, `db/put.go:16-34`

### Problem

When `/` has no `chat` query parameter, `homeHandler` loads every chat, calculates the highest ID, adds one, and redirects to that ID. No row is created during allocation.

Two simultaneous requests can observe the same chat list and receive the same next ID.

### Failure Scenario

Two browser tabs open `/` concurrently. Both redirect to `/?chat=12`. The first tab submits a message and reserves the in-memory completion tracker for chat 12. The second tab receives a conflict or later appends its message to the first tab's conversation.

This is not merely a theoretical multi-process problem. It can occur in one process with two tabs because allocation and insertion happen in different requests.

### Complexity Cost

`GetChats` loads full chat metadata, including tool JSON and timestamps, only to find a maximum ID. SQLite already provides atomic row ID allocation.

`InsertChat` already exists but is unused by the allocation path.

### Recommendation

Allocate chat identity atomically in SQLite. One practical design is to insert an empty chat and use `LastInsertId` before redirecting.

If empty allocated chats must not appear in history, make history queries include only chats with messages. Add cleanup for abandoned empty chats or reuse the latest empty chat where appropriate.

Another option is a dedicated allocation table, but that is more machinery than this application needs.

### Tests

Run concurrent requests to `/`, follow both redirects, and assert different chat IDs.

Submit first messages from both resulting pages and verify message and tool state remain isolated.

Test abandoned allocations so hidden empty rows do not accumulate indefinitely.

---

## [x] Finding 4: Active Completion Locks Can Expire While Work Still Runs

**Severity:** High

**Locations:** `tool_stream.go:98-138`, `tool_stream.go:141-176`

### Problem

Every tracker has a creation timestamp. `create` removes started trackers after ten minutes whenever another tracker is created. Expiring a tracker removes its chat from `activeChats` and closes its SSE state, but it does not cancel the LLM completion using that tracker.

The timestamp is based on creation, not completion activity or claim time.

### Failure Scenario

A completion runs for more than ten minutes because of a slow model, several long tool calls, or an endpoint that never completes. Another request creates a tracker. Cleanup removes the first tracker and releases the active-chat guard.

A second message or retry can now start for the same chat while the first completion still runs. Both requests can load overlapping history and attempt to append responses.

Possible outcomes include stale completions, unique-position failures, mixed tool state, or conversation history generated from an obsolete snapshot.

### Recommendation

Do not expire active work without canceling it. Store a cancellation function or completion context in the tracker entry. If an active deadline is required, cancellation must happen before the active-chat lock is released.

Use separate lifecycle rules for unclaimed trackers and claimed trackers:

- Unclaimed trackers can expire after a short deadline.
- Claimed trackers should be removed when completion returns.
- Claimed trackers with a hard deadline must cancel their completion before removal.

Tracker activity should not depend on an unrelated future call to `create`.

### Tests

Simulate an active tracker older than the configured deadline. Attempt another request for the same chat and verify either the original work is canceled before replacement or the second request remains blocked.

Add a test where the first completion tries to persist after expiry. Confirm it cannot race with a replacement completion.

---

## [x] Finding 5: Responses API State Is Lost After Database Reload

**Severity:** High

**Locations:** `llm/client.go:21-31`, `llm/responses.go:29-57`, `llm/responses.go:105-109`, `db/put.go:61-102`, `db/get.go:110-161`

### Problem

Responses API output items are stored in private `Message.responseItems`. This preserves reasoning and other provider-specific output during one in-memory conversation.

Database persistence stores only exported message fields. After page reload or process restart, `responseItems` is empty.

### Impact

For ordinary text responses, reconstruction may appear to work. For reasoning models and tool-call chains, provider output can include reasoning items or other state that must be supplied in later input.

After reload, `completeResponse` reconstructs only visible messages and function calls. The exact raw provider sequence is lost. A later continuation can therefore be rejected by the provider or behave differently from the uninterrupted conversation.

### Recommendation

Persist raw Responses output items in a JSON column associated with each assistant message. Restore that data when reading messages.

Avoid exposing an unstructured private field solely to satisfy persistence. Define an explicit provider metadata type or storage representation with clear cloning and validation behavior.

An alternative is provider continuation through response IDs, but that changes retention and endpoint assumptions. Persisting raw items is more compatible with the current stateless request design.

### Tests

Create a Responses completion containing a reasoning item and function call. Persist all generated messages, close and reopen the database, load messages, and send the next user turn.

Assert that the next Responses request contains the original raw reasoning and function-call items in correct order.

---

## [x] Finding 7: Server and Model Requests Have Weak Timeout and Size Controls

**Severity:** Medium

**Locations:** `server.go:80-83`, `llm/client.go:115-122`, `llm/client.go:130-165`, `llm/client.go:177-210`, `chat_handlers.go:273-294`

### Problem

The HTTP server uses `http.ListenAndServe`, leaving header, idle, and other connection timeouts at defaults.

The LLM client uses `&http.Client{}` without a client timeout or transport-level response-header timeout. Successful JSON response bodies are decoded without a maximum size. Error responses are bounded, but success responses are not.

### Impact

A slow client can hold server resources while sending headers. A slow or broken model endpoint can hold home-page, settings-page, model-list, or completion requests indefinitely.

A malicious or malfunctioning endpoint can return a very large valid JSON response and force substantial allocation during decoding.

`availableModels` is especially sensitive because it runs while rendering normal pages. A hung `/models` endpoint can make the application UI unavailable before any completion is requested.

### SSE Constraint

A single global `WriteTimeout` is not enough because `/messages/tools` intentionally keeps an SSE response open. Timeout design must distinguish ordinary requests from streaming responses.

### Recommendation

Construct an explicit `http.Server` with at least `ReadHeaderTimeout` and `IdleTimeout`. Apply request-body limits in handlers.

Configure provider clients with transport-level connect and response-header deadlines. Apply explicit per-operation context deadlines, with a short deadline for model listing and a longer configurable deadline for completions.

Decode successful responses through a bounded reader and reject bodies above the configured maximum.

Use `http.ResponseController` or route-specific write-deadline handling for SSE instead of allowing a global timeout to break streams.

### Tests

Test slow request headers, stalled model-list responses, stalled completion response headers, oversized valid completion JSON, oversized models JSON, and client cancellation.

Verify SSE continues past ordinary response deadlines while still terminating when its request context is canceled.

---

## [x] Finding 8: SQLite Concurrency Configuration Is Fragile

**Severity:** Medium

**Locations:** `server.go:33`, transaction use in `message_handlers.go:118-137` and `message_handlers.go:231-265`

### Problem

The SQLite connection enables foreign keys but does not configure a busy timeout, journal policy, or explicit connection-pool behavior.

`database/sql` can open multiple SQLite connections. Requests for different chats are allowed to write concurrently because the in-memory active-chat map serializes only requests for the same chat.

### Impact

Two short write transactions can overlap and one can fail immediately with `database is locked`, depending on connection and journal behavior.

Current tests hide this risk because in-memory test databases use `SetMaxOpenConns(1)`.

### Recommendation

Choose and document a connection policy. For this application's likely write volume, either of these approaches is reasonable:

- Serialize SQLite through one open connection.
- Use a bounded pool with a busy timeout and WAL mode after confirming deployment filesystem compatibility.

Keep LLM network calls outside database transactions, as the current completion handler already does.

### Tests

Use a temporary file-backed SQLite database with multiple open connections. Submit messages to different chats concurrently and verify both commits succeed or retry cleanly.

Test concurrent rename, delete, submission, and completion persistence.

---

## [x] Finding 9: Broad htmx Error Swapping Can Destroy UI Regions

**Severity:** Medium

**Locations:** `static/app.js:226-231`, `chat_handlers.go:116-154`, `chat_handlers.go:185-230`, `message_handlers.go:268-308`, templates using `data-swap-errors`

### Problem

A global `htmx:beforeSwap` listener forces every HTTP error response to swap when the request source is inside a `data-swap-errors` region.

Several handlers return plain text through `http.Error`. The global listener does not distinguish expected HTML fragments from unexpected plain-text server failures.

### Failure Scenarios

An invalid settings form can return plain text while targeting `#settings-page` with `outerHTML`, replacing the entire settings section with a text node.

A rename error can target `#history-entries` and replace the history list contents with a raw error message.

An unexpected database or rendering error can replace a structured message region with implementation-oriented text.

### Recommendation

Every htmx endpoint should return a response fragment appropriate for its target. Define consistent message, completion, history, and settings error components.

Once error response shapes are reliable, configure htmx `responseHandling` in the existing `htmx-config` meta element. This can replace the global `beforeSwap` override.

Use `HX-Retarget`, `HX-Reswap`, or dedicated error targets when an error belongs somewhere other than the success target.

### Tests

Exercise validation and internal errors for message submission, completion, retry, settings, history loading, rename, and delete.

Verify every response leaves required structural elements in the DOM and displays an accessible error fragment.

Official documentation: <https://htmx.org/docs/#response-handling>

---

## [x] Finding 10: History Becomes Stale After Its First Load

**Severity:** Medium

**Locations:** `templ/history.templ:31-70`, `static/app.js:173-195`

### Problem

History initially contains a loader with custom `loadHistory` trigger. Opening the panel makes JavaScript trigger that loader. The response replaces the loader, so subsequent panel openings have no element capable of loading the first page again.

### Failure Scenario

Open history before sending a first message. History displays `No saved chats yet.` Close history, send a message, then reopen history. The panel still says there are no saved chats because no refresh occurs.

Existing chat titles and ordering can also remain stale after new messages change `updated_at`.

### Recommendation

Move the `hx-get` behavior to the history button or panel and refresh the first page whenever the panel opens.

A second option is to return OOB history updates with every message mutation, but refreshing on open is simpler and guarantees ordering is current.

The browser should not need to search for a loader and manually call `htmx.trigger` for this basic request lifecycle.

### Tests

Load history, close it, create or update a chat, reopen history, and verify current title and ordering.

Test repeated open and close operations during an active history request so duplicate requests remain controlled.

Official documentation: <https://htmx.org/attributes/hx-trigger/>

---

## Finding 11: Completion Network Failures Hide Recovery UI

**Severity:** Medium

**Locations:** `templ/message.templ:139-169`, `static/styles.css:387-394`, `static/app.js:197-231`

### Problem

`.loading-message` is hidden by default and displayed only while it has the `htmx-request` class. That class is removed when an XHR ends, including connection failures where no response fragment exists.

The application handles HTTP error responses through `beforeSwap`, but it does not handle htmx connection errors such as `htmx:sendError`.

### Impact

If the completion request loses its connection, the pending article remains in the DOM but becomes invisible. No completion error component is swapped in, so the user has no visible retry control.

The stored conversation can still end with an unanswered user message. A later submission may make the resulting sequence confusing.

### Recommendation

Keep pending completion state visible until explicit success or error replacement. If hiding the article before request start is important, add a durable started state rather than tying visibility only to `htmx-request`.

Handle `htmx:sendError`, timeout events, and relevant SSE lifecycle events. Render or reveal a retryable error state without depending on a response body.

SSE failure should not determine completion correctness because the completion XHR is authoritative. It should only affect progress visibility.

### Tests

Use a browser test that aborts the completion connection after the user message is stored. Verify pending state remains visible and retry is available.

Test SSE disconnection separately and verify final completion still replaces pending UI when its XHR succeeds.

Official documentation: <https://htmx.org/reference/#events>

---

## Finding 12: Malformed Provider Responses Can Be Accepted or Executed

**Severity:** Medium

**Locations:** `llm/chat_completions.go:33-60`, `llm/convo.go:93-130`, `llm/convo.go:164-174`

### Problem

Chat Completions checks only that at least one choice exists. It does not require an assistant role or require non-empty content or tool calls.

Tool calls are validated individually, but IDs are not checked for uniqueness within a response.

### Impact

An empty choice can be stored as a successful assistant response, leaving the UI apparently complete with no answer.

Duplicate tool-call IDs can execute multiple tools before the next request is sent. Their tool outputs reuse one correlation ID, which can cause provider rejection or incorrect output association.

### Additional Logic Issue

The tool-round limit is checked after executing a round. At the terminal limit, tool calls can execute even though their results will never be sent back to the model. The error text says the limit was exceeded when the counter has reached it.

### Recommendation

Validate the complete provider message before mutating conversation history or executing tools. Require:

- Assistant role for assistant completions.
- Non-empty content or at least one tool call.
- Valid supported finish-reason combinations.
- Unique, non-empty tool-call IDs.
- Supported tool-call type and non-empty function name.

Define tool-round semantics precisely and check the limit before executing calls that cannot be followed by another model request.

### Tests

Add cases for an empty choice message, missing role, wrong role, duplicate tool-call IDs, unsupported finish reasons, `tool_calls` finish reason without calls, and exact round-limit behavior.

Verify duplicate IDs execute no tools.

---

## Finding 13: Request and Chat Title Sizes Lack Application Limits

**Severity:** Medium

**Locations:** `message_handlers.go:29-31`, `message_handlers.go:231-260`, `chat_handlers.go:198-212`

### Problem

Handlers do not apply endpoint-specific body limits. Standard URL-encoded parsing has framework limits, but multipart parsing can spill significant content to disk and current limits are much larger than the application needs.

The first user message is stored in full as the chat title. Rename accepts any non-empty title length.

### Impact

A long prompt becomes a long title returned in every history query and rendered into visible text, accessibility labels, form values, and confirmation text.

Large message bodies can consume memory, disk, database space, LLM context, and rendered HTML size.

### Recommendation

Wrap request bodies with `http.MaxBytesReader` before form parsing. Use small limits for settings and rename, and a deliberate documented maximum for messages.

Create one title normalization function that trims whitespace, chooses a useful first line, and truncates by rune count. Apply the same maximum to manual rename.

Return `413 Request Entity Too Large` for oversized bodies and an HTML error fragment suitable for the request target.

### Tests

Test bodies immediately below and above every endpoint limit. Include URL-encoded and multipart requests.

Test long Unicode prompts to ensure truncation does not split UTF-8 and stored message content remains independent from normalized title.

---

## Finding 14: Completion Persistence Uses Stale Positional Assumptions

**Severity:** Medium

**Locations:** `message_handlers.go:82-137`, `db/schema.sql:22-46`, `db/put.go:61-102`

### Problem

The completion handler loads messages, sets `position := len(messages)`, performs a potentially long completion, then inserts generated messages at positions derived from that earlier length.

This assumes positions are contiguous and conversation history has not changed.

### Failure Scenarios

If positions are `0` and `2`, `len(messages)` is `2`, so the next insert collides with position `2`.

If another process changes the chat after history is loaded, the completion can be based on stale history and can fail during persistence or append a logically obsolete response.

The in-memory tracker prevents most same-process message races, but it does not protect multiple processes or every other mutation.

### Recommendation

Create a database append operation that computes the next position inside the write transaction.

For completion persistence, pass the expected final stored position or expected last message ID. Reject the write with a conflict if history changed while the model was running.

Store all generated messages atomically, as current code already attempts through one transaction.

### Tests

Test gapped positions, concurrent append from two database connections, chat deletion during completion, and history mutation after model execution begins.

Verify conflicts return a retryable application error rather than a generic storage failure.

---

## Finding 15: Docker Health Check Depends on the LLM Endpoint

**Severity:** Medium

**Locations:** `Dockerfile:154-157`, `chat_handlers.go:24-89`, `chat_handlers.go:273-294`

### Problem

The health check requests `/`. That route redirects to a chat URL, and page rendering calls `availableModels`, which performs an outbound `/models` request.

### Impact

A healthy Kritui process and healthy database can be marked unhealthy because the configured model endpoint is slow or unavailable.

The five-second Docker health-check timeout can expire while page rendering waits on the provider.

The health check also creates unnecessary repeated traffic to the model endpoint.

### Recommendation

Add a dedicated `/healthz` endpoint. It should verify only local process readiness and a lightweight database query such as `SELECT 1`.

If provider readiness is operationally useful, expose it as a separate diagnostic endpoint rather than making container liveness depend on it.

### Tests

Verify `/healthz` succeeds while the LLM endpoint is unavailable and fails when the database cannot execute a basic query.

Update Docker health-check tests or container smoke tests to use the new route.

---

## Finding 16: Sensitive Environment and SQLite Sidecar Files Can Be Committed

**Severity:** Medium

**Locations:** `.gitignore:1-4`, `.dockerignore:1-7`

### Problem

`.gitignore` excludes `.env` and `data.db`, but it does not exclude environment variants or SQLite sidecars.

`.dockerignore` already uses broader patterns, demonstrating that the wider set is relevant to this project.

### Risk

Files such as `.env.local`, `.env.production`, `data.db-wal`, `data.db-shm`, and journal files can contain credentials or chat content and can be accidentally committed.

### Recommendation

Use patterns equivalent to:

```gitignore
.env*
!.env.example
data.db*
```

Keep `.env.example` explicitly tracked.

### Tests

No automated application test is required. A repository policy check can run `git check-ignore` against representative names.

---

## Finding 17: Interactive UI State Has Accessibility Gaps

**Severity:** Medium

**Locations:** `templ/history.templ:73-136`, `templ/history.templ:158-166`, `templ/settings.templ:40-55`, `templ/message.templ:81-107`, `static/app.js:173-195`

### Problem

History and settings buttons show and hide controlled regions but expose `aria-pressed`. Disclosure controls should expose `aria-expanded` with `aria-controls`.

The rename form has no visible submit button. Submission depends on pressing Enter in the input, which is not obvious and is weak on touch devices.

Tool-call argument disclosure uses `<strong role="button" tabindex="0">` plus inline keyboard and click handlers. Native controls already provide correct activation semantics.

Opening and closing panels does not manage focus.

### Impact

Screen-reader users receive incomplete state information. Keyboard users can become disoriented when a panel appears elsewhere while focus remains on its toggle. Touch users may not discover how to save a rename.

### Recommendation

Use `aria-expanded` for panel toggles and update it whenever panel visibility changes.

Move focus to the panel heading or first control when opening. Restore focus to the toggle when closing.

Add a visible Save button to rename forms. Use a native `<button>` or `<details>` for tool-call argument disclosure.

Add `aria-current="page"` to the active chat history link.

### Tests

Add browser accessibility tests for keyboard activation, Enter and Space behavior, focus movement, focus restoration, expanded state, rename save and cancel, and active-chat semantics.

---

## Finding 18: Tracker Cleanup Is Lazy and Can Retain State

**Severity:** Low

**Locations:** `tool_stream.go:98-138`, `tool_stream.go:153-176`, `tool_stream.go:178-228`

### Problem

Expired trackers are removed only when `create` is called. An abandoned tracker remains stored indefinitely if no later message submission occurs.

An SSE client attached to an unclaimed tracker can wait indefinitely because no timer closes the tracker by itself.

### Impact

Abandoned tracker maps and SSE handlers can retain memory and connections longer than intended. Repeated unique-chat submissions can increase retained state.

### Recommendation

Use per-entry timers or one store cleanup loop. Expiry should close tracker channels and release active-chat state immediately at the deadline.

Coordinate this change with Finding 4 so active work is canceled before its lock is released.

### Tests

Create an unclaimed tracker, wait beyond its deadline without calling `create`, and verify the tracker disappears and its SSE stream terminates.

---

## Finding 19: Deleting a Missing Chat Reports Success

**Severity:** Low

**Location:** `chat_handlers.go:157-182`

### Problem

`deleteChatHandler` executes the delete but does not inspect `RowsAffected`. A missing chat returns a normal history fragment with HTTP 200.

Rename already checks affected rows and returns `404`, so behavior is inconsistent.

### Recommendation

Check `RowsAffected`. Return a target-appropriate `404` fragment when no chat was deleted.

### Tests

Delete a nonexistent chat and verify `404`, an accessible fragment, and no misleading success response.

---

## Finding 20: Git Repository Cleanup Has Race and Leak Paths

**Severity:** Low

**Locations:** `tools/git.go:306-343`, `tools/git.go:913-936`

### Timer Publication Race

A repository is inserted into `t.repositories` before `repository.timer` is assigned. Concurrent context cancellation or close can remove and clean the repository, after which `open` assigns a timer to an already closed repository.

The result is an unnecessary live timer and delayed callback for state that no longer exists.

### Cleanup Failure Path

`removeRepository` deletes repository bookkeeping and decrements the active count before `os.RemoveAll` succeeds. If removal fails, the repository is marked closed and no tracked state remains to retry cleanup.

Temporary repository data can remain on disk permanently after a transient or permission-related failure.

### Recommendation

Initialize timer and cancellation state before publishing the repository, or publish all lifecycle state under coordinated locking.

Retain failed cleanup records for retry or use a separate cleanup queue. Do not report resource capacity as fully released until disk cleanup is either complete or explicitly tracked as failed.

### Tests

Stress cancellation during repository publication. Inject filesystem cleanup failures and verify retry behavior and correct resource accounting.

The Git tool currently has no dedicated tests despite being the largest and most resource-sensitive source file.

---

## Finding 21: History Timestamps Use Server Timezone

**Severity:** Low

**Location:** `templ/history.templ:189-195`

### Problem

`historyTime` parses a timestamp and formats it with `parsed.Local()`. That uses the server process timezone, not the browser timezone.

Docker environments commonly use UTC, so users can see UTC values presented as ordinary local-looking dates.

### Recommendation

Render the machine-readable timestamp in the existing `<time datetime>` element and localize visible text in browser JavaScript.

Keep an understandable server-rendered fallback for clients without JavaScript. Including a timezone label in the fallback avoids ambiguity.

### Tests

Test a browser in a timezone different from the server and verify displayed history times match browser locale while `datetime` remains valid ISO data.

---

## Finding 22: Vendored htmx Is Behind the Current Patch Release

**Severity:** Low

**Location:** `static/htmx.min.js`

### Problem

The vendored htmx version is `2.0.8`. Current official documentation and release artifacts use `2.0.10`.

Patch releases after `2.0.8` include fixes affecting history URL handling, disabled-element restoration, class cleanup, and selector escaping. History and `hx-disabled-elt` are used extensively in Kritui.

### Recommendation

Upgrade the vendored file to the current pinned patch release. Record the exact version and source integrity value in project documentation so future audits can identify it without inspecting minified source.

Retest boosted navigation, browser Back and Forward, pending completion controls, and controls that are already disabled before an htmx request.

### Tests

Browser tests should cover chat navigation history, disabled send-button state while a panel is open, completion request transitions, and OOB swaps.

---

# Request Handler Refactor Recommendations

## `messageCompletionHandler`

**Location:** `message_handlers.go:67-143`

This handler currently owns request parsing, tracker claiming, database reads, LLM client construction, prompt context construction, conversation execution, tracker observation, generated-message extraction, transaction management, message persistence, error classification, and HTML rendering.

The handler should remain responsible for HTTP concerns only:

- Parse and validate request data.
- Invoke one completion service operation.
- Select HTTP status and render returned view data.

A service operation such as `CompletePendingMessage` should own loading expected history, constructing the conversation, executing it, validating history has not changed, and atomically appending generated messages.

Avoid splitting every statement into tiny helpers. One cohesive service method plus one storage append method is enough.

## `homeHandler`

**Location:** `chat_handlers.go:24-90`

This handler mixes chat allocation, message loading, tool loading, settings loading, model selection, provider model discovery, and page rendering.

Useful boundaries are:

- Atomic chat allocation.
- Chat-page data loading.
- Model catalog lookup and caching.
- Rendering.

Provider model discovery should not block every normal page load indefinitely. A short-lived cache or separately loaded model picker would reduce latency and provider traffic.

## `persistUserMessage`

**Location:** `message_handlers.go:231-266`

This is database write logic located in an HTTP handler file. It marshals tool names, opens a transaction, upserts chat metadata, computes message position, inserts a message, and commits.

Move it into the database package as a single transactional operation such as `AppendUserMessage`. That operation should normalize title, encode tools using the same codec as other chat writes, calculate position, and return enough data for the response.

## `renderHistoryEntries`

**Location:** `chat_handlers.go:233-248`

This helper writes HTTP errors internally and returns no status. Callers cannot know whether it failed. `deleteChatHandler` can continue rendering an OOB message list after history rendering has already failed.

Return a component or an error. Let the handler decide status and response shape. Buffer rendering before writing headers when a render failure is realistically possible.

## Settings, Rename, Delete, and Retry SQL

Handlers currently issue SQL directly for delete, rename, and latest-role lookup. Moving these operations into one store keeps SQL invariants and not-found handling consistent.

This does not require a large repository abstraction. A small `Store` wrapping `*sql.DB` is sufficient.

## `main`

`main` reads configuration, opens and migrates the database, constructs tools, registers routes, and runs the server. This size is currently manageable, but graceful shutdown and explicit server configuration will make construction more involved.

At that point, extract configuration loading and route construction so tests can build the exact production mux without starting a listener.

---

# Database Refactor Recommendations

## Introduce a Small Store

A `Store` type should own `*sql.DB` and centralize database behavior. It should not become a generic ORM.

High-value methods include:

- `AllocateChat`
- `GetChatPage`
- `GetChatsPage`
- `RenameChat`
- `DeleteChat`
- `AppendUserMessage`
- `AppendCompletion`
- `GetMessages`
- `GetLastMessageRole`
- `GetDefaultModel`
- `SetDefaultModel`

## Simplify `GetMessages`

**Location:** `db/get.go:110-161`

The function combines query execution, SQL nullable scanning, tool-call JSON decoding, usage restoration, and message construction.

Extract one `scanStoredMessage` helper or storage DTO. That helper should also restore Responses API metadata when added.

Keep iteration and `rows.Err` handling in `GetMessages`; move only row conversion out.

## Simplify `InsertMessage`

**Location:** `db/put.go:61-102`

The function converts several optional fields into SQL values and serializes tool calls before executing the insert.

Use one storage encoder that validates role-specific invariants and returns insert arguments. This avoids independently evolving read and write rules.

Do not create a separate helper for every nullable field. One message codec is simpler than many micro-functions.

## Own Message Positions in the Database Layer

Callers should not calculate message positions from slice length. Add an append method that computes the next position in its transaction.

Completion append should include an optimistic expected-position check so stale model output cannot silently attach to changed history.

## Validate Role Invariants

The schema permits an empty `tool_call_id` if inserted outside current normalization. Add a stronger check requiring non-empty trimmed IDs for tool-role messages.

The write codec should reject:

- Tool messages without IDs.
- Non-tool messages with tool result IDs.
- Tool calls on non-assistant messages.
- Empty or unsupported roles.
- Invalid serialized provider metadata.

## Remove or Use Dead Database APIs

`InsertChat` and `SetChatTools` are currently not used by application paths. `GetChats` is used primarily for unsafe chat ID allocation.

Atomic allocation can make `InsertChat` useful. After that change, remove APIs that remain unused rather than maintaining duplicate write paths.

---

# Frontend and htmx Assessment

## Patterns That Are Already Good

Kritui's htmx architecture should not be replaced wholesale. Several patterns match official guidance well.

Scoped history links use `hx-boost`, target `main`, select `main` from a full-page response, and retain a valid ordinary `href`. This preserves deep links and full-page fallback.

`hx-history="false"` correctly prevents sensitive conversation HTML from entering htmx's local storage history cache. History restoration can still request the URL from the server.

The SSE integration uses the current extension model with `hx-ext="sse"`, `sse-connect`, named `sse-swap`, and a named `sse-close` event.

OOB swaps are used appropriately to reset the message input and clear a deleted current conversation. `allowNestedOobSwaps` is disabled, reducing accidental nested-fragment extraction.

`hx-sync` and `hx-disabled-elt` are generally being used for the right concern: preventing overlapping submission and completion requests.

## History Loading Should Be Declarative

The current custom `loadHistory` dispatch does not provide lasting refresh behavior. Put the request on the control that owns the interaction or use an explicit panel-open event with `hx-trigger` on a stable panel element.

Official reference: <https://htmx.org/attributes/hx-trigger/>

## Error Handling Should Use Response Semantics

After every handler returns target-appropriate HTML errors, use htmx `responseHandling` configuration instead of globally forcing unknown error bodies to swap.

Response headers such as `HX-Retarget`, `HX-Reswap`, and `HX-Reselect` are appropriate when an error cannot use the normal success target.

Official references:

- <https://htmx.org/docs/#response-handling>
- <https://htmx.org/reference/#response_headers>

## Request Synchronization Is Mostly Correct

The form and completion use the same synchronization anchor with different strategies. This is defensible because submission should be dropped during an active completion, while a newly inserted completion trigger may need to wait until the submission swap finishes.

Document this policy in a browser test before simplifying attributes.

The natural form submit event makes `hx-trigger="submit queue:none"` partly redundant when `hx-sync` already drops overlapping requests. Removing it is reasonable after browser verification.

Official reference: <https://htmx.org/attributes/hx-sync/>

## SSE Should Remain Progress-Only

Completion correctness should depend on the completion POST, not the SSE connection. SSE should report tool progress and close gracefully when work ends.

Handle `htmx:sseError` and `htmx:sseClose` only to improve visible status. Automatic reconnection is provided by the official extension.

Official reference: <https://htmx.org/extensions/sse/>

## Keep Privacy-Conscious History Behavior

`hx-history="false"` is correct for chat content. It prevents htmx snapshots while preserving history navigation through server restoration.

Official reference: <https://htmx.org/attributes/hx-history/>

## Keep Scoped Boosted Navigation

History links correctly return a complete HTML page and use `hx-select="main"` for boosted navigation. This is the expected progressive-enhancement shape.

Official reference: <https://htmx.org/attributes/hx-boost/>

---

# General Simplification Opportunities

## Simplify Scroll Pinning Carefully

`static/app.js:1-149` uses a `WeakMap`, a mutation observer, one resize observer attached to every message block, animation-frame stabilization, and several htmx lifecycle hooks.

The desired behavior is more nuanced than unconditional `scroll:bottom`: keep the view pinned only when the user was already at the bottom, including while content changes size.

A simpler structure may be possible by making `.message-thread` a real layout box instead of `display: contents`, observing that one box with `ResizeObserver`, and keeping one pinned boolean from the scroll event.

Do not replace this behavior with htmx's unconditional scroll modifier unless losing user-controlled scroll position is acceptable.

## Remove Inline Event Handlers

Inline handlers appear in model selection, history rename controls, and tool-call disclosure. Move local interactions to delegated listeners in `app.js` or use native HTML controls.

Benefits include easier browser testing, simpler templates, and a path toward stricter Content Security Policy and `allowEval: false` htmx configuration.

## Remove Dynamic History Page-Size Calculation Unless Proven Useful

`static/app.js:220-223` calculates a history limit from panel height. The default server page size is already close to normal viewport needs.

This hook adds request mutation and test surface for a small optimization. A fixed page size plus intersection pagination is simpler unless profiling demonstrates a meaningful benefit.

## Normalize Button Styling

History, settings, send, panel, and retry buttons repeat overlapping styles. Introduce a small set of CSS button primitives without adding a framework.

## Remove Inline Styles

Completion error and retry controls contain inline style declarations. Move them into semantic classes so theme and responsive behavior stay centralized.

## Review Unused Color Schemes

Files in `color_schemes/` are not connected to application stylesheet loading and use variable names different from `static/styles.css`.

Two winter-creek files have similar names but incompatible variable sets. Remove them if obsolete, or convert all themes to the application's `--color-*` contract and document loading.

## Split `tools/git.go`

The file is over 1,500 lines and combines lifecycle management, validation, subprocess execution, resource monitoring, output parsing, pagination, and every operation.

Split by responsibility while keeping one `GitTool` public API. Suggested files are `git.go`, `git_repository.go`, `git_command.go`, `git_validate.go`, and operation-focused files.

This change is primarily for testability and reviewability, not to increase abstraction.

## Centralize Runtime Configuration

Handlers repeatedly read environment variables for keys, model, and endpoint. Parse configuration once at startup and pass immutable configuration or constructed clients into handlers.

This makes missing configuration fail predictably and makes timeout, response-size, and endpoint policies explicit.

---

# Test Coverage Gaps

## Browser Behavior

Current handler tests inspect generated HTML but do not execute htmx or browser behavior. Add browser tests covering:

- Message submit and pending completion.
- Successful completion replacement.
- HTTP completion error and retry.
- Connection failure and retry visibility.
- SSE tool updates, reconnection, and graceful close.
- History first load, refresh, pagination, rename, and delete.
- Boosted chat navigation and Back/Forward behavior.
- Panel state, focus, and keyboard interaction.
- Send behavior while panels are open.
- Mobile viewport layout.
- Browser-local history timestamps.

## Database

The database package has tests for tool-name JSON only. Add direct tests for:

- Chat insert and allocation.
- Message insert and round trip.
- Every optional message field.
- Responses API metadata round trip.
- Role and tool-call invariants.
- Position append behavior.
- Concurrent file-backed writes.
- Every historical schema migration.
- Migration idempotency.

## Git Tool

`tools/git.go` has no dedicated tests. Add local-repository tests for every operation without network access where possible:

- Open lifecycle through an injected clone or prepared repository.
- Tree parsing.
- Literal search parsing.
- File read pagination.
- Log pagination.
- Diff parsing.
- Path and ref validation.
- Output limits.
- Context cancellation.
- Disk and repository-count limits.
- Timer expiry.
- Cleanup failure and retry.

## Web Search

Add tests for URL construction, query encoding, body-size limits, timeout behavior, malformed JSON, provider errors, empty results, and max-result truncation.

## Web Fetch

Existing tests cover request retry and timeout paths but not destination security or most argument validation. Add SSRF, redirect, response-size, content-type, HTML conversion, Unicode slicing, and timeout-bound tests.

## LLM Protocol

Add tests for malformed roles, empty messages, duplicate tool IDs, unsupported tool-call types, exact tool-round limits, oversized success responses, persistence reload, and context cancellation.

---

# Recommended Remediation Order

## Phase 1: Protect Existing Data

Implement versioned migrations for every historical schema. Add migration fixtures before changing current schema further.

Add durable Responses API metadata in the same migration framework.

## Phase 2: Fix Request Identity and Persistence Invariants

Allocate chat IDs atomically.

Move user-message and completion append behavior into the database layer. Add optimistic history validation for completions.

Configure SQLite concurrency deliberately.

## Phase 3: Secure and Bound Tool and Network Operations

Block SSRF in `webfetch`, including redirects and resolved destination addresses.

Add provider response-size limits, model-list deadlines, completion deadlines, request body limits, and explicit server connection settings.

## Phase 4: Correct Completion Lifecycle

Replace lazy tracker cleanup with deadline-driven lifecycle management.

Ensure active tracker expiry cancels work before releasing the chat lock.

Validate complete provider responses before executing tool calls.

## Phase 5: Standardize Hypermedia Errors and Refresh

Return HTML error fragments from every htmx endpoint.

Replace global error forcing with declarative response handling.

Refresh history on open and add visible recovery for connection errors.

## Phase 6: Improve Accessibility and Simplify Browser Code

Use native interactive elements, correct expanded state, visible rename submission, and focus management.

Simplify scroll observation and remove inline event handlers only after browser tests preserve behavior.

Upgrade htmx to the current pinned patch release.

## Phase 7: Deployment and Maintenance Cleanup

Add `/healthz`, fix ignore patterns, clean unused themes, improve Air environment loading, and split the Git tool into testable files.

---

# Verification Standard

After implementing each phase, run focused tests first, then full project verification:

```sh
go run github.com/a-h/templ/cmd/templ@v0.3.1020 generate
go test ./...
go vet ./...
```

Template generation is required after every `.templ` change. Generated `templ/*_templ.go` files must not be edited manually.
