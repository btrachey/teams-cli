package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/calendar"
	"github.com/fossteams/teams-cli/internal/teams"
)

// TestTabbedLayout pins the gh-dash frame: the tab row and its rule on
// top, the list and content split by a vertical rule, and the help under a
// closing rule, filling the terminal exactly.
func TestTabbedLayout(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	m := newTestModel(state)
	m.width, m.height = 100, 30
	m.help.Width = 100

	rows := strings.Split(plain(m.View()), "\n")
	if len(rows) != m.height {
		t.Fatalf("frame must fill the terminal: %d rows, want %d", len(rows), m.height)
	}
	if !strings.Contains(rows[0], "Messages") || !strings.Contains(rows[0], "Calendar") {
		t.Fatalf("tab row must list Messages then Calendar, got %q", rows[0])
	}
	if strings.Index(rows[0], "Messages") > strings.Index(rows[0], "Calendar") {
		t.Fatalf("Messages must be the first tab, got %q", rows[0])
	}
	if strings.Trim(rows[1], "─") != "" {
		t.Fatalf("a rule must sit under the tabs, got %q", rows[1])
	}

	// The sidebar is on the left: every content row carries the divider
	// right after the list column.
	col := m.conversationPaneWidth()
	for i := tabBarHeight; i < tabBarHeight+m.contentHeight(); i++ {
		if r := []rune(rows[i]); len(r) <= col || r[col] != '│' {
			t.Fatalf("row %d must have the divider at column %d: %q", i, col, rows[i])
		}
	}
	if !strings.Contains(strings.Join(rows[tabBarHeight:], "\n"), "Conversations") {
		t.Fatal("the conversation list must render in the sidebar")
	}
	footer := tabBarHeight + m.contentHeight()
	if strings.Trim(rows[footer], "─") != "" {
		t.Fatalf("a rule must sit above the help, got %q", rows[footer])
	}
}

