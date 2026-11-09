package main

import (
	"context"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sirupsen/logrus"
)

// calendarDayWindow is the number of days fetched per calendar view. A week
// round-trips comfortably and keeps the list short enough to page through.
const calendarDayWindow = 7

// calendarLoadTimeout bounds one calendar fetch.
const calendarLoadTimeout = 12 * time.Second

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
// has never been populated. The returned command starts the loading ticker.
func (s *AppState) enterCalendar() tea.Cmd {
	s.ui.page = pageCalendar
	if s.calendar.loaded || atomic.LoadUint32(&s.calendar.fetching) == 1 {
		return nil
	}
	return s.startCalendarLoad()
}

// leaveCalendar returns to the main chat page.
func (s *AppState) leaveCalendar() {
	s.ui.page = pageMain
}

// startCalendarLoad kicks off an async fetch for the current window and shows
// the loading state. Results arrive through calendarLoadedMsg /
// calendarLoadErrorMsg; loadSeq guards staleness exactly like the chat path.
func (s *AppState) startCalendarLoad() tea.Cmd {
	atomic.StoreUint32(&s.calendar.fetching, 0)
	if !atomic.CompareAndSwapUint32(&s.calendar.fetching, 0, 1) {
		return loadingTickCmd(s.currentLoadSeq(), 0)
	}

	start, end := calendarWindowRange(s.calendar.windowOffset)
	s.calendar.rangeStart = start
	s.calendar.rangeEnd = end
	s.calendar.loadingSince = time.Now()

	loadSeq := atomic.AddUint64(&s.loadSeq, 1)
	atomic.StoreUint64(&s.activeLoadSeq, loadSeq)
	loadCtx, cancel := context.WithCancel(s.appContext())
	s.replaceActiveLoadCancel(loadSeq, cancel)

	s.ui.cal.applyLoading(start, end)

	go s.fetchCalendar(loadCtx, start, end, loadSeq)
	return loadingTickCmd(loadSeq, 0)
}

// currentLoadSeq reads the last issued load sequence without mutating it.
func (s *AppState) currentLoadSeq() uint64 {
	return atomic.LoadUint64(&s.loadSeq)
}

// fetchCalendar runs the fetch off the UI thread and delivers the outcome as
// a message.
func (s *AppState) fetchCalendar(ctx context.Context, start, end time.Time, loadSeq uint64) {
	defer s.clearActiveLoadCancel(loadSeq)
	defer atomic.StoreUint32(&s.calendar.fetching, 0)

	s.appLogger().WithFields(logrus.Fields{
		"cal_start": start.Format(time.RFC3339),
		"cal_end":   end.Format(time.RFC3339),
		"load_seq":  loadSeq,
	}).Debug("loading calendar events")

	// The per-attempt network bound lives in calendarEventsForWindow, not
	// here: the token refresh-and-retry it may trigger gets its own timeout
	// from the refresh config and must run on this (uncapped) context.
	events, err := s.fetchCalendarEvents(ctx, calendarFetchParams{
		startTime: start,
		endTime:   end,
		limit:     calendarMaxEvents,
	})
	if err != nil {
		atomic.CompareAndSwapUint64(&s.activeLoadSeq, loadSeq, 0)
		if isLoadCancellation(ctx, err) {
			s.appLogger().WithField("load_seq", loadSeq).Debug("calendar load cancelled")
			return
		}
		if s.staleLoad(loadSeq) {
			return
		}
		s.appLogger().WithField("load_seq", loadSeq).WithError(err).Warn("calendar load failed")
		s.sendUIMsg(calendarLoadErrorMsg{err: err, loadSeq: loadSeq, start: start, end: end})
		return
	}

	atomic.CompareAndSwapUint64(&s.activeLoadSeq, loadSeq, 0)
	if s.staleLoad(loadSeq) {
		return
	}

	s.calendar.eventsMu.Lock()
	s.calendar.events = events
	s.calendar.loaded = true
	s.calendar.loadedAt = time.Now()
	s.calendar.eventsMu.Unlock()

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

// shiftCalendarWindow moves the visible window by deltaDays (whole days) and
// reloads. Returns the loading command to start the ticker.
func (s *AppState) shiftCalendarWindow(deltaDays int) tea.Cmd {
	s.calendar.windowOffset += deltaDays
	if s.calendar.windowOffset < 0 {
		s.calendar.windowOffset = 0
	}
	return s.startCalendarLoad()
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
