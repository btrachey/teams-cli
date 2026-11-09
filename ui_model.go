package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fossteams/teams-api/pkg/csa"
)

// appPage identifies which full-screen view the UI shows.
type appPage int

const (
	pageLogin appPage = iota
	pageMain
	pageCalendar
	pageError
)

// Composer status kinds.
const (
	statusNone = iota
	statusInfo
	statusSuccess
	statusError
)

// UI messages. Background goroutines deliver state changes to the Bubble Tea
// event loop through AppState.sendUIMsg; Update is the only place that
// mutates UI state.
type (
	messagesLoadedMsg struct {
		target            ConversationTarget
		messages          []csa.ChatMessage
		syncedAt          time.Time
		loadSeq           uint64
		preserveSelection bool
	}
	messagesLoadErrorMsg struct {
		target  ConversationTarget
		err     error
		loadSeq uint64
	}
	liveMessagesMsg struct {
		target            ConversationTarget
		messages          []csa.ChatMessage
		syncedAt          time.Time
		previousSignature string
	}
	liveMessagesRetryMsg struct {
		target ConversationTarget
		at     time.Time
	}
	treeRefreshedMsg struct {
		data *conversationData
	}
	sendResultMsg struct {
		target ConversationTarget
		err    error
	}
	calendarLoadedMsg struct {
		events   []CalendarEvent
		loadSeq  uint64
		syncedAt time.Time
		start    time.Time
		end      time.Time
	}
	calendarLoadErrorMsg struct {
		err     error
		loadSeq uint64
		start   time.Time
		end     time.Time
	}
	loginStatusMsg string
	errorPageMsg   struct {
		message string
	}
	mainReadyMsg  struct{}
	loadingTickMs struct {
		loadSeq uint64
		step    int
	}
	clockTickMsg struct{}
)

// sendUIMsg hands a message to the Bubble Tea loop. It is replaced when the
// program is created; tests can capture messages instead.
func (s *AppState) sendUIMsg(msg tea.Msg) {
	if s.send != nil {
		s.send(msg)
	}
}

// uiState holds everything the renderer reads. It lives on AppState so the
// business logic and the Bubble Tea model share one owner; only the Update
// loop mutates it during normal operation.
type uiState struct {
	page  appPage
	focus string // PaneTree | PaneMessages | PaneComposer

	loginMessage string
	errorMessage string

	msgs messagesModel

	// cal holds the calendar section's list/detail state.
	cal calendarModel

	// images holds rendered image art keyed by source URL; imageErrors
	// marks URLs that could not be downloaded or decoded.
	images          map[string]string
	imageNative     map[string]string
	imageNativeRows map[string]int
	imageErrors     map[string]bool

	composerText       string
	composerStatus     string
	composerStatusKind int
}

// chatModel is the Bubble Tea model. All TeamsState/HTTP work stays on
// AppState; the model owns rendering and key routing.
//
// The panes are the standard bubbles components: a list for conversations,
// a viewport for the conversation, a textarea for composing, and a help
// component for the footer key bindings.
type chatModel struct {
	state   *AppState
	width   int
	height  int
	spinner spinner.Model

	conversations list.Model
	messages      viewport.Model
	composer      textarea.Model
	help          help.Model

	// composerSynced is the last textarea value mirrored into
	// uiState.composerText; the pair detects external draft changes
	// (send success, conversation switch) without echoing them back.
	composerSynced string
}

func newChatModel(state *AppState) chatModel {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))

	composer := textarea.New()
	composer.Placeholder = "Send a message..."
	composer.MaxHeight = composerContentRows
	composer.ShowLineNumbers = false
	composer.SetHeight(composerContentRows)
	composer.Prompt = ""
	composer.Cursor.Style = composerCursorStyle
	composer.Cursor.TextStyle = lipgloss.NewStyle()
	focused := textarea.Style{
		Base:        lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
		CursorLine:  lipgloss.NewStyle(),
		EndOfBuffer: lipgloss.NewStyle().Foreground(lipgloss.Color("0")),
		Placeholder: placeholderStyle,
		Prompt:      composerPromptStyle,
		Text:        lipgloss.NewStyle().Foreground(lipgloss.Color("252")),
	}
	blurred := focused
	blurred.CursorLine = lipgloss.NewStyle()
	composer.FocusedStyle = focused
	composer.BlurredStyle = blurred

	vp := viewport.New(0, 0)
	vp.KeyMap = viewport.KeyMap{
		PageDown:     key.NewBinding(key.WithKeys("pgdown")),
		PageUp:       key.NewBinding(key.WithKeys("pgup")),
		HalfPageUp:   key.NewBinding(key.WithKeys("ctrl+u")),
		HalfPageDown: key.NewBinding(key.WithKeys("ctrl+d")),
		Up:           key.NewBinding(key.WithKeys("up", "k")),
		Down:         key.NewBinding(key.WithKeys("down", "j")),
		Left:         key.NewBinding(),
		Right:        key.NewBinding(),
	}

	h := help.New()
	h.ShowAll = false

	return chatModel{
		state:         state,
		spinner:       sp,
		conversations: newConversationList(),
		messages:      vp,
		composer:      composer,
		help:          h,
		width:         80,
		height:        24,
	}
}

