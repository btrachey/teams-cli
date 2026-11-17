# Changelog

All notable changes to this maintained fork are documented in this file.

The format is based on Keep a Changelog, but kept intentionally lightweight for
this repository.

## [Unreleased]

### Added

- Read receipts: a conversation that stays on screen for two seconds is
  marked read in Teams itself (the `consumptionhorizon` property the Teams
  clients set: `PUT …/conversations/{id}/properties?name=consumptionhorizon`
  with `"<newest message id>;<read time ms>;<newest client message id>"`),
  so its unread state clears on every device. Scrolling past a
  conversation does not mark it read. `--no-read-receipts` turns this off.
- Message composer: send top-level messages to channels and chats, and
  threaded replies to messages in channels (`c` writes to the selected
  conversation or replies to the highlighted message)
- Calendar tab: press `s` to view upcoming Teams meetings in a dedicated
  view (meeting list on the left, selected-meeting details — time, organizer,
  location, RSVP, and join link — on the right). Meetings are read from the
  Teams middle-tier calendar service (`teams.microsoft.com/api/mt/part/…/
  calendarView`) using the same skype token the chat client already holds, so
  no Microsoft Graph app registration is required. The tenant partition is
  discovered from the authz response with a fallback probe list; `a`/`d`
  (`[`/`]`) page the day window, `t` returns to today, `r` reloads, and
  `Tab` moves between the list and detail panes.
- Unread indicators: conversations the signed-in user has not read show a
  blue `●` next to their name in the conversations list, and the pane title
  carries the total (`Conversations · N unread`). The state comes from
  Teams' own read flags (`isRead` / `isMessageRead` on the conversations
  payload) — no heuristics — with two refinements: a chat whose last
  message is your own never counts, and opening (or watching) a conversation
  clears its dot immediately instead of waiting for Teams' backend to catch
  up; a genuinely newer message brings it back
- Composer status line showing send progress and errors
- Conversations-tree filter: `/` starts a live substring filter over the
  tree (`Enter` keeps it, `Esc` clears it); it survives background
  refreshes, and a needle that matches nothing shows a muted hint row
  instead of a bogus match
- Mouse support: the wheel scrolls the focused pane (tree, messages,
  composer caret) and clicks select conversations, message rows, or the
  composer; the messages pane shows a scrollbar track with a proportional
  thumb
- Multi-line drafts: `Alt+Enter` inserts a newline in the composer while
  plain `Enter` sends
- Automatic token refresh: expired `token-skype.jwt` / `token-chatsvcagg.jwt`
  files are renewed by re-running the built-in sign-in flow (override with
  `--token-refresh-command`), then the Teams client is rebuilt in place — no restart needed. Flags:
  `--no-auto-refresh`, `--token-refresh-command <cmd>`,
  `--token-refresh-timeout <d>`; a matching `Token Refresh` check was added to
  `teams-cli doctor`
- Built-in sign-in: the Microsoft sign-in flow (derived from
  fossteams/teams-token, MIT) lives in `token_flow.js` and is embedded in the
  binary, which runs it on a pinned Electron 12.0.2 runtime (SHA-256 verified
  download into the user cache, once) — a copied `teams-cli` binary is fully
  self-contained, no repo/node/yarn required. A fresh install signs in before
  the TUI starts; `teams-cli login [--switch-account]` signs in on demand

### Changed

- The calendar list is grouped by day: a divider (`── Today · Mon 09 Nov ──`,
  `Tomorrow`, `Yesterday`, then the date) opens each day, and the top row
  keeps its day's divider while scrolling. Meeting rows show just the time;
  all-day events read `all-day`.
- Keyboard shortcuts follow gh-dash: `s` and `h`/`l` (`←`/`→`) switch
  tabs, `p` toggles the preview pane, `ctrl+d`/`ctrl+u` page the
  conversation (also from the list), `r` refreshes the current tab and `R`
  everything, `o` opens the selected conversation or message in Teams (the
  join link on the calendar), `y`/`Y` copy its ID/link, and `c` comments:
  a message to the selected conversation, or a reply to the highlighted
  message; `ctrl+d` sends. This replaces `m` (message), `r` (reply), `c`
  (calendar), and `h`/`l` pane focus, which moved to `Tab`.
- gh-dash style layout: Messages and Calendar tabs across the top (with
  unread and meeting counts; switch with `1`/`2`, `c`, or a click), the
  list in a left sidebar split from the content by a vertical rule, and the
  key help under a rule. The dotted focus boxes are gone; the focused pane
  shows in its title color.
- Source reorganized from one flat `package main` into `internal/`
  packages (`config`, `logging`, `auth`, `teams`, `calendar`, `doctor`,
  `tui`) with `main.go` reduced to argument dispatch. Token refresh,
  the Teams backend, and the calendar client no longer hang off the UI
  state, and the conversation tree is now read under its own lock (it was
  previously read from the send path without synchronization).
- The sign-in flow no longer depends on the teams-token project, yarn,
  TypeScript, axios, jsonwebtoken or uuid (Node built-ins replace them); the
  vendored `teams-token/` and `tokenflow/` trees and the teams-token checkout
  discovery (`$TEAMS_TOKEN_DIR`, sibling checkouts, `yarn start`) are gone.
  Switching accounts clears the SSO session before the window opens, and
  token files are written with 0600 permissions
