# Architecture

The independently built desktop retains the official `com.openai.codex` bundle
identifier so Sparkle can authenticate and install official app-bundle updates.
Its separate display name, launcher, Chromium user-data directory, URL scheme,
and signing team isolate runtime state from `/Applications/ChatGPT.app`. The
Computer Use helper keeps the independent `com.cdxmux.sky.CUAService` identity.
The `.codex-mux` state directory remains stable so connected accounts and sticky
thread ownership survive rebuilds.

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
Eligible temporary accounts form a strict priority class ahead of regular
accounts; the existing quota score orders accounts inside that class.
Quota observations have three states: available, exhausted, and unknown. A
temporary `account/rateLimits/read` failure keeps the current healthy owner and
does not make the pool appear depleted. New routing prefers accounts with
known available quota, then may use an unknown account rather than falsely
claiming every subscription is exhausted.
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
the current turn then override those source settings. Capability parsing also
honors `config.model` and `config.model_reasoning_effort`. Model overrides on
`thread/resume` and `thread/fork` are checked against the current owner; when
that owner is definitively incompatible, a compatible same-boundary child uses
the shared rollout path and its effective response becomes authoritative.

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

Temporary accounts add an explicit disposable lifecycle. Structured
`usageLimitExceeded`, upstream HTTP 429, `unauthorized`/401, and narrow terminal
token-refresh messages trigger retirement. Generic 403 entitlement failures,
tool-specific 429s, timeouts, and 5xx responses do not. A direct rejected
`turn/start` can be retried after the thread is resumed on a compatible normal
candidate because no successful start response was returned. Failures reported
after a turn has begun are not replayed; instead, resumable owned threads are
evacuated for later turns. Retirement stops the child, removes the account from
`state.json`, and deletes `auth.json`. The now credential-free Codex home is
retained because target resumes reference its rollout paths.

`thread/list` results are always treated as partial observations because the
protocol supports search, working-directory, archive, source, and ancestry
filters. The router merges newly observed ownership without pruning unseen
metadata; this also prevents a stale listing from deleting a thread created
while the listing was in flight. Timestamp results are merged using the
requested `sortKey` and `sortDirection`; `section_position` keeps the
app-server's order because that opaque position is not exposed on `Thread`.

The Controller is authoritative for server-generated global identities such
as projects and thread sections. Threads that reference such identities are
persistently Controller-affined, and a cross-process section reference is
rejected instead of being sent to a database that cannot resolve it. Global
mutations with client-defined identity or idempotent desired state—currently
`environment/add`, `skills/extraRoots/set`, and
`experimentalFeature/enablement/set`—must succeed on every enabled child before
the Controller response is returned. Their desired runtime values are replayed
after a child is started and initialized, so a child restart does not recreate
split-brain state. `thread/loaded/list` is the de-duplicated union of all live
children.

Child exit removes that process from the live map, fails outstanding desktop
RPCs, drops its outstanding server-request routes, and restarts the child while
its account remains enabled. Proxied Desktop RPCs and app-server-initiated
requests have protocol lifetimes: they remain routed until a response arrives
or the owning child/connection exits. The 30-second control timeout is limited
to router-owned metadata and diagnostic calls, so a long `command/exec`, MCP
elicitation, or human approval is not converted into a false timeout while the
real operation continues. Initialization fans out concurrently under one
control timeout, but only the Controller response is returned as the logical
server identity; a Secondary success cannot mask Controller failure.

## Account isolation

The Primary account uses `~/.codex`. Added accounts use
`~/.codex-mux/accounts/<id>/codex-home`. Managed configuration is copied from
the Primary account, excluding credential-store settings and project trust.
Each isolated account forces file-backed CLI and MCP OAuth credentials.
Configuration is parsed as TOML, synchronized only when the Primary content
changes, and not rewritten when the generated secondary content is identical.

## Desktop integration

The patcher extracts `app.asar`, verifies one complete upstream structural
profile, inserts the account UI and an update-install preparation hook, and
repacks the archive with an updated integrity hash. The app receives a separate
Chromium profile and URL scheme. Sparkle remains enabled. Before an update is
installed, an external coordinator snapshots the working bundles; after the
official update it verifies compatibility and either rebuilds the router or
restores the prior version.

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