func (m chatModel) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, clockTickCmd())
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Draft sync and focus reconciliation run before any other branch:
	// external changes to uiState.composerText (send success, conversation
	// switch) land on the value copy, blink/key commands must never
	// observe a stale textarea, and Focus() returns the blink command
	// that has to reach the event loop.
	m, syncCmd := m.syncComposer()
	m = m.syncMessagesViewport()

	model, cmd := m.updateInternal(msg)
	switch {
	case syncCmd != nil && cmd != nil:
		return model, tea.Batch(syncCmd, cmd)
	case syncCmd != nil:
		return model, syncCmd
	default:
		return model, cmd
	}
}

func (m chatModel) updateInternal(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		widthChanged := m.width != msg.Width
		m.width = msg.Width
		m.height = msg.Height
		m.help.Width = msg.Width
		m.resizeComponents()
		if widthChanged {
			// Rendered image art is sized for the old pane width; drop it
			// and re-render from the cached bytes at the new width.
			u := m.ui()
			u.images = map[string]string{}
			u.imageNative = map[string]string{}
			u.imageNativeRows = map[string]int{}
			m.state.startMessageImageFetches(u.msgs.entries, m.imageMaxCols())
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case cursor.BlinkMsg:
		// The textarea drives its own caret blink while focused.
		if m.ui().focus == PaneComposer {
			var cmd tea.Cmd
			m.composer, cmd = m.composer.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.FocusMsg:
		if m.ui().focus == PaneComposer {
			var cmd tea.Cmd
			m.composer, cmd = m.composer.Update(msg)
			return m, cmd
		}
		return m, nil

	case tea.BlurMsg:
		m.composer.Blur()
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

	case loadingTickMs:
		u := m.ui()
		if u.msgs.loading && m.state.activeLoadIs(msg.loadSeq) {
			mainText, secondaryText := loadingStatusText(m.state.messageLimit, u.msgs.loadingSince)
			if len(u.msgs.entries) > 0 {
				u.msgs.entries[0].Main = m.spinner.View() + " " + mainText
				u.msgs.entries[0].Second = secondaryText
			}
			return m, loadingTickCmd(msg.loadSeq, msg.step+1)
		}
		return m, nil

	case clockTickMsg:
		// Repaint once a second so the live-sync title stays fresh.
		return m, clockTickCmd()

	case loginStatusMsg:
		u := m.ui()
		u.page = pageLogin
		u.loginMessage = string(msg)
		return m, nil

	case errorPageMsg:
		u := m.ui()
		u.page = pageError
		u.errorMessage = msg.message
		return m, nil

	case mainReadyMsg:
		u := m.ui()
		u.page = pageMain
		u.focus = PaneTree
		listCmd := m.refreshConversationItems()
		return m, tea.Batch(listCmd, m.syncConversationSelection())

	case sendResultMsg:
		return m.handleSendResult(msg)

	case messagesLoadedMsg:
		if m.state.staleLoad(msg.loadSeq) || !m.state.currentConversationIs(msg.target.ID) {
			return m, nil
		}
		m.state.rememberConversationSync(msg.target, messagesSignature(msg.messages), msg.syncedAt)
		m.ui().msgs.apply(m.state, msg.target, msg.messages, msg.syncedAt, "", msg.preserveSelection)
		m.state.noteConversationLastMessage(msg.target.ID, newestMessageTime(msg.messages))
		// The messages are on screen now: the unread dot clears even though
		// Teams' own isRead flag flips later.
		m.state.noteConversationOpened(msg.target.ID, newestMessageTime(msg.messages))
		m.state.startMessageImageFetches(m.ui().msgs.entries, m.imageMaxCols())
		return m, m.refreshConversationItems()

	case messagesLoadErrorMsg:
		if m.state.staleLoad(msg.loadSeq) || !m.state.currentConversationIs(msg.target.ID) {
			return m, nil
		}
		m.ui().msgs.applyError(m.state, msg.target, msg.err)
		return m, nil

	case liveMessagesMsg:
		if !m.state.currentConversationIs(msg.target.ID) {
			return m, nil
		}
		signature := messagesSignature(msg.messages)
		newest := newestMessageTime(msg.messages)
		m.state.rememberConversationSync(msg.target, signature, msg.syncedAt)
		m.state.noteConversationLastMessage(msg.target.ID, newest)
		m.state.startMessageImageFetches(m.ui().msgs.entries, m.imageMaxCols())
		u := m.ui()
		if signature == msg.previousSignature {
			u.msgs.paneTitle = conversationPaneTitle(msg.target, time.Time{}, m.state.conversationSyncStatus(msg.syncedAt))
			return m, m.refreshConversationItems()
		}
		u.msgs.apply(m.state, msg.target, msg.messages, msg.syncedAt, "", true)
		// New messages landed in the conversation the user is watching:
		// they've been seen, so the dot must not come back on the next
		// tree refresh (server isRead lags behind).
		m.state.noteConversationOpened(msg.target.ID, newest)
		return m, m.refreshConversationItems()

	case liveMessagesRetryMsg:
		if !m.state.currentConversationIs(msg.target.ID) {
			return m, nil
		}
		m.ui().msgs.paneTitle = conversationPaneTitle(msg.target, time.Time{}, "live retry "+msg.at.Format("15:04:05"))
		return m, nil

	case messageImageMsg:
		return m.handleMessageImage(msg)

	case treeRefreshedMsg:
		return m.handleConversationsRefresh(msg)

	case calendarLoadedMsg:
		if m.state.staleLoad(msg.loadSeq) || m.ui().page != pageCalendar {
			return m, nil
		}
		m.ui().cal.apply(msg.events, msg.syncedAt, msg.start, msg.end)
		return m, nil

	case calendarLoadErrorMsg:
		if m.state.staleLoad(msg.loadSeq) || m.ui().page != pageCalendar {
			return m, nil
		}
		m.ui().cal.applyError(msg.err, msg.start, msg.end)
		return m, nil

	case list.FilterMatchesMsg:
		// The bubbles list computes filter matches asynchronously; the
		// result must land on the widget or the pane keeps showing every
		// item while the filter input records a needle.
		var cmd tea.Cmd
		m.conversations, cmd = m.conversations.Update(msg)
		return m, cmd
	}

	return m, nil
}

func (m *chatModel) ui() *uiState { return &m.state.ui }

func clockTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return clockTickMsg{} })
}

