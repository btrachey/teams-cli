package tui

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/fossteams/teams-cli/internal/calendar"
	"github.com/sirupsen/logrus"
)

// calendarDayWindow is the number of days fetched per calendar view. A week
// round-trips comfortably and keeps the list short enough to page through.
const calendarDayWindow = 7

// calendarState is the calendar section's load bookkeeping.
type calendarState struct {
	// loadSeq/cancel track the in-flight fetch (the UI loop owns cancel).
	loadSeq      uint64
	cancel       context.CancelFunc
	rangeStart   time.Time
	loadingSince time.Time
	// windowOffset shifts the visible range by whole days for paging
	// ("today", "+1d", "-1d"). 0 is today.
	windowOffset int
	// override replaces the network fetch in tests.
	override func(ctx context.Context, params calendar.Query) ([]calendar.Event, error)
}

// calendarWindowRange computes [start, start+window) for the given day offset.
func calendarWindowRange(offsetDays int) (time.Time, time.Time) {
	start := calendarWindowBounds(offsetDays)
	return start, start.Add(calendarDayWindow * 24 * time.Hour)
}

// calendarWindowBounds is local midnight of the window start day, normalized
// to a UTC instants (the service filters on UTC bounds).
func calendarWindowBounds(offsetDays int) time.Time {
	now := time.Now()
	day := now.AddDate(0, 0, offsetDays)
	midnight := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.Local)
	return midnight.UTC()
}

// enterCalendar shows the calendar section and loads the current window if it
// has never been populated.
func (s *AppState) enterCalendar() {
	s.ui.page = pageCalendar
	if s.ui.cal.loadedAt.IsZero() && !s.ui.cal.loading {
		s.startCalendarLoad()
	}
}

// leaveCalendar returns to the main chat page.
func (s *AppState) leaveCalendar() {
	s.ui.page = pageMain
}

// startCalendarLoad kicks off an async fetch for the current window and shows
// the loading state, superseding any calendar fetch still in flight. Results
// arrive through calendarLoadedMsg / calendarLoadErrorMsg. The calendar keeps
// its own load sequence and cancel func: sharing the chat's would cancel a
// conversation load (stranding its pane on "loading") and pause live message
// refresh while the calendar fetched.
func (s *AppState) startCalendarLoad() {
	start, end := calendarWindowRange(s.calendar.windowOffset)
	s.calendar.rangeStart = start
	s.calendar.loadingSince = time.Now()

	if s.calendar.cancel != nil {
		s.calendar.cancel()
	}
	loadSeq := atomic.AddUint64(&s.calendar.loadSeq, 1)
	loadCtx, cancel := context.WithCancel(s.appContext())
	s.calendar.cancel = cancel

	s.ui.cal.applyLoading(start, end)

	go s.fetchCalendar(loadCtx, start, end, loadSeq)
}

// staleCalendarLoad reports whether a calendar result is superseded.
func (s *AppState) staleCalendarLoad(loadSeq uint64) bool {
	return atomic.LoadUint64(&s.calendar.loadSeq) != loadSeq
}

// fetchCalendar runs the fetch off the UI thread and delivers the outcome as
// a message.
func (s *AppState) fetchCalendar(ctx context.Context, start, end time.Time, loadSeq uint64) {
	s.appLogger().WithFields(logrus.Fields{
		"cal_start": start.Format(time.RFC3339),
		"cal_end":   end.Format(time.RFC3339),
		"load_seq":  loadSeq,
	}).Debug("loading calendar events")

	// The per-attempt network bound lives in the calendar client, not
	// here: the token refresh-and-retry it may trigger gets its own timeout
	// from the refresh config and must run on this (uncapped) context.
	events, err := s.fetchCalendarEvents(ctx, calendar.Query{
		Start: start,
		End:   end,
		Limit: calendar.MaxEvents,
	})
	if s.staleCalendarLoad(loadSeq) || isLoadCancellation(ctx, err) {
		return
	}
	if err != nil {
		s.appLogger().WithField("load_seq", loadSeq).WithError(err).Warn("calendar load failed")
		s.sendUIMsg(calendarLoadErrorMsg{err: err, loadSeq: loadSeq, start: start, end: end})
		return
	}

	s.appLogger().WithFields(logrus.Fields{
		"load_seq":    loadSeq,
		"event_count": len(events),
	}).Debug("calendar events loaded")
	s.sendUIMsg(calendarLoadedMsg{
		events:   events,
		loadSeq:  loadSeq,
		syncedAt: time.Now(),
		start:    start,
		end:      end,
	})
}

// fetchCalendarEvents loads meetings sorted ascending by start time.
func (s *AppState) fetchCalendarEvents(ctx context.Context, params calendar.Query) ([]calendar.Event, error) {
	if s.calendar.override != nil {
		events, err := s.calendar.override(ctx, params)
		if err != nil {
			return nil, err
		}
		return calendar.Normalize(events, params.Limit), nil
	}
	if s.teams.Client() == nil {
		return nil, errors.New("teams client is nil")
	}

	return s.calendarAPI.Events(ctx, params)
}

// shiftCalendarWindow moves the visible window by deltaDays (whole days) and
// reloads. The window never starts before today.
func (s *AppState) shiftCalendarWindow(deltaDays int) {
	offset := max(0, s.calendar.windowOffset+deltaDays)
	if offset == s.calendar.windowOffset {
		return
	}
	s.calendar.windowOffset = offset
	s.startCalendarLoad()
}

// calendarWindowLabel renders the range header for the current window, e.g.
// "Mon 03 Nov – Sun 09 Nov 2026".
func calendarWindowLabel(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return "Calendar"
	}
	last := end.Add(-time.Second)
	return "Calendar · " + start.Format("Mon 02 Jan") + " – " + last.Format("Mon 02 Jan 2006")
}
