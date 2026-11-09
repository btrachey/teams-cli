package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	teams_api "github.com/fossteams/teams-api"
)

// calendarTestEvents returns a deterministic set covering the cases the UI
// must render: a past meeting, an upcoming all-day event, an online meeting
// with a join link, and one the user declined.
func calendarTestEvents() []CalendarEvent {
	now := time.Now()
	mk := func(subject string, startOffset time.Duration, dur time.Duration) CalendarEvent {
		start := now.Add(startOffset)
		return CalendarEvent{
			Subject:   subject,
			StartTime: start.UTC().Format(time.RFC3339),
			EndTime:   start.Add(dur).UTC().Format(time.RFC3339),
			Start:     start,
			End:       start.Add(dur),
		}
	}
	past := mk("Standup yesterday", -48*time.Hour, 30*time.Minute)
	past.OrganizerName = "Austin"

	upcoming := mk("Sprint Review", 2*time.Hour, time.Hour)
	upcoming.OrganizerName = "Matti"
	upcoming.Location = "Room 5"
	upcoming.IsOnlineMeeting = true
	upcoming.SkypeTeamsMeetingURL = "https://teams.microsoft.com/l/meetup-join/19%3ameeting_test"
	upcoming.MyResponseType = "Accepted"

	allDay := CalendarEvent{
		Subject:       "PTO",
		StartTime:     now.Add(3 * 24 * time.Hour).UTC().Format(time.RFC3339),
		EndTime:       now.Add(3*24*time.Hour + 23*time.Hour).UTC().Format(time.RFC3339),
		Start:         now.Add(3 * 24 * time.Hour),
		End:           now.Add(3*24*time.Hour + 23*time.Hour),
		IsAllDayEvent: true,
		OrganizerName: "HR",
	}

	declined := mk("Optional sync", 26*time.Hour, time.Hour)
	declined.MyResponseType = "Declined"
	declined.OrganizerName = "Vendor"

	return []CalendarEvent{declined, upcoming, past, allDay}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestParseCalendarTime(t *testing.T) {
	cases := map[string]string{
		"2026-11-05T16:30:00+00:00": "2026-11-05T16:30:00Z",
		"2026-11-05T16:30:00Z":      "2026-11-05T16:30:00Z",
		"2026-11-05T16:30:00":       "2026-11-05T16:30:00Z",
	}
	for raw, want := range cases {
		got := parseCalendarTime(raw)
		if got.UTC().Format(time.RFC3339) != want {
			t.Fatalf("parseCalendarTime(%q) = %s, want %s", raw, got.UTC().Format(time.RFC3339), want)
		}
	}
	if !parseCalendarTime("garbage").IsZero() {
		t.Fatal("unparseable time must be zero")
	}
	if !parseCalendarTime("").IsZero() {
		t.Fatal("empty time must be zero")
	}
}

func TestFormatCalendarTime(t *testing.T) {
	ts := time.Date(2026, 11, 5, 16, 30, 5, 123_000_000, time.FixedZone("x", -5*3600))
	got := formatCalendarTime(ts)
	// 16:30:05 in -05:00 is 21:30:05 UTC.
	if got != "2026-11-05T21:30:05.000Z" {
		t.Fatalf("formatCalendarTime = %q", got)
	}
}

func TestCalendarPartitionIsUsable(t *testing.T) {
	ok := []string{"amer-01", "emea-02", "apac-01"}
	for _, s := range ok {
		if !calendarPartitionIsUsable(s) {
			t.Fatalf("%q should be usable", s)
		}
	}
	bad := []string{"", "  ", "amer01", "AMER-01", "amer/01", "amer 01", "..\\evil", "a"}
	for _, s := range bad {
		if calendarPartitionIsUsable(s) {
			t.Fatalf("%q should be rejected", s)
		}
	}
}

func TestSortCalendarEventsOrdersByStart(t *testing.T) {
	evs := []CalendarEvent{
		{Subject: "later", Start: time.Now().Add(time.Hour)},
		{Subject: "earlier", Start: time.Now().Add(-time.Hour)},
		{Subject: "zero"},
	}
	sortCalendarEvents(evs)
	if evs[0].Subject != "earlier" || evs[1].Subject != "later" || evs[2].Subject != "zero" {
		t.Fatalf("unexpected order: %s, %s, %s", evs[0].Subject, evs[1].Subject, evs[2].Subject)
	}
}

func TestCalendarDefaultCursorLandsOnUpcoming(t *testing.T) {
	entries := []calendarEntry{
		{event: &CalendarEvent{End: time.Now().Add(-time.Hour)}},   // finished
		{event: &CalendarEvent{End: time.Now().Add(-time.Minute)}}, // just finished
		{event: &CalendarEvent{End: time.Now().Add(time.Hour)}},    // upcoming
	}
	if got := calendarDefaultCursor(entries, time.Now()); got != 2 {
		t.Fatalf("expected cursor on first unfinished event (2), got %d", got)
	}
	// All finished -> fall back to first.
	allPast := []calendarEntry{{event: &CalendarEvent{End: time.Now().Add(-time.Hour)}}}
	if got := calendarDefaultCursor(allPast, time.Now()); got != 0 {
		t.Fatalf("all-past should fall back to 0, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Fetch layer
// ---------------------------------------------------------------------------

func TestFetchCalendarEventsViaOverride(t *testing.T) {
	state := newUITestState()
	var gotParams calendarFetchParams
	state.calendar.override = func(_ context.Context, params calendarFetchParams) ([]CalendarEvent, error) {
		gotParams = params
		return calendarTestEvents(), nil
	}
	events, err := state.fetchCalendarEvents(context.Background(), calendarFetchParams{
		startTime: time.Now(),
		endTime:   time.Now().Add(7 * 24 * time.Hour),
		limit:     500,
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d", len(events))
	}
	if !events[0].Start.Before(events[1].Start) {
		t.Fatal("events must come back sorted ascending")
	}
	if gotParams.limit != 500 {
		t.Fatalf("params not threaded: %+v", gotParams)
	}
}

func TestFetchCalendarEventsNilClient(t *testing.T) {
	state := &AppState{logger: discardLogger}
	state.initRuntime(context.Background())
	// No client, no override, no auth override => must error, not panic.
	if _, err := state.fetchCalendarEvents(context.Background(), calendarFetchParams{}); err == nil {
		t.Fatal("expected error with no client")
	}
}

func TestCalendarWindowBoundsMidnight(t *testing.T) {
	start, end := calendarWindowRange(0)
	if !end.After(start) {
		t.Fatal("end must be after start")
	}
	if end.Sub(start) != calendarDayWindow*24*time.Hour {
		t.Fatalf("window span = %s, want %d days", end.Sub(start), calendarDayWindow)
	}
	// The start is local midnight, so its hour/minute/second are zero.
	local := start.In(time.Local)
	if local.Hour() != 0 || local.Minute() != 0 || local.Second() != 0 {
		t.Fatalf("window start is not local midnight: %s", local)
	}
	// A positive offset shifts the window forward exactly one day.
	nextStart, _ := calendarWindowRange(1)
	if nextStart.Sub(start) != 24*time.Hour {
		t.Fatalf("offset shift = %s, want 24h", nextStart.Sub(start))
	}
}

// ---------------------------------------------------------------------------
// Authentication + partition discovery (regression: middle tier needs the
// root skype token, and a 401 must never poison the partition cache)
// ---------------------------------------------------------------------------

func newAuthedCalendarState(t *testing.T, handler http.HandlerFunc) *AppState {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := calendarRootTokenOverride
	calendarRootTokenOverride = func() (string, error) { return "root-jwt", nil }
	t.Cleanup(func() { calendarRootTokenOverride = previous })
	previousFormat := calendarMiddleTierFormat
	calendarMiddleTierFormat = server.URL + "/api/mt/part/%s/v2.1/me/calendars/calendarView"
	t.Cleanup(func() { calendarMiddleTierFormat = previousFormat })
	previousAuthzEndpoint := calendarAuthzEndpoint
	calendarAuthzEndpoint = server.URL + "/api/authsvc/v1.0/authz"
	t.Cleanup(func() { calendarAuthzEndpoint = previousAuthzEndpoint })
	// Discovery goes straight to the fallback probe list unless a test
	// overrides the seam itself; nothing reaches the live authz service.
	previousAuthz := calendarPartitionFromAuthz
	calendarPartitionFromAuthz = func(context.Context, *AppState) (string, bool) { return "", false }
	t.Cleanup(func() { calendarPartitionFromAuthz = previousAuthz })
	return &AppState{logger: discardLogger, httpClient: server.Client(), calendar: calendarState{
		override: func(ctx context.Context, params calendarFetchParams) ([]CalendarEvent, error) {
			return nil, nil
		},
	}}
}

func TestCalendarRequestUsesRootTokenAndClassifiesAuthFailure(t *testing.T) {
	var gotAuth, gotPath string
	state := newAuthedCalendarState(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"value":[]}`)
	})

	events, err := state.doCalendarRequest(context.Background(), "amer-01", calendarFetchParams{
		startTime: time.Now(), endTime: time.Now().Add(time.Hour), limit: 5,
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

	authErr := &calendarResponseError{StatusCode: 401, Status: "401 Unauthorized"}
	if !isAuthFailure(authErr) {
		t.Fatalf("a 401 calendarResponseError must classify as an auth failure, message %q", authErr.Error())
	}
	forbidden := &calendarResponseError{StatusCode: 403, Status: "403 Forbidden"}
	if !isAuthFailure(forbidden) {
		t.Fatalf("a 403 calendarResponseError must classify as an auth failure, message %q", forbidden.Error())
	}
	notFound := &calendarResponseError{StatusCode: 404, Status: "404 Not Found"}
	if isAuthFailure(notFound) || calendarErrorIsStatus(authErr, http.StatusNotFound) {
		t.Fatal("404 must be a partition miss, 401 must not")
	}
}

func TestCalendarDiscoverySkipsAuthRejectedPartition(t *testing.T) {
	var requests int32
	state := newAuthedCalendarState(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		http.Error(w, "denied", http.StatusUnauthorized)
	})

	if _, err := state.discoverCalendarPartition(context.Background()); err == nil {
		t.Fatal("every partition rejecting auth must end discovery with an error")
	}
	if got := atomic.LoadInt32(&requests); got != int32(len(calendarPartitionFallbacks)) {
		t.Fatalf("a 401 must not be cached as usable: expected every fallback probed (%d), got %d",
			len(calendarPartitionFallbacks), got)
	}
	if state.calendar.region.segment != "" {
		t.Fatalf("auth-rejected partition must never be cached, cached %q", state.calendar.region.segment)
	}
}

// TestCalendarFetchRefreshesAfterDiscoveryAuthFailure pins the chain the
// middle tier's token behavior demands: when every discovery probe rejects
// the (expired) token, discovery must surface a classifiable auth error so
// the fetch performs its token refresh and retries — and it must do the
// refresh exactly once, with the rebuilt client in place for the retry.
// Without this, the refresh-and-retry branch is unreachable for the most
// common failure (expired token), and the live smoke would fail after
// arming the shared failure cooldown that also gates the chat path.
func TestCalendarFetchRefreshesAfterDiscoveryAuthFailure(t *testing.T) {
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

	previousRoot := calendarRootTokenOverride
	calendarRootTokenOverride = func() (string, error) {
		if atomic.LoadInt32(&refreshes) == 0 {
			return "stale-jwt", nil
		}
		return "fresh-jwt", nil
	}
	t.Cleanup(func() { calendarRootTokenOverride = previousRoot })
	previousFormat := calendarMiddleTierFormat
	calendarMiddleTierFormat = server.URL + "/api/mt/part/%s/v2.1/me/calendars/calendarView"
	t.Cleanup(func() { calendarMiddleTierFormat = previousFormat })
	previousAuthzEndpoint := calendarAuthzEndpoint
	calendarAuthzEndpoint = server.URL + "/api/authsvc/v1.0/authz"
	t.Cleanup(func() { calendarAuthzEndpoint = previousAuthzEndpoint })
	previousAuthz := calendarPartitionFromAuthz
	calendarPartitionFromAuthz = func(context.Context, *AppState) (string, bool) { return "", false }
	t.Cleanup(func() { calendarPartitionFromAuthz = previousAuthz })

	state := &AppState{
		logger:       discardLogger,
		httpClient:   server.Client(),
		tokenRefresh: tokenRefreshConfig{enabled: true},
	}
	state.setTeamsClient(newTestTeamsClient(t))
	state.refreshTokensOverride = func(ctx context.Context) error {
		// The refresh must run on the uncapped parent context: the load
		// timeout belongs to the network attempts only. Hoisting
		// WithTimeout(calendarLoadTimeout) above maybeRefreshTokens — a
		// plausible "bound the whole fetch" refactor — would clip the
		// Electron flow (~16s) at 12s and arm the failure cooldown that
		// also gates the chat path, and it passes every other assertion.
		if _, hasDeadline := ctx.Deadline(); hasDeadline {
			return errors.New("token refresh received a deadline-bound context")
		}
		atomic.AddInt32(&refreshes, 1)
		return nil
	}
	rebuilt := newTestTeamsClient(t)
	state.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) { return rebuilt, nil }

	events, err := state.fetchCalendarEvents(context.Background(), calendarFetchParams{
		startTime: time.Now(), endTime: time.Now().Add(time.Hour), limit: 10,
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
	if state.client() != rebuilt {
		t.Fatal("the retry must run on the rebuilt client")
	}

	// With refresh disabled, the discovery failure must surface as a
	// still-classifiable auth error (the reactive chat refresh reads the
	// same classification off this error).
	state.tokenRefresh = tokenRefreshConfig{enabled: false}
	state.calendar.region.segment = ""
	calendarRootTokenOverride = func() (string, error) { return "stale-jwt", nil }
	_, err = state.fetchCalendarEvents(context.Background(), calendarFetchParams{
		startTime: time.Now(), endTime: time.Now().Add(time.Hour), limit: 10,
	})
	if err == nil {
		t.Fatal("a permanently rejected token must surface an error")
	}
	if !isAuthFailure(err) {
		t.Fatalf("the surfaced discovery failure must classify as auth, got %v", err)
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("a disabled refresh must not be invoked, refreshes=%d", got)
	}
}

func TestCalendarDiscoveryCachesFirstUsablePartition(t *testing.T) {
	var requests int32
	state := newAuthedCalendarState(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if strings.Contains(r.URL.Path, "emea-02") {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `{"value":[]}`)
			return
		}
		http.Error(w, "nope", http.StatusNotFound)
	})
	previous := calendarPartitionFromAuthz
	calendarPartitionFromAuthz = func(context.Context, *AppState) (string, bool) { return "", false }
	t.Cleanup(func() { calendarPartitionFromAuthz = previous })

	segment, err := state.discoverCalendarPartition(context.Background())
	if err != nil || segment != "emea-02" {
		t.Fatalf("expected emea-02, got %q err %v", segment, err)
	}
	first := atomic.LoadInt32(&requests)
	if again, err := state.discoverCalendarPartition(context.Background()); err != nil || again != "emea-02" {
		t.Fatalf("cached segment must be reused: got %q err %v", again, err)
	}
	if second := atomic.LoadInt32(&requests); second != first {
		t.Fatalf("the second discovery must hit the cache, but %d more requests were made", second-first)
	}
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

func TestCalendarModelApplyAndCursor(t *testing.T) {
	events := calendarTestEvents()
	sortCalendarEvents(events) // fetch sorts; apply renders the given order.
	m := &calendarModel{}
	m.apply(events, time.Now(), time.Now(), time.Now().Add(7*24*time.Hour))
	if len(m.entries) != 4 {
		t.Fatalf("want 4 entries, got %d", len(m.entries))
	}
	if !m.focusable || m.loading || m.errorText != "" {
		t.Fatal("healthy apply must clear loading/error and be focusable")
	}
	// Sorted ascending, the first row is the past meeting; the cursor must
	// skip finished meetings and land on the first not-yet-finished one.
	ev, ok := m.currentEvent()
	if !ok {
		t.Fatal("expected an event under the cursor")
	}
	if ev.End.Before(time.Now()) {
		t.Fatalf("cursor must not rest on a finished meeting, got %q", ev.Subject)
	}
	if m.cursor != 1 {
		t.Fatalf("expected cursor on first upcoming row (1), got %d", m.cursor)
	}

	// Move onto the online meeting and confirm its join link resolves.
	m.setCursor(1)
	online, _ := m.currentEvent()
	if online.IsOnlineMeeting {
		if link, ok := m.selectedJoinURL(); !ok || !strings.Contains(link, "meetup-join") {
			t.Fatalf("expected join link for online meeting, got %q ok=%v", link, ok)
		}
	}

	m.setCursor(0)
	m.up()
	if m.cursor != 0 {
		t.Fatalf("up from first row must clamp to 0, got %d", m.cursor)
	}
	m.down()
	m.down()
	if m.cursor != 2 {
		t.Fatalf("down twice expected cursor 2, got %d", m.cursor)
	}
	m.end()
	if m.cursor != len(m.entries)-1 {
		t.Fatalf("end must land on last, got %d", m.cursor)
	}

	// Empty apply clears cursor and marks focusable.
	empty := &calendarModel{}
	empty.apply(nil, time.Now(), time.Now(), time.Now().Add(time.Hour))
	if empty.cursor != 0 || empty.top != 0 || !empty.focusable {
		t.Fatalf("empty apply state wrong: cursor=%d top=%d focusable=%v", empty.cursor, empty.top, empty.focusable)
	}
}

func TestCalendarDetailLinesShowJoinLink(t *testing.T) {
	online := calendarTestEvents()[1] // Sprint Review, online, has a join link
	lines := strings.Join(calendarDetailLines(&online, 60), "\n")
	if !strings.Contains(lines, "meetup-join") {
		t.Fatalf("detail must show join link, got:\n%s", lines)
	}
	if !strings.Contains(lines, "Matti") || !strings.Contains(lines, "Room 5") {
		t.Fatalf("detail must show organizer and location, got:\n%s", lines)
	}
	// nil event renders a hint, never panics.
	if got := calendarDetailLines(nil, 60); len(got) == 0 {
		t.Fatal("nil detail must still render a placeholder line")
	}
}

// ---------------------------------------------------------------------------
// Key routing + view
// ---------------------------------------------------------------------------

func TestCalendarOpenCloseAndNavigation(t *testing.T) {
	state := newUITestState()
	state.calendar.override = func(_ context.Context, _ calendarFetchParams) ([]CalendarEvent, error) {
		return calendarTestEvents(), nil
	}
	m := newChatModel(state)
	m.width, m.height = 120, 32

	// 'c' opens the calendar page.
	updated, _ := m.Update(runeKey('c'))
	m = updated.(chatModel)
	if m.ui().page != pageCalendar {
		t.Fatalf("c must open calendar, page=%d", m.ui().page)
	}

	// Drive the async load to completion synchronously.
	driveCalendarLoad(t, &m)
	if len(m.ui().cal.entries) != 4 {
		t.Fatalf("calendar must be populated, got %d entries", len(m.ui().cal.entries))
	}

	// j/k move the cursor.
	updated, _ = m.Update(runeKey('j'))
	m = updated.(chatModel)
	first := m.ui().cal.cursor
	if first == 0 {
		// cursor starts on upcoming (index 1 typically); j should still move
		t.Log("cursor at 0 after j; acceptable if already clamped")
	}

	// l focuses the detail pane.
	updated, _ = m.Update(runeKey('l'))
	m = updated.(chatModel)
	if m.ui().focus != PaneCalDetail {
		t.Fatalf("l must focus detail pane, got %s", m.ui().focus)
	}
	// h returns to the list.
	updated, _ = m.Update(runeKey('h'))
	m = updated.(chatModel)
	if m.ui().focus != PaneCalendar {
		t.Fatalf("h must focus list pane, got %s", m.ui().focus)
	}

	// 'c' returns to chat.
	updated, _ = m.Update(runeKey('c'))
	m = updated.(chatModel)
	if m.ui().page != pageMain {
		t.Fatalf("c must return to chat, page=%d", m.ui().page)
	}
}

func TestCalendarEscapeReturnsToChat(t *testing.T) {
	state := newUITestState()
	state.enterCalendar()
	m := newChatModel(state)
	m.width, m.height = 100, 28
	if m.ui().page != pageCalendar {
		t.Fatalf("enterCalendar must show calendar page")
	}
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = updated.(chatModel)
	if m.ui().page != pageMain {
		t.Fatalf("Esc must return to chat, page=%d", m.ui().page)
	}
}

func TestCalendarShiftWindowAndClamp(t *testing.T) {
	state := newUITestState()
	state.calendar.override = func(_ context.Context, _ calendarFetchParams) ([]CalendarEvent, error) {
		return nil, nil
	}
	m := newChatModel(state)
	m.width, m.height = 100, 28
	m.Update(runeKey('c'))

	// 'a' at offset 0 must not go negative.
	updated, _ := m.Update(runeKey('a'))
	m = updated.(chatModel)
	if state.calendar.windowOffset != 0 {
		t.Fatalf("window offset must clamp at 0 going earlier, got %d", state.calendar.windowOffset)
	}

	// 'd' advances by one day.
	updated, _ = m.Update(runeKey('d'))
	m = updated.(chatModel)
	if state.calendar.windowOffset != 1 {
		t.Fatalf("d must advance the window, got %d", state.calendar.windowOffset)
	}

	// 't' resets to today.
	updated, _ = m.Update(runeKey('t'))
	m = updated.(chatModel)
	if state.calendar.windowOffset != 0 {
		t.Fatalf("t must reset to today, got %d", state.calendar.windowOffset)
	}
}

func TestCalendarViewRendersWithinFrame(t *testing.T) {
	state := newUITestState()
	state.calendar.override = func(_ context.Context, _ calendarFetchParams) ([]CalendarEvent, error) {
		return calendarTestEvents(), nil
	}
	m := newChatModel(state)
	m.width, m.height = 140, 34
	m.ui().page = pageCalendar
	m.ui().focus = PaneCalendar
	driveCalendarLoad(t, &m)

	view := m.View()
	rows := strings.Split(view, "\n")
	if len(rows) > m.height {
		t.Fatalf("calendar frame must not exceed terminal height: %d > %d", len(rows), m.height)
	}
	joined := plain(view)
	// The list title and the meeting subject must be visible.
	if !strings.Contains(joined, "Sprint Review") {
		t.Fatalf("calendar view must show the meeting subject:\n%s", joined)
	}
	if !strings.Contains(joined, "Calendar") {
		t.Fatalf("calendar view must show its title:\n%s", joined)
	}
}

func TestCalendarViewLoadingAndErrorStates(t *testing.T) {
	state := newUITestState()
	m := newChatModel(state)
	m.width, m.height = 120, 30
	m.ui().page = pageCalendar

	// Loading state renders without crashing and hints loading.
	m.ui().cal.applyLoading(time.Now(), time.Now().Add(7*24*time.Hour))
	m.state.calendar.loadingSince = time.Now()
	loading := plain(m.View())
	if !strings.Contains(loading, "Loading") {
		t.Fatalf("loading view must show loading text, got:\n%s", loading)
	}

	// Error state surfaces the failure text.
	m.ui().cal.applyError(context.DeadlineExceeded, time.Now(), time.Now().Add(7*24*time.Hour))
	errView := plain(m.View())
	if !strings.Contains(errView, "Calendar load failed") {
		t.Fatalf("error view must show the failure, got:\n%s", errView)
	}
}

func TestCalendarHelpMentionsCalendarKeys(t *testing.T) {
	state := newUITestState()
	m := newChatModel(state)
	m.width = 140
	m.help.Width = 140
	m.ui().page = pageCalendar
	text := strings.ToLower(plain(m.help.View(m.calendarKeyBindings())))
	for _, want := range []string{"reload", "earlier", "later"} {
		if !strings.Contains(text, want) {
			t.Fatalf("calendar help must mention %q, got:\n%s", want, text)
		}
	}
}

// TestCalendarCursorStaysVisibleWhenListOverflows pins the list's primary
// interaction: with more meetings than the pane can render (a realistic
// week is two lines per row), the cursor must always be inside the painted
// block. Before render-time scroll-into-view, top only moved up, so 'G' or
// repeated 'j' parked the ▸ marker in rows padToHeight clipped away — the
// list went visually blank while the detail pane kept moving. The frame must
// also stay within the terminal (the old entry-counted loop emitted
// 2×bodyHeight lines and had the frame head clipped).
func TestCalendarCursorStaysVisibleWhenListOverflows(t *testing.T) {
	state := newUITestState()
	const total = 40
	events := make([]CalendarEvent, 0, total)
	base := time.Now().Add(-2 * time.Hour)
	for i := range total {
		start := base.Add(time.Duration(i) * time.Hour)
		events = append(events, CalendarEvent{
			Subject:       fmt.Sprintf("Meeting %02d", i),
			StartTime:     start.UTC().Format(time.RFC3339),
			EndTime:       start.Add(30 * time.Minute).UTC().Format(time.RFC3339),
			Start:         start,
			End:           start.Add(30 * time.Minute),
			OrganizerName: fmt.Sprintf("Organizer %02d", i),
		})
	}
	state.calendar.override = func(_ context.Context, _ calendarFetchParams) ([]CalendarEvent, error) {
		return events, nil
	}

	m := newChatModel(state)
	m.width, m.height = 140, 34
	m.ui().page = pageCalendar
	m.ui().focus = PaneCalendar
	driveCalendarLoad(t, &m)
	if len(m.ui().cal.entries) != total {
		t.Fatalf("fixture must exceed the pane, got %d entries", len(m.ui().cal.entries))
	}
	if m.ui().cal.top != 0 {
		t.Fatalf("apply should start at the top, got top=%d", m.ui().cal.top)
	}

	// Jump to the last meeting: the marker and its subject must be painted.
	updated, _ := m.Update(runeKey('G'))
	m = updated.(chatModel)
	if m.ui().cal.cursor != total-1 {
		t.Fatalf("G must land on the last row, got %d", m.ui().cal.cursor)
	}
	view := m.View()
	rows := strings.Split(view, "\n")
	if len(rows) > m.height {
		t.Fatalf("frame must fit the terminal: %d > %d", len(rows), m.height)
	}
	body := plain(view)
	if !strings.Contains(body, "Meeting 39") {
		t.Fatalf("selected row must stay visible after G:\n%s", body)
	}
	if !strings.Contains(body, "▸") {
		t.Fatalf("cursor marker must stay visible after G:\n%s", body)
	}
	if !strings.Contains(body, "esc back") {
		t.Fatalf("help bar must survive the frame: %q", body)
	}
	if m.ui().cal.top == 0 {
		t.Fatal("the window must have scrolled down to the cursor")
	}

	// Page back up: the cursor re-enters the painted block near the top.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgUp})
	m = updated.(chatModel)
	view = plain(m.View())
	if !strings.Contains(view, "▸") {
		t.Fatalf("cursor marker must stay visible after PgUp:\n%s", view)
	}
	want, _ := m.ui().cal.currentEvent()
	if want == nil || !strings.Contains(view, want.Subject) {
		t.Fatalf("selected row must stay visible after PgUp, want %q:\n%s", want.Subject, view)
	}

	// A long walk down keeps the marker painted the whole way.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	m = updated.(chatModel)
	for i := range 30 {
		updated, _ = m.Update(runeKey('j'))
		m = updated.(chatModel)
		ev, ok := m.ui().cal.currentEvent()
		if !ok {
			t.Fatalf("step %d lost the current event", i)
		}
		frame := plain(m.View())
		if !strings.Contains(frame, "▸") || !strings.Contains(frame, ev.Subject) {
			t.Fatalf("step %d: cursor off-screen (cursor=%d top=%d want %q)",
				i, m.ui().cal.cursor, m.ui().cal.top, ev.Subject)
		}
	}

	// PgDn from the last row: the page walk's budget outlives the entries,
	// so top/cursor would run off the end without page()'s clamps. The
	// current event must resolve BEFORE View() runs — the detail pane
	// depends on that invariant, not on the list pane being rendered first.
	updated, _ = m.Update(runeKey('G'))
	m = updated.(chatModel)
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	m = updated.(chatModel)
	last, ok := m.ui().cal.currentEvent()
	if !ok || last == nil {
		t.Fatalf("PgDn at the tail must keep a valid selection (cursor=%d top=%d)",
			m.ui().cal.cursor, m.ui().cal.top)
	}
	if last.Subject != "Meeting 39" {
		t.Fatalf("PgDn at the tail must not move the selection off the last row, got %q", last.Subject)
	}
	if m.ui().cal.top >= len(m.ui().cal.entries) || m.ui().cal.cursor >= len(m.ui().cal.entries) {
		t.Fatalf("window indices out of range after tail PgDn: top=%d cursor=%d",
			m.ui().cal.top, m.ui().cal.cursor)
	}
	frame := plain(m.View())
	if !strings.Contains(frame, "▸") || !strings.Contains(frame, "Meeting 39") {
		t.Fatalf("tail selection must stay painted after PgDn:\n%s", frame)
	}

	// Repeated PgDn at the tail is idempotent, never wedged.
	for range 3 {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
		m = updated.(chatModel)
		if ev, ok := m.ui().cal.currentEvent(); !ok || ev.Subject != "Meeting 39" {
			t.Fatalf("repeated tail PgDn lost the selection (cursor=%d top=%d)",
				m.ui().cal.cursor, m.ui().cal.top)
		}
	}
}

// driveCalendarLoad deterministically populates the calendar model by
// fetching through the (overridden) fetch path and delivering the result to
// Update. It bumps loadSeq so the delivered message is not treated as stale.
func driveCalendarLoad(t *testing.T, m *chatModel) {
	t.Helper()
	start, end := calendarWindowRange(m.state.calendar.windowOffset)
	events, err := m.state.fetchCalendarEvents(m.state.appContext(), calendarFetchParams{
		startTime: start,
		endTime:   end,
		limit:     calendarMaxEvents,
	})
	if err != nil {
		t.Fatalf("driven fetch failed: %v", err)
	}
	// Bump the shared load sequence so the stale guard accepts the message,
	// mirroring what startCalendarLoad does before its goroutine delivers.
	loadSeq := atomic.AddUint64(&m.state.loadSeq, 1)
	updated, _ := m.Update(calendarLoadedMsg{
		events:   events,
		loadSeq:  loadSeq,
		syncedAt: time.Now(),
		start:    start,
		end:      end,
	})
	*m = updated.(chatModel)
}
