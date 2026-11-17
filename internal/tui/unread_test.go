package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/lipgloss"
	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
)

// unreadFixtureState seeds one team (read + unread + never-posted channels)
// and chats covering every arm of the unread rule: server-flagged unread,
// read, unread-but-my-last-message, and unread-but-no-content. The live
// middle tier flags never-posted channels unread too (observed:
// isMessageRead=false with threadVersion=-1), so those must not dot.
func unreadFixtureState() *AppState {
	recent := api.RFC3339Time(time.Now().Add(-time.Hour))
	team := csa.Team{
		DisplayName: "CyberSec",
		Channels: []csa.Channel{
			{Id: "chan-read", DisplayName: "General", IsMessageRead: true, ThreadVersion: 100},
			{Id: "chan-unread", DisplayName: "Incidents", IsMessageRead: false, ThreadVersion: 200,
				LastMessage: csa.Message{Id: "m-200", ComposeTime: recent}},
			{Id: "chan-empty", DisplayName: "Archive", IsMessageRead: false, ThreadVersion: -1},
		},
	}
	state := &AppState{logger: logging.Discard}
	state.teams.ApplyConversations(teams.NewConversationData(&csa.ConversationResponse{
		Teams: []csa.Team{team},
		Chats: []csa.Chat{
			{Id: "chat-unread", Title: "Alice", IsRead: false, ThreadVersion: 300,
				LastMessage: csa.Message{Id: "m-300", ComposeTime: recent}},
			{Id: "chat-read", Title: "Bob", IsRead: true, ThreadVersion: 400},
			{Id: "chat-mine", Title: "Note to self", IsRead: false, IsLastMessageFromMe: true, ThreadVersion: 500,
				LastMessage: csa.Message{Id: "m-500", ComposeTime: recent}},
			{Id: "chat-nomsg", Title: "Empty group", IsRead: false, ThreadVersion: -1},
		},
	}))
	return state
}

// deliverMessagesLoaded runs the production messagesLoadedMsg handler for
// target, mirroring what the fetch goroutine delivers (loadSeq bumped first
// so the stale guard accepts it).
func deliverMessagesLoaded(t *testing.T, m *chatModel, target teams.Conversation, messages []csa.ChatMessage) {
	t.Helper()
	loadSeq := atomic.AddUint64(&m.state.loadSeq, 1)
	updated, _ := m.Update(messagesLoadedMsg{
		target:   target,
		messages: messages,
		syncedAt: time.Now(),
		loadSeq:  loadSeq,
	})
	*m = updated.(chatModel)
}

func unreadDots(state *AppState) map[string]bool {
	dots := map[string]bool{}
	for _, item := range state.buildConversationItems() {
		conv := item.(conversationItem)
		dots[conv.target.ID] = conv.unread
	}
	return dots
}

// renderInto paints one row through the delegate and returns the raw
// styled output (ANSI intact; assertions look for visible runes which
// survive styling because the dot is composed before the style wraps).
func renderInto(t *testing.T, d conversationDelegate, m list.Model, index int) string {
	t.Helper()
	var b strings.Builder
	d.Render(&b, m, index, m.VisibleItems()[index])
	return b.String()
}

// TestUnreadDotsFollowServerFlags pins which rows carry the dot: server
// unread flags drive it; read conversations, my-own-last-message chats, and
// never-posted conversations never do.
func TestUnreadDotsFollowServerFlags(t *testing.T) {
	state := unreadFixtureState()

	dots := unreadDots(state)
	want := map[string]bool{
		"chan-read":   false,
		"chan-unread": true,
		"chan-empty":  false,
		"chat-unread": true,
		"chat-read":   false,
		"chat-mine":   false,
		"chat-nomsg":  false,
	}
	for id, expected := range want {
		got, ok := dots[id]
		if !ok {
			t.Fatalf("conversation %q missing from the list", id)
		}
		if got != expected {
			t.Fatalf("unread(%q) = %v, want %v", id, got, expected)
		}
	}

	if count := unreadCount(state); count != 2 {
		t.Fatalf("unread count = %d, want 2", count)
	}
}

