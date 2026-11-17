package tui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/teams"
)

// conversationItem is one row of the conversations list: a channel (shown
// as the channel name with the team as subtitle) or a chat.
type conversationItem struct {
	target        teams.Conversation
	title         string
	subtitle      string
	lastMessageAt time.Time
	// unread marks the conversation as having messages the signed-in user
	// has not read (server-side read state, locally acknowledged once the
	// messages are opened here).
	unread bool
}

func (i conversationItem) Title() string       { return i.title }
func (i conversationItem) Description() string { return i.subtitle }
func (i conversationItem) FilterValue() string { return i.title + " " + i.subtitle }

// ---------------------------------------------------------------------------
// Unread state
// ---------------------------------------------------------------------------

// unreadAck carries the local read acknowledgments. The TUI never posts
// read receipts (the middle tier does that from other clients), so Teams'
// isRead/isMessageRead flags lag behind the moment the user actually looks
// at a conversation here. Two watermarks close the gap: the server's own
// threadVersion (a dot stays cleared until the server reports a newer
// thread), and the newest message time actually put on screen (live
// arrivals are newer than the cached tree version; the tree's reported
// activity catches up to them shortly, and until then the time watermark
// keeps a watched conversation quiet).
type unreadAck struct {
	version  map[string]int64
	seenTime map[string]time.Time
}

func (a *unreadAck) noteOpened(id string, version int64, activity time.Time) {
	if id == "" {
		return
	}
	if version > 0 {
		if a.version == nil {
			a.version = make(map[string]int64)
		}
		if version > a.version[id] {
			a.version[id] = version
		}
	}
	if !activity.IsZero() {
		if a.seenTime == nil {
			a.seenTime = make(map[string]time.Time)
		}
		if activity.After(a.seenTime[id]) {
			a.seenTime[id] = activity
		}
	}
}

// acknowledged reports whether the user has already seen the conversation's
// current content: no newer thread than the last one seen, or no newer
// reported activity than the newest message actually rendered.
func (a *unreadAck) acknowledged(id string, version int64, activity time.Time) bool {
	if version > 0 && version <= a.version[id] {
		return true
	}
	if !activity.IsZero() {
		if seen, ok := a.seenTime[id]; ok && !activity.After(seen) {
			return true
		}
	}
	return false
}

// conversationHasContent reports whether the payload shows any message in
// the conversation. Channels that never saw a message carry
// isMessageRead:false with threadVersion -1; without this check every
// empty channel would wear the unread dot.
func conversationHasContent(lastMessage csa.Message, threadVersion int) bool {
	return lastMessage.Id != "" || threadVersion > 0
}

// isChatUnread applies Teams' own flag plus the local acknowledgment.
// Messages I sent myself are never unread.
func (s *AppState) isChatUnread(chat csa.Chat) bool {
	if chat.IsRead || chat.IsLastMessageFromMe {
		return false
	}
	if !conversationHasContent(chat.LastMessage, chat.ThreadVersion) {
		return false
	}
	return !s.readAck.acknowledged(chat.Id, int64(chat.ThreadVersion), teams.ChatActivityTime(chat))
}

// isChannelUnread mirrors isChatUnread for channels. isMessageRead is the
// only read signal the channel payload carries (no last-message author),
// so the watermarks do the re-arming.
func (s *AppState) isChannelUnread(channel csa.Channel) bool {
	if channel.IsMessageRead {
		return false
	}
	if !conversationHasContent(channel.LastMessage, channel.ThreadVersion) {
		return false
	}
	return !s.readAck.acknowledged(channel.Id, int64(channel.ThreadVersion), channelActivityTime(channel))
}

// channelActivityTime is the newest known time for a channel, matching the
// activity ordering used by buildConversationItems.
func channelActivityTime(channel csa.Channel) time.Time {
	if ts := time.Time(channel.LastMessage.ComposeTime); !ts.IsZero() {
		return ts
	}
	return time.Time(channel.LastMessage.OriginalArrivalTime)
}

// noteConversationOpened records that the user opened (or is watching) this
// conversation in the TUI: its unread dot clears immediately and stays gone
// until the server reports a newer thread, or activity newer than the
// newest message just rendered (newest may be zero when unknown).
func (s *AppState) noteConversationOpened(id string, newest time.Time) {
	if id == "" {
		return
	}

	var version int64
	var activity time.Time
	if channel, ok := s.teams.Channel(id); ok {
		version = int64(channel.ThreadVersion)
		activity = channelActivityTime(*channel)
	} else if conversations := s.teams.Conversations(); conversations != nil {
		for _, chat := range conversations.Chats {
			if chat.Id == id {
				version = int64(chat.ThreadVersion)
				activity = teams.ChatActivityTime(chat)
				break
			}
		}
	}
	if newest.After(activity) {
		activity = newest
	}
	s.readAck.noteOpened(id, version, activity)
}

