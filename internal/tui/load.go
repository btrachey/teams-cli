package tui

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/teams"
	"github.com/sirupsen/logrus"
)

// focusComponent moves keyboard focus between panes.
func (s *AppState) focusComponent(name string) {
	switch name {
	case PaneTree, PaneMessages, PaneComposer, PaneCalendar, PaneCalDetail:
	default:
		return
	}

	s.ui.focus = name
}

// focusMessagesPane moves focus to the messages pane when it can receive
// focus at all (a hint pane cannot).
func (s *AppState) focusMessagesPane() {
	if !s.ui.msgs.focusable() {
		return
	}

	s.focusComponent(PaneMessages)
}

// startConversationLoad kicks off an async message fetch for a conversation
// leaf and shows the animated loading placeholder. Results arrive through
// messagesLoadedMsg / messagesLoadErrorMsg; the loadSeq guards staleness.
// The spinner animating the loading row restarts itself (chatModel.Update).
func (s *AppState) startConversationLoad(target teams.Conversation) {
	if target.ID == "" {
		return
	}

	s.setCurrentConversation(target)
	s.cancelActiveLoad()
	s.resetComposer()

	loadSeq := atomic.AddUint64(&s.loadSeq, 1)
	atomic.StoreUint64(&s.activeLoadSeq, loadSeq)
	loadCtx, cancel := context.WithCancel(s.appContext())
	s.replaceActiveLoadCancel(loadSeq, cancel)

	s.ui.msgs.applyLoading(target, time.Now(), s.messageLimit)

	go s.fetchConversationMessages(loadCtx, target, loadSeq)
}

// fetchConversationMessages runs the retrying fetch off the UI thread and
// delivers the outcome as a message.
func (s *AppState) fetchConversationMessages(ctx context.Context, target teams.Conversation, loadSeq uint64) {
	defer s.clearActiveLoadCancel(loadSeq)

	s.appLogger().WithFields(logFields(target, s.messageLimit, loadSeq)).Debug("loading conversation messages")

	messages, err := s.fetchMessages(ctx, target)
	if err != nil {
		atomic.CompareAndSwapUint64(&s.activeLoadSeq, loadSeq, 0)
		if isLoadCancellation(ctx, err) {
			s.appLogger().WithFields(logFields(target, s.messageLimit, loadSeq)).Debug("conversation message load cancelled")
			return
		}
		if s.staleLoad(loadSeq) {
			return
		}

		s.appLogger().WithFields(logFields(target, s.messageLimit, loadSeq)).WithError(err).Warn("conversation message load failed")
		s.sendUIMsg(messagesLoadErrorMsg{target: target, err: err, loadSeq: loadSeq})
		return
	}

	atomic.CompareAndSwapUint64(&s.activeLoadSeq, loadSeq, 0)
	if s.staleLoad(loadSeq) {
		return
	}

	s.appLogger().WithFields(logFieldsWithCount(target, s.messageLimit, loadSeq, len(messages))).Debug("conversation messages loaded")
	s.sendUIMsg(messagesLoadedMsg{
		target:            target,
		messages:          messages,
		syncedAt:          time.Now(),
		loadSeq:           loadSeq,
		preserveSelection: false,
	})
}

// fetchMessages loads a conversation's newest messages (or the test
// override's).
func (s *AppState) fetchMessages(ctx context.Context, target teams.Conversation) ([]csa.ChatMessage, error) {
	if s.fetchMessagesOverride != nil {
		return s.fetchMessagesOverride(ctx, target)
	}

	return s.teams.FetchMessages(ctx, target, s.messageLimit)
}

func logFields(target teams.Conversation, limit int, loadSeq uint64) logrus.Fields {
	return logFieldsWithCount(target, limit, loadSeq, -1)
}

func logFieldsWithCount(target teams.Conversation, limit int, loadSeq uint64, count int) logrus.Fields {
	fields := logrus.Fields{
		"conversation_id":    target.ID,
		"conversation_title": target.Title,
		"message_limit":      limit,
		"load_seq":           loadSeq,
	}
	if count >= 0 {
		fields["message_count"] = count
	}
	return fields
}

func loadingStatusText(limit int, startedAt time.Time) (string, string) {
	elapsedSeconds := int(time.Since(startedAt).Seconds())
	mainText := fmt.Sprintf("Loading last %d messages", limit)
	secondaryText := fmt.Sprintf("Fetching recent messages from Teams... %ds", elapsedSeconds)
	return mainText, secondaryText
}
