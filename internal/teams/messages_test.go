package teams

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/teams/teamstest"
)

func TestFetchMessagesRetriesAndReturnsLatestMessages(t *testing.T) {
	var attempts int32

	state := &Service{
		client: teamstest.NewClient(t),
		HTTPClient: &http.Client{
			Transport: teamstest.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&attempts, 1)

				if got := req.URL.Query().Get("pageSize"); got != "2" {
					t.Fatalf("expected pageSize=2, got %q", got)
				}
				if got := req.Header.Get("Authentication"); !strings.HasPrefix(got, "skypetoken=") {
					t.Fatalf("expected Skype auth header, got %q", got)
				}

				if atomic.LoadInt32(&attempts) == 1 {
					return teamstest.Response(http.StatusTooManyRequests, "slow down"), nil
				}

				return teamstest.Response(http.StatusOK, teamstest.MessagesJSON(t, []csa.ChatMessage{
					teamstest.ChatMessage("message-3", "Carol", "third", time.Date(2026, time.March, 27, 9, 3, 0, 0, time.UTC)),
					teamstest.ChatMessage("message-1", "Alice", "first", time.Date(2026, time.March, 27, 9, 1, 0, 0, time.UTC)),
					teamstest.ChatMessage("message-2", "Bob", "second", time.Date(2026, time.March, 27, 9, 2, 0, 0, time.UTC)),
				})), nil
			}),
		},
	}

	messages, err := state.FetchMessages(context.Background(), Conversation{
		ID:    "19:chat-thread",
		Title: "Incident Review",
	}, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("expected 2 attempts, got %d", got)
	}
	if len(messages) != 2 {
		t.Fatalf("expected the latest 2 messages, got %d", len(messages))
	}
	if messages[0].Id != "message-2" || messages[1].Id != "message-3" {
		t.Fatalf("expected sorted trailing messages, got %q then %q", messages[0].Id, messages[1].Id)
	}
}

func TestFetchMessagesMalformedPayloadDoesNotRetry(t *testing.T) {
	var attempts int32

	state := &Service{
		client: teamstest.NewClient(t),
		HTTPClient: &http.Client{
			Transport: teamstest.RoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				atomic.AddInt32(&attempts, 1)
				return teamstest.Response(http.StatusOK, `{"messages":"bad-payload"}`), nil
			}),
		},
	}

	_, err := state.FetchMessages(context.Background(), Conversation{
		ID:    "19:bad-chat",
		Title: "Broken Payload",
	}, 5)
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if !strings.Contains(err.Error(), "unable to decode messages") {
		t.Fatalf("expected decode guidance, got %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("expected malformed payload to stop after 1 attempt, got %d", got)
	}
}

func TestWaitForMessageFetchRetryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	startedAt := time.Now()
	err := waitForMessageFetchRetry(ctx, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 50*time.Millisecond {
		t.Fatalf("expected cancellation to return quickly, got %s", elapsed)
	}
}

func TestShouldRetryMessageFetchStatusErrors(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "rate limited",
			err: &FetchError{
				StatusCode: http.StatusTooManyRequests,
				Status:     "429 Too Many Requests",
			},
			want: true,
		},
		{
			name: "server error",
			err: &FetchError{
				StatusCode: http.StatusBadGateway,
				Status:     "502 Bad Gateway",
			},
			want: true,
		},
		{
			name: "client error",
			err: &FetchError{
				StatusCode: http.StatusBadRequest,
				Status:     "400 Bad Request",
			},
			want: false,
		},
		{
			name: "cancelled",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "deadline exceeded",
			err:  context.DeadlineExceeded,
			want: true,
		},
	}

	for _, tc := range testCases {
		if got := shouldRetryMessageFetch(tc.err); got != tc.want {
			t.Fatalf("%s: expected retry=%v, got %v", tc.name, tc.want, got)
		}
	}
}

func TestMessageFetchBackoffCapsAtMaximum(t *testing.T) {
	if got := messageFetchBackoff(1); got != fetchRetryBaseDelay {
		t.Fatalf("expected first backoff %v, got %v", fetchRetryBaseDelay, got)
	}

	got := messageFetchBackoff(10)
	if got != fetchRetryMaxDelay {
		t.Fatalf("expected backoff to cap at %v, got %v", fetchRetryMaxDelay, got)
	}
}

func TestFetchMessagesDoesNotRefreshOnTransientErrors(t *testing.T) {
	var calls int32
	var refreshes int32

	state := &Service{
		HTTPClient: &http.Client{
			Transport: teamstest.RoundTripperFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return teamstest.Response(503, "unavailable"), nil
			}),
		},
	}
	state.SetClient(teamstest.NewClient(t))
	state.Reauth = func(context.Context, error) bool {
		atomic.AddInt32(&refreshes, 1)
		return true
	}

	if _, err := state.FetchMessages(context.Background(), Conversation{ID: "19:chat-1"}, 3); err == nil {
		t.Fatal("expected fetch to fail")
	}
	if got := atomic.LoadInt32(&refreshes); got != 0 {
		t.Fatalf("expected no refresh on 503, got %d", got)
	}
	if got := atomic.LoadInt32(&calls); got != int32(fetchMaxAttempts) {
		t.Fatalf("expected %d HTTP attempts, got %d", fetchMaxAttempts, got)
	}
}
