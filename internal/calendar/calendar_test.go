package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fossteams/teams-cli/internal/auth"
)

func TestParseTime(t *testing.T) {
	cases := map[string]string{
		"2026-11-05T16:30:00+00:00": "2026-11-05T16:30:00Z",
		"2026-11-05T16:30:00Z":      "2026-11-05T16:30:00Z",
		"2026-11-05T16:30:00":       "2026-11-05T16:30:00Z",
	}
	for raw, want := range cases {
		got := parseTime(raw)
		if got.UTC().Format(time.RFC3339) != want {
			t.Fatalf("parseTime(%q) = %s, want %s", raw, got.UTC().Format(time.RFC3339), want)
		}
	}
	if !parseTime("garbage").IsZero() {
		t.Fatal("unparseable time must be zero")
	}
	if !parseTime("").IsZero() {
		t.Fatal("empty time must be zero")
	}
}

func TestFormatTime(t *testing.T) {
	ts := time.Date(2026, 11, 5, 16, 30, 5, 123_000_000, time.FixedZone("x", -5*3600))
	got := formatTime(ts)
	// 16:30:05 in -05:00 is 21:30:05 UTC.
	if got != "2026-11-05T21:30:05.000Z" {
		t.Fatalf("formatTime = %q", got)
	}
}

func TestPartitionIsUsable(t *testing.T) {
	ok := []string{"amer-01", "emea-02", "apac-01"}
	for _, s := range ok {
		if !partitionIsUsable(s) {
			t.Fatalf("%q should be usable", s)
		}
	}
	bad := []string{"", "  ", "amer01", "AMER-01", "amer/01", "amer 01", "..\\evil", "a"}
	for _, s := range bad {
		if partitionIsUsable(s) {
			t.Fatalf("%q should be rejected", s)
		}
	}
}

func TestSortEventsOrdersByStart(t *testing.T) {
	evs := []Event{
		{Subject: "later", Start: time.Now().Add(time.Hour)},
		{Subject: "earlier", Start: time.Now().Add(-time.Hour)},
		{Subject: "zero"},
	}
	sortEvents(evs)
	if evs[0].Subject != "earlier" || evs[1].Subject != "later" || evs[2].Subject != "zero" {
		t.Fatalf("unexpected order: %s, %s, %s", evs[0].Subject, evs[1].Subject, evs[2].Subject)
	}
}

func newAuthedClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	pointAt(t, server.URL)
	return &Client{
		HTTPClient: server.Client(),
		RootToken:  func() (string, error) { return "root-jwt", nil },
	}
}

// pointAt sends calendar and authz requests to a local server. Discovery
// goes straight to the fallback probe list unless a test overrides the seam
// itself; nothing reaches the live authz service.
func pointAt(t *testing.T, baseURL string) {
	t.Helper()
	previousFormat, previousAuthz, previousDiscovery := middleTierFormat, authzEndpoint, partitionFromAuthz
	middleTierFormat = baseURL + "/api/mt/part/%s/v2.1/me/calendars/calendarView"
	authzEndpoint = baseURL + "/api/authsvc/v1.0/authz"
	partitionFromAuthz = func(context.Context, *Client) (string, bool) { return "", false }
	t.Cleanup(func() {
		middleTierFormat, authzEndpoint, partitionFromAuthz = previousFormat, previousAuthz, previousDiscovery
	})
}

func TestRequestUsesRootTokenAndClassifiesAuthFailure(t *testing.T) {
	var gotAuth, gotPath string
	client := newAuthedClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value":[]}`)
	})

	events, err := client.request(context.Background(), "amer-01", Query{
		Start: time.Now(), End: time.Now().Add(time.Hour), Limit: 5,
	})
	if err != nil || len(events) != 0 {
		t.Fatalf("expected empty success, got %d events, err %v", len(events), err)
	}
	if gotAuth != "Bearer root-jwt" {
		t.Fatalf("middle tier must get the root skype token as a raw bearer, got %q", gotAuth)
	}
	if !strings.Contains(gotPath, "/api/mt/part/amer-01/v2.1/me/calendars/calendarView") {
		t.Fatalf("unexpected endpoint path %q", gotPath)
	}

	authErr := &ResponseError{StatusCode: 401, Status: "401 Unauthorized"}
	if !auth.IsAuthFailure(authErr) {
		t.Fatalf("a 401 ResponseError must classify as an auth failure, message %q", authErr.Error())
	}
	forbidden := &ResponseError{StatusCode: 403, Status: "403 Forbidden"}
	if !auth.IsAuthFailure(forbidden) {
		t.Fatalf("a 403 ResponseError must classify as an auth failure, message %q", forbidden.Error())
	}
	notFound := &ResponseError{StatusCode: 404, Status: "404 Not Found"}
	if auth.IsAuthFailure(notFound) {
		t.Fatal("404 must be a partition miss, 401 must not")
	}
}