- The vendored bubbletea fork keeps only module sources (see
  `third_party/bubbletea/PATCHES.md`)
- The UI is idle when nothing is loading: the spinner only ticks during a
  load and the once-a-second repaint is gone; the message pane is rendered
  once per frame instead of twice
- The mouse wheel over the messages pane moves the message cursor (the view
  follows), instead of scrolling a viewport that snapped back to the cursor

- The TUI now runs on Bubble Tea (`bubbletea`/`bubbles`/`lipgloss`) instead of
  `rivo/tview`: the whole UI is a pure `Model` (update/view) whose rendering is
  plain strings, so every screen — message rows, composer, conversations — can
  be pinned in tests without a simulated terminal
- The panes are the standard `bubbles` components: a `list` enumerates the
  conversations (with `/` fuzzy filtering), a `viewport` renders the
  conversation, a `textarea` composes messages, and the `help` component
  renders the key hints in the footer
- Login and message-loading animation use the `bubbles` spinner component
  (`spinner.Dot`); the hand-built loading bar is gone
- Vim-style movement throughout: `j`/`k` move (arrows also work), `g`/`G`
  jump to the top/bottom, and `h`/`l` move focus between the conversations,
  messages, and composer panes
- Conversations are ordered by their newest message, newest first, using the
  conversation metadata (`ComposeTime`, falling back to arrival/join times)
  plus locally observed live-refresh activity; ties sort by title so the
  order is stable
- Message rows show the sender's time next to the author name in a dim
  style (clock time for today, date plus time for older messages), and the
  cursor row is only highlighted while the message pane has focus
- The focused pane (conversations list, conversation view, or composer) is
  surrounded by a lightweight dotted box (dim grey "\u00b7", no solid lines,
  no powerline glyphs); the inset is always reserved so the layout never
  shifts when focus moves. Inside the conversation view, the message row
  under the cursor gets a subtle full-width background highlight
- The borderless layout keeps the panes apart with spacing instead of
  frames: a gutter between the conversations and messages columns, and the
  composer stays under the conversation view (never under the list) with a
  blank row of breathing room; long message bodies wrap at the pane width
  instead of being clipped
- Image messages: `<img>` attachments are detected, downloaded from the Teams
  media service (authz-refreshed skype token as the `skypetoken_asm`
  cookie, attached only for Teams/Skype hosts) and cached on disk. WezTerm
  and iTerm2 get true inline rendering (OSC 1337) via a vendored Bubble Tea
  renderer (`third_party/bubbletea`, `replace` in go.mod): the image marker
  line is cleared with the rest of the frame and
  the OSC escape is emitted after the frame is fully painted (cursor save /
  absolute row / restore), so the image is not overwritten and no stale text
  shows around it. The image is positioned from the rendered line's own
  prefix width (the conversation column), the list column on that row is
  preserved, and the escape keeps `preserveAspectRatio=1` (WezTerm drops
  images sent with `=0`) so it fits inside the reserved cell box. Other terminals get truecolor half-block art.
  `TEAMS_CLI_IMAGES=block|native|off` overrides the renderer
- Message pagination: a "current/total" indicator on the conversation title
  row, like the conversations list, whenever a conversation needs more than
  one screen, alongside a scroll-position bar built from the `bubbles`
  progress component

### Fixed

- The "Retry" row shown after a failed conversation load now retries
  (Enter, l or Right); the cursor lands on it
- Opening the calendar no longer cancels an in-flight conversation load
  (which stranded the messages pane on "Loading…") or pauses live message
  refresh; a calendar result arriving after leaving the page is kept
- Calendar: Enter opens the meeting's join link, `?` toggles help, and the
  status line names the real window keys (a/[ and d/])
- Message wrapping, paging and image sizing used a pane width one column
  wider than the one painted

- The whole screen was scrolled two rows off the top (pane title borders
  invisible): bordered boxes were sized at their inner height instead of
  their outer height, so the right column overflowed the terminal and the
  alt screen scrolled up. Frame height is now clamped to the terminal
- Pasting text whose first character is a shortcut (`quick fix` starting
  with `q`, `?help`, …) no longer quits the app or hijacks focus: shortcuts
  fire only on single-keystroke input, while the composer accepts pasted
  batches verbatim. Typing a space in the composer also works again
  (Bubble Tea delivers it as a distinct key, not a printable rune)
- Network-sourced text (message bodies, display names, team/channel/chat
  titles) is stripped of terminal escape sequences and control characters
  before rendering: Bubble Tea passes ANSI bytes straight to the terminal,
  so a hostile message could previously clear the screen, move the cursor,
  or rewrite the window title
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
- The token sign-in window no longer sits blank forever on a network
  failure: failed login loads are retried with a bounded budget, renderer
  crashes reload once, a blank-page watchdog reloads navigations that never
  finish loading (and gives up loudly after two reloads), and the flow's
  last error lines now reach the teams-cli error message instead of being
  truncated at 300 bytes
- The macOS sign-in window now has the standard Edit menu, so macOS attaches
  its system AutoFill submenu to it (`Edit ▸ AutoFill ▸ Passwords…`, backed
  by the Passwords app / iCloud Keychain with Touch ID approval) and the
  form gets working ⌘C/⌘V/⌘A

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
