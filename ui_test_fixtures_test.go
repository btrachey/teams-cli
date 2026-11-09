package main

import (
	"github.com/fossteams/teams-api/pkg/csa"
)

// sampleConversationState seeds an AppState with a small conversation set:
// a team with two channels, an empty team, and one chat.
func sampleConversationState() *AppState {
	return &AppState{
		TeamsState: TeamsState{
			conversations: &csa.ConversationResponse{
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
			},
		},
		logger: discardLogger,
	}
}

// newTestModel builds a model with the fixture's conversations already in
// the list.
func newTestModel(state *AppState) chatModel {
	m := newChatModel(state)
	m.conversations.SetItems(state.buildConversationItems())
	return m
}
