package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
)

func conversationPaneTitle(target ConversationTarget, syncedAt time.Time, status string) string {
	// target.Title derives from network-sourced team/channel/chat names.
	title := sanitizeText(target.Title)
	if status == "" {
		if syncedAt.IsZero() {
			return title
		}
		status = "live " + syncedAt.Format("15:04:05")
	}

	if title == "" {
		return status
	}

	return title + " | " + status
}

func messagesSignature(messages []csa.ChatMessage) string {
	hasher := fnv.New64a()
	for _, message := range messages {
		_, _ = hasher.Write([]byte(message.Id))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(message.Version))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(message.ImDisplayName))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(message.Content))
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(time.Time(message.ComposeTime).Format(time.RFC3339Nano)))
		_, _ = hasher.Write([]byte{'\n'})
	}

	return fmt.Sprintf("%x", hasher.Sum64())
}

// isSelfAuthoredMessage reports whether a message was sent by the signed-in
// user. The message service stores an empty sender name for posts that
// omitted imdisplayname (including this app's own earlier sends), so the
// author contact encoded in the from-URL is the only reliable signal.
func (s *AppState) isSelfAuthoredMessage(message csa.ChatMessage) bool {
	selfMri := s.selfMri()
	if selfMri == "" {
		return false
	}

	return strings.HasSuffix(message.From, "/"+selfMri)
}

// isSystemActivityType reports whether a message type is a system-generated
// activity row rather than chat text. The "System" prefix is deliberate, not
// drift: the live wire record for "user joined a meeting" carries messagetype
// exactly "System" with an empty sender name, so it is only hidden by this
// guard. A hypothetical *named* System row would now also hide; it would have
// rendered under the old name-based filter, but no such row exists in the
// observed traffic and call/meeting XML is never chat text.
func isSystemActivityType(messageType string) bool {
	return strings.HasPrefix(messageType, "ThreadActivity/") ||
		strings.HasPrefix(messageType, "Event/") ||
		strings.HasPrefix(messageType, "System")
}

func conversationLoadErrorText(err error) (string, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "Timed out while loading recent messages", "Teams did not respond before the request timeout. Press Right or l to retry."
	case errors.Is(err, context.Canceled):
		return "Loading was cancelled", "Move to another conversation or press Right or l to retry."
	}

	var statusErr *messageFetchStatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return "Teams rejected the request", "Your Teams tokens may have expired. Refresh them and try again."
		case http.StatusTooManyRequests:
			return "Teams is rate limiting requests", "Wait a few seconds, then press Right or l to retry."
		default:
			if statusErr.StatusCode >= http.StatusInternalServerError {
				return "Teams returned a server error", "The service is temporarily unavailable. Press Right or l to retry."
			}
		}
	}

	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "An unexpected error occurred while loading recent messages."
	}

	return "Unable to load recent messages", message
}