// resizeComponents keeps the bubbles widgets in step with the layout.
func (m *chatModel) resizeComponents() {
	m.help.Width = m.width
	m.conversations.SetSize(m.conversationPaneWidth(), m.messagesBoxHeight())
	m.messages.Width = m.messagesPaneWidth()
	m.messages.Height = m.messagesInnerHeight()
}

// handleConversationsRefresh rebuilds the list while keeping the selected
// conversation and reloading its messages only when the selection moved.
func (m chatModel) handleConversationsRefresh(msg treeRefreshedMsg) (tea.Model, tea.Cmd) {
	u := m.ui()
	selected, hadSelection := m.selectedConversationItem()

	m.state.applyConversationData(msg.data)
	listCmd := m.refreshConversationItems()

	if hadSelection {
		m.selectConversationByID(selected.target.ID)
	}

	if item, ok := m.selectedConversationItem(); ok && m.state.currentConversationIs(item.target.ID) {
		m.state.updateCurrentConversationTarget(item.target)
		if _, _, _, syncedAt := m.state.currentConversationSnapshot(); !syncedAt.IsZero() {
			u.msgs.paneTitle = conversationPaneTitle(item.target, time.Time{}, m.state.conversationSyncStatus(syncedAt))
		}
		return m, listCmd
	}

	return m, tea.Batch(listCmd, m.syncConversationSelection())
}

func (m chatModel) handleSendResult(msg sendResultMsg) (tea.Model, tea.Cmd) {
	u := m.ui()

	select {
	case <-m.state.appContext().Done():
		return m, nil
	default:
	}

	if !m.state.currentConversationIs(msg.target.ID) {
		return m, nil
	}

	if msg.err != nil {
		u.composerStatus = truncateComposerStatus("Send failed: " + msg.err.Error())
		u.composerStatusKind = statusError
		return m, nil
	}

	u.composerText = ""
	m.state.composerReply = replyContext{}
	u.composerStatus = "Sent."
	u.composerStatusKind = statusSuccess
	// Watching the conversation while sending: my own message is the newest
	// thing it can hold right now.
	m.state.noteConversationOpened(msg.target.ID, time.Now())

	// Force a reload that lands on the newest message.
	return m, m.state.reloadConversationCmd()
}

func truncateComposerStatus(message string) string {
	runes := []rune(message)
	if len(runes) <= composerStatusMaxLength {
		return message
	}

	return string(runes[:composerStatusMaxLength-1]) + "…"
}

// ---------------------------------------------------------------------------
// Key routing
// ---------------------------------------------------------------------------

func (m chatModel) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	u := m.ui()

	if msg.Type == tea.KeyCtrlC {
		m.state.requestStop()
		return m, tea.Quit
	}

	// The calendar page owns its own key routing; the chat panes are not
	// reachable while it is shown.
	if u.page == pageCalendar {
		return m.handleCalendarKey(msg)
	}

	// The list's filter input owns every keystroke while it is recording,
	// including 'q' and '?' (they are filter text).
	if u.focus == PaneTree && m.conversations.SettingFilter() {
		var cmd tea.Cmd
		m.conversations, cmd = m.conversations.Update(msg)
		return m, cmd
	}

	// While the composer is focused every rune is message text; the global
	// shortcuts ('q' quit, '?' help) must not swallow it.
	if u.focus == PaneComposer {
		return m.handleComposerKey(msg)
	}

	// Shortcuts fire only on a genuine single keystroke. bubbletea fuses a
	// run of printables that share one Read() into a single KeyRunes msg
	// (paste / very fast typing), so dispatching on Runes[0] of a batch
	// would let a pasted "quick fix" quit the app.
	if msg.Type == tea.KeyRunes && len(msg.Runes) == 1 {
		switch msg.Runes[0] {
		case '?':
			m.help.ShowAll = !m.help.ShowAll
			return m, nil
		case 'q':
			m.state.requestStop()
			return m, tea.Quit
		case 'c':
			m.state.ui.focus = PaneCalendar
			return m, m.state.enterCalendar()
		}
	}

	switch u.focus {
	case PaneMessages:
		return m.handleMessagesKey(msg)
	default:
		return m.handleConversationsKey(msg)
	}
}

// handleConversationsKey drives the list: j/k and the arrows move the
// cursor (bubbles' default keymap), '/' filters, Enter opens the
// conversation, l/Tab move into the message viewport.
func (m chatModel) handleConversationsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		item, ok := m.selectedConversationItem()
		if !ok {
			return m, nil
		}
		m.state.focusMessagesPane()
		return m, m.state.startConversationLoad(item.target)
	case tea.KeyTab:
		m.state.focusMessagesPane()
		return m, nil
	case tea.KeyRunes:
		if len(msg.Runes) == 1 && msg.Runes[0] == 'l' {
			m.state.focusMessagesPane()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.conversations, cmd = m.conversations.Update(msg)
	return m, tea.Batch(cmd, m.syncConversationSelection())
}

