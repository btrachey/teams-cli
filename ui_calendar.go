package main

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// calendarModel owns the calendar list pane state: the rendered event rows,
// the cursor, and the scroll window. It mirrors messagesModel's cursor
// semantics (the cursor is the selection; rendering scrolls it into view).
type calendarModel struct {
	entries    []calendarEntry
	cursor     int
	top        int
	focusable  bool
	loading    bool
	loadedAt   time.Time
	windowLbl  string
	windowText string
	// errorText carries a load failure message; empty when healthy.
	errorText string
}

// calendarEntry is one rendered meeting row.
type calendarEntry struct {
	// mainLine is the bold subject/time line; detailLine the organizer/
	// location/status line. Placeholder rows carry no event.
	mainLine   string
	detailLine string
	event      *CalendarEvent
}

// calendarStyles are the palette the calendar section renders with, kept
// separate from the chat styles so a calendar-only tweak never moves chat.
var (
	calAllDayStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("14"))
	calCancelledHint = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	calJoinStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	calRSPVStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	calTimeStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("11")).Bold(true)
	calSubjectStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("252"))
	calMetaStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// apply replaces the pane with freshly loaded events, placing the cursor on
// the next upcoming meeting (or the first row).
func (m *calendarModel) apply(events []CalendarEvent, syncedAt time.Time, start, end time.Time) {
	entries := make([]calendarEntry, 0, len(events))
	for i := range events {
		ev := events[i]
		entries = append(entries, calendarEntry{
			mainLine:   calendarMainLine(ev),
			detailLine: calendarDetailLine(ev),
			event:      &ev,
		})
	}

	m.entries = entries
	m.loadedAt = syncedAt
	m.errorText = ""
	m.focusable = true
	m.loading = false
	m.windowLbl = calendarWindowLabel(start, end)
	m.windowText = calendarRangeSubtitle(start, end)

	if len(entries) == 0 {
		m.cursor = 0
		m.top = 0
		return
	}
	m.setCursor(calendarDefaultCursor(entries, time.Now()))
}

// applyError records a load failure, keeping any previously loaded rows.
func (m *calendarModel) applyError(err error, start, end time.Time) {
	m.errorText = truncateComposerStatus("Calendar load failed: " + err.Error())
	m.windowLbl = calendarWindowLabel(start, end)
	m.windowText = calendarRangeSubtitle(start, end)
	m.focusable = true
	m.loading = false
}

// applyLoading shows the loading placeholder for a window.
func (m *calendarModel) applyLoading(start, end time.Time) {
	m.loading = true
	m.errorText = ""
	m.windowLbl = calendarWindowLabel(start, end)
	m.windowText = calendarRangeSubtitle(start, end)
}

func (m *calendarModel) setCursor(index int) {
	if len(m.entries) == 0 {
		m.cursor = 0
		m.top = 0
		return
	}
	m.cursor = clampCursor(index, len(m.entries))
	if m.cursor < m.top {
		m.top = m.cursor
	}
}

func (m *calendarModel) up()   { m.setCursor(m.cursor - 1) }
func (m *calendarModel) down() { m.setCursor(m.cursor + 1) }

// page moves the window (and cursor) by roughly deltaLines rendered rows,
// the PgUp/PgDn semantics of a scrolling list. Entries are 1–2 lines each,
// so the walk counts calendarEntryLines rather than entry indexes.
func (m *calendarModel) page(deltaLines, bodyLines int) {
	if len(m.entries) == 0 {
		return
	}
	if bodyLines < 1 {
		bodyLines = 1
	}
	target := m.top
	used := 0
	if deltaLines > 0 {
		for target < len(m.entries) && used < deltaLines {
			used += calendarEntryLines(m.entries[target])
			target++
		}
		// The budget can outlive the entries; top must stay a valid entry
		// index or currentEvent resolves nothing between the keypress and
		// the render-time clamp.
		target = minInt(target, len(m.entries)-1)
	} else if deltaLines < 0 {
		for target > 0 && used < -deltaLines {
			target--
			used += calendarEntryLines(m.entries[target])
		}
	}
	m.top = target

	// The cursor follows into the new window: clamp it between top and the
	// last entry the window can render.
	end := target
	used = 0
	for end < len(m.entries) {
		next := used + calendarEntryLines(m.entries[end])
		if next > bodyLines {
			break
		}
		used = next
		end++
	}
	windowEnd := maxInt(target, end-1)
	m.cursor = clampCursor(m.cursor, len(m.entries))
	m.cursor = clampInt(m.cursor, target, windowEnd)
}

