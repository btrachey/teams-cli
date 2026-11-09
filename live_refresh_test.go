package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
)

func TestConversationPaneTitleIncludesLiveStatus(t *testing.T) {
	target := ConversationTarget{ID: "chat-1", Title: "General"}
	syncedAt := time.Date(2026, time.March, 27, 12, 34, 56, 0, time.UTC)

	got := conversationPaneTitle(target, syncedAt, "")
	if !strings.Contains(got, "General") {
		t.Fatalf("expected title to include conversation title, got %q", got)
	}
	if !strings.Contains(got, "live 12:34:56") {
		t.Fatalf("expected live timestamp in title, got %q", got)
	}
}

func TestMessagesSignatureChangesWhenMessagesChange(t *testing.T) {
	base := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>one</p>"},
	}

	changed := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>two</p>"},
	}

	if messagesSignature(base) == messagesSignature(changed) {
		t.Fatal("expected message signature to change when content changes")
	}
}

// TestBuildConversationItemsEnumeratesChannelsAndChats pins the flat list
// the bubbles list enumerates: every channel (channel name + team subtitle)
// followed by the chats.
func TestBuildConversationItemsEnumeratesChannelsAndChats(t *testing.T) {
	state := sampleConversationState()

	items := state.buildConversationItems()
	if len(items) != 3 {
		t.Fatalf("expected 3 conversations, got %d", len(items))
	}

	first, ok := items[0].(conversationItem)
	if !ok {
		t.Fatalf("expected conversationItem, got %T", items[0])
	}
	if first.target.ID != "chan-1" {
		t.Fatalf("expected the first channel target, got %q", first.target.ID)
	}
	if first.title != "General" || first.subtitle != "CyberSec" {
		t.Fatalf("expected General/CyberSec, got %q/%q", first.title, first.subtitle)
	}
	if first.target.Title != "CyberSec / General" {
		t.Fatalf("expected the pane title to carry the team, got %q", first.target.Title)
	}

	last, ok := items[2].(conversationItem)
	if !ok || last.target.ID != "chat-1" || last.title != "Incident Review" {
		t.Fatalf("expected the chat last, got %+v", items[2])
	}
}

// TestConversationListSelectionSurvivesRefresh pins that a background
// conversation refresh rebuilds the list with the cursor still on the same
// conversation.
func TestConversationListSelectionSurvivesRefresh(t *testing.T) {
	state := newUITestState()
	m := newTestModel(state)
	if !m.selectConversationByID("chat-1") {
		t.Fatal("expected to select the chat")
	}

	refreshed := &conversationData{
		conversations: &csa.ConversationResponse{
			Teams: []csa.Team{
				{
					DisplayName: "CyberSec",
					Channels: []csa.Channel{
						{Id: "chan-1", DisplayName: "General"},
						{Id: "chan-2", DisplayName: "Incidents"},
					},
				},
			},
			Chats: []csa.Chat{
				{Id: "chat-1", Title: "Incident Review Renamed"},
			},
		},
	}

	updated, _ := m.handleConversationsRefresh(treeRefreshedMsg{data: refreshed})
	m = updated.(chatModel)

	item, ok := m.selectedConversationItem()
	if !ok {
		t.Fatal("expected a selected conversation after the refresh")
	}
	if item.target.ID != "chat-1" {
		t.Fatalf("expected the chat to stay selected, got %q", item.target.ID)
	}
	if item.title != "Incident Review Renamed" {
		t.Fatalf("expected the refreshed title, got %q", item.title)
	}
}

// TestConversationListFilterValueMatchesTitleAndTeam pins that the list's
// fuzzy filter sees both the channel name and the team name.
func TestConversationListFilterValueMatchesTitleAndTeam(t *testing.T) {
	state := sampleConversationState()
	items := state.buildConversationItems()

	item, ok := items[0].(conversationItem)
	if !ok {
		t.Fatalf("expected conversationItem, got %T", items[0])
	}
	value := item.FilterValue()
	if !strings.Contains(value, "General") || !strings.Contains(value, "CyberSec") {
		t.Fatalf("expected the filter value to carry title and team, got %q", value)
	}
}

// TestBuildConversationItemsSortsByNewestMessage pins the list order: the
// conversation with the most recent message comes first, API metadata and
// locally observed activity both count.
func TestBuildConversationItemsSortsByNewestMessage(t *testing.T) {
	state := sampleConversationState()
	now := time.Now()

	state.conversations.Teams[0].Channels[0].LastMessage.ComposeTime = api.RFC3339Time(now.Add(-5 * time.Minute))
	state.conversations.Teams[0].Channels[1].LastMessage.ComposeTime = api.RFC3339Time(now.Add(-2 * time.Hour))
	state.conversations.Chats[0].LastMessage.ComposeTime = api.RFC3339Time(now.Add(-30 * time.Minute))

	ids := conversationItemIDs(state.buildConversationItems())
	want := []string{"chan-1", "chat-1", "chan-2"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v, got %v", want, ids)
	}

	// Locally seen activity (a live-refreshed message newer than the
	// cached metadata) reorders the list.
	state.noteConversationLastMessage("chan-2", now)
	ids = conversationItemIDs(state.buildConversationItems())
	want = []string{"chan-2", "chan-1", "chat-1"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("expected order %v after local activity, got %v", want, ids)
	}
}

func conversationItemIDs(items []list.Item) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if conv, ok := item.(conversationItem); ok {
			ids = append(ids, conv.target.ID)
		}
	}
	return ids
}
