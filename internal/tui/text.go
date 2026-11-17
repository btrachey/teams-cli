package tui

import (
	"bytes"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// terminalSequencePattern matches CSI, OSC and Fe escape sequences. It is
// deliberately broader than SGR: any ESC-initiated sequence a hostile
// message could use to restyle or control the terminal must go.
var terminalSequencePattern = regexp.MustCompile(
	`\x1b\[[0-9;?]*[ -/]*[@-~]` +
		`|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)` +
		`|\x1b[@-Z\\-_]`)

// sanitizeText strips terminal control sequences and stray control
// characters from untrusted text. Names and message bodies arrive over the
// network; lipgloss passes embedded escape sequences straight through to
// the terminal, which would corrupt the pinned two-row layout (and worse).
// The retired tview.Escape invariant is replaced by this filter at every
// content boundary.
func sanitizeText(text string) string {
	text = terminalSequencePattern.ReplaceAllString(text, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
			return -1
		default:
			return r
		}
	}, text)
}

// messageListItem renders one chat message as two plain-text lines: the
// sender's name (styled bold by the renderer) on the top line, the message
// body beneath it. Paragraph breaks in the body are joined with spaces.
// Both sides pass through sanitizeText: they are attacker-controllable.
func messageListItem(author, content string) (string, string) {
	main := sanitizeText(strings.TrimSpace(author))

	body := strings.TrimRight(textBody(content), "\n")
	body = sanitizeText(strings.TrimSpace(strings.ReplaceAll(body, "\n", " ")))

	return main, body
}

// extractImageSources returns the src attributes of <img> tags in HTML
// content: Teams stores inline images as authenticated image URLs.
func extractImageSources(input string) []string {
	var sources []string
	z := html.NewTokenizer(bytes.NewBuffer([]byte(input)))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		token := z.Token()
		if !strings.EqualFold(token.Data, "img") {
			continue
		}
		for _, attr := range token.Attr {
			if strings.EqualFold(attr.Key, "src") && strings.TrimSpace(attr.Val) != "" {
				sources = append(sources, attr.Val)
			}
		}
	}

	return sources
}

func textBody(input string) string {
	var parts []string
	z := html.NewTokenizer(bytes.NewBuffer([]byte(input)))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			break
		}

		switch tt {
		case html.TextToken:
			text := string(z.Text())
			if strings.TrimSpace(text) == "" {
				continue
			}
			parts = append(parts, text)
		}
	}

	return strings.Join(parts, "\n")
}

// messageTimestamp renders a message time for the author line: the clock
// time for today, date plus time for older messages, and the full date for
// previous years. Zero times render as the empty string.
func messageTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	now := time.Now()
	local := ts.Local()
	switch {
	case local.Year() == now.Year() && local.YearDay() == now.YearDay():
		return local.Format("15:04")
	case local.Year() == now.Year():
		return local.Format("Jan 2 15:04")
	default:
		return local.Format("Jan 2 2006 15:04")
	}
}