// ---------------------------------------------------------------------------
// List delegate
// ---------------------------------------------------------------------------

// conversationUnreadStyle is the dot that marks unread conversations. A
// bright color (not a glyph-only cue) so it reads at a glance; the dot is
// appended to the title before the selection style is applied, so the
// selected row shows the unread state too.
var conversationUnreadStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)

// conversationDelegate renders conversation rows. It ports
// list.DefaultDelegate (whose Render silently no-ops for non-DefaultItem
// types, and whose style pipeline cannot express per-row state) with one
// change: the unread dot is composed into the title before styling.
type conversationDelegate struct {
	Styles list.DefaultItemStyles
}

func newConversationDelegate() conversationDelegate {
	return conversationDelegate{Styles: list.NewDefaultItemStyles()}
}

// conversationRowHeight mirrors DefaultDelegate's geometry (2 rows when a
// description is shown) so the list's layout math matches what is painted.
const (
	conversationRowHeight   = 2
	conversationRowSpacing  = 1
	conversationUnreadLabel = " ●"
)

func (d conversationDelegate) Height() int  { return conversationRowHeight }
func (d conversationDelegate) Spacing() int { return conversationRowSpacing }

func (d conversationDelegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

func (d conversationDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	item, ok := listItem.(conversationItem)
	if !ok {
		return
	}

	title := item.title
	if item.unread {
		title += conversationUnreadStyle.Render(conversationUnreadLabel)
	}
	desc := item.subtitle

	if width := m.Width(); width > 0 {
		// Prevent text from exceeding the list width.
		textwidth := width - d.Styles.NormalTitle.GetPaddingLeft() - d.Styles.NormalTitle.GetPaddingRight()
		title = ansi.Truncate(title, textwidth, "…")
		desc = ansi.Truncate(strings.SplitN(desc, "\n", 2)[0], textwidth, "…")
	}

	var (
		isSelected  = index == m.Index()
		emptyFilter = m.FilterState() == list.Filtering && m.FilterValue() == ""
		isFiltered  = m.FilterState() == list.Filtering || m.FilterState() == list.FilterApplied
	)

	var matchedRunes []int
	if isFiltered && index < len(m.VisibleItems()) {
		matchedRunes = m.MatchesForItem(index)
	}

	s := &d.Styles
	switch {
	case emptyFilter:
		title = s.DimmedTitle.Render(title)
		desc = s.DimmedDesc.Render(desc)
	case isSelected && m.FilterState() != list.Filtering:
		if isFiltered {
			unmatched := s.SelectedTitle.Inline(true)
			matched := unmatched.Inherit(s.FilterMatch)
			title = lipgloss.StyleRunes(title, matchedRunes, matched, unmatched)
		}
		title = s.SelectedTitle.Render(title)
		desc = s.SelectedDesc.Render(desc)
	default:
		if isFiltered {
			unmatched := s.NormalTitle.Inline(true)
			matched := unmatched.Inherit(s.FilterMatch)
			title = lipgloss.StyleRunes(title, matchedRunes, matched, unmatched)
		}
		title = s.NormalTitle.Render(title)
		desc = s.NormalDesc.Render(desc)
	}

	fmt.Fprintf(w, "%s\n%s", title, desc) //nolint: errcheck
}

// ---------------------------------------------------------------------------
// List construction / population
// ---------------------------------------------------------------------------

// buildConversationItems flattens the signed-in user's conversations into a
// single list ordered by the most recent message, newest first: every
// channel of every team plus the chats. Ties (and conversations with no
// known activity) sort by title so the order is stable across refreshes.
// Rows the server reports unread (and the user has not opened since) carry
// the unread flag the delegate renders as a dot.
func (s *AppState) buildConversationItems() []list.Item {
	items := make([]list.Item, 0, 16)
	conversations := s.teams.Conversations()
	if conversations == nil {
		return items
	}

	activity := func(id string, lastMessage time.Time) time.Time {
		newest := lastMessage
		if tracked := s.lastMessageAt[id]; tracked.After(newest) {
			newest = tracked
		}
		return newest
	}

	for _, team := range conversations.Teams {
		teamName := sanitizeText(team.DisplayName)
		for _, channel := range team.Channels {
			channelName := sanitizeText(channel.DisplayName)
			items = append(items, conversationItem{
				target:        teams.Conversation{ID: channel.Id, Title: fmt.Sprintf("%s / %s", teamName, channelName)},
				title:         channelName,
				subtitle:      teamName,
				lastMessageAt: activity(channel.Id, channelActivityTime(channel)),
				unread:        s.isChannelUnread(channel),
			})
		}
	}

	for _, chat := range conversations.Chats {
		title := sanitizeText(s.teams.ChatTitle(chat))
		items = append(items, conversationItem{
			target:        teams.Conversation{ID: chat.Id, Title: title},
			title:         title,
			subtitle:      "chat",
			lastMessageAt: activity(chat.Id, teams.ChatActivityTime(chat)),
			unread:        s.isChatUnread(chat),
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		left, right := items[i].(conversationItem), items[j].(conversationItem)
		if !left.lastMessageAt.Equal(right.lastMessageAt) {
			return left.lastMessageAt.After(right.lastMessageAt)
		}
		return left.target.Title < right.target.Title
	})

	return items
}

// conversationsPaneTitle is the bubbles list title, with the unread count
// so the number is visible even when unread rows scroll off screen.
func conversationsPaneTitle(items []list.Item) string {
	if count := unreadItems(items); count > 0 {
		return fmt.Sprintf("Conversations · %d unread", count)
	}
	return "Conversations"
}

// unreadItems counts the unread conversations in a list.
func unreadItems(items []list.Item) int {
	count := 0
	for _, item := range items {
		if conv, ok := item.(conversationItem); ok && conv.unread {
			count++
		}
	}
	return count
}

// noteConversationLastMessage records the newest message time seen for a
// conversation; the conversations list is ordered by it.
func (s *AppState) noteConversationLastMessage(id string, at time.Time) {
	if id == "" || at.IsZero() {
		return
	}
	if s.lastMessageAt == nil {
		s.lastMessageAt = make(map[string]time.Time)
	}
	if at.After(s.lastMessageAt[id]) {
		s.lastMessageAt[id] = at
	}
}

// newestMessageTime returns the newest ComposeTime across the messages.
func newestMessageTime(messages []csa.ChatMessage) time.Time {
	var newest time.Time
	for _, message := range messages {
		if t := time.Time(message.ComposeTime); t.After(newest) {
			newest = t
		}
	}
	return newest
}

// newConversationList builds the bubbles list that enumerates conversations.
// Vim movement is the default (j/k); h/l are freed for the app's pane
// switching and page movement moves to pgup/pgdown. The list's own title,
// status bar and help are off: the pane is bordered by the app and the
// footer is the shared help component.
func newConversationList() list.Model {
	l := list.New(nil, newConversationDelegate(), 0, 0)
	l.Title = "Conversations"
	l.SetShowTitle(true)
	l.SetShowStatusBar(true)
	l.SetShowHelp(false)
	l.SetShowPagination(true)
	l.SetStatusBarItemName("conversation", "conversations")
	l.SetFilteringEnabled(true)
	l.DisableQuitKeybindings()

	km := list.DefaultKeyMap()
	km.PrevPage = key.NewBinding(key.WithKeys("pgup"))
	km.NextPage = key.NewBinding(key.WithKeys("pgdown"))
	l.KeyMap = km

	return l
}

// selectedConversationItem reports the conversation under the list cursor.
func (m chatModel) selectedConversationItem() (conversationItem, bool) {
	item, ok := m.conversations.SelectedItem().(conversationItem)
	return item, ok
}

// selectConversationByID places the list cursor on a conversation target.
// Select operates in visible-index space, so an active filter is honored.
func (m *chatModel) selectConversationByID(id string) bool {
	if id == "" {
		return false
	}
	for i, item := range m.conversations.VisibleItems() {
		conv, ok := item.(conversationItem)
		if ok && conv.target.ID == id {
			m.conversations.Select(i)
			return true
		}
	}
	return false
}

// refreshConversationItems rebuilds the list from current data, keeping the
// cursor on the same conversation. The returned command is the list's
// asynchronous filter recomputation (SetItems clears the filtered view when
// a filter is active, so dropping it leaves the pane blank).
func (m *chatModel) refreshConversationItems() tea.Cmd {
	selected, hadSelection := m.selectedConversationItem()
	items := m.state.buildConversationItems()
	cmd := m.conversations.SetItems(items)
	m.conversations.Title = conversationsPaneTitle(items)
	if hadSelection {
		m.selectConversationByID(selected.target.ID)
	}
	return cmd
}
