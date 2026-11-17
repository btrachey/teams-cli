package tui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"  // register decoders for Teams attachments
	_ "image/jpeg" //
	_ "image/png"  //
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	api "github.com/fossteams/teams-api/pkg"
)

// Image rendering limits: images are drawn as half-block art so they stay
// inside the text grid (the viewport, wrapping, and paging keep working).
const (
	imageMaxCols    = 60
	imageMaxRows    = 20
	imageMaxBytes   = 10 << 20
	imageMaxPerItem = 3

	// imageMarkerPrefix/Suffix encode the row span and column of an inline
	// image for the vendored Bubble Tea renderer: "\x00IMG:<rows>:<col>\x00".
	imageMarkerPrefix = "\x00IMG:"
	imageMarkerSuffix = "\x00"
)

// renderedImage is one message image's render state: half-block art, or an
// OSC 1337 escape spanning nativeRows rows in native mode. failed marks an
// image that could not be downloaded or decoded.
type renderedImage struct {
	art        string
	native     string
	nativeRows int
	failed     bool
}

// messageImageMsg carries a downloaded, rendered image (or its failure)
// back to the UI loop.
type messageImageMsg struct {
	url string
	img renderedImage
	err error
}

// messageImageURLs extracts http(s) image sources from a message body.
// Teams stores inline images as <img src="https://…/views/imgo">; the
// URLs are authenticated.
func messageImageURLs(content string) []string {
	seen := map[string]struct{}{}
	urls := make([]string, 0, 2)

	for _, raw := range extractImageSources(content) {
		src := strings.TrimSpace(raw)
		if strings.HasPrefix(src, "//") {
			src = "https:" + src
		}
		parsed, err := url.Parse(src)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			continue
		}
		if _, ok := seen[src]; ok {
			continue
		}
		seen[src] = struct{}{}
		urls = append(urls, src)
		if len(urls) >= imageMaxPerItem {
			break
		}
	}

	return urls
}

