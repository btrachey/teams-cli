package main

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-api/pkg/models"
	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

func TestMessageListItemPutsAuthorAboveBody(t *testing.T) {
	mainText, secondaryText := messageListItem("Jane Doe", "<p>first line</p><p>second line</p>")

	if mainText != "[::b]Jane Doe[-::]" {
		t.Fatalf("expected bold author as the main (top) line, got %q", mainText)
	}
	if !strings.Contains(secondaryText, "first line") || !strings.Contains(secondaryText, "second line") {
		t.Fatalf("expected body under the author, got %q", secondaryText)
	}
	if strings.Contains(secondaryText, "\n") {
		t.Fatalf("expected paragraph breaks joined for the two-row list item, got %q", secondaryText)
	}
}

func TestMessageListItemEscapesMarkupInUserContent(t *testing.T) {
	mainText, secondaryText := messageListItem("A[b]B", "<p>hello [white]not-a-color</p>")

	// Author and body must survive tview's markup parser unchanged.
	if !strings.Contains(mainText, tview.Escape("A[b]B")) {
		t.Fatalf("expected escaped author, got %q", mainText)
	}
	if !strings.Contains(secondaryText, tview.Escape("hello [white]not-a-color")) {
		t.Fatalf("expected escaped body, got %q", secondaryText)
	}
}

func TestRenderConversationMessagesOrdersAuthorFirst(t *testing.T) {
	app := tview.NewApplication()
	state := AppState{
		app:          app,
		logger:       discardLogger,
		messageLimit: defaultMessageLimit,
		components:   map[string]tview.Primitive{},
	}
	chatList := tview.NewList()
	state.components[ViChat] = chatList

	messages := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>", ComposeTime: api.RFC3339Time(time.Now())},
		{Id: "2", ImDisplayName: "", Content: "<p>anonymous skipped</p>", ComposeTime: api.RFC3339Time(time.Now())},
		{Id: "3", ImDisplayName: "Bob", Content: "<p>yo</p>", ComposeTime: api.RFC3339Time(time.Now())},
	}
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Now(), false)

	if chatList.GetItemCount() != 2 {
		t.Fatalf("expected 2 rendered messages, got %d", chatList.GetItemCount())
	}

	mainText, secondaryText := chatList.GetItemText(0)
	if mainText != "[::b]Alice[-::]" {
		t.Fatalf("expected author-first main text, got %q", mainText)
	}
	if secondaryText != "[white]hi" {
		t.Fatalf("expected body as secondary text, got %q", secondaryText)
	}

	// Auto-scroll landed on the newest row; reply-context lookup must resolve
	// the message behind the *current* row, wherever selection sits.
	msg, ok := state.messageAtCurrentListIndex()
	if !ok {
		t.Fatal("expected message lookup to succeed after render")
	}
	if msg.ImDisplayName != "Bob" {
		t.Fatalf("expected reply context on the newest row (Bob), got %q", msg.ImDisplayName)
	}
	chatList.SetCurrentItem(0)
	msg, ok = state.messageAtCurrentListIndex()
	if !ok || msg.ImDisplayName != "Alice" {
		t.Fatalf("expected reply context to follow selection to Alice, got %q (ok=%v)", msg.ImDisplayName, ok)
	}
}