// syncConversationSelection loads messages when the list cursor lands on a
// conversation that is not the active one.
func (m *chatModel) syncConversationSelection() tea.Cmd {
	item, ok := m.selectedConversationItem()
	if !ok {
		return nil
	}
	if m.state.currentConversationIs(item.target.ID) {
		return nil
	}
	return m.state.startConversationLoad(item.target)
}

// handleMessagesKey drives the viewport: j/k move the message cursor,
// pgup/pgdn page, g/G jump, m/r compose or reply, h/Esc/Tab return to the
// list, l moves into the composer.
func (m chatModel) handleMessagesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	u := m.ui()

	switch msg.Type {
	case tea.KeyUp:
		u.msgs.up()
		return m, nil
	case tea.KeyDown:
		u.msgs.down()
		return m, nil
	case tea.KeyPgUp:
		u.msgs.page(-m.messagesPageSize(), m.messagesPageSize())
		return m, nil
	case tea.KeyPgDown:
		u.msgs.page(m.messagesPageSize(), m.messagesPageSize())
		return m, nil
	case tea.KeyHome:
		u.msgs.home()
		return m, nil
	case tea.KeyEnd:
		u.msgs.end()
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab, tea.KeyEscape, tea.KeyLeft:
		m.state.focusComponent(PaneTree)
		return m, nil
	case tea.KeyRunes:
		// Same single-keystroke rule as the global shortcuts: a pasted
		// batch must not hijack focus into reply/composer mode.
		if len(msg.Runes) != 1 {
			return m, nil
		}
		switch msg.Runes[0] {
		case 'h':
			m.state.focusComponent(PaneTree)
			return m, nil
		case 'l':
			m.state.focusComponent(PaneComposer)
			return m, m.applyComposerFocus()
		case 'j':
			u.msgs.down()
			return m, nil
		case 'k':
			u.msgs.up()
			return m, nil
		case 'g':
			u.msgs.home()
			return m, nil
		case 'G':
			u.msgs.end()
			return m, nil
		case 'm':
			m.state.focusComposerForMessage()
			return m, m.applyComposerFocus()
		case 'r':
			m.state.beginReplyTo()
			return m, m.applyComposerFocus()
		}
	}

	return m, nil
}

// handleComposerKey intercepts the keys the app owns, then forwards the
// rest to the textarea so cursor movement, word edits, and insertion are
// the bubbles widget's standard behavior.
func (m chatModel) handleComposerKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Alt+Enter inserts a newline instead of sending. Interception must
	// precede the textarea (whose InsertNewline binding matches plain
	// Enter), and the synthesized Enter must never reach the textarea's
	// InsertNewline branch: bubbletea parses \x1b\r as KeyEnter with
	// Alt=true, so the forwarded key carries Alt and misses the binding.
	if msg.Type == tea.KeyEnter && msg.Alt {
		m.composer.InsertString("\n")
		m.syncComposerText()
		return m, nil
	}

	switch msg.Type {
	case tea.KeyTab, tea.KeyShiftTab, tea.KeyEscape:
		// Leaving the composer blurs the live widget now, not lazily on
		// the next Update: a focused textarea keeps scheduling blink
		// ticks (and redraws) until something blurs it.
		m.state.focusComponent(PaneMessages)
		m.composer.Blur()
		return m, nil
	case tea.KeyEnter:
		m.state.sendComposerMessage()
		m.state.focusComponent(PaneMessages)
		m.composer.Blur()
		return m, nil
	case tea.KeyBackspace:
		// Physical backspace cancels any pending thread reply as well
		// as deleting at the cursor (the textarea's delete semantics).
		m.state.clearComposerReply()
		var cmd tea.Cmd
		m.composer, cmd = m.composer.Update(msg)
		m.syncComposerText()
		return m, cmd
	}

	var cmd tea.Cmd
	m.composer, cmd = m.composer.Update(msg)
	m.syncComposerText()
	return m, cmd
}

// syncComposerText mirrors the textarea's draft into uiState after the
// widget consumed a keystroke. It must advance composerSynced too: the
// next Update's head-sync compares the pair, and a lagging mirror treats
// every keystroke as an external change, SetValue()s, and resets the
// caret to end-of-buffer — killing mid-line editing.
func (m *chatModel) syncComposerText() {
	m.ui().composerText = m.composer.Value()
	m.composerSynced = m.ui().composerText
}

// syncComposer reconciles the textarea with uiState before any message is
// handled: an external draft change (send result, conversation switch)
// lands on the widget, and the focus state follows uiState.focus. It
// returns the command Focus() produces so the caret blink keeps running.
func (m chatModel) syncComposer() (chatModel, tea.Cmd) {
	u := m.ui()

	if u.composerText != m.composerSynced {
		m.composer.SetValue(u.composerText)
		m.composerSynced = u.composerText
	}

	if u.focus == PaneComposer {
		if !m.composer.Focused() {
			return m, m.applyComposerFocus()
		}
		return m, nil
	}

	if m.composer.Focused() {
		m.composer.Blur()
	}
	return m, nil
}

// syncMessagesViewport keeps the bubbles viewport in step with the current
// entries, size, and message cursor: the content is rendered here so the
// wheel/key scrolling methods have something to move, and the cursor entry
// is scrolled into view.
func (m chatModel) syncMessagesViewport() chatModel {
	u := m.ui()

	width := m.messagesPaneWidth()
	m.messages.Width = width
	m.messages.Height = m.messagesInnerHeight()

	content, starts, heights := messagesViewportContent(u, width)
	m.messages.SetContent(content)

	if len(starts) > 0 && u.msgs.cursor >= 0 && u.msgs.cursor < len(starts) {
		start := starts[u.msgs.cursor]
		end := start + heights[u.msgs.cursor]
		if start < m.messages.YOffset {
			m.messages.SetYOffset(start)
		}
		if end > m.messages.YOffset+m.messages.Height {
			m.messages.SetYOffset(end - m.messages.Height)
		}
	}

	return m
}

