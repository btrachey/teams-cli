package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/fossteams/teams-cli/internal/calendar"
)

// calendarModel owns the calendar list pane state: the rendered event rows,
// the cursor, and the scroll window. It mirrors messagesModel's cursor
// semantics (the cursor is the selection; rendering scrolls it into view).
type calendarModel struct {
	entries   []calendarEntry
	cursor    int
	top       int
	loading   bool
	loadedAt  time.Time
	windowLbl string
	// errorText carries a load failure message; empty when healthy.
	errorText string
}

// calendarEntry is one rendered meeting row.
type calendarEntry struct {
	// mainLine is the bold subject/time line; detailLine the organizer/
	// location/status line. Placeholder rows carry no event.
	mainLine   string
	detailLine string
	event      *calendar.Event
	// day labels the meeting's local day; dayStart marks the first
	// meeting of that day, which draws the day divider above itself.
	day      string
	dayStart bool
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
	calDayStyle      = lipgloss.NewStyle().Bold(true).Foreground(themeTextPrimary)
)

// apply replaces the pane with freshly loaded events, placing the cursor on
// the next upcoming meeting (or the first row).
func (m *calendarModel) apply(events []calendar.Event, syncedAt time.Time, start, end time.Time) {
	entries := make([]calendarEntry, 0, len(events))
	now := time.Now()
	previousDay := ""
	for i := range events {
		ev := events[i]
		day := calendarDayLabel(ev.Start, now)
		entries = append(entries, calendarEntry{
			mainLine:   calendarMainLine(ev),
			detailLine: calendarDetailLine(ev),
			event:      &ev,
			day:        day,
			dayStart:   day != "" && day != previousDay,
		})
		previousDay = day
	}

	m.entries = entries
	m.loadedAt = syncedAt
	m.errorText = ""
	m.loading = false
	m.windowLbl = calendarWindowLabel(start, end)
	m.top = 0
	m.setCursor(calendarDefaultCursor(entries, time.Now()))
}

// applyError records a load failure, keeping any previously loaded rows.
func (m *calendarModel) applyError(err error, start, end time.Time) {
	m.errorText = truncateComposerStatus("Calendar load failed: " + err.Error())
	m.windowLbl = calendarWindowLabel(start, end)
	m.loading = false
}

// applyLoading shows the loading placeholder for a window.
func (m *calendarModel) applyLoading(start, end time.Time) {
	m.loading = true
	m.errorText = ""
	m.windowLbl = calendarWindowLabel(start, end)
}

func (m *calendarModel) setCursor(index int) {
	m.cursor = clampCursor(index, len(m.entries))
	m.top = min(m.top, m.cursor)
}

func (m *calendarModel) up()   { m.setCursor(m.cursor - 1) }
func (m *calendarModel) down() { m.setCursor(m.cursor + 1) }

// entryLines is the number of frame rows entry i occupies when the list
// starts at entry top: the main line, the dim detail line when there is one,
// and the day divider for the first meeting of a day, or for the top row
// so the visible day is always labelled.
func (m *calendarModel) entryLines(i, top int) int {
	entry := m.entries[i]
	lines := 1
	if strings.TrimSpace(entry.detailLine) != "" {
		lines++
	}
	if entry.day != "" && (entry.dayStart || i == top) {
		lines++
	}
	return lines
}

// page moves the window (and cursor) by roughly deltaLines rendered rows,
// the PgUp/PgDn semantics of a scrolling list. Entries span 1–3 lines, so
// the walk counts entryLines rather than entry indexes.
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
			used += m.entryLines(target, m.top)
			target++
		}
		// The budget can outlive the entries; top must stay a valid entry
		// index or currentEvent resolves nothing between the keypress and
		// the render-time clamp.
		target = min(target, len(m.entries)-1)
	} else if deltaLines < 0 {
		for target > 0 && used < -deltaLines {
			target--
			used += m.entryLines(target, m.top)
		}
	}
	m.top = target

	// The cursor follows into the new window: clamp it between top and the
	// last entry the window can render.
	end := target
	used = 0
	for end < len(m.entries) {
		next := used + m.entryLines(end, target)
		if next > bodyLines {
			break
		}
		used = next
		end++
	}
	windowEnd := max(target, end-1)
	m.cursor = max(target, min(m.cursor, windowEnd))
}

func (m *calendarModel) home() { m.setCursor(0); m.top = 0 }
func (m *calendarModel) end()  { m.setCursor(len(m.entries) - 1) }

