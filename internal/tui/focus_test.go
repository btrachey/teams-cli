package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/config"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
)

func newUITestState() *AppState {
	state := sampleConversationState()
	state.logger = logging.Discard
	state.messageLimit = config.DefaultMessageLimit
	state.initRuntime(context.Background())
	return state
}

func runeKey(r rune) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
}

// TestHelpBindingsPerPane pins that the help component shows the focused
// pane's bindings: the bubbles help widget renders the key map returned by
// keyBindings for the current focus.
func TestHelpBindingsPerPane(t *testing.T) {
	for _, tc := range []struct {
		pane    string
		mention string
	}{
		{PaneTree, "filter"},
		{PaneMessages, "reply"},
		{PaneComposer, "send"},
	} {
		state := newUITestState()
		state.ui.focus = tc.pane

		model := newChatModel(state)
		model.help.Width = 120
		got := plain(model.help.View(model.keyBindings()))
		if !strings.Contains(got, tc.mention) {
			t.Fatalf("expected %s help for pane %s, got %q", tc.mention, tc.pane, got)
		}
		if strings.Contains(got, "\n") {
			t.Fatalf("expected a single-line short help, got %q", got)
		}
	}
}

func TestFocusMessagesPaneRequiresFocusableChatPane(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneTree

	state.ui.msgs.entries = nil
	state.focusMessagesPane()
	if state.ui.focus != PaneTree {
		t.Fatalf("expected focus to remain on the list, got %s", state.ui.focus)
	}

	makeMessagesFocusable(state)
	state.focusMessagesPane()
	if state.ui.focus != PaneMessages {
		t.Fatalf("expected focus to move to messages, got %s", state.ui.focus)
	}
}

func TestMessagesPaneTabReturnsFocusToList(t *testing.T) {
	state := newUITestState()
	makeMessagesFocusable(state)
	state.ui.focus = PaneMessages

	model := newChatModel(state)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	got := updated.(chatModel)
	if got.state.ui.focus != PaneTree {
		t.Fatalf("expected focus to return to the list, got %s", got.state.ui.focus)
	}
}

// TestMessagesVimKeysMoveCursor pins hjkl in the messages pane: k/j move
// the message cursor one entry, h returns to the list, l enters the
// composer.
func TestMessagesVimKeysMoveCursor(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>one</p>"},
		{Id: "2", ImDisplayName: "Bob", Content: "<p>two</p>"},
		{Id: "3", ImDisplayName: "Carol", Content: "<p>three</p>"},
	}, time.Now(), "", false)
	makeMessagesFocusable(state)
	state.ui.focus = PaneMessages

	model := newChatModel(state)
	updated, _ := model.Update(runeKey('k'))
	m := updated.(chatModel)
	if got := m.state.ui.msgs.cursor; got != 1 {
		t.Fatalf("expected k to move the cursor up to 1, got %d", got)
	}
	updated, _ = m.Update(runeKey('j'))
	m = updated.(chatModel)
	if got := m.state.ui.msgs.cursor; got != 2 {
		t.Fatalf("expected j to move the cursor down to 2, got %d", got)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = updated.(chatModel)
	if m.state.ui.focus != PaneTree {
		t.Fatalf("expected esc to return focus to the list, got %s", m.state.ui.focus)
	}
}

func TestMessagesKeyRoutingForComposerShortcuts(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>"},
	}, time.Now(), "", false)
	state.conversationLoaded = true
	state.ui.focus = PaneTree

	model := newChatModel(state)

	// 'c' on the conversation list writes a plain top-level message.
	updated, _ := model.Update(runeKey('c'))
	if updated.(chatModel).state.ui.focus != PaneComposer {
		t.Fatalf("expected c to focus the composer, got %s", updated.(chatModel).state.ui.focus)
	}
	if updated.(chatModel).state.composerReply.messageID != "" {
		t.Fatal("expected c on the list to stay in plain send mode")
	}

	// 'c' on a channel message (gh-dash's comment) enters reply mode.
	seedChannel(state, "19:x")
	state.ui.focus = PaneMessages
	updated, _ = updated.(chatModel).Update(runeKey('c'))
	if updated.(chatModel).state.ui.focus != PaneComposer {
		t.Fatalf("expected c to focus the composer, got %s", updated.(chatModel).state.ui.focus)
	}
	if updated.(chatModel).state.composerReply.messageID != "1" {
		t.Fatalf("expected c to set the reply context, got %+v", updated.(chatModel).state.composerReply)
	}
}

