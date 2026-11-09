package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
)

// TestReproFrameHeight is the regression fence for the "layout offset off
// the top of the screen" bug: any frame taller than the terminal scrolls
// the alt screen and clips the pane top borders.
func TestReproFrameHeight(t *testing.T) {
	for _, height := range []int{1, 2, 3, 4, 6, 8, 10, 16, 24, 48, 100} {
		for _, focus := range []string{PaneTree, PaneMessages, PaneComposer} {
			for _, help := range []bool{false, true} {
				for _, status := range []string{"", "sending..."} {
					state := &AppState{logger: discardLogger, messageLimit: defaultMessageLimit}
					state.initRuntime(context.Background())
					state.ui.page = pageMain
					state.ui.focus = focus
					state.ui.composerStatus = status
					// A tall draft must not push the frame past the
					// terminal: the textarea viewport clips at MaxHeight,
					// but the fence only proves it when a multi-line
					// draft exercises the wrapped/overflow path.
					state.ui.composerText = strings.Repeat("line\n", 5) + "tail text"
					state.ui.msgs.apply(state, ConversationTarget{ID: "19:x", Title: "Chat"},
						[]csa.ChatMessage{{Id: "1", ImDisplayName: "Alice", Content: "<p>hi</p>"}},
						time.Now(), "", false)

					m := newChatModel(state)
					m.help.ShowAll = help
					m.width, m.height = 120, height
					rows := strings.Split(m.View(), "\n")
					if len(rows) > height {
						t.Errorf("h=%d focus=%s help=%v status=%q: frame is %d rows, exceeds terminal by %d",
							height, focus, help, status, len(rows), len(rows)-height)
					}
				}
			}
		}
	}
}
