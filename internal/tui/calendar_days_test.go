package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-cli/internal/calendar"
)

// multiDayEvents spreads perDay one-hour meetings over days days starting
// today at 09:00 local.
func multiDayEvents(days, perDay int) []calendar.Event {
	now := time.Now()
	base := time.Date(now.Year(), now.Month(), now.Day(), 9, 0, 0, 0, time.Local)
	var events []calendar.Event
	for d := 0; d < days; d++ {
		for i := 0; i < perDay; i++ {
			start := base.AddDate(0, 0, d).Add(time.Duration(i) * time.Hour)
			events = append(events, calendar.Event{
				Subject:       fmt.Sprintf("Day %d meeting %d", d, i),
				Start:         start,
				End:           start.Add(30 * time.Minute),
				OrganizerName: "Org",
			})
		}
	}
	return events
}

func calendarFrame(t *testing.T, events []calendar.Event, height int) (chatModel, []string) {
	t.Helper()
	state := newUITestState()
	state.ui.page = pageCalendar
	state.ui.focus = PaneCalendar
	start, end := calendarWindowRange(0)
	state.ui.cal.apply(events, time.Now(), start, end)
	m := newChatModel(state)
	m.width, m.height = 100, height
	m.help.Width = 100
	return m, strings.Split(plain(m.View()), "\n")
}

// TestCalendarDayDividers pins one divider per day, labelled relative to
// today, above that day's first meeting.
func TestCalendarDayDividers(t *testing.T) {
	m, rows := calendarFrame(t, multiDayEvents(3, 2), 40)
	// Only the list column: the details pane repeats the selected title.
	for i, row := range rows {
		if r := []rune(row); len(r) > m.calendarListWidth() {
			rows[i] = string(r[:m.calendarListWidth()])
		}
	}
	body := strings.Join(rows, "\n")

	today := time.Now()
	for _, want := range []string{
		"── Today · " + today.Format("Mon 02 Jan"),
		"── Tomorrow · " + today.AddDate(0, 0, 1).Format("Mon 02 Jan"),
		"── " + today.AddDate(0, 0, 2).Format("Mon 02 Jan"),
	} {
		if strings.Count(body, want) != 1 {
			t.Fatalf("expected exactly one divider %q in:\n%s", want, body)
		}
	}
	divider := strings.Index(body, "── Tomorrow")
	if divider < 0 || divider > strings.Index(body, "Day 1 meeting 0") || strings.Index(body, "Day 0 meeting 1") > divider {
		t.Fatalf("the divider must sit between the days:\n%s", body)
	}
	if strings.Contains(body, today.Format("Mon ")+"09:00") {
		t.Fatal("rows must not repeat the weekday the divider shows")
	}
}

// TestCalendarDayDividerSticksToTop pins that a list scrolled into the middle
// of a day still labels that day on its first row, and the cursor stays on
// screen with dividers in the line budget.
func TestCalendarDayDividerSticksToTop(t *testing.T) {
	m, _ := calendarFrame(t, multiDayEvents(2, 12), 20)
	cal := &m.ui().cal
	cal.end()
	rows := strings.Split(plain(m.View()), "\n")
	if len(rows) != m.height {
		t.Fatalf("frame must fit the terminal: %d rows", len(rows))
	}
	if cal.top == 0 {
		t.Fatal("the list must have scrolled")
	}
	firstListRow := rows[tabBarHeight+2] // pane margin, then the window title
	if !strings.Contains(firstListRow, "── Tomorrow") {
		t.Fatalf("the top row must label the visible day, got %q", firstListRow)
	}
	if !strings.Contains(strings.Join(rows, "\n"), "Day 1 meeting 11") {
		t.Fatal("the cursor's meeting must stay visible")
	}
}
