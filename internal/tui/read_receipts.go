package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/sirupsen/logrus"
)

// Read receipts tell Teams a conversation was read (the "consumption
// horizon" the GUI client sends), so its unread state clears everywhere.
// Moving the list cursor loads conversations into the preview, so a
// receipt waits until the conversation has stayed on screen for
// readReceiptDelay: scrolling past a conversation must not mark it read.
const (
	readReceiptDelay   = 2 * time.Second
	readReceiptTimeout = 10 * time.Second
)

type (
	// readReceiptDueMsg fires once a conversation has been on screen for
	// readReceiptDelay.
	readReceiptDueMsg struct {
		conversationID string
		newest         csa.ChatMessage
	}
	// readReceiptResultMsg reports a sent receipt; a failure is retried the
	// next time the conversation is shown.
	readReceiptResultMsg struct {
		conversationID string
		messageID      string
		err            error
	}
)

// scheduleReadReceipt arms a receipt for the newest message once it has been
// on screen long enough. messages are oldest first.
func (s *AppState) scheduleReadReceipt(conversationID string, messages []csa.ChatMessage) tea.Cmd {
	if !s.readReceipts || len(messages) == 0 {
		return nil
	}
	newest := messages[len(messages)-1]
	if newest.Id == "" || s.readSent[conversationID] == newest.Id {
		return nil
	}

	return tea.Tick(readReceiptDelay, func(time.Time) tea.Msg {
		return readReceiptDueMsg{conversationID: conversationID, newest: newest}
	})
}

// sendReadReceipt sends a due receipt when its conversation is still the one
// on screen. It runs on the UI loop; the request runs in the background.
func (s *AppState) sendReadReceipt(msg readReceiptDueMsg) tea.Cmd {
	if s.ui.page != pageMain || s.ui.previewHidden || !s.currentConversationIs(msg.conversationID) {
		return nil
	}
	if s.readSent[msg.conversationID] == msg.newest.Id {
		return nil
	}
	if s.readSent == nil {
		s.readSent = map[string]string{}
	}
	s.readSent[msg.conversationID] = msg.newest.Id

	return func() tea.Msg {
		mark := func() error {
			ctx, cancel := context.WithTimeout(s.appContext(), readReceiptTimeout)
			defer cancel()
			if s.markReadOverride != nil {
				return s.markReadOverride(ctx, msg.conversationID, msg.newest)
			}
			return s.teams.MarkRead(ctx, msg.conversationID, msg.newest)
		}
		err := mark()
		if err != nil && auth.IsAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
			err = mark()
		}
		return readReceiptResultMsg{conversationID: msg.conversationID, messageID: msg.newest.Id, err: err}
	}
}

// handleReadReceiptResult forgets a failed receipt so the next view retries.
func (s *AppState) handleReadReceiptResult(msg readReceiptResultMsg) {
	fields := logrus.Fields{"conversation_id": msg.conversationID, "message_id": msg.messageID}
	if msg.err == nil {
		s.appLogger().WithFields(fields).Debug("read receipt sent")
		return
	}
	s.appLogger().WithFields(fields).WithError(msg.err).Warn("read receipt failed")
	if s.readSent[msg.conversationID] == msg.messageID {
		delete(s.readSent, msg.conversationID)
	}
}