// applyComposerFocus focuses the textarea (starting its blink) after
// AppState moved uiState.focus to the composer. The guard matters:
// Focus() unconditionally returns a fresh BlinkCmd, so re-focusing an
// already-focused widget (repeat 'm', repeat click) would stack blink
// chains and double the caret rate.
func (m *chatModel) applyComposerFocus() tea.Cmd {
	if m.composer.Focused() {
		return nil
	}
	return m.composer.Focus()
}

// ---------------------------------------------------------------------------
// Mouse routing
// ---------------------------------------------------------------------------

// handleMouse routes the wheel to the pane under the pointer: the list
// cursor, the viewport, or the composer caret page.
func (m chatModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}

	u := m.ui()
	contentHeight := m.contentHeight()
	if msg.Y >= contentHeight {
		return m, nil
	}

	if msg.Button != tea.MouseButtonWheelUp && msg.Button != tea.MouseButtonWheelDown {
		return m, nil
	}
	up := msg.Button == tea.MouseButtonWheelUp

	switch {
	case msg.X < m.conversationPaneWidth():
		u.focus = PaneTree
		if up {
			m.conversations.CursorUp()
		} else {
			m.conversations.CursorDown()
		}
		return m, m.syncConversationSelection()
	case msg.X < m.conversationPaneWidth()+conversationPaneGap:
		return m, nil
	case msg.Y < m.messagesBoxHeight():
		u.focus = PaneMessages
		if up {
			m.messages.ScrollUp(3)
		} else {
			m.messages.ScrollDown(3)
		}
		return m, nil
	default:
		if msg.X < m.conversationPaneWidth()+conversationPaneGap {
			return m, nil
		}
		u.focus = PaneComposer
		return m, m.applyComposerFocus()
	}
}

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

// helpHeight is the footer's rendered height: one line for the short help,
// more when the full help is expanded.
func (m chatModel) helpHeight() int {
	return maxInt(1, lipgloss.Height(m.help.View(m.keyBindings())))
}

func (m chatModel) contentHeight() int {
	h := m.height - m.helpHeight()
	if h < 3 {
		h = 3
	}
	return h
}

// messagesBoxHeight is the messages section's height: terminal - help -
// title line - composer - status line. viewMain, handleMouse and
// messagesPageSize derive geometry from it so layout can never drift from
// what is actually rendered.
func (m chatModel) messagesBoxHeight() int {
	h := m.contentHeight() - composerGap - composerHeight - 1 - composerGap
	if h < 3 {
		h = 3
	}
	return h
}

// conversationPaneWidth is the shared list-pane width used by both the
// renderer and mouse routing.
func (m chatModel) conversationPaneWidth() int {
	w := m.width * 30 / 100
	if w < 24 {
		w = minInt(24, m.width/2)
	}
	if w > m.width-20 {
		w = maxInt(10, m.width-20)
	}
	return w
}

// messagesPaneWidth is the viewport's content width.
func (m chatModel) messagesPaneWidth() int {
	return maxInt(1, m.width-m.conversationPaneWidth()-conversationPaneGap-boxInset)
}

// messagesInnerHeight is the viewport's content height (section minus its
// title line).
func (m chatModel) messagesInnerHeight() int {
	return maxInt(1, m.messagesBoxHeight()-boxInset-1)
}

// imageMaxCols is the image width budget: the pane width, capped at the
// package maximum.
func (m chatModel) imageMaxCols() int {
	return minInt(imageMaxCols, m.messagesPaneWidth())
}

// messagesPageSize is the number of message entries that fit on one screen
// starting at the cursor. Entries are no longer a fixed two rows — bodies
// wrap — so the count comes from the wrapped heights.
func (m chatModel) messagesPageSize() int {
	u := m.ui()
	_, _, heights := messagesViewportContent(u, m.messagesPaneWidth())
	if len(heights) == 0 {
		return maxInt(1, m.messagesInnerHeight()/2)
	}

	available := m.messagesInnerHeight()
	used, count := 0, 0
	for i := u.msgs.cursor; i >= 0 && i < len(heights); i++ {
		if used+heights[i] > available && count > 0 {
			break
		}
		used += heights[i]
		count++
	}
	return maxInt(1, count)
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

var (
	paneTitleIdle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("8"))
	paneTitleFocused = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))

	authorStyle = lipgloss.NewStyle().Bold(true)
	stampStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	bodyStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	cursorStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))

	statusInfoStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	statusSuccessStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	statusErrorStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	statusDefaultStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))

	composerPromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	composerCursorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	placeholderStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))

	loginBoxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("12")).Padding(1, 2)
	errorBoxStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("9")).Padding(1, 2)
)

// boxInset is the space the focus box occupies on each side.
const boxInset = 2

// focusBorder is a dotted box: one pixel of dim grey around the focused
// pane, no solid lines and no powerline glyphs.
var focusBorder = lipgloss.Border{
	Top: "\u00b7", Bottom: "\u00b7", Left: "\u00b7", Right: "\u00b7",
	TopLeft: "\u00b7", TopRight: "\u00b7", BottomLeft: "\u00b7", BottomRight: "\u00b7",
}