func TestDiscoverySkipsAuthRejectedPartition(t *testing.T) {
	var requests int32
	client := newAuthedClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.Error(w, "denied", http.StatusUnauthorized)
	})

	if _, err := client.discoverPartition(context.Background()); err == nil {
		t.Fatal("every partition rejecting auth must end discovery with an error")
	}
	if got := atomic.LoadInt32(&requests); got != int32(len(partitionFallbacks)) {
		t.Fatalf("a 401 must not be cached as usable: expected every fallback probed (%d), got %d",
			len(partitionFallbacks), got)
	}
	if client.region.segment != "" {
		t.Fatalf("auth-rejected partition must never be cached, cached %q", client.region.segment)
	}
}

// TestEventsRefreshesAfterDiscoveryAuthFailure pins the chain the
// middle tier's token behavior demands: when every discovery probe rejects
// the (expired) token, discovery must surface a classifiable auth error so
// the fetch performs its token refresh and retries — and it must do the
// refresh exactly once.
// Without this, the refresh-and-retry branch is unreachable for the most
// common failure (expired token), and the live smoke would fail after
// arming the shared failure cooldown that also gates the chat path.
func TestEventsRefreshesAfterDiscoveryAuthFailure(t *testing.T) {
	var refreshes int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer stale-jwt" {
			http.Error(w, "expired", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value":[{"subject":"Recovered","startTime":"2026-11-05T10:00:00+00:00","endTime":"2026-11-05T11:00:00+00:00"}]}`)
	}))
	t.Cleanup(server.Close)

	pointAt(t, server.URL)

	client := &Client{
		HTTPClient: server.Client(),
		RootToken: func() (string, error) {
			if atomic.LoadInt32(&refreshes) == 0 {
				return "stale-jwt", nil
			}
			return "fresh-jwt", nil
		},
		Reauth: func(ctx context.Context, _ error) bool {
			// The refresh must run on the uncapped parent context: the
			// attempt timeout belongs to the network attempts only.
			// Hoisting WithTimeout(attemptTimeout) above Reauth — a
			// plausible "bound the whole fetch" refactor — would clip the
			// Electron flow (~16s) at 12s and arm the failure cooldown that
			// also gates the chat path, and it passes every other
			// assertion.
			if _, hasDeadline := ctx.Deadline(); hasDeadline {
				t.Error("token refresh received a deadline-bound context")
				return false
			}
			atomic.AddInt32(&refreshes, 1)
			return true
		},
	}

	events, err := client.Events(context.Background(), Query{
		Start: time.Now(), End: time.Now().Add(time.Hour), Limit: 10,
	})
	if err != nil {
		t.Fatalf("fetch after refresh must succeed: %v", err)
	}
	if len(events) != 1 || events[0].Subject != "Recovered" {
		t.Fatalf("expected the post-refresh event set, got %+v", events)
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("expected exactly one token refresh, got %d", got)
	}

	// With refresh disabled, the discovery failure must surface as a
	// still-classifiable auth error (the reactive chat refresh reads the
	// same classification off this error).
	client.Reauth = nil
	client.region.segment = ""
	client.RootToken = func() (string, error) { return "stale-jwt", nil }
	_, err = client.Events(context.Background(), Query{
		Start: time.Now(), End: time.Now().Add(time.Hour), Limit: 10,
	})
	if err == nil {
		t.Fatal("a permanently rejected token must surface an error")
	}
	if !auth.IsAuthFailure(err) {
		t.Fatalf("the surfaced discovery failure must classify as auth, got %v", err)
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("a disabled refresh must not be invoked, refreshes=%d", got)
	}
}

func TestDiscoveryCachesFirstUsablePartition(t *testing.T) {
	var requests int32
	client := newAuthedClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if strings.Contains(r.URL.Path, "emea-02") {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"value":[]}`)
			return
		}
		http.Error(w, "nope", http.StatusNotFound)
	})
	segment, err := client.discoverPartition(context.Background())
	if err != nil || segment != "emea-02" {
		t.Fatalf("expected emea-02, got %q err %v", segment, err)
	}
	first := atomic.LoadInt32(&requests)
	if again, err := client.discoverPartition(context.Background()); err != nil || again != "emea-02" {
		t.Fatalf("cached segment must be reused: got %q err %v", again, err)
	}
	if second := atomic.LoadInt32(&requests); second != first {
		t.Fatalf("the second discovery must hit the cache, but %d more requests were made", second-first)
	}
}
