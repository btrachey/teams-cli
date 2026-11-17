package tui

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	teams_api "github.com/fossteams/teams-api"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/fossteams/teams-cli/internal/teams"
	"github.com/fossteams/teams-cli/internal/teams/teamstest"
)

func TestMaybeRefreshTokensRebuildsOnAuthFailure(t *testing.T) {
	original := teamstest.NewClient(t)
	rebuilt := teamstest.NewClient(t)

	state := &AppState{
		tokens: auth.Refresher{Enabled: true},
	}
	state.teams.SetClient(original)
	state.refreshTokensOverride = func(context.Context) error { return nil }
	state.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) { return rebuilt, nil }

	if !state.maybeRefreshTokens(context.Background(), &teams.FetchError{StatusCode: 401}) {
		t.Fatal("expected refresh on 401")
	}
	if state.teams.Client() != rebuilt {
		t.Fatal("expected teams client to be rebuilt")
	}

	state2 := &AppState{tokens: auth.Refresher{Enabled: true}}
	state2.teams.SetClient(original)
	state2.refreshTokensOverride = func(context.Context) error { return nil }
	state2.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) { return rebuilt, nil }
	if state2.maybeRefreshTokens(context.Background(), &teams.FetchError{StatusCode: 503}) {
		t.Fatal("expected no refresh on non-auth failure")
	}
	if state2.teams.Client() != original {
		t.Fatal("expected teams client to stay in place")
	}

	state3 := &AppState{tokens: auth.Refresher{Enabled: false}}
	if state3.maybeRefreshTokens(context.Background(), nil) {
		t.Fatal("expected disabled refresh to be a no-op")
	}
}

func TestFetchMessagesRefreshesTokensAndRetriesOn401(t *testing.T) {
	var calls int32
	var refreshes int32

	state := &AppState{
		messageLimit: 3,
		tokens:       auth.Refresher{Enabled: true},
		teams: teams.Service{HTTPClient: &http.Client{
			Transport: teamstest.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				if atomic.AddInt32(&calls, 1) == 1 {
					return teamstest.Response(401, "token expired"), nil
				}
				messages := []csa.ChatMessage{teamstest.ChatMessage("1", "author", "hello", time.Now())}
				return teamstest.Response(200, teamstest.MessagesJSON(t, messages)), nil
			}),
		}},
	}
	state.initRuntime(context.Background())
	state.teams.SetClient(teamstest.NewClient(t))
	state.refreshTokensOverride = func(context.Context) error {
		atomic.AddInt32(&refreshes, 1)
		return nil
	}
	state.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) {
		return teamstest.NewClient(t), nil
	}

	messages, err := state.fetchMessages(context.Background(), teams.Conversation{ID: "19:chat-1"})
	if err != nil {
		t.Fatalf("expected fetch to succeed after refresh: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("expected exactly one refresh, got %d", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly two HTTP attempts, got %d", got)
	}
}
