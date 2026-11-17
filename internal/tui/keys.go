package tui

import (
	"github.com/charmbracelet/bubbles/key"
)

// Key bindings for the app follow gh-dash: vim movement (j/k, g/G),
// ctrl+d/ctrl+u to page the preview, s or h/l to switch tabs, p to toggle
// the preview, r/R to refresh, o to open, y/Y to copy, c to comment (write
// or reply), / to filter, ? for help. Tab moves focus between the sidebar
// and the content, which gh-dash does not need.
var (
	keyUp         = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up"))
	keyDown       = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down"))
	keyPageUp     = key.NewBinding(key.WithKeys("ctrl+u", "pgup"), key.WithHelp("ctrl+u", "page up"))
	keyPageDn     = key.NewBinding(key.WithKeys("ctrl+d", "pgdown"), key.WithHelp("ctrl+d", "page down"))
	keyTop        = key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "first"))
	keyBottom     = key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "last"))
	keySwitch     = key.NewBinding(key.WithKeys("s", "h", "l", "left", "right"), key.WithHelp("s/h/l", "switch tab"))
	keyPreview    = key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "toggle preview"))
	keyRefresh    = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "refresh"))
	keyRefreshAll = key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh all"))
	keyOpen       = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "read"))
	keyBrowse     = key.NewBinding(key.WithKeys("o"), key.WithHelp("o", "open in Teams"))
	keyCopyID     = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy ID"))
	keyCopyURL    = key.NewBinding(key.WithKeys("Y"), key.WithHelp("Y", "copy link"))
	keyFocus      = key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "focus pane"))
	keyBack       = key.NewBinding(key.WithKeys("esc", "tab"), key.WithHelp("esc", "back"))
	keyFilter     = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter"))
	keyComment    = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "message"))
	keyReply      = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "reply"))
	keySend       = key.NewBinding(key.WithKeys("ctrl+d", "enter"), key.WithHelp("ctrl+d/enter", "send"))
	keyNewline    = key.NewBinding(key.WithKeys("alt+enter"), key.WithHelp("alt+enter", "newline"))
	keyCancel     = key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel"))
	keyHelp       = key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help"))
	keyQuit       = key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit"))

	keyCalPrev    = key.NewBinding(key.WithKeys("a", "["), key.WithHelp("a/[", "earlier"))
	keyCalNext    = key.NewBinding(key.WithKeys("d", "]"), key.WithHelp("d/]", "later"))
	keyCalToday   = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "today"))
	keyCalJoin    = key.NewBinding(key.WithKeys("enter", "o"), key.WithHelp("o", "join"))
	keyCalCopy    = key.NewBinding(key.WithKeys("y"), key.WithHelp("y", "copy title"))
	keyCalCopyURL = key.NewBinding(key.WithKeys("Y"), key.WithHelp("Y", "copy join link"))
)

// uiKeys is the help.KeyMap the help component renders. The visible bindings
// follow the focused pane (and, on the calendar tab, the calendar's).
type uiKeys struct {
	focus string
	page  appPage
}

func (k uiKeys) ShortHelp() []key.Binding {
	if k.page == pageCalendar {
		return []key.Binding{keyUp, keyDown, keyCalJoin, keyCalPrev, keyCalNext, keyRefresh, keySwitch, keyHelp}
	}
	switch k.focus {
	case PaneMessages:
		return []key.Binding{keyUp, keyDown, keyReply, keyPageDn, keyBrowse, keyBack, keyHelp}
	case PaneComposer:
		return []key.Binding{keySend, keyNewline, keyCancel}
	default:
		return []key.Binding{keyUp, keyDown, keyOpen, keyComment, keyFilter, keySwitch, keyHelp, keyQuit}
	}
}

func (k uiKeys) FullHelp() [][]key.Binding {
	if k.page == pageCalendar {
		return [][]key.Binding{
			{keyUp, keyDown, keyPageDn, keyPageUp, keyTop, keyBottom},
			{keySwitch, keyPreview, keyFocus, keyRefresh, keyRefreshAll},
			{keyCalJoin, keyCalCopy, keyCalCopyURL},
			{keyCalPrev, keyCalNext, keyCalToday, keyHelp, keyQuit},
		}
	}
	return [][]key.Binding{
		{keyUp, keyDown, keyPageDn, keyPageUp, keyTop, keyBottom},
		{keySwitch, keyPreview, keyFocus, keyFilter, keyRefresh, keyRefreshAll},
		{keyOpen, keyBrowse, keyCopyID, keyCopyURL, keyComment, keyReply},
		{keySend, keyNewline, keyCancel, keyHelp, keyQuit},
	}
}

// keyBindings returns the help map for the current focus.
func (m chatModel) keyBindings() uiKeys {
	return uiKeys{focus: m.ui().focus, page: m.ui().page}
}