// blankBorder keeps the same inset as the focus box without drawing
// anything, so pane geometry never shifts when focus moves.
var blankBorder = lipgloss.Border{
	Top: " ", Bottom: " ", Left: " ", Right: " ",
	TopLeft: " ", TopRight: " ", BottomLeft: " ", BottomRight: " ",
}

// renderFocusedBox wraps inner content (already sized width-2 by height-2)
// in the dotted focus box, or in the invisible inset when unfocused.
func renderFocusedBox(inner string, width, height int, focused bool) string {
	border := blankBorder
	style := lipgloss.NewStyle().Border(border)
	if focused {
		border = focusBorder
		style = lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color("8"))
	}
	return style.Width(maxInt(0, width-2)).Height(maxInt(0, height-2)).Render(inner)
}

func (m chatModel) View() string {
	switch m.ui().page {
	case pageLogin:
		return m.viewLogin()
	case pageError:
		return m.viewError()
	case pageCalendar:
		return m.viewCalendar()
	default:
		return m.viewMain()
	}
}

func (m chatModel) viewLogin() string {
	message := m.ui().loginMessage
	if message == "" {
		message = "Logging in..."
	}
	box := loginBoxStyle.Width(minInt(60, maxInt(30, m.width-4)))
	content := fmt.Sprintf("%s %s", m.spinner.View(), message)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Render(content))
}

func (m chatModel) viewError() string {
	box := errorBoxStyle.Width(minInt(70, maxInt(30, m.width-4)))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Render(m.ui().errorMessage))
}

func (m chatModel) viewMain() string {
	contentHeight := m.contentHeight()
	listWidth := m.conversationPaneWidth()
	rightWidth := m.width - listWidth - conversationPaneGap

	// The list column runs the full height; the composer belongs to the
	// conversation column and must never sit under the list.
	leftPane := m.renderConversationsPane(listWidth, contentHeight)
	messagesPane := m.renderMessagesPane(rightWidth, m.messagesBoxHeight())
	composerPane := m.renderComposer(rightWidth)
	rightPane := lipgloss.JoinVertical(lipgloss.Left,
		messagesPane,
		"", // breathing room above the composer
		composerPane,
		"", // and below it
		m.renderComposerStatus(rightWidth),
	)
	rightPane = padToWidth(padToHeight(rightPane, contentHeight), rightWidth)

	top := lipgloss.JoinHorizontal(lipgloss.Top, leftPane, strings.Repeat(" ", conversationPaneGap), rightPane)

	frame := lipgloss.JoinVertical(lipgloss.Left, top, m.help.View(m.keyBindings()))

	// Belt and braces: a frame taller than the terminal scrolls the alt
	// screen and clips the pane tops off. Never hand bubbletea more rows
	// than we were given; drop surplus rows from the bottom.
	if rows := strings.Split(frame, "\n"); len(rows) > m.height && m.height > 0 {
		frame = strings.Join(rows[:m.height], "\n")
	}
	return frame
}

// renderConversationsPane renders the bubbles list. The list component
// renders its own title and status bar, so no border is needed to
// demarcate the pane.
func (m chatModel) renderConversationsPane(width, height int) string {
	innerWidth := maxInt(1, width-boxInset)
	innerHeight := maxInt(1, height-boxInset)
	l := m.conversations
	l.SetSize(maxInt(1, innerWidth-2), innerHeight)

	content := l.View()
	if len(l.Items()) == 0 && l.FilterState() != list.Filtering {
		content = hintStyle.Render("No conversations")
	}
	content = lipgloss.NewStyle().PaddingLeft(1).Render(padToHeight(content, innerHeight))
	content = padToWidth(content, innerWidth)
	return renderFocusedBox(content, width, height, m.ui().focus == PaneTree)
}

// renderMessagesPane renders a single title line (conversation name plus
// live status) above the bubbles viewport. The viewport scrolls the
// conversation; the title line is the only demarcation.
func (m chatModel) renderMessagesPane(width, height int) string {
	u := m.ui()

	titleStyle := paneTitleIdle
	if u.focus == PaneMessages {
		titleStyle = paneTitleFocused
	}

	vp := m.messages
	vp.Width = maxInt(1, width-boxInset-1)
	vp.Height = maxInt(1, height-boxInset-1)

	content, starts, heights := messagesViewportContent(u, vp.Width)
	vp.SetContent(content)

	if len(starts) > 0 && u.msgs.cursor >= 0 && u.msgs.cursor < len(starts) {
		start := starts[u.msgs.cursor]
		end := start + heights[u.msgs.cursor]
		if start < vp.YOffset {
			vp.SetYOffset(start)
		}
		if end > vp.YOffset+vp.Height {
			vp.SetYOffset(end - vp.Height)
		}
	}

	// Terminal images are anchored to cells, so an image whose block is
	// only partially visible would be painted past the pane (over the
	// composer or help bar). Draw it only when it fits entirely.
	content = clipImagesToViewport(content, vp.YOffset, vp.Height)
	vp.SetContent(content)

	title := messagePaneTitle(u)
	if maxOffset := maxInt(0, linesIn(content)-vp.Height); maxOffset > 0 {
		pages := maxOffset/vp.Height + 1
		page := vp.YOffset/vp.Height + 1
		indicator := scrollBar(vp.ScrollPercent()) + "  " + hintStyle.Render(fmt.Sprintf("%d/%d", page, pages))
		available := width - boxInset - 1 - lipgloss.Width(indicator)
		title = truncateToWidth(title, maxInt(0, available-1))
		gap := maxInt(1, available-lipgloss.Width(title))
		header := titleStyle.Render(title) + strings.Repeat(" ", gap) + indicator
		body := lipgloss.JoinVertical(lipgloss.Left, header, padToHeight(vp.View(), maxInt(1, height-boxInset-1)))
		inner := padToWidth(lipgloss.NewStyle().PaddingLeft(1).Render(body), maxInt(1, width-boxInset))
		return renderFocusedBox(inner, width, height, u.focus == PaneMessages)
	}

	header := titleStyle.Render(truncateToWidth(title, maxInt(1, width-boxInset-1)))
	body := lipgloss.JoinVertical(lipgloss.Left, header, padToHeight(vp.View(), maxInt(1, height-boxInset-1)))
	inner := padToWidth(lipgloss.NewStyle().PaddingLeft(1).Render(body), maxInt(1, width-boxInset))
	return renderFocusedBox(inner, width, height, u.focus == PaneMessages)
}

