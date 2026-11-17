package tui

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-api/pkg/models"
	"github.com/fossteams/teams-cli/internal/config"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
	"github.com/muesli/termenv"
)

// TestMain pins a color-capable profile for the whole binary. Without a
// TTY, termenv auto-detects Ascii and lipgloss then strips ANSI from
// everything it renders (including hostile content), which would make the
// escape scan in TestRenderedFrameCarriesNoHostileSequences pass
// vacuously even with a fully broken sanitizer.
func TestMain(m *testing.M) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	os.Exit(m.Run())
}

// ansiPattern matches pure SGR sequences (lipgloss's own styling); the
// broader escapeSequencePattern matches every ESC-initiated sequence, so
// anything matching it but not ansiPattern is a cursor/erase/title
// sequence hostile content smuggled into the frame.
var (
	ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")
	// sgrPattern is the anchored form: extracted sequences must equal a
	// pure SGR, not merely contain one (an unanchored match would pass a
	// longer cursor/erase sequence that happens to embed an SGR prefix).
	sgrPattern            = regexp.MustCompile("^\x1b\\[[0-9;]*m$")
	escapeSequencePattern = regexp.MustCompile(
		`\x1b\[[0-9;?]*[ -/]*[@-~]` +
			`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
			`|\x1b[@-Z\\-_]`)
)

var paginationPattern = regexp.MustCompile(`\b(\d+)/(\d+)\b`)

func plain(text string) string {
	return ansiPattern.ReplaceAllString(text, "")
}

func TestMessageListItemPutsAuthorAboveBody(t *testing.T) {
	mainText, secondaryText := messageListItem("Jane Doe", "<p>first line</p><p>second line</p>")

	if mainText != "Jane Doe" {
		t.Fatalf("expected plain author as the main (top) line, got %q", mainText)
	}
	if !strings.Contains(secondaryText, "first line") || !strings.Contains(secondaryText, "second line") {
		t.Fatalf("expected body under the author, got %q", secondaryText)
	}
	if strings.Contains(secondaryText, "\n") {
		t.Fatalf("expected paragraph breaks joined for the two-row entry, got %q", secondaryText)
	}
}

func TestMessageListItemKeepsBracketedUserContentLiteral(t *testing.T) {
	mainText, secondaryText := messageListItem("A[b]B", "<p>hello [white]not-a-color</p>")

	// The renderer styles with ANSI only; bracket markup is not a
	// markup language anymore and must survive verbatim.
	if mainText != "A[b]B" {
		t.Fatalf("expected literal author, got %q", mainText)
	}
	if secondaryText != "hello [white]not-a-color" {
		t.Fatalf("expected literal body, got %q", secondaryText)
	}
}

func TestMessageListItemStripsTerminalInjection(t *testing.T) {
	// The old tview renderer needed tview.Escape for [white]-style tags;
	// under lipgloss the injection vector is raw ANSI/OSC: message bodies
	// and display names come off the network and lipgloss passes escape
	// sequences through to the terminal untouched.
	hostile := "Alice\x1b[2;1H\x1b]0;pwned\x07\x1b[31mred"
	body := "<p>hi\x1b[2Jthere\x1b[?25l\x00\x07!</p>"

	mainText, secondaryText := messageListItem(hostile, body)

	if strings.ContainsRune(mainText, 0x1b) || strings.ContainsRune(secondaryText, 0x1b) {
		t.Fatalf("expected escape bytes stripped, got %q / %q", mainText, secondaryText)
	}
	if mainText != "Alicered" {
		t.Fatalf("expected visible text preserved without sequences, got %q", mainText)
	}
	if secondaryText != "hithere!" {
		t.Fatalf("expected body kept minus control sequences/bytes, got %q", secondaryText)
	}
}

func TestRenderedFrameCarriesNoHostileSequences(t *testing.T) {
	// End-to-end half of the injection pin: a hostile message must reach
	// the rendered frame (proven by the presence assertion) and leave no
	// non-SGR escape sequences behind.
	// \x1b[1;H is CUP (cursor position); \x1b[1;mH exercises the CSI
	// extraction edge where a naive matcher sees only the leading SGR
	// fragment "\x1b[1;m" and ignores the trailing H.
	hostile := "Alice\x1b[2;1H\x1b]0;pwned\x07\x1b[31mred"
	body := "<p>hi\x1b[2Jthere\x1b[1;H\x1b[1;mH\x1b[?25l\x00\x07!</p>"

	state := chatRenderState(t)
	// View() switches on ui.page; without this the zero value pageLogin
	// renders the login box and the escape scan below is vacuous.
	state.ui.page = pageMain
	state.ui.focus = PaneMessages
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: hostile, Content: body},
	}, time.Now(), "", false)
	model := newChatModel(state)
	model.width, model.height = 100, 24
	rendered := model.View()

	// Non-vacuity: the message rows must be in the frame. Markers are
	// chosen to survive whether or not escape bytes are present, so a
	// broken sanitizer reaches the scan instead of the presence check.
	for _, marker := range []string{"Alice", "there"} {
		if !strings.Contains(plain(rendered), marker) {
			t.Fatalf("hostile content never reached the frame; scan would be vacuous (missing %q). frame:\n%s", marker, plain(rendered))
		}
	}
	// Non-vacuity for the scan itself: the frame must actually contain
	// lipgloss SGR styling, proving the TrueColor profile took effect.
	if ansiPattern.FindString(rendered) == "" {
		t.Fatal("frame carries no lipgloss styling; color profile is not in effect")
	}

	// Every remaining escape sequence in the frame must be a pure SGR
	// style emitted by lipgloss, never a cursor/erase/title sequence.
	for _, seq := range escapeSequencePattern.FindAllString(rendered, -1) {
		if !sgrPattern.MatchString(seq) {
			t.Fatalf("non-SGR escape sequence reached the terminal: %q in view", seq)
		}
	}
}

func chatRenderState(t *testing.T) *AppState {
	t.Helper()
	state := &AppState{
		logger:       logging.Discard,
		messageLimit: config.DefaultMessageLimit,
	}
	state.initRuntime(context.Background())
	return state
}

func timedMessages(count int) []csa.ChatMessage {
	base := time.Date(2026, time.March, 27, 12, 0, 0, 0, time.UTC)
	messages := make([]csa.ChatMessage, 0, count)
	for i := 0; i < count; i++ {
		messages = append(messages, csa.ChatMessage{
			Id:            fmt.Sprintf("%d", i),
			ImDisplayName: "Bob",
			Content:       fmt.Sprintf("<p>message %d</p>", i),
			ComposeTime:   api.RFC3339Time(base.Add(time.Duration(i) * time.Minute)),
		})
	}
	return messages
}

func TestApplyConversationFiltersAndOrdersAuthorFirst(t *testing.T) {
	state := chatRenderState(t)

	messages := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>", ComposeTime: api.RFC3339Time(time.Now())},
		{Id: "2", ImDisplayName: "", Content: "<p>anonymous skipped</p>", ComposeTime: api.RFC3339Time(time.Now())},
		{Id: "3", ImDisplayName: "Bob", Content: "<p>yo</p>", ComposeTime: api.RFC3339Time(time.Now())},
	}
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	entries := state.ui.msgs.entries
	if len(entries) != 2 {
		t.Fatalf("expected 2 rendered messages, got %d", len(entries))
	}
	if !strings.HasPrefix(entries[0].Main, "Alice") {
		t.Fatalf("expected author-first main line, got %q", entries[0].Main)
	}
	if entries[0].Second != "hi" {
		t.Fatalf("expected body as second line, got %q", entries[0].Second)
	}

	// Auto-scroll landed on the newest entry; reply-context lookup must
	// resolve the message behind the *current* row, wherever cursor sits.
	msg, ok := state.messageAtCurrentIndex()
	if !ok {
		t.Fatal("expected message lookup to succeed after apply")
	}
	if msg.ImDisplayName != "Bob" {
		t.Fatalf("expected reply context on the newest row (Bob), got %q", msg.ImDisplayName)
	}
	state.ui.msgs.setCursor(0)
	msg, ok = state.messageAtCurrentIndex()
	if !ok || msg.ImDisplayName != "Alice" {
		t.Fatalf("expected reply context to follow cursor to Alice, got %q (ok=%v)", msg.ImDisplayName, ok)
	}
}

// TestRenderedMessagesDrawAuthorRowAboveBodyRow draws the shipped render
// path and asserts screen-row order: the author line directly above the
// body line, never concatenated on one row.
func TestRenderedMessagesDrawAuthorRowAboveBodyRow(t *testing.T) {
	state := chatRenderState(t)
	messages := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hello there</p>", ComposeTime: api.RFC3339Time(time.Now())},
	}
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	model := newChatModel(state)
	model.width = 80
	model.height = 12
	state.ui.focus = PaneMessages

	rendered := plain(model.renderMessagesPane(40, 12))
	rows := strings.Split(rendered, "\n")

	authorRow, bodyRow := -1, -1
	for y, row := range rows {
		if authorRow < 0 && strings.Contains(row, "Alice") {
			authorRow = y
		}
		if bodyRow < 0 && strings.Contains(row, "hello there") {
			bodyRow = y
		}
	}

	if authorRow < 0 || bodyRow < 0 {
		t.Fatalf("author row %d, body row %d; pane was:\n%s", authorRow, bodyRow, rendered)
	}
	if bodyRow != authorRow+1 {
		t.Fatalf("expected body directly below author, got rows %d and %d:\n%s", authorRow, bodyRow, rendered)
	}
	if strings.Contains(rows[authorRow], "hello there") {
		t.Fatalf("author and body concatenated on one row: %q", rows[authorRow])
	}
}

func TestApplyMessagesStartsAtNewestOnLoad(t *testing.T) {
	state := chatRenderState(t)

	// fetchMessagesOnce sorts ascending by ComposeTime; replicate that.
	messages := timedMessages(3)
	sort.Sort(csa.SortMessageByTime(messages))
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	if len(state.ui.msgs.entries) != 3 {
		t.Fatalf("expected 3 rendered messages, got %d", len(state.ui.msgs.entries))
	}
	if got := state.ui.msgs.cursor; got != 2 {
		t.Fatalf("expected cursor on newest (index 2) on load, got %d", got)
	}
}

func TestApplyMessagesRefreshKeepsScrolledPosition(t *testing.T) {
	state := chatRenderState(t)
	messages := timedMessages(3)
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	// User scrolls up to read history, then a background refresh lands.
	state.ui.msgs.setCursor(0)
	messages = append(messages, csa.ChatMessage{
		Id:            "3",
		ImDisplayName: "Bob",
		Content:       "<p>newest</p>",
		ComposeTime:   api.RFC3339Time(time.Now()),
	})
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", true)

	if got := state.ui.msgs.cursor; got != 0 {
		t.Fatalf("expected refresh to preserve scrolled position (index 0), got %d", got)
	}
}

func TestApplyMessagesRefreshFollowsTail(t *testing.T) {
	state := chatRenderState(t)
	messages := timedMessages(3)
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	// Cursor sits at the tail; a new message arrives on refresh.
	messages = append(messages, csa.ChatMessage{
		Id:            "3",
		ImDisplayName: "Bob",
		Content:       "<p>newest</p>",
		ComposeTime:   api.RFC3339Time(time.Now()),
	})
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", true)

	if got := state.ui.msgs.cursor; got != 3 {
		t.Fatalf("expected tail-following to select the new newest (index 3), got %d", got)
	}
}

// TestApplyMessagesForcedReloadJumpsToNewest pins the post-send contract:
// the reload after a send re-applies with preserveSelection=false even
// though the pane is already populated, so the user lands on their own
// message even when they had scrolled up into history. (Background live
// refresh uses true and must NOT hijack the scroll position; that arm is
// the test above.)
func TestApplyMessagesForcedReloadJumpsToNewest(t *testing.T) {
	state := chatRenderState(t)
	messages := timedMessages(3)
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	// User scrolls up to read history, then sends: the reload must snap
	// past their scroll position to the newest message.
	state.ui.msgs.setCursor(0)
	messages = append(messages, csa.ChatMessage{
		Id:            "3",
		ImDisplayName: "Brian Tracey",
		Content:       "<p>just sent</p>",
		ComposeTime:   api.RFC3339Time(time.Now()),
	})
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	if got := state.ui.msgs.cursor; got != 3 {
		t.Fatalf("expected post-send reload to jump to the newest (index 3), got %d", got)
	}
}

// TestRenderedMessagesScrollToBottomOnLoad proves the visible window lands
// on the newest messages: with 30 messages in a 12-row pane, the last
// message is visible and the oldest is scrolled off the top.
func TestRenderedMessagesScrollToBottomOnLoad(t *testing.T) {
	state := chatRenderState(t)
	messages := timedMessages(30)
	sort.Sort(csa.SortMessageByTime(messages))
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	model := newChatModel(state)
	state.ui.focus = PaneMessages

	rendered := plain(model.renderMessagesPane(40, 12))

	if !strings.Contains(rendered, "message 29") {
		t.Fatalf("expected newest message visible after load; pane was:\n%s", rendered)
	}
	if strings.Contains(rendered, "message 0 ") {
		t.Fatalf("expected oldest message scrolled off-screen after load; pane was:\n%s", rendered)
	}
}

// TestApplyMessagesShowsOwnNamelessSends pins the fix for CLI sends
// invisible in the chat window: the message service stores posts that
// omitted imdisplayname with an empty sender name, so a nameless message
// from the signed-in user must render (under the user's display name),
// while nameless system activity (thread events, call logs) stays hidden.
func TestApplyMessagesShowsOwnNamelessSends(t *testing.T) {
	state := chatRenderState(t)
	state.teams.SetProfile(&models.User{Mri: "8:orgid:self-1", DisplayName: "Brian Tracey"})

	messages := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>", ComposeTime: api.RFC3339Time(time.Now())},
		{Id: "2", ImDisplayName: "", From: "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/contacts/8:orgid:self-1", Content: "<p>this is a test</p>", ComposeTime: api.RFC3339Time(time.Now().Add(time.Second))},
		{Id: "3", ImDisplayName: "", From: "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/contacts/19:meeting_abc@thread.v2", Content: "<p>someone joined</p>", ComposeTime: api.RFC3339Time(time.Now().Add(2 * time.Second))},
		{Id: "4", ImDisplayName: "", From: "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/contacts/8:orgid:other-1", Content: "<p>other nameless</p>", ComposeTime: api.RFC3339Time(time.Now().Add(3 * time.Second))},
		// A call started by the signed-in user stores the user's own
		// contact URL with no display name: only the activity-type
		// guard keeps this out of the chat window.
		{Id: "5", MessageType: "Event/Call", ImDisplayName: "", From: "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/contacts/8:orgid:self-1", Content: "<p><partlist><part identity=\"8:orgid:other-1\">Austin Herrera</part></partlist><meetingDetails>Recurring 10/08/2026</meetingDetails></p>", ComposeTime: api.RFC3339Time(time.Now().Add(4 * time.Second))},
		{Id: "6", MessageType: "ThreadActivity/AddMember", ImDisplayName: "", From: "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/contacts/8:orgid:self-1", Content: "<p>AddMember 8:orgid:other-1</p>", ComposeTime: api.RFC3339Time(time.Now().Add(5 * time.Second))},
	}
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, messages, time.Time{}, "", false)

	entries := state.ui.msgs.entries
	if len(entries) != 2 {
		t.Fatalf("expected only Alice and the own nameless send, got %d entries (%+v)", len(entries), entries)
	}
	if !strings.HasPrefix(entries[1].Main, "Brian Tracey") {
		t.Fatalf("expected own nameless send under the self display name, got %q", entries[1].Main)
	}
	if entries[1].Second != "this is a test" {
		t.Fatalf("expected the nameless send body, got %q", entries[1].Second)
	}

	// Reply context lookup must see the rendered nameless message too.
	state.ui.msgs.setCursor(1)
	msg, ok := state.messageAtCurrentIndex()
	if !ok || msg.Id != "2" {
		t.Fatalf("expected reply context to resolve the rendered own send, got ok=%v msg=%+v", ok, msg)
	}
}

// TestIsSelfAuthoredMessage covers the from-URL author matching.
func TestIsSelfAuthoredMessage(t *testing.T) {
	state := AppState{}
	if state.isSelfAuthoredMessage(csa.ChatMessage{From: "https://host/v1/users/ME/contacts/8:orgid:x"}) {
		t.Fatal("expected no author match without a signed-in identity")
	}

	state.teams.SetProfile(&models.User{Mri: "8:orgid:self-1"})
	if !state.isSelfAuthoredMessage(csa.ChatMessage{From: "https://host/v1/users/ME/contacts/8:orgid:self-1"}) {
		t.Fatal("expected a match for the self contact URL")
	}
	if state.isSelfAuthoredMessage(csa.ChatMessage{From: "https://host/v1/users/ME/contacts/8:orgid:self-10"}) {
		t.Fatal("expected no prefix-only match for a different mri")
	}
	if state.isSelfAuthoredMessage(csa.ChatMessage{From: "https://host/v1/users/ME/contacts/19:meeting_x@thread.v2"}) {
		t.Fatal("expected meeting activity to not count as self-authored")
	}
}

// TestLongMessageBodyWrapsInViewport pins that a message wider than the
// pane wraps instead of being clipped: the tail of a long body must be
// present in the rendered view (the viewport cannot scroll horizontally).
func TestLongMessageBodyWrapsInViewport(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)

	body := strings.Repeat("word ", 80) + "ENDMARKER"
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>" + body + "</p>"},
	}, time.Now(), "", false)
	state.ui.focus = PaneMessages
	state.ui.page = pageMain

	model := newChatModel(state)
	model.width, model.height = 100, 30

	frame := plain(model.View())
	joined := strings.Join(strings.Fields(frame), "")
	if !strings.Contains(joined, "ENDMARKER") {
		t.Fatalf("long message body must wrap, not clip; view was:\n%s", frame)
	}
}

// TestMessageAuthorLineCarriesTimestamp pins the author-line timestamp:
// today's messages show the clock time next to the sender, and a message
// without a ComposeTime keeps the bare name.
func TestMessageAuthorLineCarriesTimestamp(t *testing.T) {
	state := newUITestState()
	stamp := time.Date(time.Now().Year(), time.Now().Month(), time.Now().Day(), 9, 41, 0, 0, time.Local)

	entries := state.filterDisplayedMessages([]csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>", ComposeTime: api.RFC3339Time(stamp)},
	})
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	if entries[0].Main != "Alice" {
		t.Fatalf("expected the author line without the stamp, got %q", entries[0].Main)
	}
	if entries[0].Stamp != "09:41" {
		t.Fatalf("expected the stamp 09:41, got %q", entries[0].Stamp)
	}

	// The stamp renders in its own dim style, distinct from the bold
	// author name.
	state.setCurrentConversation(teams.Conversation{ID: "19:x", Title: "Chat"})
	state.ui.msgs.apply(state, teams.Conversation{ID: "19:x", Title: "Chat"}, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>", ComposeTime: api.RFC3339Time(stamp)},
	}, time.Time{}, "", false)
	state.ui.focus = PaneTree
	state.ui.page = pageMain
	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100
	rendered := model.View()
	if !strings.Contains(rendered, stampStyle.Render("09:41")) {
		t.Fatalf("expected the stamp rendered in the dim stamp style")
	}
	if !strings.Contains(rendered, authorStyle.Render("Alice")) {
		t.Fatalf("expected the author rendered in the author style")
	}

	entries = state.filterDisplayedMessages([]csa.ChatMessage{
		{Id: "2", ImDisplayName: "Bob", Content: "<p>hi</p>"},
	})
	if entries[0].Main != "Bob" || entries[0].Stamp != "" {
		t.Fatalf("expected no timestamp without ComposeTime, got %q/%q", entries[0].Main, entries[0].Stamp)
	}
}

// TestMessagesPageSizeCountsWrappedEntries pins paging against wrapped
// bodies: a page holds however many whole entries fit in the viewport,
// not a fixed half of the inner height.
func TestMessagesPageSizeCountsWrappedEntries(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)

	long := strings.Repeat("word ", 40)
	msgs := make([]csa.ChatMessage, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, csa.ChatMessage{
			Id:            fmt.Sprintf("m-%d", i),
			ImDisplayName: "Alice",
			Content:       "<p>" + long + "</p>",
		})
	}
	state.ui.msgs.apply(state, target, msgs, time.Now(), "", false)
	state.ui.msgs.setCursor(0)

	model := newChatModel(state)
	model.width, model.height = 120, 30

	page := model.messagesPageSize()
	if page >= len(msgs)/2 {
		t.Fatalf("wrapped entries should fit far fewer than half the list per page, got %d", page)
	}
	if page < 1 {
		t.Fatalf("page size must be at least one entry, got %d", page)
	}
}

// TestMessageCursorHighlightOnlyWhenFocused pins the focus cue: the row
// under the cursor is highlighted only while the message pane owns focus;
// while browsing the list nothing in the message view looks selected.
func TestMessageCursorHighlightOnlyWhenFocused(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>one</p>"},
		{Id: "2", ImDisplayName: "Bob", Content: "<p>two</p>"},
	}, time.Now(), "", false)
	state.ui.page = pageMain
	makeMessagesFocusable(state)

	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100

	state.ui.focus = PaneTree
	unfocused := model.View()
	if strings.Contains(unfocused, cursorStyle.Render("Bob")) {
		t.Fatalf("cursor row must not be highlighted while the list has focus")
	}
	if !strings.Contains(unfocused, authorStyle.Render("Bob")) {
		t.Fatalf("cursor row should render as a plain author row while unfocused")
	}

	state.ui.focus = PaneMessages
	focused := model.View()
	if !strings.Contains(focused, cursorStyle.Render("Bob")) {
		t.Fatalf("cursor row must be highlighted once the message pane has focus")
	}
}

// TestMessagesPaneShowsPagination pins the page indicator against the
// viewport's real scroll range: at the newest message the current page must
// equal the total (a partial last screen must still read N/N), and a
// single-screen conversation has no indicator.
func TestMessagesPaneShowsPagination(t *testing.T) {
	for _, count := range []int{3, 9, 11, 40} {
		state := newUITestState()
		target := teams.Conversation{ID: "19:x", Title: "Chat"}
		state.setCurrentConversation(target)

		msgs := make([]csa.ChatMessage, 0, count)
		for i := 0; i < count; i++ {
			msgs = append(msgs, csa.ChatMessage{
				Id:            fmt.Sprintf("m-%d", i),
				ImDisplayName: "Alice",
				Content:       "<p>body text</p>",
			})
		}
		state.ui.msgs.apply(state, target, msgs, time.Now(), "", false)
		state.ui.focus = PaneMessages
		state.ui.page = pageMain

		model := newChatModel(state)
		model.width, model.height = 100, 24
		model.help.Width = 100

		rows := strings.Split(plain(model.View()), "\n")
		header := ""
		for _, row := range rows {
			if strings.Contains(row, " | ") {
				header = row
				break
			}
		}
		if header == "" {
			t.Fatalf("pane title row not found in frame:\n%s", strings.Join(rows, "\n"))
		}
		match := paginationPattern.FindStringSubmatch(header)
		if count <= 3 {
			if match != nil {
				t.Fatalf("count=%d: expected no page indicator, got %q", count, header)
			}
			continue
		}
		if match == nil {
			t.Fatalf("count=%d: expected a page indicator, got %q", count, header)
		}
		if match[1] != match[2] {
			t.Fatalf("count=%d: at the newest message current must equal total, got %s", count, match[0])
		}
	}
}

// TestMessageScrollBarTracksPosition pins the progress-bar indicator:
// it renders only for multi-screen conversations and its filled cells grow
// as the viewport moves toward the newest message.
func TestMessageScrollBarTracksPosition(t *testing.T) {
	state := newUITestState()
	target := teams.Conversation{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)

	msgs := make([]csa.ChatMessage, 0, 40)
	for i := 0; i < 40; i++ {
		msgs = append(msgs, csa.ChatMessage{
			Id:            fmt.Sprintf("m-%d", i),
			ImDisplayName: "Alice",
			Content:       "<p>body text</p>",
		})
	}
	state.ui.msgs.apply(state, target, msgs, time.Now(), "", false)
	state.ui.focus = PaneMessages
	state.ui.page = pageMain

	model := newChatModel(state)
	model.width, model.height = 100, 24
	model.help.Width = 100

	rows := strings.Split(plain(model.View()), "\n")
	header := ""
	for _, row := range rows {
		if strings.Contains(row, " | ") {
			header = row
			break
		}
	}
	if header == "" {
		t.Fatalf("pane title row not found in frame:\n%s", strings.Join(rows, "\n"))
	}
	if !strings.Contains(header, "█") {
		t.Fatalf("expected the scroll bar on the title row, got %q", header)
	}
	if !paginationPattern.MatchString(header) {
		t.Fatalf("expected the page count beside the bar, got %q", header)
	}
	bottom := strings.Count(header, "█")

	state.ui.msgs.setCursor(0)
	rows = strings.Split(plain(model.View()), "\n")
	header = ""
	for _, row := range rows {
		if strings.Contains(row, "|") {
			header = row
			break
		}
	}
	top := strings.Count(header, "█")
	if top >= bottom {
		t.Fatalf("bar must be less full at the top (%d) than at the bottom (%d)\n%s", top, bottom, header)
	}
}
