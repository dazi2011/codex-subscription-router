# Changelog

All notable changes follow [Keep a Changelog](https://keepachangelog.com/) and
this project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- One-command installer with safe source updates, prerequisite checks, signed
  rebuilds, recoverable upgrades, and automatic launch.
- Reset-aware routing that prioritizes weekly quota at risk of expiring and
  gives a bounded boost to subscriptions with banked usage resets.

### Fixed

- Aggregate model discovery across enabled subscriptions and constrain new
  threads and failover to accounts that advertise the requested model.
- Preserve each thread's concrete model across failover and serialize owner
  migration with compare-and-swap and rollback semantics.
- Remove exited children from the live pool, restart enabled children, bound
  forwarded RPC lifetimes, and initialize accounts concurrently.
- De-duplicate merged history without allowing stale account lists to steal a
  migrated thread; reconcile deleted thread metadata after complete listings.
- Avoid replaying already-submitted turns after quota errors.
- Roll back failed account creation, expose secondary account deletion, clean
  cancelled device-code accounts, and align enabled state with child lifetime.
- Parse managed configuration as TOML and avoid unchanged two-second rewrites.
- Fail closed on control-port/token mismatch and authenticate event streams by
  header instead of URL query.

## [0.1.0] - 2026-08-15

### Added

- Multi-subscription routing with quota-aware balancing and sticky threads.
- Account isolation, device-code sign-in, pooled usage, and quota failover.
- Native account menu, masked emails, plan labels, and profile photos.
- Combined Profile statistics with per-account selection.
- Account-scoped Apps and MCP connection state in Settings → Plugins.
- Per-account rate-limit reset selection and pooled depletion handling.
- Independently signed Appshots and Computer Use support.
- Fail-closed upstream compatibility checks and deepest-first nested helper signing.
- Loopback-only, token-authenticated diagnostic UI states.
- Source-only CI, draft release automation, security documentation, and smoke tests.

[Unreleased]: https://github.com/b-nnett/codex-subscription-router/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/b-nnett/codex-subscription-router/releases/tag/v0.1.0
