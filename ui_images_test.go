package main

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
)

func TestMessageImageURLsExtractsImgSources(t *testing.T) {
	html := `<p>look</p><img src="https://eu-api.asm.skype.com/v1/objects/abc/views/imgo">` +
		`<img src="//us-api.asm.skype.com/v1/objects/def/views/imgo">` +
		`<img src="https://eu-api.asm.skype.com/v1/objects/abc/views/imgo">` +
		`<img src="javascript:alert(1)">`
	urls := messageImageURLs(html)
	if len(urls) != 2 {
		t.Fatalf("expected 2 unique http(s) images, got %#v", urls)
	}
	if urls[1] != "https://us-api.asm.skype.com/v1/objects/def/views/imgo" {
		t.Fatalf("expected protocol-relative URL normalized, got %q", urls[1])
	}
}

func TestImageAuthHostAllowlist(t *testing.T) {
	for host, want := range map[string]bool{
		"eu-api.asm.skype.com":        true,
		"asyncgw.teams.microsoft.com": true,
		"evil.example.com":            false,
		"teams.microsoft.com.evil.io": false,
	} {
		if got := imageAuthHost(host); got != want {
			t.Fatalf("imageAuthHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestRenderImageArtHalfBlocks(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	art, err := renderImageArt(buf.Bytes(), 10, 6)
	if err != nil {
		t.Fatalf("renderImageArt: %v", err)
	}
	rows := strings.Split(art, "\n")
	if len(rows) < 1 || len(rows) > 6 {
		t.Fatalf("expected 1..6 rows, got %d", len(rows))
	}
	if !strings.Contains(art, "\x1b[38;2;255;0;0m") || !strings.Contains(art, "▀") {
		t.Fatalf("expected truecolor half blocks")
	}
	if _, err := renderImageArt([]byte("nope"), 10, 6); err == nil {
		t.Fatal("expected an error for non-image data")
	}
}

func TestMessageViewerShowsImagePlaceholderAndArt(t *testing.T) {
	state := newUITestState()
	target := ConversationTarget{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	imageURL := "https://eu-api.asm.skype.com/v1/objects/abc/views/imgo"
	state.ui.msgs.apply(state, target, []csa.ChatMessage{{
		Id: "1", ImDisplayName: "Alice",
		Content: `<p>see this</p><img src="` + imageURL + `">`,
	}}, time.Now(), "", false)
	state.ui.focus = PaneMessages
	state.ui.page = pageMain

	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100
	if !strings.Contains(plain(model.View()), "[image loading…]") {
		t.Fatalf("expected an image placeholder before download")
	}
	updated, _ := model.handleMessageImage(messageImageMsg{url: imageURL, art: "\x1b[38;2;0;255;0m▀\x1b[0m"})
	model = updated.(chatModel)
	if !strings.Contains(model.View(), "\x1b[38;2;0;255;0m▀") {
		t.Fatalf("expected the rendered art after download")
	}
}

// TestTransparentPixelsCompositeOverBackdrop pins that fully transparent
// pixels render as the backdrop, not as black.
func TestTransparentPixelsCompositeOverBackdrop(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{}) // transparent
	img.Set(0, 1, color.RGBA{})
	img.Set(1, 1, color.RGBA{})

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	art, err := renderImageArt(buf.Bytes(), 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(art, "38;2;0;0;0") {
		t.Fatalf("transparent pixels must not render black: %q", art)
	}
	if !strings.Contains(art, "38;2;30;30;30") {
		t.Fatalf("expected the backdrop colour for transparent pixels: %q", art)
	}
}

// TestAMSImageTokenRejectsMalformedToken pins the panic fence: a garbage
// skype token must yield an error, never a nil-JWT panic from teams-api.
func TestAMSImageTokenRejectsMalformedToken(t *testing.T) {
	t.Setenv("MS_TEAMS_SKYPE_TOKEN", "not-a-jwt")
	state := &AppState{logger: discardLogger, messageLimit: defaultMessageLimit}
	state.initRuntime(context.Background())

	if _, err := state.amsImageToken(context.Background()); err == nil {
		t.Fatal("expected an error for a malformed skype token")
	}
}

func TestImageRenderMode(t *testing.T) {
	t.Setenv("TEAMS_CLI_IMAGES", "")
	t.Setenv("TERM_PROGRAM", "xterm")
	if got := imageRenderMode(); got != imageModeBlock {
		t.Fatalf("default mode = %q, want block", got)
	}
	t.Setenv("TERM_PROGRAM", "WezTerm")
	if got := imageRenderMode(); got != imageModeNative {
		t.Fatalf("WezTerm default = %q, want native", got)
	}
	t.Setenv("TEAMS_CLI_IMAGES", "native")
	if got := imageRenderMode(); got != imageModeNative {
		t.Fatalf("expected the native mode, got %q", got)
	}
	t.Setenv("TEAMS_CLI_IMAGES", "off")
	if got := imageRenderMode(); got != imageModeOff {
		t.Fatalf("off mode = %q", got)
	}
}

func TestImageCellSizeKeepsPortraitImagesLegible(t *testing.T) {
	cols, rows := imageCellSize(1080, 2400, imageMaxCols, imageMaxRows)
	if rows != imageMaxRows {
		t.Fatalf("portrait image should fill the row budget, got %dx%d", cols, rows)
	}
	if cols < 15 {
		t.Fatalf("portrait image collapsed to %d columns", cols)
	}

	cols, rows = imageCellSize(1920, 1080, imageMaxCols, imageMaxRows)
	if cols != imageMaxCols || rows >= imageMaxRows {
		t.Fatalf("landscape image should fill the column budget, got %dx%d", cols, rows)
	}
}

// TestImageRenderModeDefaultsToNative pins the default: terminals with the
// inline-image protocol render the real image, others fall back to block
// art.
func TestImageRenderModeDefaultsToNative(t *testing.T) {
	t.Setenv("TEAMS_CLI_IMAGES", "")
	t.Setenv("TERM_PROGRAM", "WezTerm")
	if got := imageRenderMode(); got != imageModeNative {
		t.Fatalf("expected native default, got %q", got)
	}
	t.Setenv("TEAMS_CLI_IMAGES", "off")
	if got := imageRenderMode(); got != imageModeOff {
		t.Fatalf("expected the off override, got %q", got)
	}
}

// TestSampleImagePixelAverages pins area-averaging: a black/white
// checkerboard collapsed into one cell must come out mid-grey, not the
// colour of whichever corner a point sample happened to hit.
func TestSampleImagePixelAverages(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{255, 255, 255, 255})
	img.Set(1, 0, color.RGBA{0, 0, 0, 255})
	img.Set(0, 1, color.RGBA{0, 0, 0, 255})
	img.Set(1, 1, color.RGBA{255, 255, 255, 255})

	got := sampleImagePixel(img, 0, 0, 1, 1)
	if got.R < 100 || got.R > 160 {
		t.Fatalf("expected an averaged mid-grey, got %v", got)
	}
}

// TestRenderNativeImageEscape pins the OSC shape (fixed cell box,
// cursor-neutral) and that the viewport adds the row/column marker.
func TestRenderNativeImageEscape(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 20, 10))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERM_PROGRAM", "wezterm")
	escape, rows := renderNativeImage(buf.Bytes(), 40, 12)
	if !strings.Contains(escape, "]1337;File=inline=1;") {
		t.Fatalf("expected an OSC 1337 escape, got %q", escape)
	}
	if !strings.Contains(escape, "preserveAspectRatio=1") || !strings.Contains(escape, "doNotMoveCursor=1") {
		t.Fatalf("expected a fixed, cursor-neutral box, got %q", escape)
	}
	if rows < 1 {
		t.Fatalf("expected a positive row span, got %d", rows)
	}
	if strings.Contains(escape, imageMarkerPrefix) {
		t.Fatal("the marker belongs to the layout, not the escape")
	}

	state := newUITestState()
	state.ui.images = map[string]string{}
	state.ui.imageNative = map[string]string{"u": escape}
	state.ui.imageNativeRows = map[string]int{"u": rows}
	state.ui.msgs.entries = []messageEntry{{Main: "a", Second: "b", Images: []string{"u"}}}
	content, _, _ := messagesViewportContent(&state.ui, 80)
	if !strings.Contains(content, imageMarkerPrefix) || !strings.Contains(content, fmt.Sprintf("%d:0", rows)) {
		t.Fatalf("expected the row/column marker in the viewport content, got %q", content)
	}
}

// TestFocusBoxSurroundsFocusedPane pins the dotted focus box: only the
// focused pane is wrapped in "\u00b7" rows/columns.
func TestFocusBoxSurroundsFocusedPane(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	state.ui.msgs.focusable = true

	model := newChatModel(state)
	model.width, model.height = 120, 40
	model.help.Width = 120

	check := func(block string, want bool) {
		rows := strings.Split(plain(block), "\n")
		top := strings.Contains(rows[0], "\u00b7")
		bottom := strings.Contains(rows[len(rows)-1], "\u00b7")
		if top != want || bottom != want {
			t.Fatalf("box presence mismatch (want %v): top=%q bottom=%q", want, rows[0], rows[len(rows)-1])
		}
	}

	state.ui.focus = PaneTree
	check(model.renderConversationsPane(40, 20), true)
	check(model.renderMessagesPane(60, 20), false)
	check(model.renderComposer(60), false)

	state.ui.focus = PaneMessages
	check(model.renderConversationsPane(40, 20), false)
	check(model.renderMessagesPane(60, 20), true)

	state.ui.focus = PaneComposer
	check(model.renderMessagesPane(60, 20), false)
	check(model.renderComposer(60), true)
}

// TestCursorRowHighlightOnlyWhenFocused pins the row highlight: the message
// under the cursor is tinted only while the message pane has focus.
func TestCursorRowHighlightOnlyWhenFocused(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	state.ui.msgs.focusable = true
	target := ConversationTarget{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>one</p>"},
		{Id: "2", ImDisplayName: "Bob", Content: "<p>two</p>"},
	}, time.Now(), "", false)

	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100
	bg := cursorRowSGR()
	if bg == "" || strings.Contains(bg, "X") {
		t.Fatalf("bad highlight SGR: %q", bg)
	}

	state.ui.focus = PaneTree
	if strings.Contains(model.View(), bg) {
		t.Fatal("cursor row must not be highlighted while the list has focus")
	}
	state.ui.focus = PaneMessages
	if !strings.Contains(model.View(), bg) {
		t.Fatal("cursor row must be highlighted once the message pane has focus")
	}
}