func (m *calendarModel) home() { m.setCursor(0); m.top = 0 }
func (m *calendarModel) end()  { m.setCursor(len(m.entries) - 1) }

// currentEvent returns the meeting under the cursor.
func (m *calendarModel) currentEvent() (*CalendarEvent, bool) {
	if m.cursor < 0 || m.cursor >= len(m.entries) {
		return nil, false
	}
	entry := m.entries[m.cursor]
	if entry.event == nil {
		return nil, false
	}
	return entry.event, true
}

// calendarDefaultCursor picks the first event that has not finished yet, so
// opening the calendar lands on "now", falling back to the first row.
func calendarDefaultCursor(entries []calendarEntry, now time.Time) int {
	for i, entry := range entries {
		if entry.event == nil {
			continue
		}
		if entry.event.End.IsZero() || !entry.event.End.Before(now) {
			return i
		}
	}
	return 0
}

// calendarMainLine renders the bold "time · subject" line.
func calendarMainLine(ev CalendarEvent) string {
	when := calendarWhen(ev)
	subject := strings.TrimSpace(sanitizeText(ev.Subject))
	if subject == "" {
		subject = "(no title)"
	}
	return when + "  " + subject
}

// calendarDetailLine renders the dim organizer/location/status line.
func calendarDetailLine(ev CalendarEvent) string {
	parts := make([]string, 0, 3)
	if organizer := strings.TrimSpace(sanitizeText(ev.OrganizerName)); organizer != "" {
		parts = append(parts, organizer)
	}
	if location := strings.TrimSpace(sanitizeText(ev.Location)); location != "" {
		parts = append(parts, location)
	}
	if tag := calendarResponseTag(ev); tag != "" {
		parts = append(parts, tag)
	}
	return strings.Join(parts, " · ")
}

// calendarWhen renders the leading time or day label for a row.
func calendarWhen(ev CalendarEvent) string {
	if ev.IsAllDayEvent || ev.Start.IsZero() {
		day := ev.Start
		if day.IsZero() {
			return calAllDayStyle.Render("all-day")
		}
		return calAllDayStyle.Render(day.In(time.Local).Format("Mon 02 Jan"))
	}
	start := ev.Start.In(time.Local)
	end := ev.End.In(time.Local)
	timeText := start.Format("15:04")
	if !end.IsZero() && !end.Equal(start) {
		timeText = start.Format("15:04") + "–" + end.Format("15:04")
	}
	// Prefix the weekday so meetings outside today are unambiguous.
	return calTimeStyle.Render(start.Format("Mon ") + timeText)
}

// calendarResponseTag shows the RSVP state and privacy/cancellation markers.
func calendarResponseTag(ev CalendarEvent) string {
	if ev.IsCancelled {
		return calCancelledHint.Render("cancelled")
	}
	switch strings.ToLower(ev.MyResponseType) {
	case "accepted":
		return calRSPVStyle.Render("accepted")
	case "tentative":
		return calRSPVStyle.Render("tentative")
	case "declined":
		return calCancelledHint.Render("declined")
	}
	return ""
}

// calendarRangeSubtitle renders the synced/time status for the title line.
func calendarRangeSubtitle(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return ""
	}
	return calendarWindowLabel(start, end)
}

// joinable reports whether the event under the cursor has a join link.
func (m *calendarModel) selectedJoinURL() (string, bool) {
	ev, ok := m.currentEvent()
	if !ok {
		return "", false
	}
	link := strings.TrimSpace(ev.SkypeTeamsMeetingURL)
	if link == "" {
		return "", false
	}
	return link, true
}

