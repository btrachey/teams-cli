package main

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
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
			return s.displayedMessages[index].ImDisplayName
		}
	}

	return ""
}

func (s *AppState) updateComposerLabel() {
	composer := s.composerInput
	if composer == nil {
		return
	}

	if reply := s.composerReply; reply.messageID != "" {
		author := s.replyToAuthor()
		if author == "" {
			author = "message"
		}

		composer.SetLabel("Reply to " + author)
		return
	}

	composer.SetLabel("")
}

func (s *AppState) composerStatusText(message string) string {
	if len(message) <= composerStatusMaxLength {
		return message
	}

	return message[:composerStatusMaxLength-1] + "…"
}

func (s *AppState) setComposerStatus(status string, color tcell.Color) {
	textView, ok := s.components[TvComposerStatus].(*tview.TextView)
	if !ok {
		return
	}

	textView.SetTextColor(color)
	textView.SetText(s.composerStatusText(status))
}

// focusComposerForMessage opens the composer in plain top-level send mode for
// the active conversation.
func (s *AppState) focusComposerForMessage() {
	if !s.conversationLoaded {
		return
	}

	s.clearComposerReply()
	s.focusComponent(ViComposer)
}

// beginReplyTo enters reply mode for the message currently highlighted in the
// messages list. Thread replies only exist for channels, so chats fall back
// to a plain top-level send with the composer focused.
func (s *AppState) beginReplyTo() {
	message, ok := s.messageAtCurrentListIndex()
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

	s.focusComponent(ViComposer)
	s.updateComposerLabel()
}

// clearComposerReply cancels any pending thread reply, leaving the composer in
// plain send mode.
func (s *AppState) clearComposerReply() {
	if s.composerReply.messageID == "" {
		return
	}

	s.composerReply = replyContext{}
	s.updateComposerLabel()
}

// resetComposer clears the composer for a new conversation: text, reply
// context, and status.
func (s *AppState) resetComposer() {
	s.composerReply = replyContext{}
	if composer := s.composerInput; composer != nil {
		composer.SetText("")
	}
	s.updateComposerLabel()
	s.setComposerStatus("", tcell.ColorDefault)
}

// sendComposerMessage submits the composer text as a top-level message, or as
// a thread reply when the composer is in reply mode. The HTTP round trip
// happens off the UI thread so the interface stays responsive while the
// request is in flight.
func (s *AppState) sendComposerMessage() {
	composer := s.composerInput
	if composer == nil {
		return
	}

	text := strings.TrimSpace(composer.GetText())
	if text == "" {
		s.setComposerStatus("Nothing to send: the composer is empty.", tcell.ColorYellow)
		return
	}

	target, ok, _, _ := s.currentConversationSnapshot()
	if !ok || target.ID == "" {
		s.setComposerStatus("Open a channel or chat first (select it in the tree).", tcell.ColorYellow)
		return
	}

	if !atomic.CompareAndSwapUint32(&s.sendInFlight, 0, 1) {
		return
	}

	reply := s.composerReply
	s.setComposerStatus("Sending…", tcell.ColorYellow)
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
		s.app.QueueUpdateDraw(func() {
			if !s.applySendOutcome(target, err) {
				return
			}
			s.reloadCurrentConversation()
		})
	}()
}

// applySendOutcome updates the composer after a send attempt: on failure the
// draft stays intact so the user can retry; on success the draft and any
// thread-reply context are cleared. It reports whether the send succeeded
// and the conversation should be reloaded.
func (s *AppState) applySendOutcome(target ConversationTarget, err error) bool {
	select {
	case <-s.appContext().Done():
		return false
	default:
	}

	if !s.currentConversationIs(target.ID) {
		return false
	}

	if err != nil {
		s.setComposerStatus("Send failed: "+err.Error(), tcell.ColorRed)
		return false
	}

	if composer := s.composerInput; composer != nil {
		composer.SetText("")
	}
	s.clearComposerReply()
	s.setComposerStatus("Sent.", tcell.ColorGreen)
	return true
}

// reloadCurrentConversation refetches the active conversation so the message
// just sent (and any server-assigned ID) shows up in the list.
func (s *AppState) reloadCurrentConversation() {
	target, ok, _, _ := s.currentConversationSnapshot()
	if !ok {
		return
	}

	s.cancelActiveLoad()
	loadSeq := atomic.AddUint64(&s.loadSeq, 1)
	atomic.StoreUint64(&s.activeLoadSeq, loadSeq)
	loadCtx, cancel := context.WithCancel(s.appContext())
	s.replaceActiveLoadCancel(loadSeq, cancel)

	chatList := s.currentMessageList()
	if chatList != nil {
		chatList.SetTitle(conversationPaneTitle(target, time.Time{}, "syncing…")).
			SetBorder(true).
			SetTitleAlign(tview.AlignCenter)
	}

	done := make(chan struct{})
	go s.loadMessages(loadCtx, target, loadSeq, done)
}

// composerKeyHandler handles keys while the composer input has focus.
// Tab/Backtab return focus to the messages list; Backspace clears a pending
// thread reply; Escape falls through to the input field's built-in handler
// (which aborts input and returns focus).
func (s *AppState) composerKeyHandler() func(event *tcell.EventKey) *tcell.EventKey {
	return func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab, tcell.KeyBacktab:
			s.focusComponent(ViChat)
			return nil
		case tcell.KeyBackspace:
			s.clearComposerReply()
		}

		return event
	}
}