// currentEvent returns the meeting under the cursor.
func (m *calendarModel) currentEvent() (*calendar.Event, bool) {
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
func calendarMainLine(ev calendar.Event) string {
	when := calendarWhen(ev)
	subject := strings.TrimSpace(sanitizeText(ev.Subject))
	if subject == "" {
		subject = "(no title)"
	}
	return when + "  " + subject
}

// calendarDetailLine renders the dim organizer/location/status line.
func calendarDetailLine(ev calendar.Event) string {
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
func calendarWhen(ev calendar.Event) string {
	if ev.IsAllDayEvent || ev.Start.IsZero() {
		return calAllDayStyle.Render("all-day")
	}
	start := ev.Start.In(time.Local)
	end := ev.End.In(time.Local)
	timeText := start.Format("15:04")
	if !end.IsZero() && !end.Equal(start) {
		timeText = start.Format("15:04") + "–" + end.Format("15:04")
	}
	// The day divider above the row carries the date.
	return calTimeStyle.Render(timeText)
}

// calendarDayLabel names a meeting's local day for its divider ("Today ·
// Tue 10 Nov"); empty when the start is unknown.
func calendarDayLabel(start, now time.Time) string {
	if start.IsZero() {
		return ""
	}
	day := start.In(time.Local)
	label := day.Format("Mon 02 Jan")
	today := now.In(time.Local)
	switch {
	case sameDay(day, today):
		return "Today · " + label
	case sameDay(day, today.AddDate(0, 0, 1)):
		return "Tomorrow · " + label
	case sameDay(day, today.AddDate(0, 0, -1)):
		return "Yesterday · " + label
	}
	return label
}

func sameDay(a, b time.Time) bool {
	return a.Year() == b.Year() && a.YearDay() == b.YearDay()
}

// calendarDayDivider renders the rule that opens a day in the list.
func calendarDayDivider(label string, width int) string {
	head := ruleStyle.Render("── ") + calDayStyle.Render(truncateToWidth(label, max(1, width-4))) + " "
	return head + ruleStyle.Render(strings.Repeat("─", max(0, width-lipgloss.Width(head))))
}

// calendarResponseTag shows the RSVP state and privacy/cancellation markers.
func calendarResponseTag(ev calendar.Event) string {
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

// selectedJoinURL returns the Teams join link of the meeting under the
// cursor, when it has one.
func (m *calendarModel) selectedJoinURL() (string, bool) {
	ev, ok := m.currentEvent()
	if !ok {
		return "", false
	}
	link := strings.TrimSpace(ev.SkypeTeamsMeetingURL)
	return link, strings.HasPrefix(link, "https://")
}

// calendarDetailLines renders the right-hand detail block for the selected
// event: full times, organizer, location, meeting link, RSVP.
func calendarDetailLines(ev *calendar.Event, width int) []string {
	if ev == nil {
		return []string{hintStyle.Render("Select a meeting to see details")}
	}

	lines := []string{calSubjectStyle.Render(truncateToWidth(sanitizeText(ev.Subject), max(1, width-1)))}

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
		lines = append(lines, calMetaStyle.Render(truncateToWidth(org, max(1, width-1))))
	}
	if ev.Location != "" {
		lines = append(lines, calMetaStyle.Render("Location: "+truncateToWidth(sanitizeText(ev.Location), max(1, width-1))))
	}
	if ev.IsOnlineMeeting {
		lines = append(lines, calMetaStyle.Render("Online meeting"))
	}
	if tag := calendarResponseTag(*ev); tag != "" {
		lines = append(lines, tag)
	}
	if link := strings.TrimSpace(ev.SkypeTeamsMeetingURL); link != "" {
		lines = append(lines, calJoinStyle.Render("Join (Enter): "+truncateToWidth(link, max(1, width-1))))
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

// handleCalendarKey drives the calendar tab with gh-dash's keys: j/k (and
// arrows) move the meeting cursor, g/G jump, ctrl+d/ctrl+u (pgdn/pgup) page,
// h/l and s switch tabs, Tab moves between the list and the details, Enter
// or o opens the selected meeting's join link, y/Y copy its subject/link,
// p toggles the details, r reloads, R refreshes everything, a/d ([ / ])
// shift the day window, t returns to today, and ? toggles help.
func (m chatModel) handleCalendarKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	cal := &m.ui().cal

	key := msg.String()
	if msg.Type == tea.KeyRunes && len(msg.Runes) != 1 {
		// A pasted batch of runes is never a shortcut.
		return m, nil
	}

	switch key {
	case "q":
		m.state.requestStop()
		return m, tea.Quit
	case "?":
		m.help.ShowAll = !m.help.ShowAll
	case "esc", "1", "s", "h", "l", "left", "right":
		m.switchTab(pageMain)
	case "j", "down":
		cal.down()
	case "k", "up":
		cal.up()
	case "g", "home":
		cal.home()
	case "G", "end":
		cal.end()
	case "pgup", "ctrl+u":
		cal.page(-m.calendarBodyHeight(), m.calendarBodyHeight())
	case "pgdown", "ctrl+d":
		cal.page(m.calendarBodyHeight(), m.calendarBodyHeight())
	case "tab", "shift+tab":
		if m.ui().focus == PaneCalDetail {
			m.state.focusComponent(PaneCalendar)
		} else {
			m.showPreview()
			m.state.focusComponent(PaneCalDetail)
		}
	case "p":
		m.togglePreview()
	case "enter", "o":
		if link, ok := cal.selectedJoinURL(); ok {
			return m, openURLCmd(link)
		}
		m.ui().setFlash("This meeting has no join link.", true)
	case "y":
		if ev, ok := cal.currentEvent(); ok {
			return m, copyCmd(ev.Subject, "meeting title")
		}
	case "Y":
		if link, ok := cal.selectedJoinURL(); ok {
			return m, copyCmd(link, "join link")
		}
		m.ui().setFlash("This meeting has no join link.", true)
	case "r":
		m.state.startCalendarLoad()
	case "R":
		return m, m.refreshAll()
	case "d", "]":
		m.state.shiftCalendarWindow(1)
	case "a", "[":
		m.state.shiftCalendarWindow(-1)
	case "t":
		if m.state.calendar.windowOffset != 0 {
			m.state.calendar.windowOffset = 0
			m.state.startCalendarLoad()
		}
	}
	return m, nil
}

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

// calendarListWidth is the left column width for the meeting list.
func (m chatModel) calendarListWidth() int {
	if m.ui().previewHidden {
		return m.width
	}
	w := m.width * 45 / 100
	if w < 30 {
		w = min(30, max(10, m.width-2))
	}
	if w > m.width-24 {
		w = max(10, m.width-24)
	}
	return w
}

// calendarBodyHeight is the list rows' height (and the PgUp/PgDn budget):
// terminal minus help, title line, and status line, with the focus-box
// inset.
func (m chatModel) calendarBodyHeight() int {
	return max(3, m.contentHeight()-1-1-boxInset)
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
	list := m.renderCalendarList(listWidth, contentHeight)
	if m.ui().previewHidden {
		return m.frame(list, "")
	}
	detailWidth := m.width - listWidth - conversationPaneGap
	return m.frame(list, m.renderCalendarDetail(detailWidth, contentHeight))
}

// renderCalendarList draws the meeting list with a title line above and a
// paging/status line below, wrapped in the focus box when it owns focus.
func (m chatModel) renderCalendarList(width, height int) string {
	u := m.ui()
	focused := u.focus == PaneCalendar
	innerWidth := max(1, width-boxInset-1)

	header := paneTitleStyle(focused).Render(truncateToWidth(u.cal.windowLbl, innerWidth))
	body := padToHeight(strings.Join(m.calendarListLines(innerWidth), "\n"), m.calendarBodyHeight())
	status := hintStyle.Render(truncateToWidth(m.calendarListStatus(), innerWidth))
	return renderTitledPane(header, body+"\n"+status, width, height, focused)
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
	top := cal.cursor
	used := cal.entryLines(top, top)
	for top > 0 {
		// Moving the window up one entry: that entry joins as the new top
		// and the old top may lose its sticky day divider.
		next := used - cal.entryLines(top, top) + cal.entryLines(top, top-1) + cal.entryLines(top-1, top-1)
		if next > body {
			break
		}
		used = next
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
		return []string{statusErrorStyle.Render(truncateToWidth(cal.errorText, max(1, width)))}
	}
	if len(cal.entries) == 0 {
		return []string{hintStyle.Render(calendarEmptyHint(m.state.calendar.rangeStart))}
	}

	m.calendarScrollIntoView()

	showCursor := u.focus == PaneCalendar
	body := m.calendarBodyHeight()
	lines := make([]string, 0, min(len(cal.entries)-cal.top, body))
	used := 0
	for i := cal.top; i < len(cal.entries); i++ {
		entry := cal.entries[i]
		need := cal.entryLines(i, cal.top)
		if used+need > body {
			break
		}
		used += need
		if entry.day != "" && (entry.dayStart || i == cal.top) {
			lines = append(lines, calendarDayDivider(entry.day, width))
		}
		marker := "  "
		if showCursor && i == cal.cursor {
			marker = cursorStyle.Render("▸ ")
		}
		main := entry.mainLine
		if showCursor && i == cal.cursor {
			main = calSubjectStyle.Render(truncateToWidth(main, max(1, width-2)))
		} else {
			main = truncateToWidth(main, max(1, width-2))
		}
		lines = append(lines, marker+main)
		if detail := truncateToWidth(entry.detailLine, max(1, width-2)); detail != "" {
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
	return count + synced + " · a/[ prev · d/] next · r reload"
}

// renderCalendarDetail draws the selected meeting's detail block.
func (m chatModel) renderCalendarDetail(width, height int) string {
	u := m.ui()
	focused := u.focus == PaneCalDetail
	innerWidth := max(1, width-boxInset-1)

	header := paneTitleStyle(focused).Render(truncateToWidth("Details", innerWidth))
	ev, _ := u.cal.currentEvent()
	body := padToHeight(strings.Join(calendarDetailLines(ev, innerWidth), "\n"), max(1, height-boxInset-1))
	return renderTitledPane(header, body, width, height, focused)
}

// pluralSuffix returns "s" for anything but exactly one.
func pluralSuffix(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