// TestUnreadDotClearsOnOpenAndSurvivesRefresh pins the acknowledgment:
// rendering the conversation's messages clears the dot, and a background
// tree refresh that still reports the stale server flag for the SAME thread
// version must not bring it back.
func TestUnreadDotClearsOnOpenAndSurvivesRefresh(t *testing.T) {
	state := unreadFixtureState()
	state.initRuntime(context.Background())
	m := newChatModel(state)
	m.width, m.height = 120, 32

	target := teams.Conversation{ID: "chan-unread", Title: "CyberSec / Incidents"}
	m.state.setCurrentConversation(target)
	deliverMessagesLoaded(t, &m, target, []csa.ChatMessage{
		{Id: "m-200", ComposeTime: api.RFC3339Time(time.Now().Add(-time.Hour)), Content: "<p>hello</p>"},
	})

	if dots := unreadDots(state); dots["chan-unread"] {
		t.Fatal("opening must clear the dot")
	}
	if !state.readAck.acknowledged("chan-unread", 200, time.Time{}) {
		t.Fatal("opening must watermark the current thread version")
	}
	if count := unreadCount(state); count != 1 {
		t.Fatalf("count after open = %d, want 1 (only chat-unread left)", count)
	}

	// Tree refresh: the server flag is still false for the same version
	// (its read-processing lags). The dot must stay off.
	updated, _ := m.handleConversationsRefresh(treeRefreshedMsg{data: teams.NewConversationData(state.teams.Conversations())})
	_ = updated.(chatModel)
	if dots := unreadDots(state); dots["chan-unread"] {
		t.Fatal("tree refresh must not re-arm an acknowledged dot")
	}
}

// TestUnreadDotReturnsForGenuinelyNewerActivity pins the other edge: the
// ack must never silence real new messages. A newer thread version re-arms
// the dot; without a usable version, activity newer than the newest
// rendered message does.
func TestUnreadDotReturnsForGenuinelyNewerActivity(t *testing.T) {
	state := unreadFixtureState()

	// Versioned channel: quiet after the ack…
	base := *testChannel(state, "chan-unread")
	state.noteConversationOpened("chan-unread", time.Time{})
	if state.isChannelUnread(base) {
		t.Fatal("acknowledged channel must read quiet")
	}
	// …and re-armed when the server reports a genuinely new message: both
	// the thread version and the last-message time advance (a version bump
	// alone — an edit or deletion at the same compose time — is not new
	// content and must stay quiet).
	newer := base
	newer.ThreadVersion = 250
	newer.LastMessage = csa.Message{Id: "m-250", ComposeTime: api.RFC3339Time(time.Now())}
	if !state.isChannelUnread(newer) {
		t.Fatal("a newer thread with newer activity must re-arm the dot")
	}

	// Unversioned chat: the live payload carries threadVersion -1 whenever
	// the thread has never posted; a legacy 0 behaves the same. The time
	// watermark alone decides.
	recent := api.RFC3339Time(time.Now().Add(-time.Hour))
	chat := state.teams.Conversations().Chats[0] // chat-unread
	state.noteConversationOpened(chat.Id, time.Time(recent))
	if state.isChatUnread(chat) {
		t.Fatal("opening a chat must clear its dot")
	}
	chat.ThreadVersion = -1
	chat.LastMessage.ComposeTime = api.RFC3339Time(time.Now())
	if !state.isChatUnread(chat) {
		t.Fatal("activity newer than the newest seen message must re-arm the dot")
	}
}

