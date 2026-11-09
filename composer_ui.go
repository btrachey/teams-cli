package main

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

const composerStatusMaxLength = 200

// replyToAuthor returns the display name of the message being replied to, or
// an empty string when the reply context references a message that is no
// longer in the displayed window.
func (s *AppState) replyToAuthor() string {
	reply := s.composerReply
	if reply.messageID == "" {
		return ""
	}

	for index := range s.displayedMessages {
		if s.displayedMessages[index].Id == reply.messageID {
			return sanitizeText(s.displayedMessages[index].ImDisplayName)
		}
	}

	return ""
}

func (s *AppState) setComposerStatus(status string, kind int) {
	s.ui.composerStatus = truncateComposerStatus(status)
	s.ui.composerStatusKind = kind
}

// focusComposerForMessage opens the composer in plain top-level send mode for
// the active conversation.
func (s *AppState) focusComposerForMessage() {
	if !s.conversationLoaded {
		return
	}

	s.clearComposerReply()
	s.focusComponent(PaneComposer)
}

// beginReplyTo enters reply mode for the message currently highlighted in the
// messages pane. Thread replies only exist for channels, so chats fall back
// to a plain top-level send with the composer focused.
func (s *AppState) beginReplyTo() {
	message, ok := s.messageAtCurrentIndex()
	if !ok {
		return
	}

	target, ok, _, _ := s.currentConversationSnapshot()
	if !ok {
		return
	}

	if s.isChannelConversation(target.ID) {
		s.composerReply = replyContext{target: target, messageID: message.Id}
	}

	s.focusComponent(PaneComposer)
}

// clearComposerReply cancels any pending thread reply, leaving the composer in
// plain send mode.
func (s *AppState) clearComposerReply() {
	if s.composerReply.messageID == "" {
		return
	}

	s.composerReply = replyContext{}
}

// resetComposer clears the composer for a new conversation: text, reply
// context, and status.
func (s *AppState) resetComposer() {
	s.composerReply = replyContext{}
	s.ui.composerText = ""
	s.setComposerStatus("", statusNone)
}

// sendComposerMessage submits the composer text as a top-level message, or as
// a thread reply when the composer is in reply mode. The HTTP round trip
// happens off the UI thread; the outcome arrives as a sendResultMsg.
func (s *AppState) sendComposerMessage() {
	text := strings.TrimSpace(s.ui.composerText)
	if text == "" {
		s.setComposerStatus("Nothing to send: the composer is empty.", statusInfo)
		return
	}

	target, ok, _, _ := s.currentConversationSnapshot()
	if !ok || target.ID == "" {
		s.setComposerStatus("Open a channel or chat first (select it in the tree).", statusInfo)
		return
	}

	if !atomic.CompareAndSwapUint32(&s.sendInFlight, 0, 1) {
		return
	}

	reply := s.composerReply
	s.setComposerStatus("Sending…", statusInfo)
	go func() {
		defer atomic.StoreUint32(&s.sendInFlight, 0)

		sendMessage := func(ctx context.Context) error {
			if reply.messageID != "" && reply.target.ID == target.ID {
				return s.sendThreadReply(ctx, target, reply.messageID, text)
			}
			return s.sendConversationMessage(ctx, target, text)
		}

		sendContext, cancel := context.WithTimeout(s.appContext(), messageSendTimeout)
		err := sendMessage(sendContext)
		cancel()

		if err != nil && isAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
			retryContext, retryCancel := context.WithTimeout(s.appContext(), messageSendTimeout)
			err = sendMessage(retryContext)
			retryCancel()
		}

		s.sendUIMsg(sendResultMsg{target: target, err: err})
	}()
}

// reloadConversationCmd refetches the active conversation so the message
// just sent (and any server-assigned ID) shows up in the list. Selection is
// not preserved: the user lands on the newest message.
func (s *AppState) reloadConversationCmd() tea.Cmd {
	target, ok, _, _ := s.currentConversationSnapshot()
	if !ok {
		return nil
	}

	s.cancelActiveLoad()
	loadSeq := atomic.AddUint64(&s.loadSeq, 1)
	atomic.StoreUint64(&s.activeLoadSeq, loadSeq)
	loadCtx, cancel := context.WithCancel(s.appContext())
	s.replaceActiveLoadCancel(loadSeq, cancel)

	s.ui.msgs.paneTitle = conversationPaneTitle(target, time.Time{}, "syncing…")

	return func() tea.Msg {
		messages, err := s.fetchMessages(loadCtx, target)
		if err != nil {
			atomic.CompareAndSwapUint64(&s.activeLoadSeq, loadSeq, 0)
			if isLoadCancellation(loadCtx, err) {
				return nil
			}
			return messagesLoadErrorMsg{target: target, err: err, loadSeq: loadSeq}
		}
		atomic.CompareAndSwapUint64(&s.activeLoadSeq, loadSeq, 0)
		return messagesLoadedMsg{
			target:            target,
			messages:          messages,
			syncedAt:          time.Now(),
			loadSeq:           loadSeq,
			preserveSelection: false,
		}
	}
}

func isLoadCancellation(loadCtx context.Context, err error) bool {
	return loadCtx.Err() != nil && errors.Is(err, context.Canceled)
}
