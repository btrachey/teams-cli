package main

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/rivo/tview"
	"golang.org/x/net/html"
)

// messageListItem renders one chat message as a two-line list item: the
// sender's name (bold) on the top line, the message body beneath it. tview
// List items are exactly two rows, so paragraph breaks in the body are
// joined with spaces.
func messageListItem(author, content string) (string, string) {
	main := "[::b]" + tview.Escape(strings.TrimSpace(author)) + "[-::]"

	body := strings.TrimRight(textBody(content), "\n")
	body = strings.TrimSpace(strings.ReplaceAll(body, "\n", " "))

	// "[white]" keeps the body in the same color it had as the main text;
	// tview's default secondary color is green.
	return main, "[white]" + body
}

func textBody(input string) string {
	output := ""
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
			output += fmt.Sprintf("\t%v\n", tview.Escape(text))
		}
	}

	return output
}

func (s *AppState) chatTitle(chat csa.Chat) string {
	selfMri := ""
	selfDisplayName := ""
	if s.me != nil {
		selfMri = s.me.Mri
		selfDisplayName = s.me.DisplayName
	}

	return resolveChatTitle(chat, selfMri, selfDisplayName)
}

func resolveChatTitle(chat csa.Chat, selfMri, selfDisplayName string) string {
	if title := strings.TrimSpace(chat.Title); title != "" && !sameDisplayName(title, selfDisplayName) {
		return title
	}

	if subject := strings.TrimSpace(chat.MeetingInformation.Subject); subject != "" {
		return subject
	}

	names := chatParticipantNames(chat, selfMri)
	switch {
	case len(names) == 0:
	case len(names) <= 3:
		return strings.Join(names, ", ")
	default:
		return fmt.Sprintf("%s +%d", strings.Join(names[:3], ", "), len(names)-3)
	}

	if title := strings.TrimSpace(chat.LastMessage.ImDisplayName); title != "" && !sameDisplayName(title, selfDisplayName) {
		return title
	}

	return anonymousChatTitle(chat, selfMri)
}

func chatParticipantNames(chat csa.Chat, selfMri string) []string {
	names := []string{}
	seen := map[string]struct{}{}

	for _, member := range chat.Members {
		name := strings.TrimSpace(member.FriendlyName)
		if name == "" || member.Mri == selfMri {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		names = append(names, name)
	}

	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})

	return names
}

func anonymousChatTitle(chat csa.Chat, selfMri string) string {
	participantCount := 0
	for _, member := range chat.Members {
		if member.Mri == selfMri {
			continue
		}
		participantCount += 1
	}

	switch {
	case strings.EqualFold(chat.ChatType, "meeting"):
		return "Meeting chat"
	case chat.IsOneOnOne || participantCount <= 1:
		return "Direct chat"
	case participantCount > 1:
		return fmt.Sprintf("Group chat (%d people)", participantCount)
	case len(chat.Members) > 0:
		return fmt.Sprintf("Group chat (%d people)", len(chat.Members))
	default:
		return "Conversation"
	}
}

func sameDisplayName(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" || right == "" {
		return false
	}

	return strings.EqualFold(left, right)
}