// TestUnreadDotClearsWhileConversationIsOpen pins the live-arrival case
// the dual watermark exists for: new messages land in the conversation the
// user is watching; the later tree refresh reports a thread version the
// live fetch never learned about, with activity matching what the user
// already saw — the dot must stay off.
func TestUnreadDotClearsWhileConversationIsOpen(t *testing.T) {
	state := unreadFixtureState()
	state.initRuntime(context.Background())
	m := newChatModel(state)
	m.width, m.height = 120, 32

	target := teams.Conversation{ID: "chan-unread", Title: "CyberSec / Incidents"}
	m.state.setCurrentConversation(target)

	arrived := time.Now().Add(-2 * time.Minute)
	updated, _ := m.Update(liveMessagesMsg{
		target:            target,
		messages:          []csa.ChatMessage{{Id: "m-201", ComposeTime: api.RFC3339Time(arrived), Content: "<p>live</p>"}},
		syncedAt:          time.Now(),
		previousSignature: messagesSignature(nil),
	})
	m = updated.(chatModel)

	// Tree refresh after the live arrival: version bumped past anything the
	// live fetch knew (200 -> 210), activity at the arrival time the user
	// already read.
	testChannel(state, "chan-unread").ThreadVersion = 210
	testChannel(state, "chan-unread").LastMessage.ComposeTime = api.RFC3339Time(arrived)
	if state.isChannelUnread(*testChannel(state, "chan-unread")) {
		t.Fatal("watching a live-updated conversation must keep the dot off")
	}

	// Truly newer (activity past the newest rendered) re-arms even while
	// open.
	testChannel(state, "chan-unread").LastMessage.ComposeTime = api.RFC3339Time(time.Now())
	if !state.isChannelUnread(*testChannel(state, "chan-unread")) {
		t.Fatal("activity newer than anything rendered must re-arm the dot")
	}
}

// TestConversationDelegateRendersUnreadDot pins what the user actually
// sees: the dot paints for unread rows only, survives the selected-row
// styling, and never breaks the row width.
func TestConversationDelegateRendersUnreadDot(t *testing.T) {
	state := unreadFixtureState()

	m := newConversationList()
	m.SetSize(40, 10)
	m.SetItems(state.buildConversationItems())

	delegate := newConversationDelegate()
	indexOf := func(title string) int {
		for i, item := range m.VisibleItems() {
			if item.(conversationItem).title == title {
				return i
			}
		}
		t.Fatalf("row %q missing", title)
		return -1
	}

	unreadRow := renderInto(t, delegate, m, indexOf("Incidents"))
	if !strings.Contains(unreadRow, "●") {
		t.Fatalf("unread row must paint the dot, got %q", unreadRow)
	}
	if !strings.Contains(unreadRow, "Incidents") {
		t.Fatalf("unread row must keep its title, got %q", unreadRow)
	}

	readRow := renderInto(t, delegate, m, indexOf("General"))
	if strings.Contains(readRow, "●") {
		t.Fatalf("read row must not paint a dot, got %q", readRow)
	}

	m.Select(indexOf("Incidents"))
	selected := renderInto(t, delegate, m, indexOf("Incidents"))
	if !strings.Contains(selected, "●") || !strings.Contains(selected, "Incidents") {
		t.Fatalf("selected unread row must keep dot and title, got %q", selected)
	}

	narrow := newConversationList()
	narrow.SetSize(12, 10)
	narrow.SetItems(state.buildConversationItems())
	line0 := strings.Split(renderInto(t, delegate, narrow, indexOf("Incidents")), "\n")[0]
	if width := lipgloss.Width(line0); width > 12 {
		t.Fatalf("row overflowed the list width: %q (%d)", line0, width)
	}
}

// TestConversationPaneTitleCountsUnread pins the aggregate readout,
// including the no-unread case.
func TestConversationPaneTitleCountsUnread(t *testing.T) {
	state := unreadFixtureState()
	if got := conversationsPaneTitle(state.buildConversationItems()); !strings.Contains(got, "Conversations") || !strings.Contains(got, "2 unread") {
		t.Fatalf("expected unread count in title, got %q", got)
	}

	state.teams.Conversations().Chats[0].IsRead = true
	testChannel(state, "chan-unread").IsMessageRead = true
	if got := conversationsPaneTitle(state.buildConversationItems()); got != "Conversations" {
		t.Fatalf("unread-free title must be plain, got %q", got)
	}
}

// unreadCount is how many listed conversations carry the unread dot.
func unreadCount(state *AppState) int {
	count := 0
	for _, item := range state.buildConversationItems() {
		if item.(conversationItem).unread {
			count++
		}
	}
	return count
}
