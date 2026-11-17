package tui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-cli/internal/teams"
)

func TestConversationLoadErrorTextForUnauthorized(t *testing.T) {
	mainText, secondaryText := conversationLoadErrorText(&teams.FetchError{
		StatusCode: http.StatusUnauthorized,
		Status:     "401 Unauthorized",
	})

	if !strings.Contains(mainText, "Teams rejected") {
		t.Fatalf("expected auth failure title, got %q", mainText)
	}
	if !strings.Contains(secondaryText, "tokens may have expired") {
		t.Fatalf("expected auth guidance, got %q", secondaryText)
	}
}

func TestConversationLoadErrorTextForTimeout(t *testing.T) {
	mainText, secondaryText := conversationLoadErrorText(context.DeadlineExceeded)

	if !strings.Contains(mainText, "Timed out") {
		t.Fatalf("expected timeout title, got %q", mainText)
	}
	if !strings.Contains(secondaryText, "request timeout") {
		t.Fatalf("expected timeout guidance, got %q", secondaryText)
	}
}

func TestRequestStopCancelsRuntimeContext(t *testing.T) {
	state := AppState{}
	state.initRuntime(context.Background())

	state.requestStop()

	select {
	case <-state.appContext().Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected runtime context to be cancelled")
	}
}
