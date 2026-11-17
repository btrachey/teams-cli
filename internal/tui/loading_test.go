package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-cli/internal/teams"
)

func TestLoadingStatusTextIncludesMessageLimit(t *testing.T) {
	mainText, secondaryText := loadingStatusText(25, time.Now().Add(-2*time.Second))
	if !strings.Contains(mainText, "25") {
		t.Fatalf("expected main text to include the message limit, got %q", mainText)
	}
	if !strings.Contains(secondaryText, "Fetching recent messages from Teams") {
		t.Fatalf("unexpected secondary text: %q", secondaryText)
	}
}

// TestLoginViewRendersSpinnerComponent pins that the login page uses the
// bubbles spinner widget, not a hand-built animation.
func TestLoginViewRendersSpinnerComponent(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageLogin
	state.ui.loginMessage = "Logging in..."

	model := newChatModel(state)
	model.width, model.height = 80, 24

	rendered := plain(model.View())
	if !strings.Contains(rendered, "Logging in...") {
		t.Fatalf("expected the login message, got %q", rendered)
	}
	if frame := plain(model.spinner.View()); frame == "" || !strings.Contains(rendered, frame) {
		t.Fatalf("expected the spinner frame %q in the login view", frame)
	}
}

// TestMessagesLoadingRowCarriesSpinner pins that the message-loading row is
// animated by the same spinner component.
func TestMessagesLoadingRowCarriesSpinner(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain

	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100

	state.ui.msgs.applyLoading(teams.Conversation{ID: "19:x", Title: "Chat"}, time.Now(), 200)
	content, _, _ := model.messagesContent(model.messagesPaneWidth())

	row := plain(strings.SplitN(content, "\n", 2)[0])
	if !strings.HasPrefix(row, plain(model.spinner.View())) {
		t.Fatalf("expected the spinner frame to lead the loading row, got %q", row)
	}
	if !strings.Contains(row, "Loading last 200 messages") {
		t.Fatalf("expected the loading text, got %q", row)
	}
}
