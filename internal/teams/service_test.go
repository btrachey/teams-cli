package teams

import (
	"testing"
	"time"

	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/fossteams/teams-cli/internal/teams/teamstest"
)

func TestResolveChatTitlePrefersMeetingSubject(t *testing.T) {
	chat := csa.Chat{
		MeetingInformation: csa.MeetingInfo{Subject: "Daily Standup"},
		Members: []csa.ChatMember{
			{Mri: "self", FriendlyName: "Me"},
			{Mri: "other", FriendlyName: "Alex"},
		},
	}

	got := resolveChatTitle(chat, "self", "Me")
	if got != "Daily Standup" {
		t.Fatalf("expected meeting subject title, got %q", got)
	}
}

func TestResolveChatTitleUsesSortedParticipantNames(t *testing.T) {
	chat := csa.Chat{
		Members: []csa.ChatMember{
			{Mri: "self", FriendlyName: "Me"},
			{Mri: "b", FriendlyName: "bravo"},
			{Mri: "a", FriendlyName: "Alpha"},
			{Mri: "dup", FriendlyName: "alpha"},
		},
	}

	got := resolveChatTitle(chat, "self", "Me")
	if got != "Alpha, bravo" {
		t.Fatalf("expected sorted unique participant names, got %q", got)
	}
}

func TestResolveChatTitleAvoidsCurrentUserNameFallback(t *testing.T) {
	chat := csa.Chat{
		ChatType: "chat",
		Members: []csa.ChatMember{
			{Mri: "self"},
			{Mri: "other-a"},
			{Mri: "other-b"},
		},
		LastMessage: csa.Message{ImDisplayName: "Me"},
	}

	got := resolveChatTitle(chat, "self", "Me")
	if got != "Group chat (2 people)" {
		t.Fatalf("expected neutral fallback title, got %q", got)
	}
}

func TestSortChannelsPrefersPinnedThenGeneralThenName(t *testing.T) {
	channels := []csa.Channel{
		{Id: "random", DisplayName: "Random"},
		{Id: "general", DisplayName: "General", IsGeneral: true},
		{Id: "announcements", DisplayName: "Announcements"},
	}

	sortChannels(channels, map[string]int{"announcements": 0})

	want := []string{"announcements", "general", "random"}
	for idx, channel := range channels {
		if channel.Id != want[idx] {
			t.Fatalf("expected channel %q at position %d, got %q", want[idx], idx, channel.Id)
		}
	}
}

func TestSortChatsPrefersRecentActivity(t *testing.T) {
	chats := []csa.Chat{
		{
			Id: "older",
			Members: []csa.ChatMember{
				{Mri: "older", FriendlyName: "Older"},
			},
			LastMessage: csa.Message{ComposeTime: mustRFC3339Time(t, "2026-03-26T10:00:00Z")},
		},
		{
			Id: "newer",
			Members: []csa.ChatMember{
				{Mri: "newer", FriendlyName: "Newer"},
			},
			LastMessage: csa.Message{ComposeTime: mustRFC3339Time(t, "2026-03-27T10:00:00Z")},
		},
	}

	sortChats(chats, "", "")

	if chats[0].Id != "newer" {
		t.Fatalf("expected most recent chat first, got %q", chats[0].Id)
	}
}

func mustRFC3339Time(t *testing.T, raw string) api.RFC3339Time {
	t.Helper()

	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("unable to parse test time %q: %v", raw, err)
	}

	return api.RFC3339Time(parsed)
}

func TestNewClientRecoversFromMalformedTokenPanic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(auth.EnvName(auth.TokenTypeSkype), "not-a-jwt")
	t.Setenv(auth.EnvName(auth.TokenTypeChatSvcAgg), "also-not-a-jwt")

	// teams-api dereferences a nil JWT inside the authz exchange for
	// malformed tokens; NewClient must surface that as an error.
	client, err := NewClient()
	if err == nil {
		t.Fatalf("expected an error from malformed token input, got client %v", client)
	}
	if client != nil {
		t.Fatal("expected nil client alongside the error")
	}
}

func TestSetClientClearsCachedRegion(t *testing.T) {
	state := &Service{region: "emea"}

	state.SetClient(teamstest.NewClient(t))
	if state.region != "" {
		t.Fatalf("expected cached region to be cleared, got %q", state.region)
	}
}

func TestLink(t *testing.T) {
	state := &Service{}
	state.ApplyConversations(NewConversationData(&csa.ConversationResponse{
		Teams: []csa.Team{{Id: "team-1", Channels: []csa.Channel{{Id: "19:chan@thread.tacv2", DisplayName: "General Chat"}}}},
	}))

	cases := map[string]string{
		state.Link("19:chat@thread.v2", ""):          "https://teams.microsoft.com/l/chat/19:chat@thread.v2/0",
		state.Link("19:chan@thread.tacv2", ""):       "https://teams.microsoft.com/l/channel/19:chan@thread.tacv2/General%20Chat",
		state.Link("19:chat@thread.v2", "170000001"): "https://teams.microsoft.com/l/message/19:chat@thread.v2/170000001",
	}
	for got, want := range cases {
		if got != want {
			t.Fatalf("link = %q, want %q", got, want)
		}
	}
}
