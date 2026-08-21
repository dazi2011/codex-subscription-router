# Codex Subscription Router

![Multi-subscription account menu](screenshots/account-menu.png)

Use multiple ChatGPT subscriptions from one independent macOS desktop app.

Codex Subscription Router creates a locally patched copy of the official
ChatGPT app, balances new chats across connected subscriptions, and keeps every
thread on one subscription so follow-up turns retain conversation context and
benefit from account-level caching.

The official ChatGPT installation is used only as build input and is never
modified. This repository contains source code and build tooling—not OpenAI
binaries or a prebuilt application.

> [!WARNING]
> This is an unofficial, version-sensitive project. It is not affiliated with
> or supported by OpenAI. Review the source and ensure your use complies with
> the terms governing every connected subscription.

![Combined multi-account profile](screenshots/combined-profile-20px.png)

## Highlights

- **Quota-aware routing.** New chats favour weekly allowance that will expire
  sooner, with a bounded boost for accounts holding banked usage resets.
- **Disposable subscriptions.** An account added as temporary is preferred for
  new chats and ordinary failover, then automatically removed after an explicit
  subscription 429 or terminal token/reauthentication failure.
- **Sticky conversations.** Once a thread is assigned, every follow-up returns
  to the same subscription unless that subscription is depleted.
- **Guarded automatic failover.** A depleted legacy-history thread continues
  through another account only when quota, model/reasoning/tier capability,
  and the account data boundary are compatible; otherwise the app shows an
  explicit error.
- **Multi-account model discovery.** The model selector is the de-duplicated
  union of enabled subscriptions rather than the Primary account's list;
  duplicate models retain one real account's correlated capability tuple.
- **Long-lived approvals and commands.** Proxied RPCs are tied to their actual
  response or child connection, so the router does not cancel a human approval
  or a still-running command after 30 seconds.
- **Controller-consistent global state.** Server-generated project and section
  identities remain Controller-affined, while safe process-wide settings are
  synchronized across enabled children before success is reported.
- **Native account management.** The existing profile menu shows pooled usage,
  profile photos, plan names, masked emails, and device-code sign-in.
- **Account-aware settings.** Profile statistics can be viewed together or per
  subscription, while the Plugins page can switch Apps and MCP connections
  between accounts.
- **Per-account resets.** The native rate-limit sheet shows and consumes resets
  for the selected subscription.
- **Working macOS integrations.** The copied Appshots and Computer Use helper is
  independently identified and signed so it can receive its own privacy grants.

## How it works

The patched desktop still opens one app-server connection. A small Go
multiplexer fans that connection out to one official Codex child per account.
Each child has an isolated Codex home, while the multiplexer records the owner
of every thread.

```text
Codex Subscription Router.app
        │
        │ one app-server connection
        ▼
    codex-mux
    ├── Primary       → ~/.codex
    ├── Subscription 2 → isolated Codex home
    └── Subscription 3 → isolated Codex home
             │
             └── thread ID → persistent account owner
```

New-thread routing first prefers eligible temporary subscriptions, then compares
the quota burn rate needed before each weekly reset and applies a capped
banked-reset boost. Short-window usage, pinned-thread count, and stable account
order break close results. Existing threads do not migrate merely for load
balancing.

Read [the architecture](docs/ARCHITECTURE.md) for the request flow and
[the security model](docs/SECURITY-MODEL.md) for trust boundaries.

## Compatibility

Codex Subscription Router currently targets:

| Component | Supported value |
| --- | --- |
| Platform | macOS on Apple silicon |
| Official ChatGPT version | `26.803.61601` |
| Official bundle build | `6396` |
| Go | 1.26 or newer |
| Node.js | 22.12 or newer |

The patcher verifies the official version, build, ASAR hash, renderer anchors,
and native binary constants before changing anything. An unknown upstream build
is rejected by default rather than being partially patched. See
[Compatibility](docs/COMPATIBILITY.md) for the recorded hash and test details.

## Requirements

- The official ChatGPT app installed at `/Applications/ChatGPT.app`
- Xcode Command Line Tools
- Go 1.26+
- Node.js 22.12+ and npm
- An Apple Development or Developer ID Application signing identity

A team-backed signing identity is required for reliable Appshots and Computer
Use permissions. Ad-hoc signing is intended only for diagnostics.

## Install

Run one command. It downloads or updates the source, installs the locked build
dependency, creates the independently signed app, and launches it:

```sh
curl -fsSL https://raw.githubusercontent.com/dazi2011/codex-subscription-router/main/install.sh | /bin/bash
```

> [!CAUTION]
> This fork's audit remediations have source-level validation but do not yet
> have a signed-app E2E report or immutable release tag. The command above
> follows this fork's `main`. Inspect and pin a reviewed commit when
> reproducibility matters; do not treat the older v0.1.0 E2E report as evidence
> for the remediated code.