// calendarDetailLines renders the right-hand detail block for the selected
// event: full times, organizer, location, meeting link, RSVP.
func calendarDetailLines(ev *CalendarEvent, width int) []string {
	if ev == nil {
		return []string{hintStyle.Render("Select a meeting to see details")}
	}

	lines := []string{calSubjectStyle.Render(truncateToWidth(sanitizeText(ev.Subject), maxInt(1, width-1)))}

	if ev.IsAllDayEvent {
		lines = append(lines, calMetaStyle.Render("All day"))
	} else if !ev.Start.IsZero() {
		when := ev.Start.In(time.Local).Format("Mon 02 Jan 2006 15:04")
		if !ev.End.IsZero() {
			when += " – " + ev.End.In(time.Local).Format("15:04")
		}
		lines = append(lines, calMetaStyle.Render(when))
	}

	if ev.OrganizerName != "" {
		org := "Organizer: " + sanitizeText(ev.OrganizerName)
		if addr := strings.TrimSpace(ev.OrganizerAddress); addr != "" {
			org += " <" + sanitizeText(addr) + ">"
		}
		lines = append(lines, calMetaStyle.Render(truncateToWidth(org, maxInt(1, width-1))))
	}
	if ev.Location != "" {
		lines = append(lines, calMetaStyle.Render("Location: "+truncateToWidth(sanitizeText(ev.Location), maxInt(1, width-1))))
	}
	if ev.IsOnlineMeeting {
		lines = append(lines, calMetaStyle.Render("Online meeting"))
	}
	if tag := calendarResponseTag(*ev); tag != "" {
		lines = append(lines, tag)
	}
	if link := strings.TrimSpace(ev.SkypeTeamsMeetingURL); link != "" {
		lines = append(lines, calJoinStyle.Render("Join (Enter): "+truncateToWidth(link, maxInt(1, width-1))))
	}
	return lines
}

// calendarEmptyHint is the single message shown when a window has no events.
func calendarEmptyHint(start time.Time) string {
	if start.IsZero() {
		return "No meetings"
	}
	if start.After(time.Now()) {
		return fmt.Sprintf("No meetings after %s", start.In(time.Local).Format("Mon 02 Jan"))
	}
	return "No meetings in this range"
}

// ---------------------------------------------------------------------------
// Key routing
// ---------------------------------------------------------------------------

// handleCalendarKey drives the calendar section: j/k (and arrows) move the
// meeting cursor, g/G jump, pgup/pgdn page, h/l move between the list and the
// detail pane, r reloads, [ / ] shift the day window, and c / Esc / q return
// to chat.
func (m chatModel) handleCalendarKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	u := m.ui()
	cal := &u.cal

	// Global exits.
	switch msg.Type {
	case tea.KeyEscape:
		m.state.leaveCalendar()
		return m, nil
	case tea.KeyCtrlC:
		m.state.requestStop()
		return m, tea.Quit
	}

	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
		switch msg.Runes[0] {
		case 'c':
			m.state.leaveCalendar()
			return m, nil
		case 'q':
			m.state.requestStop()
			return m, tea.Quit
		case 'j':
			cal.down()
			return m, nil
		case 'k':
			cal.up()
			return m, nil
		case 'g':
			cal.home()
			return m, nil
		case 'G':
			cal.end()
			return m, nil
		case 'h':
			m.state.focusComponent(PaneCalendar)
			return m, nil
		case 'l':
			m.state.focusComponent(PaneCalDetail)
			return m, nil
		case 'r':
			return m, m.state.startCalendarLoad()
		case 'd':
			return m, m.state.shiftCalendarWindow(1)
		case 'a':
			return m, m.state.shiftCalendarWindow(-1)
		case 't':
			// Back to today: reset the offset if it moved, then reload.
			if m.state.calendar.windowOffset != 0 {
				m.state.calendar.windowOffset = 0
				return m, m.state.startCalendarLoad()
			}
			return m, nil
		}
	}

	switch msg.Type {
	case tea.KeyUp:
		cal.up()
		return m, nil
	case tea.KeyDown:
		cal.down()
		return m, nil
	case tea.KeyLeft:
		m.state.focusComponent(PaneCalendar)
		return m, nil
	case tea.KeyRight:
		m.state.focusComponent(PaneCalDetail)
		return m, nil
	case tea.KeyPgUp:
		cal.page(-m.calendarPageLines(), m.calendarPageLines())
		return m, nil
	case tea.KeyPgDown:
		cal.page(m.calendarPageLines(), m.calendarPageLines())
		return m, nil
	case tea.KeyTab:
		m.state.focusComponent(PaneCalDetail)
		return m, nil
	case tea.KeyShiftTab:
		m.state.focusComponent(PaneCalendar)
		return m, nil
	case tea.KeyHome:
		cal.home()
		return m, nil
	case tea.KeyEnd:
		cal.end()
		return m, nil
	case tea.KeyEnter:
		// Focus stays on the list; Enter just selects/reveals the row
		// (the join link is shown in the detail pane).
		m.state.focusComponent(PaneCalDetail)
		return m, nil
	case tea.KeyRunes:
		if len(msg.Runes) == 1 {
			switch msg.Runes[0] {
			case '[':
				return m, m.state.shiftCalendarWindow(-1)
			case ']':
				return m, m.state.shiftCalendarWindow(1)
			}
		}
	}

	return m, nil
}

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

