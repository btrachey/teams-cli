package tui

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/sirupsen/logrus"
)

func (s *AppState) runLiveRefreshLoop() {
	messageTicker := time.NewTicker(s.messageRefreshInterval())
	conversationTicker := time.NewTicker(s.conversationRefreshInterval())
	defer messageTicker.Stop()
	defer conversationTicker.Stop()
	defer s.appLogger().Debug("live refresh loop stopped")

	for {
		select {
		case <-messageTicker.C:
			if !atomic.CompareAndSwapUint32(&s.liveMessageRefreshActive, 0, 1) {
				continue
			}
			go func() {
				defer atomic.StoreUint32(&s.liveMessageRefreshActive, 0)
				s.refreshSelectedConversationMessages()
			}()
		case <-conversationTicker.C:
			if !atomic.CompareAndSwapUint32(&s.liveTreeRefreshActive, 0, 1) {
				continue
			}
			go func() {
				defer atomic.StoreUint32(&s.liveTreeRefreshActive, 0)
				s.refreshConversationTree()
			}()
		case <-s.appContext().Done():
			return
		}
	}
}

func (s *AppState) refreshSelectedConversationMessages() {
	select {
	case <-s.appContext().Done():
		return
	default:
	}

	if atomic.LoadUint64(&s.activeLoadSeq) != 0 {
		return
	}

	target, ok, previousSignature, _ := s.currentConversationSnapshot()
	if !ok {
		return
	}

	messages, err := s.fetchMessages(s.appContext(), target)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		s.appLogger().WithFields(logrus.Fields{
			"conversation_id":    target.ID,
			"conversation_title": target.Title,
		}).WithError(err).Warn("live message refresh failed")
		s.sendUIMsg(liveMessagesRetryMsg{target: target, at: time.Now()})
		return
	}

	s.sendUIMsg(liveMessagesMsg{
		target:            target,
		messages:          messages,
		syncedAt:          time.Now(),
		previousSignature: previousSignature,
	})
}

func (s *AppState) refreshConversationTree() {
	select {
	case <-s.appContext().Done():
		return
	default:
	}

	data, err := s.teams.FetchConversations()
	if err != nil && auth.IsAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
		data, err = s.teams.FetchConversations()
	}
	if err != nil {
		s.appLogger().WithError(err).Warn("live conversation refresh failed")
		return
	}

	s.sendUIMsg(treeRefreshedMsg{data: data})
}
