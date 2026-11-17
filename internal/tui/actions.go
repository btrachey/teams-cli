package tui

import (
	"os/exec"
	"runtime"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
)

// Actions shared by the tabs, bound to gh-dash's keys: o opens the selected
// item, y/Y copy its ID/link, p toggles the preview, r/R refresh.

// actionResultMsg reports the outcome of an open or copy command; ok is the
// notice shown on success.
type actionResultMsg struct {
	ok  string
	err error
}

// setFlash shows a one-line notice in the tab row until the next key.
func (u *uiState) setFlash(text string, isErr bool) {
	u.flash = truncateComposerStatus(text)
	u.flashErr = isErr
}

// openURLCmd opens a link with the platform's URL handler off the UI loop.
func openURLCmd(link string) tea.Cmd {
	return func() tea.Msg {
		opener := "xdg-open"
		if runtime.GOOS == "darwin" {
			opener = "open"
		}
		if err := exec.Command(opener, link).Run(); err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{ok: "Opened " + link}
	}
}

// copyCmd puts text on the system clipboard; what names it in the notice.
func copyCmd(text, what string) tea.Cmd {
	return func() tea.Msg {
		if err := clipboard.WriteAll(text); err != nil {
			return actionResultMsg{err: err}
		}
		return actionResultMsg{ok: "Copied " + what}
	}
}

// selectedLink is the Teams link for the selected item: the highlighted
// message when the messages pane has focus, else the conversation.
func (m chatModel) selectedLink() (link, id string, ok bool) {
	u := m.ui()
	target, loaded, _, _ := m.state.currentConversationSnapshot()
	if u.focus == PaneMessages && loaded {
		if message, ok := m.state.messageAtCurrentIndex(); ok {
			return m.state.teams.Link(target.ID, message.Id), message.Id, true
		}
	}
	if item, ok := m.selectedConversationItem(); ok {
		return m.state.teams.Link(item.target.ID, ""), item.target.ID, true
	}
	return "", "", false
}

// togglePreview hides or shows the content pane; the sidebar takes the full
// width while it is hidden and keeps focus.
func (m *chatModel) togglePreview() {
	u := m.ui()
	u.previewHidden = !u.previewHidden
	if u.previewHidden {
		switch u.focus {
		case PaneMessages, PaneComposer:
			u.focus = PaneTree
			m.composer.Blur()
		case PaneCalDetail:
			u.focus = PaneCalendar
		}
	}
	m.resizeComponents()
}

// showPreview brings a hidden content pane back (Enter or Tab into it).
func (m *chatModel) showPreview() {
	if m.ui().previewHidden {
		m.togglePreview()
	}
}

// refreshAll reruns every load: the conversation list, the open
// conversation, and the calendar when it has been opened.
func (m chatModel) refreshAll() tea.Cmd {
	go m.state.refreshConversationTree()
	if !m.ui().cal.loadedAt.IsZero() || m.ui().cal.loading {
		m.state.startCalendarLoad()
	}
	return m.state.reloadConversationCmd()
}