The installer keeps its source checkout in
`~/.codex-subscription-router/source`. On an existing installation it uses the
same account state, creates a recoverable backup, and requires signing-team
continuity so macOS privacy grants remain valid. It stops with a clear message
instead of making a partial installation when a prerequisite or upstream
compatibility check fails.

> [!TIP]
> To inspect the installer before running it, open
> [`install.sh`](install.sh) or download it without piping it into a shell.

### Install via prompt

> Install Codex Subscription Router from `https://github.com/dazi2011/codex-subscription-router` on this Mac using the repository's supported one-command installer, without modifying the official ChatGPT app or deleting any existing router state. Verify the resulting app and Computer Use helper signatures, launch the app, and ask me only if a prerequisite or macOS permission requires interaction.

### Install from a clone

```sh
git clone https://github.com/dazi2011/codex-subscription-router.git
cd codex-subscription-router
npm ci --ignore-scripts
python3 scripts/patch_app.py
open "$HOME/Applications/Codex Subscription Router.app"
```

This creates:

- `~/Applications/Codex Subscription Router.app`
- `~/Applications/Codex Subscription Router Computer Use.app`
- an independent desktop profile under
  `~/Library/Application Support/Codex Subscription Router`

The first valid Developer ID Application identity is selected, falling back to
an Apple Development identity. Select a certificate explicitly when needed:

```sh
CODEX_MUX_SIGNING_IDENTITY="Developer ID Application: Example Corp (TEAMID1234)" \
  python3 scripts/patch_app.py
```

Reuse the same Apple team for every rebuild. Changing teams changes the app's
designated requirement and can invalidate existing macOS privacy consent. The
patcher refuses an unexpected team change unless you deliberately pass
`--allow-signing-team-change`.

For diagnostic builds without a certificate:

```sh
python3 scripts/patch_app.py --allow-adhoc-signing
```

Appshots and Computer Use may not function with an ad-hoc signature.

## Grant macOS permissions

Open **System Settings → Privacy & Security** and grant:

| Permission | Application |
| --- | --- |
| Accessibility | Codex Subscription Router |
| Screen & System Audio Recording | Codex Subscription Router Computer Use |

When macOS offers **Quit & Reopen**, use it. If the app does not relaunch,
reopen Codex Subscription Router manually. If the Computer Use row does not
appear, press the plus button and choose
`~/Applications/Codex Subscription Router Computer Use.app`.

Do not select the official ChatGPT or Codex Computer Use helper for this build;
the independent app has its own identity and permission rows. macOS may also
request Automation access the first time Computer Use controls another app.

## Add subscriptions

1. Open the profile menu at the bottom of the sidebar.
2. Select **Add regular subscription**, or **Add temporary subscription** for a
   disposable account that should be consumed first and removed automatically
   after a confirmed 429 or terminal authentication failure.
3. Complete the displayed device-code sign-in in your browser.
4. Return to Codex Subscription Router and wait for the account row to appear.

While the code is visible, clicking away does not dismiss the menu. Clicking
the code copies it and opens the verification page.

The profile menu displays combined weekly usage followed by one row per
subscription. Temporary rows are labeled explicitly. Email addresses remain
masked until hovered. The final two rows start regular or temporary sign-in.

## Routing behavior

| Situation | Behaviour |
| --- | --- |
| New chat | Eligible temporary accounts first; otherwise assigned by quota-at-risk, banked resets, and short-window pressure |
| Follow-up | Sent to the thread's persisted account owner |
| Either owner quota window depleted | Continued only through a capability- and data-boundary-compatible account |
| Existing thread changes model/tier | Owner capability is rechecked; a compatible legacy thread can move |
| Resume/fork supplies model config overrides | `config.model` and reasoning effort are checked before routing; a compatible same-boundary child uses the shared rollout path |
| Historical thread lacks router model metadata | Effective settings are recovered lazily from its source app-server before failover |
| Settings update or server model reroute | Effective capability state is updated from the corresponding notification |
| Quota read temporarily fails | Treated as unknown, not depleted; the current healthy owner stays in place |
| Project or thread-section identity is referenced | Kept on the Controller; cross-process section references fail explicitly instead of corrupting state |
| Thread list requests a sort order | Timestamp keys honor the requested direction; section order is preserved |
| Approval or command exceeds 30 seconds | Remains pending until its real response or child exit |
| Paginated thread needs failover | Rejected explicitly; no verified cross-process writer-release RPC exists |
| Post-submit quota error | Original error returned without replaying side effects |
| Temporary account returns a direct 429 before the turn starts | Current request is moved through normal capability/data-boundary failover, then the temporary account is removed |
| Temporary account reports 429 or terminal authentication failure during a running turn/probe | Owned resumable threads are moved best-effort for subsequent turns; the credential and account row are removed immediately after evacuation |
| Every account depleted | Combined quota alert with the next known reset |
| Account disabled | Excluded from routing and pooled usable quota |

