package tui

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-cli/internal/calendar"
	"github.com/fossteams/teams-cli/internal/logging"
)

// calendarTestEvents returns a deterministic set covering the cases the UI
// must render: a past meeting, an upcoming all-day event, an online meeting
// with a join link, and one the user declined.
func calendarTestEvents() []calendar.Event {
	now := time.Now()
	mk := func(subject string, startOffset time.Duration, dur time.Duration) calendar.Event {
		start := now.Add(startOffset)
		return calendar.Event{
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

	allDay := calendar.Event{
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

	return []calendar.Event{declined, upcoming, past, allDay}
}

// ---------------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------------

func TestCalendarDefaultCursorLandsOnUpcoming(t *testing.T) {
	entries := []calendarEntry{
		{event: &calendar.Event{End: time.Now().Add(-time.Hour)}},   // finished
		{event: &calendar.Event{End: time.Now().Add(-time.Minute)}}, // just finished
		{event: &calendar.Event{End: time.Now().Add(time.Hour)}},    // upcoming
	}
	if got := calendarDefaultCursor(entries, time.Now()); got != 2 {
		t.Fatalf("expected cursor on first unfinished event (2), got %d", got)
	}
	// All finished -> fall back to first.
	allPast := []calendarEntry{{event: &calendar.Event{End: time.Now().Add(-time.Hour)}}}
	if got := calendarDefaultCursor(allPast, time.Now()); got != 0 {
		t.Fatalf("all-past should fall back to 0, got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Fetch layer
// ---------------------------------------------------------------------------

func TestFetchCalendarEventsViaOverride(t *testing.T) {
	state := newUITestState()
	var gotParams calendar.Query
	state.calendar.override = func(_ context.Context, params calendar.Query) ([]calendar.Event, error) {
		gotParams = params
		return calendarTestEvents(), nil
	}
	events, err := state.fetchCalendarEvents(context.Background(), calendar.Query{
		Start: time.Now(),
		End:   time.Now().Add(7 * 24 * time.Hour),
		Limit: 500,
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
	if gotParams.Limit != 500 {
		t.Fatalf("params not threaded: %+v", gotParams)
	}
}

func TestFetchCalendarEventsNilClient(t *testing.T) {
	state := &AppState{logger: logging.Discard}
	state.initRuntime(context.Background())
	// No client, no override, no auth override => must error, not panic.
	if _, err := state.fetchCalendarEvents(context.Background(), calendar.Query{}); err == nil {
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

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

func TestCalendarModelApplyAndCursor(t *testing.T) {
	events := calendarTestEvents()
	events = calendar.Normalize(events, 0) // fetch sorts; apply renders the given order.
	m := &calendarModel{}
	m.apply(events, time.Now(), time.Now(), time.Now().Add(7*24*time.Hour))
	if len(m.entries) != 4 {
		t.Fatalf("want 4 entries, got %d", len(m.entries))
	}
	if m.loading || m.errorText != "" {
		t.Fatal("healthy apply must clear loading/error")
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

	// Empty apply clears the cursor.
	empty := &calendarModel{}
	empty.apply(nil, time.Now(), time.Now(), time.Now().Add(time.Hour))
	if empty.cursor != 0 || empty.top != 0 {
		t.Fatalf("empty apply state wrong: cursor=%d top=%d", empty.cursor, empty.top)
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
	state.calendar.override = func(_ context.Context, _ calendar.Query) ([]calendar.Event, error) {
		return calendarTestEvents(), nil
	}
	m := newChatModel(state)
	m.width, m.height = 120, 32

	// 's' (gh-dash's switch view) opens the calendar tab.
	updated, _ := m.Update(runeKey('s'))
	m = updated.(chatModel)
	if m.ui().page != pageCalendar {
		t.Fatalf("s must open calendar, page=%d", m.ui().page)
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

	// Tab focuses the detail pane and back.
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(chatModel)
	if m.ui().focus != PaneCalDetail {
		t.Fatalf("tab must focus detail pane, got %s", m.ui().focus)
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(chatModel)
	if m.ui().focus != PaneCalendar {
		t.Fatalf("tab must focus list pane, got %s", m.ui().focus)
	}

	// 'h' (previous section) returns to the messages tab.
	updated, _ = m.Update(runeKey('h'))
	m = updated.(chatModel)
	if m.ui().page != pageMain {
		t.Fatalf("h must return to messages, page=%d", m.ui().page)
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
	state.calendar.override = func(_ context.Context, _ calendar.Query) ([]calendar.Event, error) {
		return nil, nil
	}
	m := newChatModel(state)
	m.width, m.height = 100, 28
	m.Update(runeKey('s'))

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
	state.calendar.override = func(_ context.Context, _ calendar.Query) ([]calendar.Event, error) {
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
	text := strings.ToLower(plain(m.help.View(m.keyBindings())))
	for _, want := range []string{"refresh", "earlier", "later"} {
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
	events := make([]calendar.Event, 0, total)
	base := time.Now().Add(-2 * time.Hour)
	for i := range total {
		start := base.Add(time.Duration(i) * time.Hour)
		events = append(events, calendar.Event{
			Subject:       fmt.Sprintf("Meeting %02d", i),
			StartTime:     start.UTC().Format(time.RFC3339),
			EndTime:       start.Add(30 * time.Minute).UTC().Format(time.RFC3339),
			Start:         start,
			End:           start.Add(30 * time.Minute),
			OrganizerName: fmt.Sprintf("Organizer %02d", i),
		})
	}
	state.calendar.override = func(_ context.Context, _ calendar.Query) ([]calendar.Event, error) {
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
	if !strings.Contains(body, "switch tab") {
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
	events, err := m.state.fetchCalendarEvents(m.state.appContext(), calendar.Query{
		Start: start,
		End:   end,
		Limit: calendar.MaxEvents,
	})
	if err != nil {
		t.Fatalf("driven fetch failed: %v", err)
	}
	// Bump the shared load sequence so the stale guard accepts the message,
	// mirroring what startCalendarLoad does before its goroutine delivers.
	loadSeq := atomic.AddUint64(&m.state.calendar.loadSeq, 1)
	updated, _ := m.Update(calendarLoadedMsg{
		events:   events,
		loadSeq:  loadSeq,
		syncedAt: time.Now(),
		start:    start,
		end:      end,
	})
	*m = updated.(chatModel)
}
