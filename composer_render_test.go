package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/fossteams/teams-api/pkg/csa"
)

// drawMainViewOnSim renders the shipped main view on a simulated terminal
// with the composer focused and returns the visible screen rows.
func drawMainViewOnSim(t *testing.T, state *AppState, root tview.Primitive) []string {
	t.Helper()
	sim := tcell.NewSimulationScreen("")
	if err := sim.Init(); err != nil {
		t.Fatalf("unable to init simulation screen: %v", err)
	}
	sim.SetSize(120, 30)

	state.app.SetScreen(sim)
	state.app.SetRoot(root, true)
	state.updateComposerLabel()
	state.focusComponent(ViComposer)
	state.app.ForceDraw()

	width, height := sim.Size()
	contents, _, _ := sim.GetContents()
	rows := make([]string, height)
	for y := range height {
		var builder strings.Builder
		for x := range width {
			builder.WriteString(string(contents[y*width+x].Runes))
		}
		rows[y] = builder.String()
	}
	return rows
}

func newComposerRenderState(t *testing.T) (*AppState, tview.Primitive) {
	t.Helper()
	state := &AppState{
		app:          tview.NewApplication(),
		logger:       discardLogger,
		messageLimit: defaultMessageLimit,
		pages:        tview.NewPages(),
		components:   map[string]tview.Primitive{},
	}
	state.initRuntime(context.Background())
	root := state.createMainView()
	return state, root
}

// TestComposerDrawsTypedTextInRowBelowMessages pins the screen-visible
// contract that the composer is a full-width input line stacked under the
// messages list: typed text must appear on screen, below the messages
// border. A FlexColumn layout regression shrinks the composer to a
// composerHeight-wide column on the right edge where text is invisible.
func TestComposerDrawsTypedTextInRowBelowMessages(t *testing.T) {
	state, root := newComposerRenderState(t)
	state.composerInput.SetText("hello there")
	rows := drawMainViewOnSim(t, state, root)

	textRow := -1
	textCol := -1
	listBottom := -1
	for y, row := range rows {
		if index := strings.Index(row, "hello there"); index >= 0 && textRow < 0 {
			textRow = y
			textCol = index
		}
		if strings.Contains(row, "Messages") {
			listBottom = y
		}
	}
	if textRow < 0 {
		t.Fatalf("typed composer text never rendered; screen was:\n%s", strings.Join(rows, "\n"))
	}
	if textRow <= listBottom {
		t.Fatalf("expected composer text below the messages area (row %d), got row %d:\n%s",
			listBottom, textRow, strings.Join(rows, "\n"))
	}
	// A FlexColumn layout shrinks the composer to a composerHeight-wide
	// column on the right edge, hiding the text; a full-width input line
	// starts the text well past the left border.
	if textCol < 10 {
		t.Fatalf("expected a wide composer field, text rendered at column %d:\n%s",
			textCol, strings.Join(rows, "\n"))
	}
}

// TestComposerReplyLabelRenders pins that reply mode shows the "Reply to
// <author>" label inside the visible composer row area.
func TestComposerReplyLabelRenders(t *testing.T) {
	state, root := newComposerRenderState(t)
	state.composerReply = replyContext{target: ConversationTarget{ID: "19:x"}, messageID: "7"}
	state.displayedMessages = []csa.ChatMessage{{Id: "7", ImDisplayName: "Alice"}}

	rows := drawMainViewOnSim(t, state, root)

	labelRow := -1
	for y, row := range rows {
		if strings.Contains(row, "Reply to Alice") {
			labelRow = y
			break
		}
	}
	if labelRow < 0 {
		t.Fatalf("reply label never rendered; screen was:\n%s", strings.Join(rows, "\n"))
	}
}
