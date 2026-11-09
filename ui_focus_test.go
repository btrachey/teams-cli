package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-api/pkg/csa"
)

func newUITestState() *AppState {
	state := sampleConversationState()
	state.logger = discardLogger
	state.messageLimit = defaultMessageLimit
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

	state.ui.msgs.focusable = false
	state.focusMessagesPane()
	if state.ui.focus != PaneTree {
		t.Fatalf("expected focus to remain on the list, got %s", state.ui.focus)
	}

	state.ui.msgs.focusable = true
	state.focusMessagesPane()
	if state.ui.focus != PaneMessages {
		t.Fatalf("expected focus to move to messages, got %s", state.ui.focus)
	}
}

func TestMessagesPaneTabReturnsFocusToList(t *testing.T) {
	state := newUITestState()
	state.ui.msgs.focusable = true
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
	target := ConversationTarget{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>one</p>"},
		{Id: "2", ImDisplayName: "Bob", Content: "<p>two</p>"},
		{Id: "3", ImDisplayName: "Carol", Content: "<p>three</p>"},
	}, time.Now(), "", false)
	state.ui.msgs.focusable = true
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
	updated, _ = m.Update(runeKey('h'))
	m = updated.(chatModel)
	if m.state.ui.focus != PaneTree {
		t.Fatalf("expected h to return focus to the list, got %s", m.state.ui.focus)
	}
}

func TestMessagesKeyRoutingForComposerShortcuts(t *testing.T) {
	state := newUITestState()
	target := ConversationTarget{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>"},
	}, time.Now(), "", false)
	state.ui.focus = PaneMessages

	model := newChatModel(state)

	// 'm' opens the composer in plain send mode.
	updated, _ := model.Update(runeKey('m'))
	if updated.(chatModel).state.ui.focus != PaneComposer {
		t.Fatalf("expected m to focus the composer, got %s", updated.(chatModel).state.ui.focus)
	}
	if updated.(chatModel).state.composerReply.messageID != "" {
		t.Fatal("expected m to stay in plain send mode")
	}

	// 'r' on a channel message enters reply mode.
	state.channelById = map[string]Channel{"19:x": {}}
	state.ui.focus = PaneMessages
	updated, _ = updated.(chatModel).Update(runeKey('r'))
	if updated.(chatModel).state.ui.focus != PaneComposer {
		t.Fatalf("expected r to focus the composer, got %s", updated.(chatModel).state.ui.focus)
	}
	if updated.(chatModel).state.composerReply.messageID != "1" {
		t.Fatalf("expected r to set the reply context, got %+v", updated.(chatModel).state.composerReply)
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
	state.ui.msgs.focusable = true

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
	target := ConversationTarget{ID: "19:x", Title: "Chat"}
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

	// A resize message syncs the viewport content and scrolls the newest
	// entry into view without moving the cursor.
	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = updated.(chatModel)
	if cursor := m.state.ui.msgs.cursor; cursor != 50 {
		t.Fatalf("a fresh conversation should select the newest entry, got cursor=%d", cursor)
	}
	bottom := m.messages.YOffset
	if bottom == 0 {
		t.Fatal("expected the viewport to scroll to the newest entry")
	}

	updated, _ = m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress, X: 100, Y: 10})
	m = updated.(chatModel)
	if m.messages.YOffset >= bottom {
		t.Fatalf("wheel up must scroll the viewport up, offset %d -> %d", bottom, m.messages.YOffset)
	}

	updated, _ = m.handleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress, X: 100, Y: 10})
	m = updated.(chatModel)
	if m.messages.YOffset <= bottom-3 {
		t.Fatalf("wheel down must scroll the viewport back down, offset %d", m.messages.YOffset)
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
	case nil:
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

// TestPaneSwitchingWithHL pins both directions of the vim pane movement:
// l moves list -> messages -> composer, and the composer keeps l as text.
func TestPaneSwitchingWithHL(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneTree
	state.ui.msgs.focusable = true

	m := newTestModel(state)

	updated, _ := m.Update(runeKey('l'))
	m = updated.(chatModel)
	if m.state.ui.focus != PaneMessages {
		t.Fatalf("expected l to move focus to messages, got %s", m.state.ui.focus)
	}

	updated, _ = m.Update(runeKey('l'))
	m = updated.(chatModel)
	if m.state.ui.focus != PaneComposer {
		t.Fatalf("expected l to move focus to the composer, got %s", m.state.ui.focus)
	}

	updated, _ = m.Update(runeKey('l'))
	m = updated.(chatModel)
	if got := m.state.ui.composerText; got != "l" {
		t.Fatalf("expected l to be typed in the composer, got %q", got)
	}
}
