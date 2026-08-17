# Architecture

The independently built desktop uses bundle identifier `app.cdxmux.multi`; its
Computer Use helper uses `com.cdxmux.sky.CUAService`. Neither identifier is used
by the official ChatGPT installation. These identifiers and the `.codex-mux`
state directory remain stable across the product rename so existing macOS
privacy grants, connected accounts, and sticky thread ownership continue to
work.

Codex Subscription Router replaces the copied app's bundled `codex` executable
with a small Go multiplexer and keeps the original binary beside it as
`codex.real`.

## Request routing

The desktop app opens one JSON-RPC app-server connection to the multiplexer.
The multiplexer starts one real app-server child for every enabled account,
each with its own `CODEX_HOME` and `CODEX_SQLITE_HOME`.

New threads are assigned using a quota-urgency score: weekly percentage
remaining divided by the hours until that account resets. Banked usage resets
add a capped bonus, while short-window usage, existing pinned-thread count, and
stable account order break close results. Reset-credit metadata is fetched in
parallel, cached for five minutes, and treated as neutral when unavailable.
Once a thread ID is known, `state.json` persists its owner. Requests, responses,
approvals, and notifications are rewritten only as needed to preserve one
coherent desktop session.

If the owner is depleted in either its short or long quota window, the
multiplexer resumes the rollout on an account with capacity that advertises
the thread's concrete model, reasoning effort, and service tier. Capability
queries include hidden catalog entries. The model selector is the
de-duplicated union of successful enabled-child `model/list` responses, and a
single failed secondary no longer hides every other account's catalog. When
the same model occurs on multiple accounts, the selector retains one actual
account's complete capability tuple. It does not independently union effort
and tier dimensions, which would advertise combinations that no account has.

Thread owner, model, reasoning effort, and service tier metadata are persisted
together. The router learns authoritative settings from effective
`thread/start`, `thread/resume`, and `thread/fork` responses, plus
`thread/settings/updated` and `model/rerouted` notifications. Omitted fields
leave stored state unchanged; explicit `null` values record a cleared/default
setting. Before a failover, the source process is resumed/rejoined without
overrides so historical pre-router threads can recover their effective model
without relying on a nonexistent `Thread.model` field. Explicit settings on
the current turn then override those source settings.

A successful failover uses the target resume response as the effective model
baseline and injects the resulting values into the first target turn so the
target does not silently fall back to different defaults. Per-thread locks
serialize failover; asynchronous `thread/started` notifications can only learn
a previously unknown owner, and owner changes use idempotent compare-and-swap
plus rollback when the target request cannot be sent.

Organization failover is allowed only inside the same verified workspace and
plan class. Personal plans are treated as the explicitly shared personal pool;
Business/Team, Enterprise, and Education are separate classes. Paginated
threads fail closed instead of attempting cross-process migration because the
current app-server schema exposes no verified operation that releases the
single-process writer. A quota error received after `turn/start` was submitted
is not replayed, because the router cannot prove the failed turn had no
external side effects. Threads do not migrate for ordinary load balancing.

`thread/list` results are always treated as partial observations because the
protocol supports search, working-directory, archive, source, and ancestry
filters. The router merges newly observed ownership without pruning unseen
metadata; this also prevents a stale listing from deleting a thread created
while the listing was in flight.

Child exit removes that process from the live map, fails outstanding desktop
RPCs, drops its outstanding server-request routes, and restarts the child while
its account remains enabled. Proxied Desktop RPCs and app-server-initiated
requests have protocol lifetimes: they remain routed until a response arrives
or the owning child/connection exits. The 30-second control timeout is limited
to router-owned metadata and diagnostic calls, so a long `command/exec`, MCP
elicitation, or human approval is not converted into a false timeout while the
real operation continues. Initialization fans out concurrently under one
control timeout.

## Account isolation

The Primary account uses `~/.codex`. Added accounts use
`~/.codex-mux/accounts/<id>/codex-home`. Managed configuration is copied from
the Primary account, excluding credential-store settings and project trust.
Each isolated account forces file-backed CLI and MCP OAuth credentials.
Configuration is parsed as TOML, synchronized only when the Primary content
changes, and not rewritten when the generated secondary content is identical.

## Desktop integration

The patcher extracts `app.asar`, verifies exact upstream anchors, inserts the
account UI, disables self-update, and repacks the archive with an updated
integrity hash. The app receives a separate Chromium profile and URL scheme.

The copied Computer Use service, Node runtime, and callers are re-signed under
one Apple team. The helper uses a separate bundle identity and socket, avoiding
the official app's privacy grants and app-group container.

## Plugin behavior

Plugin definitions and managed MCP configuration are shared. The Plugins page
adds an account selector and marks Apps, MCP status, and MCP OAuth requests with
the selected account ID. The multiplexer removes that private routing marker
before forwarding the strict RPC request to the chosen child.

## Control API

The renderer talks to a loopback-only HTTP service on port 48123. All private
routes require a random 256-bit token. CORS is limited to the copied app's
`app://-` origin. The service exposes account metadata, aggregated usage and
profile data, thread ownership, login/logout actions, and an authenticated SSE
event stream; it never returns OAuth tokens. SSE sends the token in a request
header, not the URL. A missing build token or occupied control port fails the
app-server closed instead of silently sending credentials to an unknown
listener. Runtime token and port overrides are intentionally unsupported
because renderer and server configuration must stay identical.
