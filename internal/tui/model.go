package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/calendar"
	"github.com/fossteams/teams-cli/internal/teams"
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
		target            teams.Conversation
		messages          []csa.ChatMessage
		syncedAt          time.Time
		loadSeq           uint64
		preserveSelection bool
	}
	messagesLoadErrorMsg struct {
		target  teams.Conversation
		err     error
		loadSeq uint64
	}
	liveMessagesMsg struct {
		target            teams.Conversation
		messages          []csa.ChatMessage
		syncedAt          time.Time
		previousSignature string
	}
	liveMessagesRetryMsg struct {
		target teams.Conversation
		at     time.Time
	}
	treeRefreshedMsg struct {
		data *teams.ConversationData
	}
	sendResultMsg struct {
		target teams.Conversation
		err    error
	}
	calendarLoadedMsg struct {
		events   []calendar.Event
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
	mainReadyMsg struct{}
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

	// images holds each message image's render state keyed by source URL.
	images map[string]renderedImage

	composerText       string
	composerStatus     string
	composerStatusKind int

	// flash is a one-line notice (copied, opened, or why not) shown in the
	// tab row until the next key.
	flash    string
	flashErr bool

	// previewHidden hides the content pane (gh-dash's p); the sidebar then
	// takes the full width.
	previewHidden bool
}

// chatModel is the Bubble Tea model. All Teams/HTTP work stays on
// AppState; the model owns rendering and key routing.
//
// The panes are the standard bubbles components: a list for conversations,
// a viewport (built per frame) for the conversation, a textarea for
// composing, and a help component for the footer key bindings.
type chatModel struct {
	state   *AppState
	width   int
	height  int
	spinner spinner.Model
	// spinning reports whether a spinner tick is in flight. The spinner
	// only runs while something is loading: every tick repaints the whole
	// frame, so an idle app must not keep ticking.
	spinning bool

	conversations list.Model
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

	return chatModel{
		state:         state,
		spinner:       sp,
		conversations: newConversationList(),
		composer:      composer,
		help:          help.New(),
		width:         80,
		height:        24,
	}
}

func (m chatModel) Init() tea.Cmd {
	return nil
}

func (m chatModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Draft sync and focus reconciliation run before any other branch:
	// external changes to uiState.composerText (send success, conversation
	// switch) land on the value copy, blink/key commands must never
	// observe a stale textarea, and Focus() returns the blink command
	// that has to reach the event loop.
	m, syncCmd := m.syncComposer()

	model, cmd := m.updateInternal(msg)
	next := model.(chatModel)

	// Whatever the message started (a conversation or calendar load, the
	// login page) may need the spinner; restart it if it went idle.
	var spinCmd tea.Cmd
	if !next.spinning && next.busy() {
		next.spinning = true
		spinCmd = next.spinner.Tick
	}

	return next, tea.Batch(syncCmd, cmd, spinCmd)
}