// calendarListWidth is the left column width for the meeting list.
func (m chatModel) calendarListWidth() int {
	w := m.width * 45 / 100
	if w < 30 {
		w = minInt(30, maxInt(10, m.width-2))
	}
	if w > m.width-24 {
		w = maxInt(10, m.width-24)
	}
	return w
}

// calendarPageLines is the rendered-row budget for one PgUp/PgDn move and
// for the cursor window clamp.
func (m chatModel) calendarPageLines() int {
	return maxInt(1, m.calendarBodyHeight())
}

// calendarBodyHeight is the list rows' height: terminal minus help, title
// line, and status line, with the focus-box inset.
func (m chatModel) calendarBodyHeight() int {
	h := m.contentHeight() - 1 - 1 - boxInset
	if h < 3 {
		h = 3
	}
	return h
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

// viewCalendar renders the calendar section: a meeting list on the left, the
// selected meeting's details on the right, a title line above, and a status
// line below, all inside the app's frame with the help bar.
func (m chatModel) viewCalendar() string {
	contentHeight := m.contentHeight()
	listWidth := m.calendarListWidth()

	listPane := m.renderCalendarList(listWidth, contentHeight)
	detailWidth := m.width - listWidth - conversationPaneGap
	detailPane := m.renderCalendarDetail(detailWidth, contentHeight)

	top := lipgloss.JoinHorizontal(lipgloss.Top,
		listPane,
		strings.Repeat(" ", conversationPaneGap),
		detailPane,
	)
	frame := lipgloss.JoinVertical(lipgloss.Left, top, m.help.View(m.calendarKeyBindings()))

	if rows := strings.Split(frame, "\n"); len(rows) > m.height && m.height > 0 {
		frame = strings.Join(rows[:m.height], "\n")
	}
	return frame
}

// renderCalendarList draws the meeting list with a title line above and a
// paging/status line below, wrapped in the focus box when it owns focus.
func (m chatModel) renderCalendarList(width, height int) string {
	u := m.ui()
	cal := &u.cal
	focused := u.focus == PaneCalendar

	titleStyle := paneTitleIdle
	if focused {
		titleStyle = paneTitleFocused
	}
	title := truncateToWidth(cal.windowLbl, maxInt(1, width-boxInset-1))

	bodyLines := m.calendarListLines(width - boxInset - 1)
	body := padToHeight(strings.Join(bodyLines, "\n"), m.calendarBodyHeight())

	status := m.calendarListStatus()
	statusLine := truncateToWidth(status, maxInt(1, width-boxInset-1))

	inner := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render(title),
		lipgloss.NewStyle().PaddingLeft(1).Render(body),
		hintStyle.Render(statusLine),
	)
	inner = padToWidth(inner, maxInt(1, width-boxInset))
	return renderFocusedBox(inner, width, height, focused)
}

// calendarEntryLines is the number of frame rows one entry occupies: the
// main line, plus the dim detail line when there is one.
func calendarEntryLines(entry calendarEntry) int {
	if strings.TrimSpace(entry.detailLine) != "" {
		return 2
	}
	return 1
}