// imageAuthHost reports whether a URL belongs to a Teams media host that
// may receive the skype token. The token must never be sent to arbitrary
// hosts named by message content.
func imageAuthHost(host string) bool {
	host = strings.ToLower(host)
	for _, suffix := range []string{".teams.microsoft.com", ".skype.com", ".skype.net"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// startMessageImageFetches downloads the images referenced by the current
// entries in the background; results arrive as messageImageMsg.
func (s *AppState) startMessageImageFetches(entries []messageEntry, maxCols int) {
	if imageRenderMode() == imageModeOff {
		return
	}

	mode := imageRenderMode()
	for _, entry := range entries {
		for _, imageURL := range entry.Images {
			if parsed, err := url.Parse(imageURL); err == nil && imageAuthHost(parsed.Hostname()) && s.imageAuthBlocked.Load() {
				continue
			}
			if _, done := s.ui.images[imageURL]; done {
				continue
			}
			if !s.claimImageFetch(imageURL) {
				continue
			}

			go func(rawURL string) {
				defer s.releaseImageFetch(rawURL)

				img, err := s.fetchRenderedImage(rawURL, mode, maxCols)
				s.sendUIMsg(messageImageMsg{url: rawURL, img: img, err: err})
			}(imageURL)
		}
	}
}

// fetchRenderedImage downloads an image and renders it for the given mode:
// the OSC 1337 escape in native mode (block art only as the fallback when
// the terminal-side encoding fails), half-block art otherwise.
func (s *AppState) fetchRenderedImage(rawURL, mode string, maxCols int) (renderedImage, error) {
	payload, err := s.downloadMessageImage(s.appContext(), rawURL)
	if err != nil {
		return renderedImage{}, err
	}
	if mode == imageModeNative {
		if native, rows := renderNativeImage(payload, maxCols, imageMaxRows); native != "" {
			return renderedImage{native: native, nativeRows: rows}, nil
		}
	}
	art, err := renderImageArt(payload, maxCols, imageMaxRows)
	return renderedImage{art: art}, err
}

// amsImageToken exchanges the login-flow skype token for the authz skype
// token the media service accepts, caching it until shortly before expiry.
func (s *AppState) amsImageToken(ctx context.Context) (token string, err error) {
	// teams-api panics on malformed token files (nil JWT inside the authz
	// exchange) and this runs from an image-fetch goroutine, where an
	// unrecovered panic would take down the whole TUI.
	defer func() {
		if recovered := recover(); recovered != nil {
			token = ""
			err = fmt.Errorf("AMS image token exchange failed: %v", recovered)
		}
	}()

	s.imageTokenMu.Lock()
	defer s.imageTokenMu.Unlock()

	if s.imageToken != "" && time.Now().Before(s.imageTokenExpiry) {
		return s.imageToken, nil
	}

	root, getErr := api.GetRootToken()
	if getErr != nil {
		return "", getErr
	}
	if root == nil || root.Inner == nil || root.Inner.Raw == "" {
		return "", fmt.Errorf("skype token is not a valid JWT")
	}

	authClient := api.New(s.teams.HTTP())
	exchanged, authErr := authClient.Authz(root, api.AuthzRefresh)
	if authErr != nil {
		return "", authErr
	}
	token = exchanged.Inner.Raw

	s.imageToken = token
	s.imageTokenExpiry = time.Now().Add(30 * time.Minute)
	return s.imageToken, nil
}

// claimImageFetch marks a URL as being downloaded; false means another
// fetch is already in flight.
func (s *AppState) claimImageFetch(rawURL string) bool {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	if s.imageInFlight == nil {
		s.imageInFlight = map[string]bool{}
	}
	if s.imageInFlight[rawURL] {
		return false
	}
	s.imageInFlight[rawURL] = true
	return true
}

func (s *AppState) releaseImageFetch(rawURL string) {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	delete(s.imageInFlight, rawURL)
}

// downloadMessageImage fetches an image, using the skype token only for
// Teams media hosts, and caches the bytes on disk.
func (s *AppState) downloadMessageImage(ctx context.Context, rawURL string) ([]byte, error) {
	cachePath, err := messageImageCachePath(rawURL)
	if err != nil {
		return nil, err
	}
	if cached, readErr := os.ReadFile(cachePath); readErr == nil && len(cached) > 0 {
		return cached, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	authenticated := false
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil && imageAuthHost(parsed.Hostname()) {
		if amsToken, tokenErr := s.amsImageToken(ctx); tokenErr == nil {
			// The Teams media service (AMS) authenticates image requests
			// with the authz-refreshed skype token as a cookie; the
			// login-flow skype token itself is rejected.
			req.Header.Set("Cookie", "skypetoken_asm="+amsToken)
			authenticated = true
		} else {
			s.appLogger().WithError(tokenErr).Debug("AMS image token unavailable")
		}
	}

	resp, err := s.teams.HTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// Only an authenticated media host can be "blocked": public
		// images keep working regardless.
		// Image fetches against the media host are skipped for the
		// rest of the session (until a token refresh resets this).
		if authenticated {
			s.imageAuthBlocked.Store(true)
		}
		return nil, fmt.Errorf("image download unauthorized: %s", resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("image download failed: %s", resp.Status)
	}

	payload, err := io.ReadAll(io.LimitReader(resp.Body, imageMaxBytes))
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("image download returned no data")
	}

	// Only cache real images: a URL that returned HTML or JSON must not be
	// persisted and re-decoded on every future session.
	if _, _, decErr := image.DecodeConfig(bytes.NewReader(payload)); decErr == nil {
		if mkErr := os.MkdirAll(filepath.Dir(cachePath), 0o755); mkErr == nil {
			_ = os.WriteFile(cachePath, payload, 0o644)
		}
	}

	return payload, nil
}

func messageImageCachePath(rawURL string) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(rawURL))
	return filepath.Join(base, "teams-cli", "images", hex.EncodeToString(digest[:])+".bin"), nil
}

// renderImageArt draws an image as ANSI truecolor half-block art: every
// text cell carries two vertical pixels (upper half block, foreground =
// top pixel, background = bottom pixel), so it renders inline in any
// truecolor terminal and scrolls with the text.
func renderImageArt(data []byte, maxCols, maxRows int) (string, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", fmt.Errorf("unsupported image: %w", err)
	}

	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return "", fmt.Errorf("empty image")
	}

	cols, rows := imageCellSize(srcW, srcH, maxCols, maxRows)

	var out strings.Builder
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			top := sampleImagePixel(img, col, row*2, cols, rows*2)
			bottom := sampleImagePixel(img, col, row*2+1, cols, rows*2)
			fmt.Fprintf(&out, "\x1b[38;2;%d;%d;%dm\x1b[48;2;%d;%d;%dm▀",
				top.R, top.G, top.B, bottom.R, bottom.G, bottom.B)
		}
		out.WriteString("\x1b[0m")
		if row < rows-1 {
			out.WriteString("\n")
		}
	}

	return out.String(), nil
}

// imageBackdrop is the colour transparent pixels composite against. The
// terminal's own background is unknowable here, so transparent images
// (stickers, emoji, avatars) get a neutral dark backdrop instead of
// rendering as black rectangles.
var imageBackdrop = color.RGBA{R: 0x1e, G: 0x1e, B: 0x1e, A: 255}

