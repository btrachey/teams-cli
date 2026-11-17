package tui

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-api/pkg/models"
	"github.com/fossteams/teams-cli/internal/config"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/teams"
)

// driveProgram runs the REAL bubbletea event loop: keys are written to an
// input pipe and parsed by bubbletea's input reader, exactly like a
// terminal. Only the network startup (state.start) is bypassed; everything
// else (input parsing, key routing, model update, rendering) is production
// code. Frames are discarded; assertions read the shared AppState the
// program mutates.
//
// input is split into individual key tokens so every keystroke arrives as
// its own KeyMsg — deliberate keystrokes. It deliberately does NOT model
// paste or very fast typing: bubbletea's detectOneMsg fuses printable bytes
// sharing one Read() into a SINGLE KeyRunes message, and a real terminal
// delivers pasted text exactly that way. driveProgramRaw below feeds whole
// strings as single segments to pin that production handlers survive fused
// batches (shortcuts must never fire from a batch).
func driveProgram(t *testing.T, state *AppState, input string) {
	t.Helper()
	driveProgramRaw(t, state, splitKeyTokens(input))
}

// driveProgramRaw writes each segment as ONE write to the input pipe (one
// reader Read), so a multi-character printable segment is delivered fused,
// exactly like a paste. Segments are separated by a terminal-style gap.
//
// Teardown is program.Quit(), NOT a Ctrl+C keystroke: Ctrl+C flows through
// handleKey which calls state.requestStop() and cancels the app context,
// so a post-run appContext().Err() check could never distinguish "the app
// quit because a paste hijacked the q shortcut" from "the harness quit it".
// Quit() posts QuitMsg straight to the event loop, bypassing Update; the
// only path that cancels the app context is requestStop() from a real quit
// binding, so tests may assert appContext().Err() == nil to prove the
// program survived hostile input.
func driveProgramRaw(t *testing.T, state *AppState, segments []string) {
	t.Helper()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	program := tea.NewProgram(newChatModel(state),
		tea.WithInput(stdinR),
		tea.WithOutput(io.Discard),
		tea.WithoutRenderer(),
	)
	state.createUI(program)

	done := make(chan struct{})
	var runErr error
	go func() {
		defer close(done)
		_, runErr = program.Run()
	}()

	state.sendUIMsg(mainReadyMsg{})

	// Let the program enter its event loop before pumping keys.
	time.Sleep(150 * time.Millisecond)

	// Each segment is one write (one reader Read); a terminal-style gap
	// separates segments. Closing the write end makes the reader hit EOF
	// (silently ignored by the read loop), then a settle window lets the
	// event loop drain every queued key before Quit.
	for _, seg := range segments {
		if _, err := io.WriteString(stdinW, seg); err != nil {
			t.Fatalf("write segment %q: %v", seg, err)
		}
		time.Sleep(15 * time.Millisecond)
	}
	stdinW.Close()
	time.Sleep(250 * time.Millisecond)
	// In a goroutine: if the loop is wedged (the production deadlock this
	// harness exists to catch), p.Send inside Quit would block the test
	// before the watchdog select below can fire.
	go program.Quit()

	// A production deadlock (e.g. a synchronous program.Kill or
	// program.Send from inside Update) strands eventLoop on a channel
	// send; fail the test instead of hanging the suite. Kill is only a
	// best-effort courtesy for the leaked event loop and must never be
	// waited on: if the loop is wedged, waiting re-creates the hang.
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		program.Kill()
		stdinR.Close()
		t.Fatal("real event loop did not exit after Quit")
	}
	if runErr != nil {
		t.Fatalf("program run: %v", runErr)
	}
	stdinR.Close()
}

// splitKeyTokens breaks a raw input string into individual keypress
// tokens. CSI/SS3 escape sequences (arrow keys, etc.) stay intact; every
// other Unicode code point becomes its own token, matching how a terminal
// frames keystrokes for bubbletea's reader.
func splitKeyTokens(input string) []string {
	var tokens []string
	rs := []rune(input)
	for i := 0; i < len(rs); {
		if rs[i] == 0x1b && i+1 < len(rs) && (rs[i+1] == '[' || rs[i+1] == 'O') {
			j := i + 2
			for j < len(rs) && !(rs[j] >= 0x40 && rs[j] <= 0x7e) {
				j++
			}
			if j < len(rs) {
				j++
			}
			tokens = append(tokens, string(rs[i:j]))
			i = j
			continue
		}
		tokens = append(tokens, string(rs[i]))
		i++
	}
	return tokens
}

// smokeFetch serves canned messages for any conversation so the
// auto-load on mainReadyMsg succeeds without network access.
func smokeFetch(canned []csa.ChatMessage) func(context.Context, teams.Conversation) ([]csa.ChatMessage, error) {
	return func(_ context.Context, _ teams.Conversation) ([]csa.ChatMessage, error) {
		return canned, nil
	}
}

func smokeState(t *testing.T, canned []csa.ChatMessage) *AppState {
	t.Helper()
	state := sampleConversationState()
	state.logger = logging.Discard
	state.messageLimit = config.DefaultMessageLimit
	state.initRuntime(context.Background())
	state.teams.SetProfile(&models.User{Mri: "8:orgid:self-1", DisplayName: "Brian"})
	state.fetchMessagesOverride = smokeFetch(canned)
	return state
}

