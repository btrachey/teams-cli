package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/calendar"
	"github.com/fossteams/teams-cli/internal/teams"
)

// TestLoadErrorRetryRowReloads pins that the retry row the error pane
// advertises actually retries: the cursor lands on it and Enter reloads.
func TestLoadErrorRetryRowReloads(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.applyError(state, target, errors.New("boom"))
	state.ui.focus = PaneMessages
	if !state.ui.msgs.onRetryRow() {
		t.Fatal("a failed load must park the cursor on the retry row")
	}

	fetched := make(chan struct{}, 1)
	state.fetchMessagesOverride = func(context.Context, teams.Conversation) ([]csa.ChatMessage, error) {
		fetched <- struct{}{}
		return nil, nil
	}

	updated, _ := newChatModel(state).Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !updated.(chatModel).ui().msgs.loading {
		t.Fatal("Enter on the retry row must start a reload")
	}
	select {
	case <-fetched:
	case <-time.After(2 * time.Second):
		t.Fatal("retry never fetched")
	}
}

// TestCalendarLoadKeepsConversationLoad pins that opening the calendar does
// not cancel an in-flight conversation load (which would strand the
// messages pane on its loading row).
func TestCalendarLoadKeepsConversationLoad(t *testing.T) {
	state := newUITestState()
	state.calendar.override = func(ctx context.Context, _ calendar.Query) ([]calendar.Event, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	loadCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state.replaceActiveLoadCancel(7, cancel)

	state.enterCalendar()
	if loadCtx.Err() != nil {
		t.Fatal("starting a calendar load cancelled the conversation load")
	}
	if !state.ui.cal.loading {
		t.Fatal("enterCalendar must start the first calendar load")
	}
}

// TestCalendarKeys pins '?' toggling help and Enter opening the join link.
func TestCalendarKeys(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageCalendar
	state.ui.cal.apply(calendarTestEvents(), time.Now(), time.Now(), time.Now().Add(time.Hour))

	m := newChatModel(state)
	updated, _ := m.Update(runeKey('?'))
	m = updated.(chatModel)
	if !m.help.ShowAll {
		t.Fatal("? must toggle the full help on the calendar page")
	}

	for i := range state.ui.cal.entries {
		state.ui.cal.setCursor(i)
		if _, ok := state.ui.cal.selectedJoinURL(); ok {
			break
		}
	}
	if _, ok := state.ui.cal.selectedJoinURL(); !ok {
		t.Fatal("fixture needs a meeting with a join link")
	}
	if _, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd == nil {
		t.Fatal("Enter on a meeting with a join link must open it")
	}
}