// TestTabSwitching covers gh-dash's tab keys (s, h/l), 1/2, and clicking a
// tab.
func TestTabSwitching(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	state.calendar.override = func(context.Context, calendar.Query) ([]calendar.Event, error) { return nil, nil }
	m := newTestModel(state)
	m.width, m.height = 100, 30

	press := func(msg tea.Msg) {
		updated, _ := m.Update(msg)
		m = updated.(chatModel)
	}

	press(runeKey('2'))
	if state.ui.page != pageCalendar || state.ui.focus != PaneCalendar {
		t.Fatalf("2 must open the calendar tab, page=%v focus=%s", state.ui.page, state.ui.focus)
	}
	press(runeKey('1'))
	if state.ui.page != pageMain || state.ui.focus != PaneTree {
		t.Fatalf("1 must return to the messages tab, page=%v focus=%s", state.ui.page, state.ui.focus)
	}
	press(runeKey('s'))
	if state.ui.page != pageCalendar {
		t.Fatal("s must switch to the calendar")
	}
	press(runeKey('s'))
	if state.ui.page != pageMain {
		t.Fatal("s must switch back to messages")
	}
	press(runeKey('l'))
	if state.ui.page != pageCalendar {
		t.Fatal("l must move to the next tab")
	}

	press(tea.MouseMsg{X: tabColumn(m, "Messages"), Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if state.ui.page != pageMain {
		t.Fatal("clicking the Messages tab must switch to it")
	}
	press(tea.MouseMsg{X: tabColumn(m, "Calendar"), Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if state.ui.page != pageCalendar {
		t.Fatal("clicking the Calendar tab must switch to it")
	}
}

// tabColumn is the screen column where a tab's label starts.
func tabColumn(m chatModel, label string) int {
	row := plain(strings.Split(m.View(), "\n")[0])
	return len([]rune(row[:strings.Index(row, label)]))
}

// TestPreviewToggle pins gh-dash's p: the content pane hides, the sidebar
// takes the full width and keeps focus, and Enter brings the pane back.
func TestPreviewToggle(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	makeMessagesFocusable(state)
	state.ui.focus = PaneMessages
	m := newTestModel(state)
	m.width, m.height = 100, 30

	updated, _ := m.Update(runeKey('p'))
	m = updated.(chatModel)
	if !state.ui.previewHidden || state.ui.focus != PaneTree {
		t.Fatalf("p must hide the preview and focus the list, hidden=%v focus=%s", state.ui.previewHidden, state.ui.focus)
	}
	if m.conversationPaneWidth() != m.width {
		t.Fatal("the sidebar must take the full width while the preview is hidden")
	}
	for _, row := range strings.Split(plain(m.View()), "\n")[tabBarHeight : tabBarHeight+m.contentHeight()] {
		// The list draws its own │ selection marker in the first columns.
		if r := []rune(row); len(r) > 4 && strings.Contains(string(r[4:]), "│") {
			t.Fatalf("no divider while the preview is hidden: %q", row)
		}
	}

	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = updated.(chatModel)
	if state.ui.previewHidden {
		t.Fatal("enter must bring the preview back")
	}
}

// TestPreviewPagingFromSidebar pins gh-dash's ctrl+d/ctrl+u: they page the
// conversation while the list keeps focus.
func TestPreviewPagingFromSidebar(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	target := teams.Conversation{ID: "chat-1", Title: "Chat"}
	state.setCurrentConversation(target)
	var msgs []csa.ChatMessage
	for i := 0; i < 40; i++ {
		msgs = append(msgs, csa.ChatMessage{Id: fmt.Sprint(i), ImDisplayName: "A", Content: "<p>x</p>"})
	}
	state.ui.msgs.apply(state, target, msgs, time.Now(), "", false)
	state.ui.focus = PaneTree
	m := newTestModel(state)
	m.width, m.height = 100, 30

	start := state.ui.msgs.cursor
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	m = updated.(chatModel)
	if state.ui.msgs.cursor >= start || state.ui.focus != PaneTree {
		t.Fatalf("ctrl+u must page the conversation up from the list: cursor %d -> %d, focus %s", start, state.ui.msgs.cursor, state.ui.focus)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if state.ui.msgs.cursor != start {
		t.Fatalf("ctrl+d must page back down, cursor %d want %d", state.ui.msgs.cursor, start)
	}
}

// TestComposerCtrlDSends pins gh-dash's submit key in the composer.
func TestComposerCtrlDSends(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneComposer
	state.ui.composerText = ""
	m := newChatModel(state)

	m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !strings.Contains(state.ui.composerStatus, "Nothing to send") {
		t.Fatalf("ctrl+d must submit the composer, status %q", state.ui.composerStatus)
	}
	if state.ui.focus != PaneMessages {
		t.Fatalf("submitting must leave the composer, focus %s", state.ui.focus)
	}
}

// TestSelectedItemActions pins o/y/Y: they act on the highlighted message,
// or the selected conversation from the list.
func TestSelectedItemActions(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	m := newTestModel(state)
	m.conversations.Select(0)

	link, id, ok := m.selectedLink()
	if !ok || id != "chan-1" || !strings.HasPrefix(link, "https://teams.microsoft.com/l/channel/chan-1/") {
		t.Fatalf("list selection link = %q id %q ok %v", link, id, ok)
	}

	target := teams.Conversation{ID: "chan-1", Title: "General"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{{Id: "m-9", ImDisplayName: "A", Content: "<p>x</p>"}}, time.Now(), "", false)
	state.ui.focus = PaneMessages
	link, id, _ = m.selectedLink()
	if id != "m-9" || link != "https://teams.microsoft.com/l/message/chan-1/m-9" {
		t.Fatalf("message link = %q id %q", link, id)
	}

	for _, r := range []rune{'o', 'y', 'Y'} {
		if _, cmd := m.Update(runeKey(r)); cmd == nil {
			t.Fatalf("%c must return a command", r)
		}
	}

	updated, _ := m.Update(actionResultMsg{ok: "Copied link"})
	if row := plain(strings.Split(updated.(chatModel).View(), "\n")[0]); !strings.Contains(row, "Copied link") {
		t.Fatalf("the notice must show in the tab row, got %q", row)
	}
}
