package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// syncBuffer is a mutex-guarded buffer so the live smoke can read the bytes
// bubbletea's real renderer wrote without racing the event loop.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Flush() error { return nil }

func (b *syncBuffer) snapshot() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestZZLiveCalendarSmoke drives the real startup (auth + teams client from
// the real token files) and the real bubbletea event loop, presses 'c', and
// asserts the calendar section painted live meetings. Skipped unless
// ZZ_LIVE=1.
func TestZZLiveCalendarSmoke(t *testing.T) {
	if os.Getenv("ZZ_LIVE") != "1" {
		t.Skip("set ZZ_LIVE=1 to run the live calendar smoke")
	}

	state := &AppState{}
	state.logger = discardLogger
	state.messageLimit = defaultMessageLimit
	state.liveRefreshDisabled = true
	state.tokenRefresh = tokenRefreshConfig{enabled: true, timeout: 3 * time.Minute}
	state.initRuntime(context.Background())

	// Real startup: resolves tokens, builds the teams client, loads state.
	state.start()
	if state.client() == nil {
		t.Fatal("live startup did not produce a teams client")
	}

	out := &syncBuffer{}
	program := tea.NewProgram(newChatModel(state),
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(out),
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	state.createUI(program)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = program.Run()
	}()

	state.sendUIMsg(mainReadyMsg{})
	time.Sleep(600 * time.Millisecond)

	// Press 'c' to open the calendar through the real key path.
	program.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})

	// Wait for the async calendar fetch to populate the shared state. The
	// budget must clear a full token cycle: with expired token files,
	// state.start() runs the Electron silent-SSO refresh (~16s measured)
	// before the calendar fetch even begins, and a too-tight deadline
	// false-fails mid-refresh.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if state.ui.page == pageCalendar && state.calendar.loaded {
			time.Sleep(500 * time.Millisecond)
			break
		}
	}
	_ = out.snapshot()

	program.Quit()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		program.Kill()
	}

	t.Logf("page=%d loaded=%v offset=%d events=%d loadedAt=%v",
		state.ui.page, state.calendar.loaded, state.calendar.windowOffset,
		len(state.ui.cal.entries), state.calendar.loadedAt)
	for i, e := range state.ui.cal.entries {
		if i >= 8 {
			break
		}
		t.Logf("row %d: %s | %s", i, e.mainLine, e.detailLine)
	}

	// Render the current model directly so the assertion is on the exact
	// frame independent of renderer timing.
	state.ui.page = pageCalendar
	final := newChatModel(state).View()
	plainOut := stripANSIFrame(final)
	t.Logf("=== calendar frame ===\n%s", plainOut)

	if !strings.Contains(plainOut, "Calendar") {
		t.Fatalf("live calendar frame missing title:\n%s", plainOut)
	}
	if !state.calendar.loaded {
		t.Fatalf("live calendar never loaded: entries=%d error=%q",
			len(state.ui.cal.entries), state.ui.cal.errorText)
	}
	if len(state.ui.cal.entries) == 0 {
		t.Logf("NOTE: window %s..%s has no meetings",
			state.calendar.rangeStart.Format(time.RFC3339),
			state.calendar.rangeEnd.Format(time.RFC3339))
	}
}

func stripANSIFrame(s string) string {
	var b strings.Builder
	inEsc := false
	for i := range len(s) {
		ch := s[i]
		switch {
		case inEsc:
			if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
				inEsc = false
			}
		case ch == 0x1b:
			inEsc = true
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}
