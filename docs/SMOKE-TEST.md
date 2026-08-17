# Signed-app smoke test

Complete this checklist on the exact official build recorded in
`docs/COMPATIBILITY.md` before publishing a release draft. Use a team-backed
signature and reuse the same Apple team as the previous installed build.

## Build and identity

- Confirm the patcher reports the expected version, build, and ASAR SHA-256.
- Verify the official `/Applications/ChatGPT.app` is unchanged.
- Verify the app and every nested Computer Use application with
  `codesign --verify --deep --strict`.
- Confirm the installed app and helper report the intended bundle IDs and the
  same `TeamIdentifier`.

## Accounts and routing

- Connect at least two subscriptions and confirm photos, plans, masked emails,
  pooled usage, and loading states.
- Start chats until each account has received one; confirm every follow-up stays
  on its original account.
- Leave a command approval open for more than 30 seconds, then approve it and
  confirm the original app-server request receives the decision.
- Run a `command/exec` lasting more than 30 seconds and confirm the Desktop does
  not report a timeout while the process continues.
- Spoof one depleted account and confirm the thread continues on an account with
  quota. Spoof all accounts depleted and confirm the combined alert.
- Open a thread created before Router capability metadata existed, spoof its
  owner as depleted, and confirm the source resume recovers its effective model
  before a legacy-history failover.
- Change an existing thread's model, reasoning effort, and service tier through
  thread settings; clear the service tier again and confirm failover does not
  restore the cleared value.
- Run a thread on a Secondary account and confirm `model/rerouted`,
  `model/verification`, and `model/safetyBuffering/updated` reach the Desktop;
  confirm a reroute updates the model used for later failover selection.
- Open a quota-triggered reset sheet, switch subscriptions, consume a reset, and
  confirm only the selected account changes.

## Settings and plugins

- Confirm Profile opens in the combined state, uses 20 px avatar overlap, and
  toggles between combined and per-account statistics.
- In Settings → Plugins, select each subscription and verify Apps, MCP status,
  and MCP OAuth login reflect that account while installed definitions remain
  shared.

## Appshots and Computer Use

- In System Settings, grant Accessibility to Codex Subscription Router and
  Screen & System Audio Recording to Codex Subscription Router Computer Use.
  Quit and reopen when macOS asks.
- Capture an Appshot from the attachment menu and with the Command-key shortcut.
- Run a Computer Use task and confirm the native helper performs the action
  without falling back to `osascript`.
- Rebuild once with the same signing team and confirm existing permissions still
  work without adding duplicate permission rows.

Record the tested commit, macOS version, signing team ID, and any deviations in
the release draft before publishing it.
