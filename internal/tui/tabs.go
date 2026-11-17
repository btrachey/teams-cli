package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The main screen follows gh-dash: a row of section tabs over a rule, a
// sidebar and content area split by a vertical rule, and the key help under
// a closing rule. Unlike gh-dash the sidebar (the list) sits on the left and
// the content (conversation or meeting details) on the right.

// gh-dash default theme colors.
var (
	themeTextPrimary     = lipgloss.AdaptiveColor{Dark: "#ffffff", Light: "#000000"}
	themeTextFaint       = lipgloss.AdaptiveColor{Dark: "#8a8a8a", Light: "#808080"}
	themeSelectedBG      = lipgloss.AdaptiveColor{Dark: "#3a3a3a", Light: "#d0d0d0"}
	themeBorderPrimary   = lipgloss.AdaptiveColor{Dark: "#808080", Light: "#c0c0c0"}
	themeBorderSecondary = lipgloss.AdaptiveColor{Dark: "#c0c0c0", Light: "#808080"}
)

var (
	tabActiveStyle   = lipgloss.NewStyle().Bold(true).Foreground(themeTextPrimary).Background(themeSelectedBG).Padding(0, 1)
	tabInactiveStyle = lipgloss.NewStyle().Foreground(themeTextFaint).Padding(0, 1)
	tabSepStyle      = lipgloss.NewStyle().Foreground(themeBorderSecondary)
	ruleStyle        = lipgloss.NewStyle().Foreground(themeBorderPrimary)
)

// tabBarHeight is the tab row plus the rule under it.
const tabBarHeight = 2

// tabPages lists the tabs left to right.
var tabPages = []appPage{pageMain, pageCalendar}

// tabLabel names a tab, with its item count (unread conversations, loaded
// meetings) when there is one, like gh-dash's section counts.
func (m chatModel) tabLabel(page appPage) string {
	u := m.ui()
	switch page {
	case pageCalendar:
		if !u.cal.loadedAt.IsZero() {
			return fmt.Sprintf("Calendar (%d)", len(u.cal.entries))
		}
		return "Calendar"
	default:
		if unread := unreadItems(m.conversations.Items()); unread > 0 {
			return fmt.Sprintf("Messages (%d)", unread)
		}
		return "Messages"
	}
}

// tabCells renders each tab; renderTabs and tabAt share them so clicks
// land on what is drawn.
func (m chatModel) tabCells() []string {
	cells := make([]string, len(tabPages))
	for i, page := range tabPages {
		style := tabInactiveStyle
		if page == m.ui().page {
			style = tabActiveStyle
		}
		cells[i] = style.Render(m.tabLabel(page))
	}
	return cells
}

// renderTabs draws the tab row, with any flash notice right-aligned, and
// the rule beneath it.
func (m chatModel) renderTabs() string {
	row := " " + strings.Join(m.tabCells(), tabSepStyle.Render("│"))
	if u := m.ui(); u.flash != "" {
		style := statusSuccessStyle
		if u.flashErr {
			style = statusErrorStyle
		}
		room := m.width - lipgloss.Width(row) - 3
		if room > 0 {
			flash := style.Render(truncateToWidth(u.flash, room))
			row += strings.Repeat(" ", max(1, m.width-lipgloss.Width(row)-lipgloss.Width(flash)-1)) + flash
		}
	}
	return padToWidth(row, m.width) + "\n" + m.rule()
}

// tabAt maps a column on the tab row to its tab.
func (m chatModel) tabAt(x int) (appPage, bool) {
	left := 1
	for i, cell := range m.tabCells() {
		right := left + lipgloss.Width(cell)
		if x >= left && x < right {
			return tabPages[i], true
		}
		left = right + 1 // the separator
	}
	return 0, false
}

// switchTab shows the messages or calendar tab (gh-dash's s, h/l).
func (m chatModel) switchTab(page appPage) {
	if m.ui().page == page {
		return
	}
	if page == pageCalendar {
		m.state.ui.focus = PaneCalendar
		m.state.enterCalendar()
		return
	}
	m.state.leaveCalendar()
	m.state.ui.focus = PaneTree
}

// rule is a full-width horizontal border line.
func (m chatModel) rule() string {
	return ruleStyle.Render(strings.Repeat("─", max(0, m.width)))
}

// divider is the vertical border between the sidebar and the content.
func divider(height int) string {
	line := ruleStyle.Render("│")
	return strings.TrimSuffix(strings.Repeat(line+"\n", max(1, height)), "\n")
}