// TestRenderedMessagesDrawAuthorRowAboveBodyRow draws the shipped render
// path onto a simulated terminal and asserts screen-row order: the author
// line directly above the body line, never concatenated on one row.
func TestRenderedMessagesDrawAuthorRowAboveBodyRow(t *testing.T) {
	sim := tcell.NewSimulationScreen("")
	if err := sim.Init(); err != nil {
		t.Fatalf("unable to init simulation screen: %v", err)
	}
	sim.SetSize(80, 12)

	app := tview.NewApplication()
	app.SetScreen(sim)

	chatList := tview.NewList()
	chatList.SetBorder(true).SetTitle("Chat")

	state := AppState{
		app:          app,
		logger:       discardLogger,
		messageLimit: defaultMessageLimit,
		components:   map[string]tview.Primitive{ViChat: chatList},
	}
	messages := []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>hello there</p>", ComposeTime: api.RFC3339Time(time.Now())},
	}
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	app.SetRoot(chatList, true).SetFocus(chatList)
	app.ForceDraw()

	width, height := sim.Size()
	contents, _, _ := sim.GetContents()
	authorRow, bodyRow := -1, -1
	rows := make([]string, height)
	for y := range height {
		var builder strings.Builder
		for x := range width {
			builder.WriteString(string(contents[y*width+x].Runes))
		}
		rows[y] = builder.String()
		if authorRow < 0 && strings.Contains(rows[y], "Alice") {
			authorRow = y
		}
		if bodyRow < 0 && strings.Contains(rows[y], "hello there") {
			bodyRow = y
		}
	}

	if authorRow < 0 || bodyRow < 0 {
		t.Fatalf("author row %d, body row %d; screen was:\n%s", authorRow, bodyRow, strings.Join(rows, "\n"))
	}
	if bodyRow != authorRow+1 {
		t.Fatalf("expected body directly below author, got rows %d and %d:\n%s", authorRow, bodyRow, strings.Join(rows, "\n"))
	}
	if strings.Contains(rows[authorRow], "hello there") {
		t.Fatalf("author and body concatenated on one row: %q", rows[authorRow])
	}
}

func chatRenderState(t *testing.T) (*AppState, *tview.List) {
	t.Helper()
	chatList := tview.NewList()
	state := &AppState{
		app:          tview.NewApplication(),
		logger:       discardLogger,
		messageLimit: defaultMessageLimit,
		components:   map[string]tview.Primitive{ViChat: chatList},
	}
	return state, chatList
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

func TestRenderConversationMessagesStartsAtNewestOnLoad(t *testing.T) {
	state, chatList := chatRenderState(t)

	// fetchMessagesOnce sorts ascending by ComposeTime; replicate that.
	messages := timedMessages(3)
	sort.Sort(csa.SortMessageByTime(messages))
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	if chatList.GetItemCount() != 3 {
		t.Fatalf("expected 3 rendered messages, got %d", chatList.GetItemCount())
	}
	if got := chatList.GetCurrentItem(); got != 2 {
		t.Fatalf("expected selection on newest (index 2) on load, got %d", got)
	}
}

func TestRenderConversationMessagesRefreshKeepsScrolledPosition(t *testing.T) {
	state, chatList := chatRenderState(t)
	messages := timedMessages(3)
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	// User scrolls up to read history, then a background refresh lands.
	chatList.SetCurrentItem(0)
	messages = append(messages, csa.ChatMessage{
		Id:            "3",
		ImDisplayName: "Bob",
		Content:       "<p>newest</p>",
		ComposeTime:   api.RFC3339Time(time.Now()),
	})
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, true)

	if got := chatList.GetCurrentItem(); got != 0 {
		t.Fatalf("expected refresh to preserve scrolled position (index 0), got %d", got)
	}
}

func TestRenderConversationMessagesRefreshFollowsTail(t *testing.T) {
	state, chatList := chatRenderState(t)
	messages := timedMessages(3)
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	// Selection sits at the tail; a new message arrives on refresh.
	messages = append(messages, csa.ChatMessage{
		Id:            "3",
		ImDisplayName: "Bob",
		Content:       "<p>newest</p>",
		ComposeTime:   api.RFC3339Time(time.Now()),
	})
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, true)

	if got := chatList.GetCurrentItem(); got != 3 {
		t.Fatalf("expected tail-following to select the new newest (index 3), got %d", got)
	}
}