// calendarScrollIntoView clamps cal.top (an entry index) so the cursor's
// whole block sits inside the rendered line budget. Rendering emits entries
// from top downward until the budget is spent, so top is backed off from the
// cursor until the block and the rows above it fit. Like the messages
// pane's SetYOffset, this render-time adjustment persists on the shared
// uiState; the key handlers only ever move the cursor.
func (m chatModel) calendarScrollIntoView() {
	cal := &m.ui().cal
	if len(cal.entries) == 0 {
		cal.top = 0
		return
	}
	cal.cursor = clampCursor(cal.cursor, len(cal.entries))
	if cal.top > cal.cursor {
		cal.top = cal.cursor
	}

	body := m.calendarBodyHeight()
	used := calendarEntryLines(cal.entries[cal.cursor])
	top := cal.cursor
	for top > 0 {
		prev := calendarEntryLines(cal.entries[top-1])
		if used+prev > body {
			break
		}
		used += prev
		top--
	}
	cal.top = top
}

// calendarListLines renders the visible meeting rows with the cursor marker.
func (m chatModel) calendarListLines(width int) []string {
	u := m.ui()
	cal := &u.cal

	if cal.loading {
		elapsed := int(time.Since(m.state.calendar.loadingSince).Seconds())
		return []string{m.spinner.View() + " " + hintStyle.Render("Loading calendar…"+fmt.Sprintf(" %ds", elapsed))}
	}
	if cal.errorText != "" && len(cal.entries) == 0 {
		return []string{statusErrorStyle.Render(truncateToWidth(cal.errorText, maxInt(1, width)))}
	}
	if len(cal.entries) == 0 {
		return []string{hintStyle.Render(calendarEmptyHint(m.state.calendar.rangeStart))}
	}

	m.calendarScrollIntoView()

	showCursor := u.focus == PaneCalendar
	body := m.calendarBodyHeight()
	lines := make([]string, 0, minInt(len(cal.entries)-cal.top, body))
	used := 0
	for i := cal.top; i < len(cal.entries); i++ {
		entry := cal.entries[i]
		need := calendarEntryLines(entry)
		if used+need > body {
			break
		}
		used += need
		marker := "  "
		if showCursor && i == cal.cursor {
			marker = cursorStyle.Render("▸ ")
		}
		main := entry.mainLine
		if showCursor && i == cal.cursor {
			main = calSubjectStyle.Render(truncateToWidth(main, maxInt(1, width-2)))
		} else {
			main = truncateToWidth(main, maxInt(1, width-2))
		}
		lines = append(lines, marker+main)
		if detail := truncateToWidth(entry.detailLine, maxInt(1, width-2)); detail != "" {
			lines = append(lines, "  "+calMetaStyle.Render(detail))
		}
	}
	return lines
}

// calendarListStatus is the one-line footer under the list: count + paging.
func (m chatModel) calendarListStatus() string {
	cal := &m.ui().cal
	if cal.loading {
		return "Loading…"
	}
	if cal.errorText != "" {
		return cal.errorText
	}
	total := len(cal.entries)
	count := fmt.Sprintf("%d meeting%s", total, pluralSuffix(total))
	synced := ""
	if !cal.loadedAt.IsZero() {
		synced = " · updated " + cal.loadedAt.In(time.Local).Format("15:04")
	}
	return count + synced + " · a/← prev · d/→ next · r reload · c back"
}

// renderCalendarDetail draws the selected meeting's detail block.
func (m chatModel) renderCalendarDetail(width, height int) string {
	u := m.ui()
	cal := &u.cal
	focused := u.focus == PaneCalDetail

	titleStyle := paneTitleIdle
	if focused {
		titleStyle = paneTitleFocused
	}
	title := truncateToWidth("Details", maxInt(1, width-boxInset-1))

	ev, _ := cal.currentEvent()
	lines := calendarDetailLines(ev, width-boxInset-1)
	body := padToHeight(strings.Join(lines, "\n"), maxInt(1, height-boxInset-1))

	inner := lipgloss.JoinVertical(lipgloss.Left,
		titleStyle.Render(title),
		lipgloss.NewStyle().PaddingLeft(1).Render(body),
	)
	inner = padToWidth(inner, maxInt(1, width-boxInset))
	return renderFocusedBox(inner, width, height, focused)
}

// pluralSuffix returns "s" for anything but exactly one.
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
