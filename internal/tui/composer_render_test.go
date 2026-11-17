package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/config"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
)

// TestComposerDrawsTypedTextInRowBelowMessages pins the screen-visible
// contract that the composer is a full-width input line stacked under the
// messages list: typed text must appear in the rendered main view, below
// the messages pane. A FlexColumn layout regression (the original bug)
// shrank the composer to a narrow right-hand column where text was
// invisible.
func TestComposerDrawsTypedTextInRowBelowMessages(t *testing.T) {
	state := &AppState{
		logger:       logging.Discard,
		messageLimit: config.DefaultMessageLimit,
	}
	state.initRuntime(context.Background())
	state.setCurrentConversation(teams.Conversation{ID: "19:x", Title: "Chat"})
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>visible body</p>"},
	}, time.Now(), "", false)
	state.ui.page = pageMain
	state.ui.focus = PaneComposer
	state.ui.composerText = "typed draft text"

	model := newChatModel(state)
	model.width = 120
	model.height = 30

	rendered := plain(model.View())
	rows := strings.Split(rendered, "\n")

	msgRow, composerRow := -1, -1
	for y, row := range rows {
		if msgRow < 0 && strings.Contains(row, "visible body") {
			msgRow = y
		}
		if composerRow < 0 && strings.Contains(row, "typed draft text") {
			composerRow = y
		}
	}

	if msgRow < 0 {
		t.Fatalf("expected the message body on screen; view was:\n%s", rendered)
	}
	if composerRow < 0 {
		t.Fatalf("expected the typed composer text on screen; view was:\n%s", rendered)
	}
	if composerRow <= msgRow {
		t.Fatalf("expected the composer below the messages (msg row %d, composer row %d)", msgRow, composerRow)
	}
	// The composer belongs to the conversation column: it starts right
	// after the list column and its gutter, never under the list.
	wantColumn := model.conversationPaneWidth() + conversationPaneGap + boxInset/2 + composerPaddingX
	column := lipgloss.Width(rows[composerRow][:strings.Index(rows[composerRow], "typed draft text")])
	if column != wantColumn {
		t.Fatalf("expected composer text at column %d (under the conversation view), got column %d in %q", wantColumn, column, rows[composerRow])
	}
}

// TestComposerReplyLabelRenders pins that reply mode shows the "Reply to
// <author>" prompt inside the visible composer row area.
func TestComposerReplyLabelRenders(t *testing.T) {
	state := &AppState{
		logger:       logging.Discard,
		messageLimit: config.DefaultMessageLimit,
	}
	state.initRuntime(context.Background())
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "msg-1", ImDisplayName: "Austin Herrera", Content: "<p>be right there</p>"},
	}, time.Now(), "", false)
	state.composerReply = replyContext{target: target, messageID: "msg-1"}
	state.ui.page = pageMain
	state.ui.focus = PaneComposer

	model := newChatModel(state)
	model.width = 120
	model.height = 30

	rendered := plain(model.View())
	if !strings.Contains(rendered, "Reply to Austin Herrera") {
		t.Fatalf("expected the reply label in the composer; view was:\n%s", rendered)
	}
}

