package tui

import (
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
)

// sampleConversationState seeds an AppState with a small conversation set:
// a team with two channels, an empty team, and one chat.
func sampleConversationState() *AppState {
	state := &AppState{logger: logging.Discard}
	state.teams.ApplyConversations(teams.NewConversationData(&csa.ConversationResponse{
		Teams: []csa.Team{
			{
				DisplayName: "CyberSec",
				Channels: []csa.Channel{
					{Id: "chan-1", DisplayName: "General"},
					{Id: "chan-2", DisplayName: "Incidents"},
				},
			},
			{DisplayName: "Empty Team"},
		},
		Chats: []csa.Chat{
			{Id: "chat-1", Title: "Incident Review"},
		},
	}))
	return state
}

// makeMessagesFocusable gives the messages pane something to show, which is
// what lets it take focus.
func makeMessagesFocusable(state *AppState) {
	if len(state.ui.msgs.entries) == 0 {
		state.ui.msgs.entries = []messageEntry{{Main: noMessagesMain}}
	}
}

// newTestModel builds a model with the fixture's conversations already in
// the list.
func newTestModel(state *AppState) chatModel {
	m := newChatModel(state)
	m.conversations.SetItems(state.buildConversationItems())
	return m
}

// seedChannel makes id a known channel (a one-channel conversation tree).
func seedChannel(state *AppState, id string) {
	state.teams.ApplyConversations(teams.NewConversationData(&csa.ConversationResponse{
		Teams: []csa.Team{{Id: "team-1", Channels: []csa.Channel{{Id: id}}}},
	}))
}

// testChannel returns the installed channel so tests can change its state.
func testChannel(state *AppState, id string) *csa.Channel {
	ch, _ := state.teams.Channel(id)
	return ch
}
