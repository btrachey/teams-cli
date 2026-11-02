# Changelog

All notable changes to this maintained fork are documented in this file.

The format is based on Keep a Changelog, but kept intentionally lightweight for
this repository.

## [Unreleased]

### Added

- Message composer: send top-level messages to channels and chats, and
  threaded replies to messages in channels (`m` opens the composer, `r`
  replies to the highlighted message)
- Composer status line showing send progress and errors
- Automatic token refresh: expired `token-skype.jwt` / `token-chatsvcagg.jwt`
  files are renewed by re-running the vendored teams-token Electron flow (the
  `teams-token/` directory in this repository, or one found via
  `TEAMS_TOKEN_DIR`/sibling checkouts; override with `--token-refresh-command`),
  then the Teams client is rebuilt in place — no restart needed. Flags:
  `--no-auto-refresh`, `--token-refresh-command <cmd>`,
  `--token-refresh-timeout <d>`; a matching `Token Refresh` check was added to
  `teams-cli doctor`
- Vendored the teams-token login flow into `teams-token/` (MIT-licensed copy
  of fossteams/teams-token, see its README/LICENSE) as the maintained origin
  of the token fetcher
- The flow is also embedded in the teams-cli binary (`tokenflow/`): with no
  checkout discoverable, refresh materializes the bundled flow and
  bootstraps the pinned Electron 12.0.2 runtime (SHA-256 verified download
  into the user cache, once) — a copied `teams-cli` binary is fully
  self-contained, no repo/node/yarn required

### Fixed

- Sending a message triggered a forced interactive sign-in even with fresh
  tokens: top-level sends now post to the same message-service host the read
  path uses and set the `Authentication` header explicitly (the teams-api
  helper only attaches the skype token for its own host, so the old
  region-probed URL went out unauthenticated and returned 401)
- The region probe used for thread replies no longer treats a 401 as an
  accepted region; it now requires a 2xx answer before caching a region
- A failed send keeps the composer draft (previously the text was wiped
  before the request was made), and a successful send clears the stale
  thread-reply context
- Messages sent from the CLI were invisible in the chat window (and any
  name-keyed reader): the POST body omitted `imdisplayname`, so the service
  stored an empty sender name. Sends now include the signed-in user's
  display name, and nameless messages authored by the signed-in user render
  under that name instead of being skipped
- System activity rows (call logs, thread membership events) can no longer
  leak into the chat window as raw call/meeting XML, even when the
  signed-in user started the call and the row carries no sender name
- Client message id generation could overflow `int64` and produce negative
  ids; ids are now drawn from a range whose addition stays in bounds

## [v0.2.3] - 2026-04-02

### Added

- Migrated detailed documentation directly to the repository's native GitHub Wiki tab
- Added comprehensive CI/CD pipeline architectural explanations

### Changed

- Stripped `README.md` down to a minimal Quick Start and Lite Overview
- Simplified release assets to the standard consumer set: platform archives,
  one checksums file, one checksum signature bundle, and one bundled SBOM
  archive, while keeping GitHub attestations in the platform attestation store

## [v0.2.2] - 2026-03-27

### Added

- `CODEOWNERS` coverage for repository-critical paths
- CodeQL, dependency-review, and secret-detection workflows for the maintained
  fork
- Trusted release metadata generation for SBOMs, GitHub attestations, and
  cosign keyless signatures on release artifacts
- Release archive smoke tests before publication
- Governed release environment approval and documented rollback guidance

### Changed

- Source governance now expects protected `dev` and `main` branches with
  required checks and review policy
- Security gates now run inside the single `CI and Release` workflow instead of
  separate workflow files

## [v0.2.1] - 2026-03-27

### Changed

- The repository now uses a `dev` integration branch and a `main` release branch
- CI and release automation now run from a single combined workflow
- Releases are now created only from pushes to `main` when `version.go` is bumped to the next semantic version
- Future release runs now publish through the GitHub CLI in workflow steps
  instead of the deprecated Node 20 release action
- The release workflow artifact upload step now uses the current
  `actions/upload-artifact` major

## [v0.2.0] - 2026-03-27

### Added

- Expanded CLI configuration with `--help`, `--version`, `--log-level`,
  `--token-dir`, `--refresh-messages`, `--refresh-tree`, `--no-live`, and
  `doctor`
- Structured JSON logging with redaction and user-local log files
- Security policy, issue templates, pull request template, and a lightweight
  changelog process for this maintained fork
- Failure-mode tests for transport retries, cancellation, token edge cases,
  malformed payloads, UI focus transitions, and option parsing

### Changed

- Hardened runtime shutdown, timeout handling, retry behavior, and inline error
  recovery
- Refactored the TUI/runtime implementation into smaller files by concern
- Raised the Go baseline to `1.26.1`
- Fixed the CI workflow syntax, aligned CI with the current Go baseline,
  upgraded GitHub Actions to current majors, and opted workflows into the Node
  24 JavaScript action runtime
- Clarified maintainer workflow, support paths, and module-path expectations

## [v0.1.0] - 2026-03-27

### Added

- Release automation with darwin/linux tarballs for amd64 and arm64 plus
  checksums
- Conversation browsing for Teams, Channels, and Chats with recent-message
  loading
- Keyboard navigation help, loading indicators, and live refresh behavior
- Runtime hardening, structured logging, and `doctor` diagnostics
- CI validation with build, test, race, coverage, `staticcheck`, and
  `govulncheck`

### Changed

- Positioned this repository as the maintained fork of the archived upstream
- Updated contributor and runtime documentation to match the current behavior
