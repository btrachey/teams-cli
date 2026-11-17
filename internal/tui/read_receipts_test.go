package tui

import (
	"context"
	"errors"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/teams"
)

// receiptState is a messages-tab state with read receipts on and a
// recording MarkRead.
func receiptState(t *testing.T) (*AppState, *[]string) {
	t.Helper()
	state := newUITestState()
	state.ui.page = pageMain
	state.readReceipts = true
	var sent []string
	state.markReadOverride = func(_ context.Context, id string, newest csa.ChatMessage) error {
		sent = append(sent, id+"@"+newest.Id)
		return nil
	}
	return state, &sent
}

var receiptMessages = []csa.ChatMessage{
	{Id: "100", ImDisplayName: "A", Content: "<p>old</p>"},
	{Id: "200", ImDisplayName: "B", Content: "<p>new</p>"},
}

// deliverReceipt runs a due receipt through the model and its command.
func deliverReceipt(t *testing.T, m chatModel, due readReceiptDueMsg) chatModel {
	t.Helper()
	updated, cmd := m.Update(due)
	m = updated.(chatModel)
	if cmd != nil {
		if result := cmd(); result != nil {
			updated, _ = m.Update(result)
			m = updated.(chatModel)
		}
	}
	return m
}

// TestReadReceiptAfterDwell pins the receipt: the newest message of the
// conversation still on screen, sent once.
func TestReadReceiptAfterDwell(t *testing.T) {
	state, sent := receiptState(t)
	target := teams.Conversation{ID: "19:a", Title: "A"}
	state.setCurrentConversation(target)
	m := newChatModel(state)

	if cmd := state.scheduleReadReceipt(target.ID, receiptMessages); cmd == nil {
		t.Fatal("a newly shown conversation must arm a receipt")
	}
	due := readReceiptDueMsg{conversationID: target.ID, newest: receiptMessages[1]}
	m = deliverReceipt(t, m, due)
	if len(*sent) != 1 || (*sent)[0] != "19:a@200" {
		t.Fatalf("expected one receipt for the newest message, got %v", *sent)
	}

	deliverReceipt(t, m, due)
	if state.scheduleReadReceipt(target.ID, receiptMessages) != nil || len(*sent) != 1 {
		t.Fatalf("an already receipted message must not be sent again, got %v", *sent)
	}
}

// TestReadReceiptSkippedAfterMovingOn pins the dwell: a conversation the
// cursor merely passed over, or one hidden behind the calendar or a
// collapsed preview, is not marked read.
func TestReadReceiptSkippedAfterMovingOn(t *testing.T) {
	state, sent := receiptState(t)
	m := newChatModel(state)
	due := readReceiptDueMsg{conversationID: "19:a", newest: receiptMessages[1]}

	state.setCurrentConversation(teams.Conversation{ID: "19:b"})
	m = deliverReceipt(t, m, due)

	state.setCurrentConversation(teams.Conversation{ID: "19:a"})
	state.ui.page = pageCalendar
	m = deliverReceipt(t, m, due)

	state.ui.page = pageMain
	state.ui.previewHidden = true
	deliverReceipt(t, m, due)

	if len(*sent) != 0 {
		t.Fatalf("no receipt may be sent for a conversation that is not on screen, got %v", *sent)
	}
}

// TestReadReceiptRetriesAfterFailure pins that a failed receipt is
// forgotten so the next view sends it again.
func TestReadReceiptRetriesAfterFailure(t *testing.T) {
	state, _ := receiptState(t)
	state.markReadOverride = func(context.Context, string, csa.ChatMessage) error { return errors.New("boom") }
	target := teams.Conversation{ID: "19:a"}
	state.setCurrentConversation(target)

	deliverReceipt(t, newChatModel(state), readReceiptDueMsg{conversationID: target.ID, newest: receiptMessages[1]})
	if state.scheduleReadReceipt(target.ID, receiptMessages) == nil {
		t.Fatal("a failed receipt must be re-armed on the next view")
	}
}

// TestReadReceiptsOff pins --no-read-receipts.
func TestReadReceiptsOff(t *testing.T) {
	state, _ := receiptState(t)
	state.readReceipts = false
	if state.scheduleReadReceipt("19:a", receiptMessages) != nil {
		t.Fatal("receipts must not be armed when disabled")
	}
}

// TestLoadingAConversationArmsReceipt pins the wiring: a message load
// schedules the receipt.
func TestLoadingAConversationArmsReceipt(t *testing.T) {
	state, _ := receiptState(t)
	target := teams.Conversation{ID: "19:a"}
	state.setCurrentConversation(target)
	m := newChatModel(state)

	_, cmd := m.Update(messagesLoadedMsg{target: target, messages: receiptMessages, syncedAt: time.Now(), loadSeq: state.loadSeq})
	due, ok := findMsg[readReceiptDueMsg](cmd)
	if !ok {
		t.Fatal("the load must arm a receipt for the newest message")
	}
	if due.conversationID != target.ID || due.newest.Id != "200" {
		t.Fatalf("unexpected receipt %+v", due)
	}
}

// findMsg runs cmd (descending into batches) and returns the first message
// of type T it produces. Timers run to completion.
func findMsg[T any](cmd tea.Cmd) (T, bool) {
	var zero T
	if cmd == nil {
		return zero, false
	}
	switch msg := cmd().(type) {
	case T:
		return msg, true
	case tea.BatchMsg:
		for _, c := range msg {
			if found, ok := findMsg[T](c); ok {
				return found, true
			}
		}
	}
	return zero, false
}
