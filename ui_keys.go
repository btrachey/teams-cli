package main

import (
	"github.com/charmbracelet/bubbles/key"
)

// Key bindings for the app. Movement is vim-first (j/k, g/G) with arrows as
// aliases; h/l move focus between panes so the composer can keep h/l as
// text.
var (
	keyUp      = key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up"))
	keyDown    = key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down"))
	keyPageUp  = key.NewBinding(key.WithKeys("pgup", "ctrl+u"), key.WithHelp("pgup", "page up"))
	keyPageDn  = key.NewBinding(key.WithKeys("pgdown", "ctrl+d"), key.WithHelp("pgdn", "page down"))
	keyTop     = key.NewBinding(key.WithKeys("home", "g"), key.WithHelp("g", "top"))
	keyBottom  = key.NewBinding(key.WithKeys("end", "G"), key.WithHelp("G", "bottom"))
	keyOpen    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "read"))
	keyBack    = key.NewBinding(key.WithKeys("esc", "tab"), key.WithHelp("esc/tab", "back"))
	keyFilter  = key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter"))
	keyCompose = key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "message"))
	keyReply   = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reply"))
	keySend    = key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send"))
	keyNewline = key.NewBinding(key.WithKeys("alt+enter"), key.WithHelp("alt+enter", "newline"))
	keyHelp    = key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help"))
	keyQuit    = key.NewBinding(key.WithKeys("q"), key.WithHelp("q", "quit"))

	keyCalendar  = key.NewBinding(key.WithKeys("c"), key.WithHelp("c", "calendar"))
	keyCalPrev   = key.NewBinding(key.WithKeys("a", "["), key.WithHelp("a", "earlier"))
	keyCalNext   = key.NewBinding(key.WithKeys("d", "]"), key.WithHelp("d", "later"))
	keyCalToday  = key.NewBinding(key.WithKeys("t"), key.WithHelp("t", "today"))
	keyCalReload = key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "reload"))
	keyCalBack   = key.NewBinding(key.WithKeys("esc", "c"), key.WithHelp("esc", "back"))
	keyCalLeft   = key.NewBinding(key.WithKeys("h", "left"), key.WithHelp("h", "list"))
	keyCalRight  = key.NewBinding(key.WithKeys("l", "right"), key.WithHelp("l", "details"))
)

// uiKeys is the help.KeyMap the help component renders. The visible bindings
// follow the focused pane (and, on the calendar page, the calendar mode).
type uiKeys struct {
	focus string
	page  appPage
}

func (k uiKeys) ShortHelp() []key.Binding {
	if k.page == pageCalendar {
		return []key.Binding{keyUp, keyDown, keyCalPrev, keyCalNext, keyCalReload, keyCalBack, keyHelp}
	}
	switch k.focus {
	case PaneMessages:
		return []key.Binding{keyUp, keyDown, keyPageUp, keyCompose, keyReply, keyBack, keyHelp}
	case PaneComposer:
		return []key.Binding{keySend, keyNewline, keyBack}
	default:
		return []key.Binding{keyUp, keyDown, keyOpen, keyFilter, keyCalendar, keyHelp, keyQuit}
	}
}

func (k uiKeys) FullHelp() [][]key.Binding {
	if k.page == pageCalendar {
		return [][]key.Binding{
			{keyUp, keyDown, keyPageUp, keyPageDn, keyTop, keyBottom},
			{keyCalPrev, keyCalNext, keyCalToday, keyCalReload},
			{keyCalLeft, keyCalRight, keyCalBack, keyHelp},
		}
	}
	return [][]key.Binding{
		{keyUp, keyDown, keyPageUp, keyPageDn, keyTop, keyBottom},
		{keyOpen, keyFilter, keyBack},
		{keyCompose, keyReply, keySend, keyNewline},
		{keyCalendar, keyHelp, keyQuit},
	}
}

// keyBindings returns the help map for the current focus.
func (m chatModel) keyBindings() uiKeys {
	return uiKeys{focus: m.ui().focus, page: m.ui().page}
}

// calendarKeyBindings returns the help map for the calendar page.
func (m chatModel) calendarKeyBindings() uiKeys {
	return uiKeys{focus: m.ui().focus, page: pageCalendar}
}
