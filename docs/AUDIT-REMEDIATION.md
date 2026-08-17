# Audit remediation status

This document maps the 88-item static review of commit
`b30d97769f2b35facaf22c00365832b62f6b123e` to the current source. “Fixed”
means the relevant code path and a focused static or unit check exist. It does
not mean the patched desktop has completed a new signed-app E2E run.

## Fixed in source

- **1–5 — model-aware routing and failover.** `model/list` is aggregated and
  de-duplicated across enabled children. New-thread placement queries each
  candidate's model list. Sticky model, reasoning-effort, and service-tier
  settings are persisted from successful requests, passed to the target where
  the protocol permits, and required in its catalog. Capability queries include
  hidden models, and current-turn overrides take precedence over stored values.
- **6–8 — child lifecycle.** Exited children leave the live map, outstanding
  forwarded requests fail explicitly, enabled children restart, and request
  paths can restart a child on demand. Disabled children are not started.
- **9, 11–13 — failover/list races.** Fixed striped per-thread locks serialize
  migration. Owner updates use idempotent compare-and-swap, while asynchronous
  `thread/started` notifications only fill an absent owner. History is
  de-duplicated by thread ID, and Store reconciliation preserves existing and
  concurrently created owners under the same lock used to write state.
- **14–15 — false quota retry.** Only the structured Codex subscription usage
  code (or an exact top-level usage-limit message) is classified. A submitted
  turn is never replayed after its response, eliminating duplicate external
  side effects.
- **16–20 — account lifecycle.** Failed creation rolls back metadata and the
  new account home. Cancelling or failing device-code login deletes the new
  secondary account. Incomplete accounts are visible and removable. The
  delete endpoint stops the child, removes metadata, owner/model records,
  caches, reset previews, and the managed account directory. Default labels
  are based on all stored accounts.
- **23, 25–28 — control endpoint consistency.** Port binding is fail-closed,
  SSE credentials use a header rather than a URL, a missing token requires a
  rebuild, runtime token overrides are rejected, and runtime port overrides
  are no longer honored.
- **29–31, 33–34 — managed configuration.** The Primary content is hashed
  before fan-out, identical generated files are not rewritten, and a real
  TOML parser handles quoted keys, arrays of tables, and multiline values.
- **42–44 — misleading pool display.** The UI no longer sums relative account
  percentages into values such as 360%. A single percentage is refused for
  mixed plan types instead of averaging incomparable capacities.
- **51–55 — initialization and route lifetime.** Child initialization is
  concurrent under one pool timeout. Desktop-to-child and child-to-desktop
  routes expire. Child exit fails desktop requests that were awaiting it.
- **56–60, 63 — history metadata lifecycle.** History aggregation consumes all
  pages for the requested query, reconciles owner/model maps once, avoids
  unchanged state writes, and reports an incomplete child history instead of
  silently returning a partial list. Because `thread/list` can be filtered and
  is not an authoritative global inventory, reconciliation never prunes unseen
  metadata.
- **65 — duplicate account refresh.** The renderer uses one reconnecting SSE
  stream and event-triggered refresh rather than SSE plus a 30-second poll,
  warm-up poll, and loading-deadline poll.
- **69 — profile-image host validation.** HTTPS is still required and accepted
  hosts are limited to the documented OpenAI/ChatGPT/OAI content domains and
  the supported Google profile-image host.
- **71–78 — enabled, healthy, and quota-unknown state.** Controller selection
  skips disabled accounts, disabling stops the child, startup skips disabled
  accounts, snapshots expose process/RPC health separately from login state,
  missing quota is not treated as capacity, and exhaustion in either advertised
  short or long window makes the account unavailable.

- **Follow-up model/catalog findings.** One failed secondary `model/list` now
  produces a partial union instead of failing the whole selector. Duplicate
  model entries union advertised reasoning and service-tier options. Existing
  threads re-check an explicit model/tier change before forwarding.

## Mitigated, not transactional or independently verified

