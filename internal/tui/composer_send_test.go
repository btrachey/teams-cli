package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fossteams/teams-cli/internal/config"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
)

func TestHelpBarTextForComposer(t *testing.T) {
	state := newUITestState()
	state.ui.focus = PaneComposer
	model := newChatModel(state)
	model.help.Width = 120
	text := strings.ToLower(plain(model.help.View(model.keyBindings())))
	if !strings.Contains(text, "send") {
		t.Fatalf("expected composer help to mention send, got %q", text)
	}
	// The global 'q'/'?' shortcuts are suppressed while the composer is
	// focused, so they must not be advertised on its help line.
	if strings.Contains(text, " q") || strings.Contains(text, "?") {
		t.Fatalf("composer help should not advertise the global q/? keys, got %q", text)
	}
}

func sendOutcomeState(t *testing.T) (*AppState, chatModel) {
	t.Helper()
	state := &AppState{
		logger:       logging.Discard,
		messageLimit: config.DefaultMessageLimit,
	}
	state.initRuntime(context.Background())
	return state, newChatModel(state)
}

func TestSendResultPreservesDraftOnFailure(t *testing.T) {
	state, model := sendOutcomeState(t)
	target := teams.Conversation{ID: "19:abc@thread.v2"}
	state.setCurrentConversation(target)
	state.composerReply = replyContext{target: target, messageID: "msg-1"}
	state.ui.composerText = "my draft"

	updated, _ := model.Update(sendResultMsg{target: target, err: errors.New("message send failed: 500")})
	got := updated.(chatModel).state

	if got.ui.composerText != "my draft" {
		t.Fatalf("expected the draft to survive a failed send, got %q", got.ui.composerText)
	}
	if got.composerReply.messageID != "msg-1" {
		t.Fatal("expected the reply context to survive a failed send")
	}
	if !strings.Contains(got.ui.composerStatus, "Send failed") {
		t.Fatalf("expected a failure status, got %q", got.ui.composerStatus)
	}
}

func TestSendResultClearsDraftAndReplyOnSuccess(t *testing.T) {
	state, model := sendOutcomeState(t)
	target := teams.Conversation{ID: "19:abc@thread.v2"}
	state.setCurrentConversation(target)
	state.composerReply = replyContext{target: target, messageID: "msg-1"}
	state.ui.composerText = "my draft"

	updated, _ := model.Update(sendResultMsg{target: target, err: nil})
	got := updated.(chatModel).state

	if got.ui.composerText != "" {
		t.Fatalf("expected the draft to clear after success, got %q", got.ui.composerText)
	}
	if got.composerReply.messageID != "" {
		t.Fatal("expected the reply context to clear after success")
	}
	if !strings.Contains(got.ui.composerStatus, "Sent") {
		t.Fatalf("expected a sent status, got %q", got.ui.composerStatus)
	}
}

func TestSendResultIgnoresStaleConversation(t *testing.T) {
	state, model := sendOutcomeState(t)
	target := teams.Conversation{ID: "19:old@thread.v2"}
	state.setCurrentConversation(teams.Conversation{ID: "19:new@thread.v2"})
	state.ui.composerText = "draft for old conversation"

	updated, _ := model.Update(sendResultMsg{target: target, err: nil})
	got := updated.(chatModel).state

	if got.ui.composerText != "draft for old conversation" {
		t.Fatalf("expected the other conversation's draft to stay untouched, got %q", got.ui.composerText)
	}
	if got.ui.composerStatus != "" {
		t.Fatalf("expected no status change, got %q", got.ui.composerStatus)
	}
}