// TestComposerReplyDraftReservesPromptWidth pins that SetWidth runs
// after SetPromptFunc in renderComposer. SetWidth snapshots promptWidth
// for the reserved inner width; on the per-frame copy of the composer,
// a prompt installed afterwards never reaches layout: the text area
// keeps the full inner width instead of wrapping at (inner − prompt),
// so the part of the draft behind the prompt's columns never appears
// on any row. The marker sits at the exact prompt-corrected wrap
// column: with the reservation in force it lands whole on the wrapped
// second row; without it the draft is one row and the marker is gone.
func TestComposerReplyDraftReservesPromptWidth(t *testing.T) {
	state := &AppState{logger: logging.Discard, messageLimit: config.DefaultMessageLimit}
	state.initRuntime(context.Background())
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "msg-1", ImDisplayName: "Alice", Content: "<p>be right there</p>"},
	}, time.Now(), "", false)
	state.composerReply = replyContext{target: target, messageID: "msg-1"}
	state.ui.page = pageMain
	state.ui.focus = PaneComposer

	model := newChatModel(state)
	model.width = 140
	model.height = 30

	// 95 chars at width 140: with the reservation the text area is
	// ~78 wide, so the draft wraps and the marker (index 80) lands
	// whole on the second row. Without it, SetWidth saw promptWidth=0,
	// the draft renders as one prompt-prefixed row, and the box clip
	// drops everything past draft index ~79 — including the marker.
	draft := strings.Repeat("a", 80) + "TAILMARKER" + strings.Repeat("a", 5)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(draft)})
	m := updated.(chatModel)
	if m.state.ui.composerText != draft {
		t.Fatalf("draft must land intact, got %q", m.state.ui.composerText)
	}

	rendered := plain(m.View())
	if !strings.Contains(rendered, "Reply to Alice") {
		t.Fatalf("expected the reply prompt; view was:\n%s", rendered)
	}
	// The draft wraps at the prompt-corrected column, which can split the
	// marker across two rows; reassemble the composer rows' text before
	// looking for it.
	const prompt = "Reply to Alice ›"
	var draftView strings.Builder
	for _, row := range strings.Split(rendered, "\n") {
		if idx := strings.Index(row, prompt); idx >= 0 {
			draftView.WriteString(strings.TrimSpace(row[idx+len(prompt):]))
		}
	}
	if !strings.Contains(draftView.String(), "TAILMARKER") {
		t.Fatalf("draft beyond the prompt reservation must wrap onto a visible row (lost = SetWidth ran before SetPromptFunc);\nview was:\n%s", rendered)
	}
}

// TestComposerOpensEmptyWithPlaceholder pins the original user report
// ("after pressing m there is no place to enter text"): with a full
// message list and the composer focused via 'm', the frame must show a
// Compose box carrying the placeholder, and keystrokes must land both in
// the draft and on screen.
func TestComposerOpensEmptyWithPlaceholder(t *testing.T) {
	state := &AppState{logger: logging.Discard, messageLimit: config.DefaultMessageLimit}
	state.initRuntime(context.Background())
	target := teams.Conversation{ID: "19:1", Title: "Brian and Austin 1:1"}
	state.setCurrentConversation(target)
	var msgs []csa.ChatMessage
	for i := 0; i < 51; i++ {
		msgs = append(msgs, csa.ChatMessage{
			Id:            fmt.Sprintf("m-%d", i),
			ImDisplayName: "Austin Herrera",
			Content:       "<p>message body</p>",
		})
	}
	state.ui.msgs.apply(state, target, msgs, time.Now(), "", false)
	state.ui.page = pageMain
	state.ui.focus = PaneMessages

	model := newChatModel(state)
	model.width = 148
	model.height = 42

	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	m := updated.(chatModel)
	if m.state.ui.focus != PaneComposer {
		t.Fatalf("'c' must focus the composer, got %s", m.state.ui.focus)
	}
	if !m.composer.Focused() {
		t.Fatal("textarea must be focused after 'c' (caret blink depends on it)")
	}

	rendered := plain(m.View())
	if !strings.Contains(rendered, "Send a message...") {
		t.Fatalf("empty focused composer must show the placeholder; view was:\n%s", rendered)
	}

	typed, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h', 'i'}})
	m2 := typed.(chatModel)
	if m2.state.ui.composerText != "hi" {
		t.Fatalf("typed runes must reach the draft, got %q", m2.state.ui.composerText)
	}
	if !strings.Contains(plain(m2.View()), "hi") {
		t.Fatal("typed draft must be visible in the frame")
	}

	spaced, _ := m2.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	if got := spaced.(chatModel).state.ui.composerText; got != "hi " {
		t.Fatalf("space must insert at the cursor, got %q", got)
	}
}