// TestRenderConversationMessagesForcedReloadJumpsToNewest pins the post-send
// contract: sendComposerMessage -> reloadCurrentConversation -> loadMessages
// re-renders with preserveSelection=false even though the list is already
// populated, so the user lands on their own message even when they had
// scrolled up into history. (Background live refresh uses true and must NOT
// hijack the scroll position; that arm is the test above.)
func TestRenderConversationMessagesForcedReloadJumpsToNewest(t *testing.T) {
	state, chatList := chatRenderState(t)
	messages := timedMessages(3)
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	// User scrolls up to read history, then sends: the reload must snap
	// past their scroll position to the newest message.
	chatList.SetCurrentItem(0)
	messages = append(messages, csa.ChatMessage{
		Id:            "3",
		ImDisplayName: "Brian Tracey",
		Content:       "<p>just sent</p>",
		ComposeTime:   api.RFC3339Time(time.Now()),
	})
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	if got := chatList.GetCurrentItem(); got != 3 {
		t.Fatalf("expected post-send reload to jump to the newest (index 3), got %d", got)
	}
}

// TestRenderedMessagesScrollToBottomOnLoad proves the visible window lands on
// the newest messages: with 30 messages on a 12-row screen, the last message
// is on screen and the oldest is scrolled off the top.
func TestRenderedMessagesScrollToBottomOnLoad(t *testing.T) {
	sim := tcell.NewSimulationScreen("")
	if err := sim.Init(); err != nil {
		t.Fatalf("unable to init simulation screen: %v", err)
	}
	sim.SetSize(80, 12)

	app := tview.NewApplication()
	app.SetScreen(sim)

	chatList := tview.NewList()
	chatList.SetBorder(true).SetTitle("Chat")
	state := &AppState{
		app:          app,
		logger:       discardLogger,
		messageLimit: defaultMessageLimit,
		components:   map[string]tview.Primitive{ViChat: chatList},
	}

	messages := timedMessages(30)
	sort.Sort(csa.SortMessageByTime(messages))
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)
	app.SetRoot(chatList, true).SetFocus(chatList)
	app.ForceDraw()

	width, height := sim.Size()
	contents, _, _ := sim.GetContents()
	var screenText strings.Builder
	for y := range height {
		for x := range width {
			screenText.WriteString(string(contents[y*width+x].Runes))
		}
	}
	visible := screenText.String()

	if !strings.Contains(visible, "message 29") {
		t.Fatalf("expected newest message visible after load; screen was:\n%s", visible)
	}
	if strings.Contains(visible, "message 0 ") {
		t.Fatalf("expected oldest message scrolled off-screen after load; screen was:\n%s", visible)
	}
}

// TestRenderConversationMessagesShowsOwnNamelessSends pins the fix for CLI
// sends invisible in the chat window: the message service stores posts that
// omitted imdisplayname with an empty sender name, so a nameless message
// from the signed-in user must render (under the user's display name),
// while nameless system activity (thread events, call logs) stays hidden.
func TestRenderConversationMessagesShowsOwnNamelessSends(t *testing.T) {
	state, chatList := chatRenderState(t)
	state.me = &models.User{Mri: "8:orgid:self-1", DisplayName: "Brian Tracey"}

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
	state.renderConversationMessages(ConversationTarget{ID: "19:x", Title: "Chat"}, messages, time.Time{}, false)

	if chatList.GetItemCount() != 2 {
		main, secondary := chatList.GetItemText(0)
		t.Fatalf("expected only Alice and the own nameless send, got %d items (0=%q/%q)", chatList.GetItemCount(), main, secondary)
	}

	mainText, secondaryText := chatList.GetItemText(1)
	if mainText != "[::b]Brian Tracey[-::]" {
		t.Fatalf("expected own nameless send under the self display name, got %q", mainText)
	}
	if secondaryText != "[white]this is a test" {
		t.Fatalf("expected the nameless send body, got %q", secondaryText)
	}

	// Reply context lookup must see the rendered nameless message too.
	chatList.SetCurrentItem(1)
	msg, ok := state.messageAtCurrentListIndex()
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

	state.me = &models.User{Mri: "8:orgid:self-1"}
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