The subscription assigned to the current thread appears in its pinned summary.

## Profiles, plugins, and resets

**Profile statistics** begin in a combined view with overlapping account
photos. Select a photo to see only that subscription's identity and statistics;
select it again to return to the combined view.

**Settings → Plugins** includes a subscription picker. Plugin definitions and
managed MCP configuration are shared, while Apps, connection status, and OAuth
login are scoped to the selected subscription.

**Rate-limit resets** remain native to the app, with an account picker added to
the sheet. Selecting a subscription changes the displayed balance and ensures
the reset is consumed only for that account.

![Account-scoped plugin connections](screenshots/plugin-account-picker-secondary-final.png)

## Update or rebuild

The copied app's updater is disabled so an official update cannot overwrite the
patch. Update `/Applications/ChatGPT.app`, verify that the new build is listed
as compatible, then rebuild:

```sh
python3 scripts/patch_app.py --force
```

Quit Codex Subscription Router and its Computer Use helper first. Existing
destinations are moved to timestamped directories under `~/.codex-mux/backups`;
account state and credentials are stored outside the app bundle and remain
intact. Delete old backups manually after the rebuilt app passes the smoke test.

Build separately for each macOS user. Generated bundles contain user-specific
helper and socket paths and are not relocatable or intended for redistribution.

## Local data and security

| Path | Purpose |
| --- | --- |
| `~/.codex` | Primary credentials, conversations, and cache |
| `~/.codex-mux/state.json` | Account metadata plus sticky thread owner and effective model settings |
| `~/.codex-mux/accounts/<id>/codex-home` | Isolated secondary account data; an auto-retired temporary account keeps credential-free rollout history so migrated chats remain resumable |
| `~/.codex-mux/control-token` | Token for the loopback-only control service |
| `~/.codex-mux/backups` | Recoverable app and helper backups |
| `~/Library/Application Support/Codex Subscription Router` | Independent desktop profile |

The control service binds only to `127.0.0.1` and protects private routes with a
random 256-bit token. OAuth tokens stay inside their account's Codex home and
are never returned by the control API. Account directories are owner-only.

Plugin configuration is intentionally synchronized from the Primary account.
Inline secrets inside shared MCP configuration are therefore copied to each
isolated account home; the account homes are not separate secret boundaries.

See [SECURITY.md](SECURITY.md) before reporting a credential, signing, or local
control-service issue.

## Development and verification

```sh
npm ci --ignore-scripts
npm run check
npm run release:check
```

The injected renderer has no runtime package dependency. The Go backend uses a
locked TOML parser for safe managed-config merging; `@electron/asar` remains
build-only. Deterministic UI preview routes are enabled only
when `CODEX_MUX_UI_TESTS=1` is present at launch and remain token-authenticated.

The signed-app test procedure is in [SMOKE-TEST.md](docs/SMOKE-TEST.md). The
latest completed run is recorded in
[E2E-REPORT-0.1.0.md](docs/E2E-REPORT-0.1.0.md).
The static-review remediation and remaining evidence boundaries are tracked in
[AUDIT-REMEDIATION.md](docs/AUDIT-REMEDIATION.md).

## Known limitations

- Upstream ChatGPT updates can require new, reviewed patch anchors.
- Quota percentages are not normalized into absolute capacity across Plus,
  Pro, Business, Enterprise, and Edu plans.
- Personal plans form the explicitly shared Personal pool. Business/Team,
  Enterprise, and Education failover requires the same verified workspace and
  plan class. Account-specific plugin OAuth, MCP OAuth, and project trust are
  still not a portable capability contract, so accounts inside an allowed pool
  should still be equivalent for tool access.
- Paginated-history threads cannot automatically fail over. The currently
  verified app-server schema enforces a single process writer but exposes no
  verified writer-release operation; the router fails closed instead of
  racing two app-server processes.
- Filtered `thread/list` results cannot prove deletion. Routing metadata is
  therefore retained for unseen threads and may contain stale entries until a
  future protocol supplies a complete authoritative inventory.
- The injected renderer is trusted with the control API token; renderer code
  execution is therefore control API execution, not a separate security
  boundary.
- Profile and reset-credit features depend on version-sensitive ChatGPT
  backend endpoints in addition to stable app-server RPCs.
- Combined “skills explored” totals can count the same skill once per account
  because the upstream profile response exposes counts rather than skill IDs.
- Generated app bundles are tied to one macOS user and signing team.
- Releases are source-only; patched OpenAI binaries are never distributed.

## Contributing and releases

Read [CONTRIBUTING.md](CONTRIBUTING.md) before submitting changes. Releases use
the source-only process in [RELEASING.md](docs/RELEASING.md) and require a
completed signed-app smoke test for the exact tagged commit.

## License

Project source is available under the [MIT License](LICENSE). ChatGPT, Codex,
and the official macOS application are OpenAI products and are not covered by
this license.