func TestComposerTabReturnsToMessages(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneComposer

	model := newChatModel(state)
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyTab})
	m := updated.(chatModel)
	if got := m.state.ui.focus; got != PaneMessages {
		t.Fatalf("expected tab to leave the composer toward messages, got %s", got)
	}
	// A focused textarea keeps scheduling blink ticks; leaving via Tab
	// must blur the live widget, not defer it to the next Update.
	if m.composer.Focused() {
		t.Fatal("expected textarea to blur when Tab leaves the composer")
	}
}

func TestComposerRuneTypingNotSwallowedByGlobalKeys(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneComposer

	model := newChatModel(state)
	updated, _ := model.Update(runeKey('q'))
	updated, _ = updated.(chatModel).Update(runeKey('?'))
	if got := updated.(chatModel).state.ui.composerText; got != "q?" {
		t.Fatalf("expected q/? typed as text while composing, got %q", got)
	}
	if updated.(chatModel).help.ShowAll {
		t.Fatal("expected full help to stay closed while composing")
	}
}

// TestHelpTogglesFullView pins '?' as the help-component toggle and 'q' as
// quit when the help is shown.
func TestHelpTogglesFullView(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneTree

	model := newChatModel(state)
	model.help.Width = 120
	updated, _ := model.Update(runeKey('?'))
	if !updated.(chatModel).help.ShowAll {
		t.Fatal("expected ? to expand the full help view")
	}
	if updated.(chatModel).state.ui.focus != PaneTree {
		t.Fatalf("expected help to keep the focus, got %s", updated.(chatModel).state.ui.focus)
	}

	updated, _ = updated.(chatModel).Update(runeKey('?'))
	if updated.(chatModel).help.ShowAll {
		t.Fatal("expected a second ? to collapse the help view")
	}

	updated, _ = updated.(chatModel).Update(runeKey('q'))
	if updated.(chatModel).state.appContext().Err() == nil {
		t.Fatal("expected q to stop the app")
	}
}

// TestListFilterRoutesTypedKeys pins '/' as the list filter: 'q' is needle
// text, not a quit, and the needle reaches the list's filter value.
func TestListFilterRoutesTypedKeys(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneTree
	state.ui.page = pageMain

	m := newTestModel(state)
	m.width, m.height = 120, 40

	updated, _ := m.Update(runeKey('/'))
	m = updated.(chatModel)
	if !m.conversations.SettingFilter() {
		t.Fatal("expected '/' to start the list filter")
	}

	updated, _ = m.Update(runeKey('q'))
	m = updated.(chatModel)
	if !m.conversations.SettingFilter() {
		t.Fatal("typing 'q' in the filter must not leave the filter")
	}
	if got := m.conversations.FilterValue(); got != "q" {
		t.Fatalf("expected 'q' in the filter value, got %q", got)
	}
	if m.state.appContext().Err() != nil {
		t.Fatal("filter typing must never cancel the app context")
	}
}