// busy reports whether anything on screen animates with the spinner.
func (m chatModel) busy() bool {
	u := m.ui()
	return u.page == pageLogin || u.msgs.loading || (u.page == pageCalendar && u.cal.loading)
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
			u.images = nil
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
		// Let the tick chain lapse once nothing is loading; Update
		// restarts it when the next load begins.
		if !m.busy() {
			m.spinning = false
			return m, nil
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd

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
		m.syncConversationSelection()
		return m, listCmd

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
		return m, tea.Batch(m.refreshConversationItems(), m.state.scheduleReadReceipt(msg.target.ID, msg.messages))

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
		return m, tea.Batch(m.refreshConversationItems(), m.state.scheduleReadReceipt(msg.target.ID, msg.messages))

	case readReceiptDueMsg:
		return m, m.state.sendReadReceipt(msg)

	case readReceiptResultMsg:
		m.state.handleReadReceiptResult(msg)
		return m, nil

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
		// The result applies even when the user already left the page:
		// dropping it would strand the calendar in its loading state.
		if m.state.staleCalendarLoad(msg.loadSeq) {
			return m, nil
		}
		m.ui().cal.apply(msg.events, msg.syncedAt, msg.start, msg.end)
		return m, nil

	case calendarLoadErrorMsg:
		if m.state.staleCalendarLoad(msg.loadSeq) {
			return m, nil
		}
		m.ui().cal.applyError(msg.err, msg.start, msg.end)
		return m, nil

	case actionResultMsg:
		if msg.err != nil {
			m.ui().setFlash(msg.err.Error(), true)
		} else {
			m.ui().setFlash(msg.ok, false)
		}
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

func (m chatModel) ui() *uiState { return &m.state.ui }

// resizeComponents keeps the bubbles widgets in step with the layout.
func (m *chatModel) resizeComponents() {
	m.help.Width = m.width
	m.conversations.SetSize(m.conversationPaneWidth(), m.messagesBoxHeight())
}

// handleConversationsRefresh rebuilds the list while keeping the selected
// conversation and reloading its messages only when the selection moved.
func (m chatModel) handleConversationsRefresh(msg treeRefreshedMsg) (tea.Model, tea.Cmd) {
	u := m.ui()
	selected, hadSelection := m.selectedConversationItem()

	m.state.teams.ApplyConversations(msg.data)
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

	m.syncConversationSelection()
	return m, listCmd
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
	u.flash = ""

	if msg.Type == tea.KeyCtrlC {
		m.state.requestStop()
		return m, tea.Quit
	}

	// The calendar tab owns its own key routing; the chat panes are not
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

	switch msg.String() {
	case "left", "right":
		m.switchTab(pageCalendar)
		return m, nil
	case "ctrl+d":
		u.msgs.page(m.messagesPageSize())
		return m, nil
	case "ctrl+u":
		u.msgs.page(-m.messagesPageSize())
		return m, nil
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
		case 's', 'h', 'l', '2':
			m.switchTab(pageCalendar)
			return m, nil
		case '1':
			return m, nil
		case 'p':
			m.togglePreview()
			return m, nil
		case 'r':
			if target, ok, _, _ := m.state.currentConversationSnapshot(); ok {
				m.state.startConversationLoad(target)
			}
			return m, nil
		case 'R':
			return m, m.refreshAll()
		case 'o':
			if link, _, ok := m.selectedLink(); ok {
				return m, openURLCmd(link)
			}
			return m, nil
		case 'y':
			if _, id, ok := m.selectedLink(); ok {
				return m, copyCmd(id, "ID")
			}
			return m, nil
		case 'Y':
			if link, _, ok := m.selectedLink(); ok {
				return m, copyCmd(link, "link")
			}
			return m, nil
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
// conversation, Tab moves into the message viewport, and c writes to the
// selected conversation.
func (m chatModel) handleConversationsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		item, ok := m.selectedConversationItem()
		if !ok {
			return m, nil
		}
		m.showPreview()
		m.state.focusMessagesPane()
		m.state.startConversationLoad(item.target)
		return m, nil
	case tea.KeyTab:
		m.showPreview()
		m.state.focusMessagesPane()
		return m, nil
	case tea.KeyRunes:
		if len(msg.Runes) == 1 && msg.Runes[0] == 'c' {
			m.showPreview()
			m.state.focusComposerForMessage()
			return m, m.applyComposerFocus()
		}
	}

	var cmd tea.Cmd
	m.conversations, cmd = m.conversations.Update(msg)
	m.syncConversationSelection()
	return m, cmd
}

// syncConversationSelection loads messages when the list cursor lands on a
// conversation that is not the active one.
func (m chatModel) syncConversationSelection() {
	if item, ok := m.selectedConversationItem(); ok && !m.state.currentConversationIs(item.target.ID) {
		m.state.startConversationLoad(item.target)
	}
}

// handleMessagesKey drives the viewport: j/k move the message cursor,
// pgup/pgdn page, g/G jump, c replies to the highlighted message (gh-dash's
// comment), and Esc/Tab return to the list. On a failed load, Enter
// retries.
func (m chatModel) handleMessagesKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	u := m.ui()

	if u.msgs.onRetryRow() && msg.Type == tea.KeyEnter {
		if target, ok, _, _ := m.state.currentConversationSnapshot(); ok {
			m.state.startConversationLoad(target)
		}
		return m, nil
	}

	switch msg.Type {
	case tea.KeyUp:
		u.msgs.up()
		return m, nil
	case tea.KeyDown:
		u.msgs.down()
		return m, nil
	case tea.KeyPgUp:
		u.msgs.page(-m.messagesPageSize())
		return m, nil
	case tea.KeyPgDown:
		u.msgs.page(m.messagesPageSize())
		return m, nil
	case tea.KeyHome:
		u.msgs.home()
		return m, nil
	case tea.KeyEnd:
		u.msgs.end()
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab, tea.KeyEscape:
		m.state.focusComponent(PaneTree)
		return m, nil
	case tea.KeyRunes:
		// Same single-keystroke rule as the global shortcuts: a pasted
		// batch must not hijack focus into reply/composer mode.
		if len(msg.Runes) != 1 {
			return m, nil
		}
		switch msg.Runes[0] {
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
		case 'c':
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
	case tea.KeyEnter, tea.KeyCtrlD:
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

// handleMouse routes the wheel to the pane under the pointer: it moves the
// list cursor or the message cursor (the viewport follows the cursor), and
// focuses the composer.
func (m chatModel) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	if msg.Action != tea.MouseActionPress {
		return m, nil
	}
	u := m.ui()
	if u.page != pageMain && u.page != pageCalendar {
		return m, nil
	}

	// Clicking a tab switches to it.
	if msg.Y == 0 && msg.Button == tea.MouseButtonLeft {
		if page, ok := m.tabAt(msg.X); ok {
			m.switchTab(page)
		}
		return m, nil
	}

	y := msg.Y - tabBarHeight
	if y < 0 || y >= m.contentHeight() {
		return m, nil
	}
	if msg.Button != tea.MouseButtonWheelUp && msg.Button != tea.MouseButtonWheelDown {
		return m, nil
	}
	up := msg.Button == tea.MouseButtonWheelUp

	if u.page == pageCalendar {
		if up {
			u.cal.up()
		} else {
			u.cal.down()
		}
		return m, nil
	}

	switch {
	case msg.X < m.conversationPaneWidth():
		u.focus = PaneTree
		if up {
			m.conversations.CursorUp()
		} else {
			m.conversations.CursorDown()
		}
		m.syncConversationSelection()
		return m, nil
	case msg.X < m.conversationPaneWidth()+conversationPaneGap:
		return m, nil
	case y < m.messagesBoxHeight():
		if !u.msgs.focusable() {
			return m, nil
		}
		u.focus = PaneMessages
		if up {
			u.msgs.up()
		} else {
			u.msgs.down()
		}
		return m, nil
	default:
		u.focus = PaneComposer
		return m, m.applyComposerFocus()
	}
}

// ---------------------------------------------------------------------------
// Geometry
// ---------------------------------------------------------------------------

// helpHeight is the footer's rendered height: the rule above the help plus
// one line for the short help, more when the full help is expanded.
func (m chatModel) helpHeight() int {
	return 1 + max(1, lipgloss.Height(m.help.View(m.keyBindings())))
}

// contentHeight is the height of the sidebar and content area between the
// tab bar and the footer.
func (m chatModel) contentHeight() int {
	return max(3, m.height-tabBarHeight-m.helpHeight())
}

// messagesBoxHeight is the messages section's height: terminal - help -
// title line - composer - status line. viewMain, handleMouse and
// messagesPageSize derive geometry from it so layout can never drift from
// what is actually rendered.
func (m chatModel) messagesBoxHeight() int {
	return max(3, m.contentHeight()-composerGap-composerHeight-1-composerGap)
}

// conversationPaneWidth is the shared list-pane width used by both the
// renderer and mouse routing.
func (m chatModel) conversationPaneWidth() int {
	if m.ui().previewHidden {
		return m.width
	}
	w := m.width * 30 / 100
	if w < 24 {
		w = min(24, m.width/2)
	}
	if w > m.width-20 {
		w = max(10, m.width-20)
	}
	return w
}

// rightColumnWidth is the width of the conversation column (messages,
// composer, status) to the right of the list.
func (m chatModel) rightColumnWidth() int {
	return m.width - m.conversationPaneWidth() - conversationPaneGap
}

// messagesPaneWidth is the viewport's content width: the right column minus
// the focus-box inset and the one-column left padding. Wrapping, paging and
// image sizing all use it, so they always agree with what is painted.
func (m chatModel) messagesPaneWidth() int {
	return max(1, m.rightColumnWidth()-boxInset-1)
}

// messagesInnerHeight is the viewport's content height (section minus its
// title line).
func (m chatModel) messagesInnerHeight() int {
	return max(1, m.messagesBoxHeight()-boxInset-1)
}

// imageMaxCols is the image width budget: the pane width, capped at the
// package maximum.
func (m chatModel) imageMaxCols() int {
	return min(imageMaxCols, m.messagesPaneWidth())
}

// messagesPageSize is the number of message entries that fit on one screen
// starting at the cursor. Entries are no longer a fixed two rows — bodies
// wrap — so the count comes from the wrapped heights.
func (m chatModel) messagesPageSize() int {
	u := m.ui()
	_, _, heights := m.messagesContent(m.messagesPaneWidth())
	available := m.messagesInnerHeight()
	used, count := 0, 0
	for i := u.msgs.cursor; i >= 0 && i < len(heights); i++ {
		if used+heights[i] > available && count > 0 {
			break
		}
		used += heights[i]
		count++
	}
	return max(1, count)
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

// boxInset is the blank margin around each pane (one column or row per
// side).
const boxInset = 2

var paneMarginStyle = lipgloss.NewStyle().Padding(1)

// renderFocusedBox pads inner content (already sized width-2 by height-2)
// with the pane margin. Focus shows in the pane title, gh-dash style, not
// in a border.
func renderFocusedBox(inner string, width, height int, _ bool) string {
	return paneMarginStyle.Width(max(0, width)).Height(max(0, height)).Render(inner)
}

// renderTitledPane lays out a pane's title line above its body, indents
// both by one column, pads the block to the box's inner width, and wraps it
// in the focus box.
func renderTitledPane(header, body string, width, height int, focused bool) string {
	inner := lipgloss.JoinVertical(lipgloss.Left, header, body)
	inner = padToWidth(lipgloss.NewStyle().PaddingLeft(1).Render(inner), max(1, width-boxInset))
	return renderFocusedBox(inner, width, height, focused)
}

// paneTitleStyle is the title style for a pane, highlighted when focused.
func paneTitleStyle(focused bool) lipgloss.Style {
	if focused {
		return paneTitleFocused
	}
	return paneTitleIdle
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
	box := loginBoxStyle.Width(min(60, max(30, m.width-4)))
	content := fmt.Sprintf("%s %s", m.spinner.View(), message)
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Render(content))
}

func (m chatModel) viewError() string {
	box := errorBoxStyle.Width(min(70, max(30, m.width-4)))
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box.Render(m.ui().errorMessage))
}

func (m chatModel) viewMain() string {
	contentHeight := m.contentHeight()
	listWidth := m.conversationPaneWidth()
	rightWidth := m.rightColumnWidth()

	// The list column runs the full height; the composer belongs to the
	// conversation column and must never sit under the list.
	leftPane := m.renderConversationsPane(listWidth, contentHeight)
	if m.ui().previewHidden {
		return m.frame(leftPane, "")
	}
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

	return m.frame(leftPane, rightPane)
}

// frame lays out the tab bar, the sidebar and content split by the vertical
// rule, and the ruled help footer. A frame taller than the terminal scrolls
// the alt screen and clips the pane tops off, so surplus rows are dropped
// from the bottom.
func (m chatModel) frame(left, right string) string {
	body := left
	if !m.ui().previewHidden {
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, divider(m.contentHeight()), right)
	}
	frame := lipgloss.JoinVertical(lipgloss.Left, m.renderTabs(), body, m.rule(), m.help.View(m.keyBindings()))
	if rows := strings.Split(frame, "\n"); len(rows) > m.height && m.height > 0 {
		frame = strings.Join(rows[:m.height], "\n")
	}
	return frame
}

// renderConversationsPane renders the bubbles list. The list component
// renders its own title and status bar, so no border is needed to
// demarcate the pane.
func (m chatModel) renderConversationsPane(width, height int) string {
	innerWidth := max(1, width-boxInset)
	innerHeight := max(1, height-boxInset)
	l := m.conversations
	l.SetSize(max(1, innerWidth-2), innerHeight)

	content := l.View()
	if len(l.Items()) == 0 && l.FilterState() != list.Filtering {
		content = hintStyle.Render("No conversations")
	}
	content = lipgloss.NewStyle().PaddingLeft(1).Render(padToHeight(content, innerHeight))
	content = padToWidth(content, innerWidth)
	return renderFocusedBox(content, width, height, m.ui().focus == PaneTree)
}

// renderMessagesPane renders a single title line (conversation name, live
// status and, when the content overflows, a scroll position indicator)
// above a viewport scrolled to keep the message cursor in view.
func (m chatModel) renderMessagesPane(width, height int) string {
	u := m.ui()
	innerWidth := max(1, width-boxInset-1)
	innerHeight := max(1, height-boxInset-1)

	content, starts, heights := m.messagesContent(innerWidth)

	// Scroll minimally so the cursor entry is on screen. The offset lives
	// on the shared uiState so the view does not jump between frames.
	totalLines := linesIn(content)
	offset := max(0, min(u.msgs.yOffset, totalLines-innerHeight))
	if c := u.msgs.cursor; c >= 0 && c < len(starts) {
		if starts[c] < offset {
			offset = starts[c]
		}
		if end := starts[c] + heights[c]; end > offset+innerHeight {
			offset = end - innerHeight
		}
	}
	u.msgs.yOffset = offset

	// Terminal images are anchored to cells, so an image whose block is
	// only partially visible would be painted past the pane (over the
	// composer or help bar). Draw it only when it fits entirely.
	vp := viewport.New(innerWidth, innerHeight)
	vp.SetContent(clipImagesToViewport(content, offset, innerHeight))
	vp.SetYOffset(offset)

	title := messagePaneTitle(u)
	indicator := ""
	if maxOffset := totalLines - innerHeight; maxOffset > 0 {
		pages := maxOffset/innerHeight + 1
		page := offset/innerHeight + 1
		indicator = scrollBar(vp.ScrollPercent()) + "  " + hintStyle.Render(fmt.Sprintf("%d/%d", page, pages))
	}
	available := innerWidth - lipgloss.Width(indicator)
	header := paneTitleStyle(u.focus == PaneMessages).Render(truncateToWidth(title, max(1, available-1)))
	if indicator != "" {
		header += strings.Repeat(" ", max(1, available-lipgloss.Width(header))) + indicator
	}

	return renderTitledPane(header, padToHeight(vp.View(), innerHeight), width, height, u.focus == PaneMessages)
}

// cursorRowSGR is the background highlight for the message row under the
// cursor, derived from one colour constant so tests and renderer agree.
// It is computed per call: the colour profile is only known at runtime.
func cursorRowSGR() string {
	return strings.SplitN(lipgloss.NewStyle().Background(lipgloss.Color("235")).Render("X"), "X", 2)[0]
}

// imageMarker is the line suffix that tells the vendored renderer an inline
// image spans rows terminal rows starting at this line.
func imageMarker(rows int) string {
	return fmt.Sprintf("%s%d:0%s", imageMarkerPrefix, rows, imageMarkerSuffix)
}

// clipImagesToViewport drops inline-image markers whose block does not fit
// entirely inside the visible viewport window.
func clipImagesToViewport(content string, offset, height int) string {
	if !strings.Contains(content, imageMarkerPrefix) {
		return content
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if !strings.Contains(line, imageMarkerPrefix) {
			continue
		}
		start := i - offset
		if start < 0 || start+max(1, markerRowSpan(line)) > height {
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
	if j := strings.IndexAny(rest, ":"+imageMarkerSuffix); j >= 0 {
		rest = rest[:j]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

// scrollIndicator is the bubbles progress component re-purposed as a
// scroll position indicator.
var scrollIndicator = func() progress.Model {
	bar := progress.New(progress.WithWidth(12), progress.WithSolidFill("6"))
	bar.ShowPercentage = false
	return bar
}()

func scrollBar(percent float64) string {
	return scrollIndicator.ViewAs(max(0, min(1, percent)))
}

// linesIn reports how many rows the rendered content occupies.
func linesIn(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

// messagesContent renders the message entries as viewport lines. Bodies
// wrap at the pane width (long tokens hard-wrap), so nothing is silently
// clipped; the returned starts/heights give each entry's first line and
// line count for cursor-into-view scrolling. While a load is in flight the
// single loading row animates with the spinner.
func (m chatModel) messagesContent(width int) (string, []int, []int) {
	u := m.ui()
	entries := u.msgs.entries
	if u.msgs.loading && len(entries) > 0 {
		mainText, secondaryText := loadingStatusText(u.msgs.loadingLimit, u.msgs.loadingSince)
		entries = []messageEntry{{Main: m.spinner.View() + " " + mainText, Second: secondaryText}}
	}
	return renderMessageLines(entries, u.images, u.msgs.cursor, u.focus == PaneMessages, width)
}

// renderMessageLines lays out entries as styled lines. The cursor row is
// emphasized (highlighted background, bright name) only when showCursor.
func renderMessageLines(entries []messageEntry, images map[string]renderedImage, cursor int, showCursor bool, width int) (string, []int, []int) {
	if len(entries) == 0 {
		return "", nil, nil
	}

	mode := imageRenderMode()
	starts := make([]int, 0, len(entries))
	heights := make([]int, 0, len(entries))
	lines := make([]string, 0, len(entries)*2)
	for i, entry := range entries {
		entryStart := len(lines)
		starts = append(starts, entryStart)
		selected := showCursor && i == cursor

		if entry.Message == nil {
			// Placeholder rows (hints, loading, errors) are dim text.
			for _, line := range wrapLines(entry.Main, width) {
				lines = append(lines, hintStyle.Render(line))
			}
			for _, line := range wrapLines(entry.Second, width) {
				lines = append(lines, hintStyle.Render(line))
			}
		} else {
			nameStyle := authorStyle
			if selected {
				nameStyle = cursorStyle
			}
			// The stamp is app-generated metadata: style it separately
			// from the author name (dim, not bold) and keep it on the
			// name's last wrapped row when it fits.
			nameLines := wrapLines(entry.Main, width)
			last := len(nameLines) - 1
			for j, line := range nameLines {
				line = nameStyle.Render(line)
				if j == last && entry.Stamp != "" && lipgloss.Width(nameLines[last])+2+lipgloss.Width(entry.Stamp) <= width {
					line += "  " + stampStyle.Render(entry.Stamp)
					entry.Stamp = ""
				}
				lines = append(lines, line)
			}
			if entry.Stamp != "" {
				lines = append(lines, stampStyle.Render(entry.Stamp))
			}
			for _, line := range wrapLines(entry.Second, width) {
				lines = append(lines, bodyStyle.Render(line))
			}
		}

		for _, imageURL := range entry.Images {
			if mode == imageModeOff {
				break
			}
			img := images[imageURL]
			switch {
			case mode == imageModeNative && img.native != "":
				rows := max(1, img.nativeRows)
				lines = append(lines, img.native+imageMarker(rows))
				for j := 1; j < rows; j++ {
					lines = append(lines, "")
				}
			case img.art != "":
				lines = append(lines, strings.Split(img.art, "\n")...)
			case img.failed:
				lines = append(lines, hintStyle.Render("[image unavailable]"))
			default:
				lines = append(lines, hintStyle.Render("[image loading…]"))
			}
		}

		if selected {
			highlightRows(lines[entryStart:], width)
		}
		heights = append(heights, len(lines)-entryStart)
	}

	return strings.Join(lines, "\n"), starts, heights
}

// highlightRows paints the cursor background across an entry's rows, padded
// to the pane width. Native image rows (the marker line and the blank rows
// reserved under it) are left alone: the terminal draws the image there.
func highlightRows(lines []string, width int) {
	bg := cursorRowSGR()
	for idx := 0; idx < len(lines); idx++ {
		if strings.Contains(lines[idx], imageMarkerPrefix) {
			idx += max(0, markerRowSpan(lines[idx])-1)
			continue
		}
		padded := lines[idx] + strings.Repeat(" ", max(0, width-lipgloss.Width(lines[idx])))
		lines[idx] = bg + strings.ReplaceAll(padded, "\x1b[0m", "\x1b[0m"+bg) + "\x1b[0m"
	}
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
	text := truncateToWidth(u.composerStatus, max(1, width))
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

	innerWidth := max(1, width-boxInset)
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
		promptWidth := min(lipgloss.Width(prompt), max(0, innerWidth/2))
		ta.SetPromptFunc(promptWidth, func(int) string { return prompt })
	} else {
		ta.SetPromptFunc(0, func(int) string { return "" })
	}

	// SetWidth must follow SetPromptFunc: it snapshots promptWidth for
	// the reserved inner width (textarea.go SetWidth), and because the
	// composer is a per-frame copy of m.composer a prompt set afterwards
	// would never reach layout.
	ta.SetWidth(max(1, innerWidth-2*composerPaddingX))
	padded := lipgloss.NewStyle().Padding(0, composerPaddingX).Render(ta.View())
	inner := padToWidth(padToHeight(padded, max(1, composerHeight-boxInset)), innerWidth)
	return renderFocusedBox(inner, width, composerHeight, u.focus == PaneComposer)
}

// padToWidth pads every row of a block to exactly width columns, so
// side-by-side panes line up no matter how wide their widgets rendered.
func padToWidth(content string, width int) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if strings.Contains(line, imageMarkerPrefix) {
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