// TestComposerAltEnterNewlineEnterSend pins the two Enter paths: Alt+
// Enter extends the draft (multi-line), plain Enter hands it to the send
// pipeline, moves focus out, and blurs the live widget.
func TestComposerAltEnterNewlineEnterSend(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneComposer

	model := newChatModel(state)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	updated, _ = updated.(chatModel).Update(tea.KeyMsg{Type: tea.KeyEnter, Alt: true})
	m := updated.(chatModel)
	if m.state.ui.composerText != "a\n" {
		t.Fatalf("Alt+Enter must insert a newline, got %q", m.state.ui.composerText)
	}

	sent, _ := updated.(chatModel).Update(tea.KeyMsg{Type: tea.KeyEnter})
	ms := sent.(chatModel)
	if ms.state.ui.focus == PaneComposer {
		t.Fatal("plain Enter must leave the composer")
	}
	if ms.composer.Focused() {
		t.Fatal("a focused textarea keeps blinking forever; Enter must blur it")
	}
}

// TestComposerMidLineTypingKeepsCaret pins the composerSynced fix: the
// head-of-Update draft sync must not treat a just-mirrored keystroke as
// an external change. Without syncComposerText advancing
// composerSynced, the Update after each keystroke SetValue()s the whole
// draft, snapping the caret to end-of-buffer — the second character
// typed after a relative cursor move lands at the end instead. A
// single insert after Home+Right cannot catch this: Home's Update
// absorbs the stale SetValue and re-positions absolutely.
func TestComposerMidLineTypingKeepsCaret(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneComposer

	m := newChatModel(state)
	send := func(model chatModel, msg tea.Msg) chatModel {
		updated, _ := model.Update(msg)
		return updated.(chatModel)
	}

	m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h', 'l', 'l', 'o'}})
	if m.state.ui.composerText != "hllo" {
		t.Fatalf("seed draft must land, got %q", m.state.ui.composerText)
	}

	// Absolute Home (absorbs any stale SetValue), then a relative Right
	// that must survive, then two inserts: both belong after the "h"
	// and before the trailing "llo".
	m = send(m, tea.KeyMsg{Type: tea.KeyHome})
	m = send(m, tea.KeyMsg{Type: tea.KeyRight})
	m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'e'}})
	m = send(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	if got := m.state.ui.composerText; got != "hexllo" {
		t.Fatalf("mid-line typing must keep the caret: want %q, got %q (end-of-buffer snap = composerSynced regression)", "hexllo", got)
	}
	if got := m.composer.Value(); got != "hexllo" {
		t.Fatalf("live widget must agree with the mirrored draft: got %q", got)
	}
}

// TestComposerStatusTruncatesToColumn pins that an overlong status cannot
// widen the frame: every rendered row stays within the terminal width.
func TestComposerStatusTruncatesToColumn(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	state.ui.focus = PaneComposer
	state.ui.composerStatus = strings.Repeat("send failed: ", 20)
	state.ui.composerStatusKind = statusError

	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100

	for i, row := range strings.Split(model.View(), "\n") {
		if w := lipgloss.Width(row); w > model.width {
			t.Fatalf("row %d is %d columns wide, exceeds terminal width %d: %q", i, w, model.width, plain(row))
		}
	}
}

// TestTruncateComposerStatusIsRuneSafe pins that a multibyte status is cut
// on rune boundaries, never mid-UTF-8-sequence.
func TestTruncateComposerStatusIsRuneSafe(t *testing.T) {
	message := strings.Repeat("é", composerStatusMaxLength+20)
	got := truncateComposerStatus(message)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated status is not valid UTF-8: %q", got)
	}
	if n := len([]rune(got)); n > composerStatusMaxLength {
		t.Fatalf("truncated status is %d runes, exceeds %d", n, composerStatusMaxLength)
	}
}
