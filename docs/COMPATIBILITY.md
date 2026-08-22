# Compatibility

The patcher is intentionally tied to known ChatGPT desktop bundle structures.
It verifies every modified renderer, main-process, and native binary anchor and
stops instead of applying a partial patch.

## Release 0.1.0

### Recorded source builds

| Version | Build | `app.asar` SHA-256 | Structural profile | Current evidence |
| --- | --- | --- | --- | --- |
| `26.803.61601` | `6396` | `d5a44ed9e2f1db5f81dbbe85408aed256f3203c5b16f00817bb9d7cd941343cf` | `legacy-26.803` | Historical v0.1.0 E2E; pre-migration updater design |
| `26.818.41509` | `6962` | `8eb91bd9efbf9a4dd04b9b0afdbfcb4e0bab5da18c1919ad74ca327c00c7e791` | `current-26.818` | Current compatibility and full temporary signed build passed |

Only Apple-signed arm64 source bundles from OpenAI team `2DC432GLL2` are
accepted. Version/build/hash identify exact previously inspected artifacts but
are not a compatibility whitelist. `--check-compatibility` extracts an unknown
build in a temporary directory and requires every anchor and native constant in
one complete profile to match. A partial or ambiguous match stops without
changing the destination. `--allow-untested-source` is retained only as a
deprecated command-line compatibility alias and no longer bypasses analysis.

The `26.818.41509` build passed compatibility analysis and a complete temporary
copy/repack/team-sign/strict-signature verification on the current source
commit. That is not evidence that a future real Sparkle update or the full
Desktop UI lifecycle has completed end to end.