// Image render modes. Native (OSC 1337) is the default in terminals that
// support it; block art is the fallback everywhere else.
const (
	imageModeBlock  = "block"
	imageModeNative = "native"
	imageModeOff    = "off"
)

// imageRenderMode resolves the TEAMS_CLI_IMAGES override (off or block).
// The default is the real image in terminals that implement the
// inline-image protocol — the vendored renderer understands the image
// marker and leaves the reserved rows unpainted — and block art everywhere
// else.
func imageRenderMode() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TEAMS_CLI_IMAGES"))) {
	case imageModeOff:
		return imageModeOff
	case imageModeBlock:
		return imageModeBlock
	}
	if terminalSupportsInlineImages() {
		return imageModeNative
	}
	return imageModeBlock
}

// terminalSupportsInlineImages reports whether TERM_PROGRAM identifies a
// terminal with the iTerm2 inline-image protocol (OSC 1337).
func terminalSupportsInlineImages() bool {
	switch strings.ToLower(os.Getenv("TERM_PROGRAM")) {
	case "wezterm", "iterm.app":
		return true
	default:
		return false
	}
}

// renderNativeImage builds an OSC 1337 inline-image escape plus the
// renderer marker ("\x00IMG:<rows>\x00") that tells the vendored Bubble Tea
// renderer to leave the image's rows unpainted.
func renderNativeImage(data []byte, maxCols, maxRows int) (string, int) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return "", 0
	}

	cols, rows := imageCellSize(cfg.Width, cfg.Height, maxCols, maxRows)
	cursorArg := ""
	if strings.EqualFold(os.Getenv("TERM_PROGRAM"), "wezterm") {
		cursorArg = ";doNotMoveCursor=1"
	}
	// preserveAspectRatio=1 fits the image inside the reserved cell box;
	// WezTerm ignores =0 and drops the image entirely.
	escape := fmt.Sprintf("\x1b]1337;File=inline=1;width=%d;height=%d;preserveAspectRatio=1%s:%s\x07",
		cols, rows, cursorArg, base64.StdEncoding.EncodeToString(data))
	return escape, rows
}

func imageCellSize(srcW, srcH, maxCols, maxRows int) (cols, rows int) {
	if srcW <= 0 || srcH <= 0 {
		return 1, 1
	}
	scale := math.Min(float64(maxCols)/float64(srcW), float64(maxRows*2)/float64(srcH))
	return max(1, int(float64(srcW)*scale)), max(1, int(float64(srcH)*scale/2))
}

// sampleImagePixel averages the source pixels covered by one cell,
// composited over the backdrop. Averaging (rather than point sampling) is
// what makes a downscaled screenshot read as a picture instead of dither.
// image.Image returns premultiplied values, so translucent pixels are
// un-premultiplied before accumulating.
func sampleImagePixel(img image.Image, col, row, cols, rows int) color.RGBA {
	bounds := img.Bounds()
	x0 := bounds.Min.X + col*bounds.Dx()/cols
	x1 := bounds.Min.X + (col+1)*bounds.Dx()/cols
	y0 := bounds.Min.Y + row*bounds.Dy()/rows
	y1 := bounds.Min.Y + (row+1)*bounds.Dy()/rows
	if x1 <= x0 {
		x1 = x0 + 1
	}
	if y1 <= y0 {
		y1 = y0 + 1
	}

	var sumR, sumG, sumB, sumA, count float64
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				count++
				continue
			}
			if a < 0xffff {
				r = r * 0xffff / a
				g = g * 0xffff / a
				b = b * 0xffff / a
			}
			sumR += float64(r >> 8)
			sumG += float64(g >> 8)
			sumB += float64(b >> 8)
			sumA += float64(a) / 0xffff
			count++
		}
	}
	if count == 0 {
		return imageBackdrop
	}

	alpha := sumA / count
	blend := func(sum float64, bg uint8) uint8 {
		return uint8((sum/count)*alpha + float64(bg)*(1-alpha))
	}
	return color.RGBA{
		R: blend(sumR, imageBackdrop.R),
		G: blend(sumG, imageBackdrop.G),
		B: blend(sumB, imageBackdrop.B),
		A: 255,
	}
}

// handleMessageImage stores a downloaded image (or its failure) for
// rendering.
func (m chatModel) handleMessageImage(msg messageImageMsg) (tea.Model, tea.Cmd) {
	u := m.ui()
	if u.images == nil {
		u.images = map[string]renderedImage{}
	}
	if msg.err != nil {
		m.state.appLogger().WithError(msg.err).Debug("message image unavailable")
		u.images[msg.url] = renderedImage{failed: true}
		return m, nil
	}
	u.images[msg.url] = msg.img
	return m, nil
}
