package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
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

// messagesModel owns the messages pane state: filtered entries, the cursor,
// and the first visible row. It mirrors the old tview.List semantics: the
// cursor is the selection, and rendering scrolls the cursor into view.
type messagesModel struct {
	entries []messageEntry
	cursor  int
	top     int

	// paneTitle is the border title (conversation + sync status).
	paneTitle string
	// focusable reports whether the pane may receive focus at all.
	focusable bool
	// loading marks the animated loading placeholder state.
	loading bool
	// loadingSince anchors the elapsed counter in the loading text.
	loadingSince time.Time
}

const (
	noMessagesMain  = "No recent messages found"
	retryItemMain   = "Retry"
	retryItemSecond = "Press Right or l to retry this conversation. Esc returns to the list."
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
			author = strings.TrimSpace(s.selfDisplayName())
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
func (m *messagesModel) apply(s *AppState, target ConversationTarget, messages []csa.ChatMessage, syncedAt time.Time, status string, preserveSelection bool) {
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
	m.focusable = true
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

// applyError replaces the pane with a failure notice plus a retry row.
func (m *messagesModel) applyError(s *AppState, target ConversationTarget, err error) {
	s.displayedMessages = nil
	s.conversationLoaded = false
	mainText, secondaryText := conversationLoadErrorText(err)
	m.entries = []messageEntry{
		{Main: mainText, Second: secondaryText},
		{Main: retryItemMain, Second: retryItemSecond},
	}
	m.paneTitle = conversationPaneTitle(target, time.Time{}, "load failed")
	m.focusable = true
	m.loading = false
}

// applyLoading replaces the pane with the animated loading status row.
func (m *messagesModel) applyLoading(target ConversationTarget, startedAt time.Time, step int, limit int) {
	mainText, secondaryText := loadingStatusText(limit, startedAt)
	m.entries = []messageEntry{{Main: mainText, Second: secondaryText}}
	m.paneTitle = conversationPaneTitle(target, time.Time{}, "loading...")
	m.focusable = true
	m.loading = true
	m.loadingSince = startedAt
}

// setCursor moves the cursor and scrolls it into view, mirroring what
// tview.List.Draw did automatically.
func (m *messagesModel) setCursor(index int) {
	if len(m.entries) == 0 {
		m.cursor = 0
		m.top = 0
		return
	}
	m.cursor = clampCursor(index, len(m.entries))
	if m.cursor < m.top {
		m.top = m.cursor
	}
}

func (m *messagesModel) up() { m.setCursor(m.cursor - 1) }

func (m *messagesModel) down() {
	m.setCursor(m.cursor + 1)
}

// page moves the window (and cursor) by +/- pageSize rows, the old PgUp/PgDn
// semantics of a scrolling list.
func (m *messagesModel) page(delta int, pageSize int) {
	if len(m.entries) == 0 {
		return
	}
	lastTop := maxInt(0, len(m.entries)-pageSize)
	m.top = clampInt(m.top+delta, 0, lastTop)
	m.cursor = clampInt(m.cursor+delta, m.top, minInt(m.top+pageSize-1, len(m.entries)-1))
}

func (m *messagesModel) home() { m.setCursor(0); m.top = 0 }

func (m *messagesModel) end() { m.setCursor(len(m.entries) - 1) }

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

func clampInt(value, low, high int) int {
	if high < low {
		return low
	}
	if value < low {
		return low
	}
	if value > high {
		return high
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func clampCursor(cursor, count int) int {
	if count <= 0 || cursor < 0 {
		return 0
	}
	if cursor >= count {
		return count - 1
	}
	return cursor
}