// cursorRowSGR is the background highlight for the message row under the
// cursor, derived from one colour constant so tests and renderer agree.
func cursorRowSGR() string {
	rendered := lipgloss.NewStyle().Background(lipgloss.Color("235")).Render("X")
	return strings.SplitN(rendered, "X", 2)[0]
}

// clipImagesToViewport drops inline-image markers whose block does not fit
// entirely inside the visible viewport window.
func clipImagesToViewport(content string, offset, height int) string {
	lines := strings.Split(content, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.Contains(lines[i], imageMarkerPrefix) {
			continue
		}
		span := maxInt(1, markerRowSpan(lines[i]))
		start := i - offset
		if start < 0 || start+span > height {
			lines[i] = ""
		}
	}
	return strings.Join(lines, "\n")
}

// markerRowSpan extracts the row span from an image marker line.
func markerRowSpan(line string) int {
	i := strings.Index(line, imageMarkerPrefix)
	if i < 0 {
		return 0
	}
	rest := line[i+len(imageMarkerPrefix):]
	if j := strings.Index(rest, ":"); j >= 0 {
		rest = rest[:j]
	}
	if j := strings.Index(rest, imageMarkerSuffix); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

// scrollBarWidth is the width of the chat position bar.
const scrollBarWidth = 12

// scrollBar re-purposes the bubbles progress component as a scroll
// position indicator.
func scrollBar(percent float64) string {
	if percent < 0 {
		percent = 0
	}
	if percent > 1 {
		percent = 1
	}
	bar := progress.New(progress.WithWidth(scrollBarWidth), progress.WithSolidFill("6"))
	bar.ShowPercentage = false
	return bar.ViewAs(percent)
}

// linesIn reports how many rows the rendered content occupies.
func linesIn(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

// messagesViewportContent renders the message entries as viewport lines.
// Bodies wrap at the pane width (long tokens hard-wrap), so nothing is
// silently clipped; the returned starts/heights give each entry's first
// line and line count for cursor-into-view scrolling.
func messagesViewportContent(u *uiState, width int) (string, []int, []int) {
	entries := u.msgs.entries
	if len(entries) == 0 {
		return "", nil, nil
	}

	// The cursor row is emphasized (bold name) only while the message pane
	// owns focus.
	showCursor := u.focus == PaneMessages

	starts := make([]int, 0, len(entries))
	heights := make([]int, 0, len(entries))
	lines := make([]string, 0, len(entries)*2)
	for i, entry := range entries {
		starts = append(starts, len(lines))

		mainLines := wrapLines(entry.Main, width)

		// The stamp is app-generated metadata: style it separately from
		// the author name (dim, not bold) and keep it on the name's last
		// wrapped row when it fits.
		stampRow := -1
		if entry.Stamp != "" {
			last := len(mainLines) - 1
			if lipgloss.Width(mainLines[last])+2+lipgloss.Width(entry.Stamp) <= width {
				stampRow = last
			} else {
				mainLines = append(mainLines, entry.Stamp)
				stampRow = len(mainLines) - 1
			}
		}

		bodyLines := wrapLines(entry.Second, width)
		for lineIndex, line := range mainLines {
			if entry.Message == nil {
				lines = append(lines, hintStyle.Render(line))
				continue
			}
			if lineIndex == stampRow && entry.Stamp != "" && line != entry.Stamp {
				nameStyle := authorStyle
				if showCursor && i == u.msgs.cursor {
					nameStyle = cursorStyle
				}
				lines = append(lines, nameStyle.Render(line)+"  "+stampStyle.Render(entry.Stamp))
				continue
			}
			if entry.Stamp != "" && line == entry.Stamp {
				lines = append(lines, stampStyle.Render(line))
				continue
			}
			if showCursor && i == u.msgs.cursor {
				lines = append(lines, cursorStyle.Render(line))
			} else {
				lines = append(lines, authorStyle.Render(line))
			}
		}
		for _, line := range bodyLines {
			if entry.Message == nil {
				lines = append(lines, hintStyle.Render(line))
			} else {
				lines = append(lines, bodyStyle.Render(line))
			}
		}

		extra := 0
		for _, imageURL := range entry.Images {
			if imageRenderMode() == imageModeOff {
				continue
			}
			switch {
			case nativeImageSupported() && u.imageNative[imageURL] != "":
				rows := maxInt(1, u.imageNativeRows[imageURL])
				line := u.imageNative[imageURL] + fmt.Sprintf("%s%d:0%s",
					imageMarkerPrefix, rows, imageMarkerSuffix)
				lines = append(lines, line)
				for j := 1; j < rows; j++ {
					lines = append(lines, "")
				}
				extra += rows
			case u.images[imageURL] != "":
				for _, line := range strings.Split(u.images[imageURL], "\n") {
					lines = append(lines, line)
					extra++
				}
			case u.imageErrors[imageURL]:
				lines = append(lines, hintStyle.Render("[image unavailable]"))
				extra++
			default:
				lines = append(lines, hintStyle.Render("[image loading…]"))
				extra++
			}
		}
		if showCursor && i == u.msgs.cursor {
			bg := cursorRowSGR()
			entryStart := starts[len(starts)-1]
			skipRows := 0
			for idx := entryStart; idx < len(lines); idx++ {
				if skipRows > 0 {
					skipRows--
					continue
				}
				if strings.Contains(lines[idx], imageMarkerPrefix) {
					skipRows = maxInt(0, markerRowSpan(lines[idx])-1)
					continue
				}
				padded := lines[idx] + strings.Repeat(" ", maxInt(0, width-lipgloss.Width(lines[idx])))
				lines[idx] = bg + strings.ReplaceAll(padded, "\x1b[0m", "\x1b[0m"+bg) + "\x1b[0m"
			}
		}
		heights = append(heights, len(mainLines)+len(bodyLines)+extra)
	}

	return strings.Join(lines, "\n"), starts, heights
}

// wrapLines wraps raw text to the given width, breaking tokens that are
// longer than a whole line.
func wrapLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	if text == "" {
		return []string{""}
	}
	wrapped := ansi.Hardwrap(ansi.Wordwrap(text, width, " "), width, false)
	return strings.Split(wrapped, "\n")
}

func messagePaneTitle(u *uiState) string {
	if u.msgs.paneTitle != "" {
		return u.msgs.paneTitle
	}
	return "Messages"
}

func (m chatModel) renderComposerStatus(width int) string {
	u := m.ui()
	if u.composerStatus == "" {
		return ""
	}
	// Truncate the raw text before styling: styling first and cutting the
	// rendered bytes would slice through SGR sequences, and an overlong
	// row would widen the frame past the terminal.
	text := truncateToWidth(u.composerStatus, maxInt(1, width))
	switch u.composerStatusKind {
	case statusInfo:
		return statusInfoStyle.Render(text)
	case statusSuccess:
		return statusSuccessStyle.Render(text)
	case statusError:
		return statusErrorStyle.Render(text)
	default:
		return statusDefaultStyle.Render(text)
	}
}

// renderComposer draws the textarea directly: the widget (placeholder,
// caret, reply prompt) is the composer's own demarcation.
func (m chatModel) renderComposer(width int) string {
	u := m.ui()

	ta := m.composer
	if value := ta.Value(); value != u.composerText {
		ta.SetValue(u.composerText)
	}
	if u.focus == PaneComposer {
		ta.Focus()
	} else {
		ta.Blur()
	}

	innerWidth := maxInt(1, width-boxInset)
	if reply := m.state.composerReply; reply.messageID != "" {
		author := m.state.replyToAuthor()
		if author == "" {
			author = "message"
		}
		// Plain text: textarea.View renders the prompt through
		// FocusedStyle.Prompt (composerPromptStyle) itself, and
		// getPromptString measures the returned string with
		// uniseg.StringWidth — pre-styled bytes would double-render and
		// distort that measurement.
		prompt := "Reply to " + sanitizeText(truncateToWidth(author, 24)) + " › "
		promptWidth := minInt(lipgloss.Width(prompt), maxInt(0, innerWidth/2))
		ta.SetPromptFunc(promptWidth, func(int) string { return prompt })
	} else {
		ta.SetPromptFunc(0, func(int) string { return "" })
	}

	// SetWidth must follow SetPromptFunc: it snapshots promptWidth for
	// the reserved inner width (textarea.go SetWidth), and because the
	// composer is a per-frame copy of m.composer a prompt set afterwards
	// would never reach layout.
	ta.SetWidth(maxInt(1, innerWidth-2*composerPaddingX))
	padded := lipgloss.NewStyle().Padding(0, composerPaddingX).Render(ta.View())
	inner := padToWidth(padToHeight(padded, maxInt(1, composerHeight-boxInset)), innerWidth)
	return renderFocusedBox(inner, width, composerHeight, u.focus == PaneComposer)
}

// padToWidth pads every row of a block to exactly width columns, so
// side-by-side panes line up no matter how wide their widgets rendered.
func padToWidth(content string, width int) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, "\x00IMG:") {
			continue
		}
		gap := width - lipgloss.Width(line)
		if gap > 0 {
			lines[i] = line + strings.Repeat(" ", gap)
		}
	}
	return strings.Join(lines, "\n")
}

// padToHeight clamps a block to exactly height rows, padding with blank
// lines when the widget rendered fewer.
func padToHeight(content string, height int) string {
	lines := strings.Split(content, "\n")
	for len(lines) < height {
		lines = append(lines, "")
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return strings.Join(lines, "\n")
}

func truncateToWidth(text string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(text) <= width {
		return text
	}
	runes := []rune(text)
	for len(runes) > 0 && lipgloss.Width(string(runes)) > width-1 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "…"
}

// Pane identifiers for keyboard focus.
const (
	PaneTree      = "tree"
	PaneMessages  = "messages"
	PaneComposer  = "composer"
	PaneCalendar  = "calendar"
	PaneCalDetail = "caldetail"
)
