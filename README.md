# teams-cli

A Command Line Interface (or TUI) to interact with Microsoft Teams that uses the [teams-api](https://github.com/fossteams/teams-api) Go package.

## Status

Upstream `fossteams/teams-cli` has been archived and is read-only. This fork is the active maintenance branch for the codebase, and new work should target this repository.

This project is still WIP and will be updated with more features. The goal is to have a CLI / TUI replacement for the Microsoft Teams desktop client. Today the client can browse conversations, read recent messages, and post replies: send a top-level message to any channel or chat, or reply to a specific message in a channel (threaded replies).

## Documentation

**We have moved our comprehensive documentation to the Wiki.** 

Please browse the [GitHub Wiki](https://github.com/vaishnavucv/teams-cli/wiki) for detailed guides:
- [Installation Guide](https://github.com/vaishnavucv/teams-cli/wiki/Installation)
- [Usage & Keyboard Navigation](https://github.com/vaishnavucv/teams-cli/wiki/Usage)
- [Features List](https://github.com/vaishnavucv/teams-cli/wiki/Features)
- [Development, CI/CD, & Governance](https://github.com/vaishnavucv/teams-cli/wiki/CI-CD)

## Quick Start

### Requirements
- [Golang](https://golang.org/) 1.26.1 or newer
- Teams JWT tokens: minted by the token flow — embedded in the binary (auto-bootstraps its
  Electron runtime on first refresh) or run from the vendored `teams-token/` directory
  (`cd teams-token && yarn install` once for development builds)
- A terminal with cursor-addressing support (e.g. Terminal.app, iTerm2)

### Basic Usage

Run the app locally once you have obtained your tokens:
```bash
go run ./
```

To limit each conversation view to the most recent `N` messages:
```bash
go run ./ msg=20
```

*For more runtime flags and usage examples, see the [Usage Wiki](wiki/Usage.md).*

### Building a Standalone Binary

teams-cli compiles to a single Go binary with no runtime dependencies — the token
login flow is embedded, so the result can be copied to any supported machine as-is:

```bash
# current platform, version stamped (prints via teams-cli --version)
go build -trimpath -ldflags="-s -w -X main.version=v0.3.0" -o teams-cli .
```

To produce distributables for every supported platform (darwin/linux × amd64/arm64),
each as a `.tar.gz` with the README, LICENSE, and a SHA-256 checksums file:

```bash
./scripts/build-release-artifacts.sh v0.3.0 dist
```

What "standalone" means on the target machine:

- **No repo, node, or yarn required.** The binary reads tokens from
  `~/.config/fossteams` (or `--token-dir` / `MS_TEAMS_*_TOKEN`).
- **Automatic token refresh works out of the box.** When tokens expire and no
  `teams-token/` checkout is discoverable, the binary materializes its embedded
  token flow and downloads the pinned Electron runtime (~90 MB, SHA-256
  verified against the upstream release, cached under the user cache directory,
  fetched once) — see [Diagnostics](#diagnostics) for the discovery order and
  flags.
- **First refresh needs network** for that one-time Electron download, and if
  the saved SSO session has lapsed the login window opens for an interactive
  sign-in. No further downloads are needed; the app talks only to the Teams
  service endpoints.
- Build requirement: Go 1.26.1 or newer (see `go.mod`). If your environment has
  several Go toolchains installed, pin an explicit version (e.g.
  `GOTOOLCHAIN=go1.26.1 go build ...`); `GOTOOLCHAIN=local` fails when the
  installed toolchain is older than `go.mod`.

## Lite Overview

### Core Features
- Browse your Teams, Channels, and direct/group Chats from a TUI.
- Automatically load and read recent messages in selected conversations.
- Background refresh for both message lists and conversation trees.
- Automatic token refresh: when the stored `token-skype.jwt` / `token-chatsvcagg.jwt` expire, teams-cli re-runs the teams-token Electron flow (which reuses its saved SSO session) and reconnects without a restart.
- Navigate the UI entirely with keyboard shortcuts.
- Clean shutdown on `q` or `Ctrl+C`.

### Keyboard Navigation
- **Arrow Keys**: Move up/down lists, expand (`Right`) or collapse (`Left`) trees.
- **Enter**: Open the selected channel or chat.
- **m**: Open the composer to send a message to the current conversation.
- **r**: Reply to the highlighted message (channels only; chats send a top-level message).
- **Tab**: Switch focus between the conversations tree, messages pane, and composer.
- **Esc**: Go back one level or return to the conversations tree.
- **?**: Show/hide the full keyboard help inside the app.
- **q**: Quit the app.

### Diagnostics
If you have issues logging in or starting the app, run the built-in doctor to test token validity, automatic token refresh setup, and network reachability:
```bash
go run ./ doctor
```

Token refresh is enabled by default. Discovery order: `--token-refresh-command <cmd>` (any command that rewrites the token files) → `$TEAMS_TOKEN_DIR` → a `teams-token/` checkout next to the working directory or binary → sibling checkouts. With none of those present, teams-cli falls back to the token flow **embedded in the binary**: it downloads the pinned Electron runtime (~90 MB, SHA-256 verified) into the user cache on first refresh, so a copied `teams-cli` binary is fully self-contained — no repo, node, or yarn required. When the saved SSO session has lapsed, the Electron window opens and you can sign in interactively within `--token-refresh-timeout <d>` (default 3m); `--no-auto-refresh` disables the whole mechanism. The flow writes the directory teams-cli actually reads (`--token-dir` included); unmodified external checkouts fall back to mirroring from `~/.config/fossteams`.

## Support

- Found an issue or have a feature request? Please use our **built-in issue templates**.
- Found a security vulnerability? Please review our **[SECURITY.md](./SECURITY.md)** guidelines privately before opening a public issue.
- View our **[CHANGELOG.md](./CHANGELOG.md)** for detailed release notes and updates.
- This fork maintains the `github.com/fossteams/teams-cli` module path for compatibility, but the releases published here are the officially supported install path.
