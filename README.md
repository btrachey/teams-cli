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
- Nothing else: on first launch teams-cli opens a Microsoft sign-in window
  (its built-in flow on a pinned Electron runtime, downloaded once) and saves
  the Teams tokens. Run `teams-cli login --switch-account` to change accounts.
- A terminal with cursor-addressing support (e.g. Terminal.app, iTerm2)

### Basic Usage

Run the app locally (the first run signs you in):
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
- **Sign-in and token refresh work out of the box.** The binary embeds its
  sign-in flow (`internal/auth/token_flow.js`) and downloads the pinned Electron runtime
  (~90 MB, SHA-256 verified against the upstream release, cached under the user
  cache directory, fetched once) — see [Diagnostics](#diagnostics) for the
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
- Automatic token refresh: when the stored `token-skype.jwt` / `token-chatsvcagg.jwt` expire, teams-cli re-runs its sign-in flow (which reuses the saved SSO session) and reconnects without a restart.
- Navigate the UI entirely with keyboard shortcuts, or drive it with the
  mouse: wheel scrolls the focused pane, clicks select conversations,
  messages, and the composer.
- Clean shutdown on `q` or `Ctrl+C`.
- **Tabs**: the screen is laid out like [gh-dash](https://www.gh-dash.dev): a **Messages** tab and a **Calendar** tab across the top (each with its unread or meeting count), a sidebar list on the left, and the selected conversation or meeting on the right.
- **Calendar**: press **s** (or **2**) for the Calendar tab and your upcoming Teams meetings (fetched from the Teams middle-tier calendar service with the same skype token the chat client uses) — no Microsoft Graph setup required.
- **Unread indicators**: conversations with messages you haven't read show a blue `●` next to the name, using Teams' own read state (a chat's dot also never counts messages you sent yourself); opening a conversation clears its dot instantly. After a conversation has been on screen for two seconds, teams-cli also sends Teams the same read receipt the desktop client does, so it shows as read on your other devices too (and as "seen" to others where read receipts are on). Pass `--no-read-receipts` to keep reading locally only.

### Keyboard Navigation
The keys follow [gh-dash](https://www.gh-dash.dev/getting-started/keybindings/); arrow keys work everywhere too.
- **j / k** (or **Down / Up**): Move within the focused pane.
- **g / G** (or **Home / End**): Jump to the first/last item.
- **ctrl+d / ctrl+u** (or **PgDn / PgUp**): Page the conversation (also from the conversation list).
- **s**, **h / l** (or **Left / Right**), **1 / 2**, or click a tab: Switch between the Messages and Calendar tabs.
- **Enter**: Open the selected conversation.
- **Tab**: Move focus between the conversation list and the conversation.
- **/**: Filter the conversation list (type to narrow, `Enter` keeps the
  filter, `Esc` clears it).
- **p**: Hide/show the preview (the conversation or meeting details); the list takes the full width while it is hidden.
- **r**: Refresh the current tab. **R**: Refresh everything.
- **c**: Comment: on the conversation list, write a message to the selected conversation; on a message, reply to it (channels; chats send a top-level message).
- **ctrl+d** or **Enter**: Send the message being written. **Alt+Enter** inserts a newline. **Backspace** deletes at the cursor and also cancels a pending thread reply. **Esc** leaves the composer.
- **o**: Open the selected conversation (or highlighted message) in Teams.
- **y / Y**: Copy its ID / its Teams link.
- **Esc**: Return to the conversation list.
- **?**: Show/hide the full keyboard help inside the app.
- **q**: Quit the app.

### Calendar tab (`s` or `2`)
Switch to the Calendar tab to list meetings for the current week in the left sidebar, with the selected meeting's details (time, organizer, location, RSVP, join link) on the right.
- **j / k**, **g / G**, **ctrl+d / ctrl+u**: Move, jump, and page through the list.
- **Enter** or **o**: Open the selected meeting's Teams join link in your browser (online meetings).
- **y / Y**: Copy the meeting title / join link.
- **Tab**: Move focus between the meeting list and the details. **p** hides/shows the details.
- **a / [** and **d / ]**: Move the window to the earlier / next range of days; **t** jumps back to today.
- **r**: Reload the current window. **R**: Refresh everything.
- **s**, **h / l**, **1**, or **Esc**: Return to the Messages tab.
- **?**: Toggle the full key help.

### Diagnostics
If you have issues logging in or starting the app, run the built-in doctor to test token validity, automatic token refresh setup, and network reachability:
```bash
go run ./ doctor
```

Sign-in and token refresh are enabled by default and use the sign-in flow **embedded in the binary** (`internal/auth/token_flow.js`): it downloads the pinned Electron runtime (~90 MB, SHA-256 verified) into the user cache on first use, so a copied `teams-cli` binary is fully self-contained — no repo, node, or yarn required. A fresh install (no token files) signs in before the TUI starts; `teams-cli login` signs in on demand and `--switch-account` forgets the saved session first. When the saved SSO session has lapsed during a refresh, the window opens and you can sign in within `--token-refresh-timeout <d>` (default 3m). `--token-refresh-command <cmd>` replaces the flow with any command that writes the token files into `$FOSSTEAMS_TOKEN_DIR`; `--no-auto-refresh` disables automatic sign-in and refresh. Tokens are written to the directory teams-cli reads (`--token-dir` included).

## Support

- Found an issue or have a feature request? Please use our **built-in issue templates**.
- Found a security vulnerability? Please review our **[SECURITY.md](./SECURITY.md)** guidelines privately before opening a public issue.
- View our **[CHANGELOG.md](./CHANGELOG.md)** for detailed release notes and updates.
- This fork maintains the `github.com/fossteams/teams-cli` module path for compatibility, but the releases published here are the officially supported install path.
