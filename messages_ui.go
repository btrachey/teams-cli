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
	"github.com/rivo/tview"
)

func conversationPaneTitle(target ConversationTarget, syncedAt time.Time, status string) string {
	if status == "" {
		if syncedAt.IsZero() {
			return target.Title
		}
		status = "live " + syncedAt.Format("15:04:05")
	}

	if target.Title == "" {
		return status
	}

	return target.Title + " | " + status
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

func (s *AppState) renderConversationMessages(target ConversationTarget, messages []csa.ChatMessage, syncedAt time.Time, preserveSelection bool) {
	chatList := s.components[ViChat].(*tview.List)
	previousCount := chatList.GetItemCount()
	previousIndex := chatList.GetCurrentItem()
	followTail := previousCount > 0 && previousIndex >= previousCount-1
	status := ""
	if !syncedAt.IsZero() {
		status = s.conversationSyncStatus(syncedAt)
	}

	chatList.Clear()
	chatList.SetTitle(conversationPaneTitle(target, time.Time{}, status)).
		SetBorder(true).
		SetTitleAlign(tview.AlignCenter)
	s.chatPaneFocusable = true

	displayed := make([]csa.ChatMessage, 0, len(messages))
	for _, message := range messages {
		// System activity rows (thread membership events, call logs) are
		// not chat text and stay hidden unconditionally: a call started
		// by the signed-in user stores the user's own contact URL in
		// from with no display name, so the author gate below alone
		// would surface call/meeting XML as chat text.
		if isSystemActivityType(message.MessageType) {
			continue
		}

		author := strings.TrimSpace(message.ImDisplayName)
		if author == "" {
			// Remaining nameless messages are this app's own sends that
			// omitted imdisplayname (stored nameless server-side).
			// Render only those, under the signed-in user's display
			// name; nameless messages from anyone else stay hidden.
			if !s.isSelfAuthoredMessage(message) {
				continue
			}
			author = strings.TrimSpace(s.selfDisplayName())
			if author == "" {
				author = "You"
			}
		}

		mainText, secondaryText := messageListItem(author, message.Content)
		chatList.AddItem(mainText, secondaryText, 0, nil)
		displayed = append(displayed, message)
	}

	s.displayedMessages = displayed
	s.conversationLoaded = true

	if chatList.GetItemCount() == 0 {
		chatList.AddItem("No recent messages found", fmt.Sprintf("Loaded the latest %d messages for this conversation.", s.messageLimit), 0, nil)
	}

	// Always land on the newest message (last row; messages are sorted
	// ascending by ComposeTime) and tview's Draw scrolls it into view.
	// Refreshes keep the user's place while scrolling history, unless they
	// were already pinned at the tail — then keep following new messages.
	last := chatList.GetItemCount() - 1
	switch {
	case !preserveSelection || previousCount == 0, followTail:
		chatList.SetCurrentItem(last)
	case previousIndex <= last:
		chatList.SetCurrentItem(previousIndex)
	default:
		chatList.SetCurrentItem(last)
	}
}

func (s *AppState) updateConversationPaneTitle(target ConversationTarget, syncedAt time.Time, status string) {
	chatList, ok := s.components[ViChat].(*tview.List)
	if !ok {
		return
	}
	if status == "" && !syncedAt.IsZero() {
		status = s.conversationSyncStatus(syncedAt)
	}

	chatList.SetTitle(conversationPaneTitle(target, time.Time{}, status)).
		SetBorder(true).
		SetTitleAlign(tview.AlignCenter)
}

func (s *AppState) renderConversationLoadError(target ConversationTarget, err error) {
	chatList, ok := s.components[ViChat].(*tview.List)
	if !ok {
		return
	}

	s.displayedMessages = nil
	s.conversationLoaded = false
	mainText, secondaryText := conversationLoadErrorText(err)
	chatList.Clear()
	chatList.SetTitle(conversationPaneTitle(target, time.Time{}, "load failed")).
		SetBorder(true).
		SetTitleAlign(tview.AlignCenter)
	chatList.AddItem(mainText, secondaryText, 0, nil)
	chatList.AddItem("Retry", "Press Right or l to retry this conversation. Esc returns to the list.", 0, nil)
	s.chatPaneFocusable = true
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
