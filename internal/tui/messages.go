package tui

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"strings"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/teams"
)

// messageEntry is one rendered row in the messages pane. Placeholder rows
// (hints, loading, errors) carry no Message.
type messageEntry struct {
	Main    string
	Second  string
	Stamp   string
	Images  []string
	Message *csa.ChatMessage
}

// messagesModel owns the messages pane state: filtered entries, the cursor
// (the selection), and the viewport scroll offset that keeps the cursor in
// view. The pane can take focus once it holds any entry.
type messagesModel struct {
	entries []messageEntry
	cursor  int
	// yOffset is the viewport's first visible line. Rendering adjusts it
	// minimally to keep the cursor entry on screen, and it persists here
	// so scrolling does not jump between frames.
	yOffset int

	// paneTitle is the border title (conversation + sync status).
	paneTitle string
	// loading marks the animated loading placeholder state; the loading
	// row's text is derived at render time from loadingSince/loadingLimit.
	loading      bool
	loadingSince time.Time
	loadingLimit int
}

const (
	noMessagesMain  = "No recent messages found"
	retryItemMain   = "Retry"
	retryItemSecond = "Press Enter or l to retry this conversation. Esc returns to the list."
)

// filterDisplayedMessages applies the visibility rules: system activity rows
// are always hidden; nameless rows render only when they were sent by the
// signed-in user, under that user's display name.
func (s *AppState) filterDisplayedMessages(messages []csa.ChatMessage) []messageEntry {
	entries := make([]messageEntry, 0, len(messages))
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
			author = strings.TrimSpace(s.teams.SelfDisplayName())
			if author == "" {
				author = "You"
			}
		}

		mainText, secondaryText := messageListItem(author, message.Content)
		msg := message
		entries = append(entries, messageEntry{
			Main:    mainText,
			Second:  secondaryText,
			Stamp:   messageTimestamp(time.Time(message.ComposeTime)),
			Images:  messageImageURLs(message.Content),
			Message: &msg,
		})
	}

	return entries
}

// apply replaces the pane contents with real messages, preserving the
// user's place when requested and following the tail when they were pinned
// at the newest message.
func (m *messagesModel) apply(s *AppState, target teams.Conversation, messages []csa.ChatMessage, syncedAt time.Time, status string, preserveSelection bool) {
	previousCount := len(m.entries)
	previousIndex := m.cursor
	followTail := previousCount > 0 && previousIndex >= previousCount-1
	if status == "" && !syncedAt.IsZero() {
		status = s.conversationSyncStatus(syncedAt)
	}

	entries := s.filterDisplayedMessages(messages)
	s.displayedMessages = make([]csa.ChatMessage, 0, len(entries))
	for _, entry := range entries {
		if entry.Message != nil {
			s.displayedMessages = append(s.displayedMessages, *entry.Message)
		}
	}
	s.conversationLoaded = true

	if len(entries) == 0 {
		entries = []messageEntry{{
			Main:   noMessagesMain,
			Second: fmt.Sprintf("Loaded the latest %d messages for this conversation.", s.messageLimit),
		}}
	}

	m.entries = entries
	m.paneTitle = conversationPaneTitle(target, time.Time{}, status)
	m.loading = false

	// Always land on the newest message (last row; messages are sorted
	// ascending by ComposeTime) and keep it in view. Refreshes keep the
	// user's place while scrolling history, unless they were already
	// pinned at the tail — then keep following new messages.
	last := len(m.entries) - 1
	switch {
	case !preserveSelection || previousCount == 0, followTail:
		m.setCursor(last)
	case previousIndex <= last:
		m.setCursor(previousIndex)
	default:
		m.setCursor(last)
	}
}

// applyError replaces the pane with a failure notice plus a retry row, and
// parks the cursor on the retry row so Enter/l retries straight away.
func (m *messagesModel) applyError(s *AppState, target teams.Conversation, err error) {
	s.displayedMessages = nil
	s.conversationLoaded = false
	mainText, secondaryText := conversationLoadErrorText(err)
	m.entries = []messageEntry{
		{Main: mainText, Second: secondaryText},
		{Main: retryItemMain, Second: retryItemSecond},
	}
	m.paneTitle = conversationPaneTitle(target, time.Time{}, "load failed")
	m.loading = false
	m.setCursor(len(m.entries) - 1)
}

// applyLoading replaces the pane with the animated loading status row.
func (m *messagesModel) applyLoading(target teams.Conversation, startedAt time.Time, limit int) {
	mainText, secondaryText := loadingStatusText(limit, startedAt)
	m.entries = []messageEntry{{Main: mainText, Second: secondaryText}}
	m.paneTitle = conversationPaneTitle(target, time.Time{}, "loading...")
	m.loading = true
	m.loadingSince = startedAt
	m.loadingLimit = limit
	m.setCursor(0)
}

// focusable reports whether the pane may receive focus: it has rendered
// something (messages, a loading row, or an error).
func (m *messagesModel) focusable() bool { return len(m.entries) > 0 }

// onRetryRow reports whether the cursor sits on the load-failure retry row.
func (m *messagesModel) onRetryRow() bool {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return false
	}
	entry := m.entries[m.cursor]
	return entry.Message == nil && entry.Main == retryItemMain
}

// setCursor moves the cursor; rendering scrolls it into view.
func (m *messagesModel) setCursor(index int) {
	m.cursor = clampCursor(index, len(m.entries))
}

func (m *messagesModel) up()            { m.setCursor(m.cursor - 1) }
func (m *messagesModel) down()          { m.setCursor(m.cursor + 1) }
func (m *messagesModel) page(delta int) { m.setCursor(m.cursor + delta) }
func (m *messagesModel) home()          { m.setCursor(0) }
func (m *messagesModel) end()           { m.setCursor(len(m.entries) - 1) }

// currentMessage returns the message under the cursor, or false when the
// pane shows a placeholder or the count drifted.
func (m *messagesModel) currentMessage() (*csa.ChatMessage, bool) {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return nil, false
	}
	entry := m.entries[m.cursor]
	if entry.Message == nil {
		return nil, false
	}
	return entry.Message, true
}

// clampCursor bounds a cursor to [0, count), or 0 for an empty list.
func clampCursor(cursor, count int) int {
	return max(0, min(cursor, count-1))
}

func conversationPaneTitle(target teams.Conversation, syncedAt time.Time, status string) string {
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
	selfMri := s.teams.SelfMri()
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

	var statusErr *teams.FetchError
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