func cannedMessages() []csa.ChatMessage {
	return []csa.ChatMessage{
		{Id: "msg-1", ImDisplayName: "Austin", Content: "<p>ping</p>", ComposeTime: api.RFC3339Time(time.Now().Add(-time.Minute))},
		{Id: "msg-2", ImDisplayName: "Alice", Content: "<p>pong</p>", ComposeTime: api.RFC3339Time(time.Now())},
	}
}

func TestRealProgramComposerTypingAndNavigation(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// Tab tree->messages, 'c' into composer, type text, Esc back.
	driveProgram(t, state, "\tcHello world\x1b")

	if state.ui.focus != PaneMessages {
		t.Fatalf("expected focus back on messages after Tab, got %s", state.ui.focus)
	}
	if state.ui.composerText != "Hello world" {
		t.Fatalf("expected typed text to reach the composer draft, got %q", state.ui.composerText)
	}
}

func TestRealProgramPasteNeverQuitsFromMessagesPane(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// Paste "quick fix" onto the messages pane. bubbletea fuses a paste
	// into a single KeyRunes message, so a handler dispatching on
	// Runes[0] would see 'q' and quit the app. The paste must be inert:
	// no shutdown request, focus unchanged.
	driveProgramRaw(t, state, []string{"\t", "quick fix"})

	if err := state.appContext().Err(); err != nil {
		t.Fatalf("pasting text starting with 'q' must not quit the app: %v", err)
	}
	if state.ui.focus != PaneMessages {
		t.Fatalf("expected focus to stay on messages after a paste, got %s", state.ui.focus)
	}
	if state.ui.composerText != "" {
		t.Fatalf("expected a paste on the messages pane to be inert, draft is %q", state.ui.composerText)
	}
}

func TestRealProgramPasteReachesComposerIntact(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// 'c' (deliberate single key) focuses the composer; the paste that
	// follows arrives fused — including a leading run that starts with a
	// shortcut-looking character — and must land in the draft verbatim.
	driveProgramRaw(t, state, []string{"\t", "c", "mcp review at 3"})

	if state.ui.focus != PaneComposer {
		t.Fatalf("expected 'c' to focus the composer, got %s", state.ui.focus)
	}
	if state.ui.composerText != "mcp review at 3" {
		t.Fatalf("expected the full pasted draft, got %q", state.ui.composerText)
	}
}

func TestRealProgramReplyModeAndCancel(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// The tree auto-load lands on msg-2 (newest, Alice). Tab to
	// messages, 'c' for reply, type, then Backspace: it must both delete
	// the last character and cancel the pending thread reply. Esc then
	// leaves the composer.
	driveProgram(t, state, "\tcPing\x7f\x1b")

	if state.composerReply.messageID != "" {
		t.Fatalf("expected Backspace to cancel the reply context, got %+v", state.composerReply)
	}
	if state.ui.composerText != "Pin" {
		t.Fatalf("expected Backspace to delete one character, got %q", state.ui.composerText)
	}
	if state.ui.focus != PaneMessages {
		t.Fatalf("expected Esc to leave the composer toward messages, got %s", state.ui.focus)
	}
}

func TestRealProgramReplyModeSetsContext(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// Tab, 'c' replies to the newest message, type, Tab out (no cancel):
	// the reply context and draft must both survive leaving the pane.
	driveProgram(t, state, "\tcPing\t")

	if state.composerReply.messageID != "msg-2" {
		t.Fatalf("expected reply context on the newest message, got %+v", state.composerReply)
	}
	if state.ui.composerText != "Ping" {
		t.Fatalf("expected the draft to survive leaving the composer, got %q", state.ui.composerText)
	}
	if state.ui.focus != PaneMessages {
		t.Fatalf("expected Tab to leave the composer, got %s", state.ui.focus)
	}
}

func TestRealProgramHelpAndQuitKeys(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// '?' toggles the help component's full view and keeps the focus.
	driveProgram(t, state, "?")

	if state.ui.focus != PaneTree {
		t.Fatalf("expected help toggle to preserve list focus, got %s", state.ui.focus)
	}
	if state.appContext().Err() != nil {
		t.Fatal("expected the app to stay running after the help toggle")
	}
}

func TestRealProgramQuitViaQKey(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// 'q' returns tea.Quit: Run returns, and the model requested
	// shutdown (cancelled the app context).
	driveProgram(t, state, "q")

	if state.appContext().Err() == nil {
		t.Fatal("expected 'q' to request shutdown (cancel the app context)")
	}
}

func TestRealProgramTreeNavigationLoadsConversation(t *testing.T) {
	state := smokeState(t, cannedMessages())

	// The mainReadyMsg auto-load already selects the first conversation
	// (General, chan-1); move the cursor down three rows and back up
	// through the same rows, proving tree navigation drives loads
	// through the real loop without wedging.
	driveProgram(t, state, "\x1b[B\x1b[B\x1b[B\x1b[A\x1b[A\x1b[A")

	if !state.conversationLoaded {
		t.Fatal("expected the conversation to be loaded via the real event loop")
	}
	if _, ok := state.messageAtCurrentIndex(); !ok {
		t.Fatal("expected the auto-loaded conversation to have selectable messages")
	}
}