- **10 — failover atomicity.** Resume, owner change, and send cannot be one
  transaction across two child processes and a filesystem. The source now
  serializes each thread, compares the expected owner, and rolls ownership
  back when the target send fails synchronously. A crash after a successful
  send remains an ambiguous distributed outcome; the target retains the
  resumed thread and the request fails explicitly rather than being replayed.
- **Paginated history single-writer rule.** Cross-process migration is rejected
  before target resume. The verified app-server schema exposes
  `thread/unsubscribe`, but does not specify it as a writer-release operation;
  using it as one would be an unsafe protocol guess.
- **24 — hostile process on port 48123.** The mux now binds before starting
  children and exits if it cannot own the port. The renderer still contains
  the token by design, so a renderer that continues running after app-server
  startup failure remains inside the renderer-compromise boundary described
  below.
- **32 — config write races.** Removing unchanged writes and syncing only after
  Primary content changes removes the continuous race. A Primary change can
  still coincide with a child saving project trust; there is no upstream
  cross-process config transaction.
- **61–62 — very large history.** Child requests run concurrently and the
  complete merge has one 30-second deadline, but an accurate global list still
  has to consume child pages and wait for every successful child.
- **64 — snapshot request volume.** Periodic renderer polling was removed and
  child RPCs are concurrent. A fresh routing decision still needs current
  account and rate-limit data.
- **70 — Controller semantics.** Global RPCs still need one Controller, but a
  disabled Primary no longer remains the effective Controller.
- **88 — failure-path coverage.** Focused tests now cover model matching,
  structured quota classification, owner compare-and-swap/reconciliation,
  model persistence, account removal, unchanged-config mtime, real TOML
  structures, header-only SSE authentication, token mismatch, mixed-plan
  aggregation, and profile host validation. A new signed-app/fault-injection
  E2E run is still required.

## Remaining product, trust, release, or upstream boundaries

- **21–22 — renderer/control trust.** The account UI must be able to call the
  control API. The renderer therefore remains trusted with its token; renderer
  code execution is control API execution. The token protects against
  unrelated web origins, not a compromised renderer.
- **35–41 — release provenance and upstream updates.** A safe default installer
  requires an immutable, reviewed release/tag and an E2E record for this exact
  change. This source checkout must not invent such evidence. The copied app's
  updater remains disabled because an upstream update would erase or break the
  version-specific patch.
- **45–50 — absolute capacity and identity compatibility.** The app-server
  provides relative percentages, not a stable absolute quota unit. Automatic
  failover now separates Personal, Business/Team, Enterprise, and Education;
  organization plans additionally require the same local ChatGPT workspace ID.
  Plugin OAuth, MCP OAuth, and project trust remain account-local and are not
  exposed as a complete portable capability contract. Users should still pool
  only accounts they consider equivalent for tool access.
- **Filtered-history garbage collection.** Retaining unseen metadata is the
  safe choice for filtered and concurrent `thread/list` calls, but stale owner
  entries cannot be collected until the upstream protocol exposes a complete,
  authoritative inventory or an explicit deletion signal.
- **66–68 — OAuth and shared-secret boundary.** Profile/reset features still
  read the account's local OAuth file to call the documented version-sensitive
  ChatGPT endpoints, and managed MCP/plugin config can contain inline secrets.
  Account homes are not claimed as independent secret boundaries.
- **79–81 — reset/profile backend schemas.** Reset-aware routing and profile
  aggregation depend on ChatGPT backend endpoints outside app-server RPC.
  Failures remain bounded and neutral where possible, but upstream schema
  changes cannot be prevented locally.
- **82–87 — patch, signing, TCC, and per-user build identity.** Minified bundle
  anchors, the diagnostic `--allow-untested-source` escape hatch, ad-hoc
  signing limitations, independent TCC identity, signing-team continuity, and
  user-specific output paths are inherent to this patched-app design.

Before a release, run the source checks and then complete
[`SMOKE-TEST.md`](SMOKE-TEST.md) against the exact commit. Record the result as
a new E2E report; do not relabel the older v0.1.0 report as evidence for these
changes.