// TestListFilterNoMatchesLeavesNavigationAlive pins that a needle matching
// nothing leaves the list usable: Enter accepts the filter, Tab still
// leaves for the messages pane.
func TestListFilterNoMatchesLeavesNavigationAlive(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneTree
	state.ui.page = pageMain
	makeMessagesFocusable(state)

	m := newTestModel(state)
	m.width, m.height = 140, 24

	updated, cmd := m.Update(runeKey('/'))
	m = drainUICmds(updated.(chatModel), cmd)
	for _, r := range "zzz" {
		updated, cmd = m.Update(runeKey(r))
		m = drainUICmds(updated.(chatModel), cmd)
	}
	if n := len(m.conversations.VisibleItems()); n != 0 {
		t.Fatalf("expected no visible items for a gibberish needle, got %d", n)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(chatModel)
	if m.conversations.SettingFilter() {
		t.Fatal("expected Enter to accept the filter")
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(chatModel)
	if m.state.ui.focus != PaneMessages {
		t.Fatalf("expected Tab to leave the filtered list, focus=%s", m.state.ui.focus)
	}
}

// TestMessagesWheelScrollsViewport pins wheel scrolling against a viewport
// longer than the pane: the notch moves the window and both extremes are
// reachable.
func TestMessagesWheelScrollsViewport(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	var msgs []csa.ChatMessage
	for i := 0; i < 51; i++ {
		msgs = append(msgs, csa.ChatMessage{
			Id:            fmt.Sprintf("m-%d", i),
			ImDisplayName: "Alice",
			Content:       "<p>body</p>",
		})
	}
	state.ui.msgs.apply(state, target, msgs, time.Now(), "", false)
	state.ui.focus = PaneMessages
	state.ui.page = pageMain

	m := newChatModel(state)
	m.width, m.height = 120, 40

	// Rendering scrolls the newest entry into view without moving the
	// cursor.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(chatModel)
	_ = m.View()
	if cursor := m.state.ui.msgs.cursor; cursor != 50 {
		t.Fatalf("a fresh conversation should select the newest entry, got cursor=%d", cursor)
	}
	bottom := m.state.ui.msgs.yOffset
	if bottom == 0 {
		t.Fatal("expected the viewport to scroll to the newest entry")
	}

	// The wheel walks the message cursor and the viewport follows it.
	for i := 0; i < 20; i++ {
		updated, _ = m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress, X: 100, Y: 10})
		m = updated.(chatModel)
		_ = m.View()
	}
	if cursor := m.state.ui.msgs.cursor; cursor != 30 {
		t.Fatalf("wheel up must move the cursor up one entry per notch, got cursor=%d", cursor)
	}
	if m.state.ui.msgs.yOffset >= bottom {
		t.Fatalf("wheel up must scroll the viewport up, offset %d -> %d", bottom, m.state.ui.msgs.yOffset)
	}

	updated, _ = m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress, X: 100, Y: 10})
	m = updated.(chatModel)
	if cursor := m.state.ui.msgs.cursor; cursor != 31 {
		t.Fatalf("wheel down must move the cursor back down, got cursor=%d", cursor)
	}
}

// drainUICmds executes the commands a widget Update returns (bubbles list
// filtering computes matches asynchronously) and feeds their messages back
// into the model, mirroring the real event loop.
func drainUICmds(m chatModel, cmd tea.Cmd) chatModel {
	if cmd == nil {
		return m
	}
	msg := cmd()
	switch v := msg.(type) {
	case nil, spinner.TickMsg:
		// Spinner ticks animate forever while a load is pending; they are
		// not part of the behavior under test.
		return m
	case tea.BatchMsg:
		for _, c := range v {
			m = drainUICmds(m, c)
		}
		return m
	default:
		updated, next := m.Update(msg)
		return drainUICmds(updated.(chatModel), next)
	}
}

// TestPaneSwitchingWithTab pins pane focus: Tab moves list -> messages,
// c opens the composer, and the composer keeps h/l as text.
func TestPaneSwitchingWithTab(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneTree
	makeMessagesFocusable(state)
	state.conversationLoaded = true

	m := newTestModel(state)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(chatModel)
	if m.state.ui.focus != PaneMessages {
		t.Fatalf("expected tab to move focus to messages, got %s", m.state.ui.focus)
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(chatModel)
	if m.state.ui.focus != PaneTree {
		t.Fatalf("expected tab to return focus to the list, got %s", m.state.ui.focus)
	}

	updated, _ = m.Update(runeKey('c'))
	m = updated.(chatModel)
	if m.state.ui.focus != PaneComposer {
		t.Fatalf("expected c to open the composer, got %s", m.state.ui.focus)
	}

	updated, _ = m.Update(runeKey('l'))
	m = updated.(chatModel)
	if got := m.state.ui.composerText; got != "l" {
		t.Fatalf("expected l to be typed in the composer, got %q", got)
	}
}