// TestCursorHighlightStopsAtEntryEnd pins that the tint covers exactly the
// cursor entry's rows and does not bleed into the next message.
func TestCursorHighlightStopsAtEntryEnd(t *testing.T) {
	state := newUITestState()
	state.ui.page = pageMain
	state.ui.msgs.focusable = true
	target := ConversationTarget{ID: "19:x", Title: "Chat"}
	state.setCurrentConversation(target)
	state.ui.msgs.apply(state, target, []csa.ChatMessage{
		{Id: "1", ImDisplayName: "Alice", Content: "<p>one</p>"},
		{Id: "2", ImDisplayName: "Bob", Content: "<p>two</p>"},
		{Id: "3", ImDisplayName: "Carol", Content: "<p>three</p>"},
	}, time.Now(), "", false)
	state.ui.msgs.setCursor(1)
	state.ui.focus = PaneMessages

	model := newChatModel(state)
	model.width, model.height = 100, 30
	model.help.Width = 100

	_, _, heights := messagesViewportContent(&state.ui, model.messagesPaneWidth())
	bg := cursorRowSGR()
	tinted := 0
	for _, row := range strings.Split(model.View(), "\n") {
		if strings.Contains(row, bg) {
			tinted++
		}
	}
	if tinted != heights[state.ui.msgs.cursor] {
		t.Fatalf("highlight covers %d rows, want the cursor entry's %d", tinted, heights[state.ui.msgs.cursor])
	}
}

// TestClipImagesToViewport pins that an image block is drawn only when it
// fits entirely inside the visible window.
func TestClipImagesToViewport(t *testing.T) {
	marker := imageMarkerPrefix + "4:0" + imageMarkerSuffix
	content := "a\nb\nc\nd\n" + marker + "\n1\n2\n3\n4"

	if got := clipImagesToViewport(content, 0, 7); strings.Contains(got, imageMarkerPrefix) {
		t.Fatalf("partially visible image must be dropped: %q", got)
	}
	if got := clipImagesToViewport(content, 0, 9); !strings.Contains(got, imageMarkerPrefix) {
		t.Fatalf("fully visible image must be kept: %q", got)
	}
	if got := clipImagesToViewport(content, 6, 10); strings.Contains(got, imageMarkerPrefix) {
		t.Fatalf("image scrolled above the window must be dropped: %q", got)
	}
}
