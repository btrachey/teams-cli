package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
)

const csaMessagesPropertiesEndpoint = "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/properties"

// messageServiceHost is the message-service base URL used to send top-level
// messages. The read path pins csa.MessagesHost (emea) and that is the host
// proven to accept this app's skype token, so sends use the same host
// instead of a region probe. Overridable for tests.
var messageServiceHost = csa.MessagesHost

// regionProbeFormat builds the per-region probe endpoint used to
// discover the tenant home region for CSA v2 thread replies.
var regionProbeFormat = "https://%s.ng.msg.teams.microsoft.com/v1/users/ME/properties"

// csaMessageRegions mirrors the known regional message-service hosts. The
// tenant's home region is probed on first use.
var csaMessageRegions = []string{"amer", "emea", "apac"}

// SendError is a rejected message post.
type SendError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *SendError) Error() string {
	message := fmt.Sprintf("message send failed: %d %s", e.StatusCode, e.Status)
	if e.Body != "" {
		message += " (" + e.Body + ")"
	}
	return message
}

func (e *SendError) HTTPStatus() int { return e.StatusCode }

// skypeAuthHeader returns the raw value this app already sends to the message
// service for its read calls ("skypetoken=<raw>"), so sends authenticate
// exactly like the existing GETs.
func (s *Service) skypeAuthHeader() (string, error) {
	if s.skypeAuthHeaderOverride != nil {
		return s.skypeAuthHeaderOverride()
	}

	client := s.Client()
	if client == nil {
		return "", fmt.Errorf("teams client is nil")
	}

	req, err := client.ChatSvc().AuthenticatedRequest(http.MethodGet, csaMessagesPropertiesEndpoint, nil)
	if err != nil {
		return "", err
	}

	value := req.Header.Get("Authentication")
	if value == "" {
		return "", fmt.Errorf("missing skype authentication header")
	}
	return value, nil
}

// probeMessageRegion discovers which regional message service hosts this
// tenant's skype token and caches it.
func (s *Service) probeMessageRegion(ctx context.Context) (string, error) {
	s.regionMu.Lock()
	defer s.regionMu.Unlock()

	if s.region != "" {
		return s.region, nil
	}

	if s.Client() == nil && s.skypeAuthHeaderOverride == nil {
		return "", fmt.Errorf("teams client is nil")
	}

	auth, err := s.skypeAuthHeader()
	if err != nil {
		return "", err
	}

	var lastErr error
	for _, region := range csaMessageRegions {
		endpoint := fmt.Sprintf(regionProbeFormat, region)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Authentication", auth)

		resp, err := s.HTTP().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			s.region = region
			s.logger().WithField("region", region).Debug("message service region discovered")
			return region, nil
		}
		lastErr = fmt.Errorf("%s rejected the skype token (%s)", region, resp.Status)
	}

	return "", fmt.Errorf("unable to determine message service region: %v", lastErr)
}

// wrapMessageContent renders plain user input as the Teams RichText/Html
// content body: HTML-escaped, newlines as <br>.
func wrapMessageContent(raw string) string {
	escaped := strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(raw, "&", "&amp;"), "<", "&lt;"), ">", "&gt;")
	escaped = strings.ReplaceAll(escaped, "\r\n", "\n")
	escaped = strings.ReplaceAll(escaped, "\n", "<br>")
	return "<p>" + escaped + "</p>"
}

func newConversationMessageBody(content, clientMessageID, displayName string) map[string]string {
	body := map[string]string{
		"content":         wrapMessageContent(content),
		"messagetype":     "RichText/Html",
		"contenttype":     "text",
		"clientmessageid": clientMessageID,
	}
	// Without imdisplayname the message service stores the message with an
	// empty sender name (verified against the live wire record), so it
	// renders nameless in Teams clients and is invisible to readers that
	// key off the display name.
	if name := strings.TrimSpace(displayName); name != "" {
		body["imdisplayname"] = name
	}
	return body
}

func newThreadReplyBody(content, parentMessageID string) map[string]string {
	return map[string]string{
		"content":         wrapMessageContent(content),
		"parentMessageId": parentMessageID,
	}
}

func newClientMessageID() string {
	// 19-digit numeric ID in the range the Teams clients use. The span stays
	// below math.MaxInt64 - base so the addition can never overflow.
	const base = 1_000_000_000_000_000_000
	const span = 8_000_000_000_000_000_000
	return strconv.FormatInt(base+rand.Int64N(span), 10)
}

// conversationMessagesEndpoint builds the message-post URL for a
// conversation on the given message-service host (with trailing slash).
func conversationMessagesEndpoint(host, conversationID string) string {
	return fmt.Sprintf("%sv1/users/ME/conversations/%s/messages",
		strings.TrimSuffix(host, "/")+"/", url.PathEscape(conversationID))
}

func threadReplyEndpoint(region, teamID, channelID, messageID string) string {
	return fmt.Sprintf("https://teams.microsoft.com/api/csa/%s/api/v2/teams/%s/channels/%s/messages/%s/replies",
		region, url.PathEscape(teamID), url.PathEscape(channelID), url.PathEscape(messageID))
}

func (s *Service) doMessageSend(ctx context.Context, req *http.Request, label string) error {
	resp, err := s.HTTP().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &SendError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(bodyBytes)),
		}
	}

	s.logger().WithField("label", label).Debug("message sent")
	return nil
}

// SendMessage posts a top-level message to a conversation. It
// targets the same message-service host the read path uses and sets the
// Authentication header explicitly: the teams-api AuthenticatedRequest
// helper only attaches the skype token for URLs under csa.MessagesHost, so
// relying on it for probed regional hosts silently sent unauthenticated
// POSTs that failed with spurious 401s.
func (s *Service) SendMessage(ctx context.Context, target Conversation, content string) error {
	if s.Client() == nil && s.skypeAuthHeaderOverride == nil {
		return fmt.Errorf("teams client is nil")
	}

	auth, err := s.skypeAuthHeader()
	if err != nil {
		return err
	}

	endpoint := conversationMessagesEndpoint(messageServiceHost, target.ID)
	displayName := s.SelfDisplayName()
	bodyBytes, err := json.Marshal(newConversationMessageBody(content, newClientMessageID(), displayName))
	if err != nil {
		return err
	}
	if displayName == "" {
		// Without a display name the service stores the message with an
		// empty sender (the bug that made CLI sends invisible); surface
		// that instead of shipping it silently.
		s.logger().Warn("sending without imdisplayname: signed-in profile unavailable; the message may render anonymously")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authentication", auth)
	req.Header.Set("Content-Type", "application/json")

	return s.doMessageSend(ctx, req, "conversation message")
}

// SendReply posts a threaded reply under a specific message. Only
// channels have a thread backend; chats do not.
func (s *Service) SendReply(ctx context.Context, target Conversation, messageID, content string) error {
	if !s.IsChannel(target.ID) {
		return fmt.Errorf("thread replies are only available for channels, not chats")
	}

	teamID, ok := s.teamOf(target.ID)
	if !ok {
		return fmt.Errorf("unable to resolve the team for channel %s", target.ID)
	}

	region, err := s.probeMessageRegion(ctx)
	if err != nil {
		return err
	}
	auth, err := s.skypeAuthHeader()
	if err != nil {
		return err
	}

	endpoint := threadReplyEndpoint(region, teamID, target.ID, messageID)
	bodyBytes, err := json.Marshal(newThreadReplyBody(content, messageID))
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("X-Skypetoken", strings.TrimPrefix(auth, "skypetoken="))
	req.Header.Set("Content-Type", "application/json")

	return s.doMessageSend(ctx, req, "thread reply")
}

// consumptionHorizon is the read receipt Teams clients send when a
// conversation is read: "<newest message id>;<read time, unix ms>;<newest
// client message id>". Message ids are millisecond timestamps; the client
// message id falls back to the message id when the message has none.
func consumptionHorizon(newest csa.ChatMessage, readAt time.Time) string {
	clientID := newest.ClientMessageId
	if clientID == "" {
		clientID = newest.Id
	}
	return fmt.Sprintf("%s;%d;%s", newest.Id, readAt.UnixMilli(), clientID)
}

// MarkRead tells Teams the signed-in user has read a conversation up to
// newest, which clears its unread state on every client (and shows as
// "seen" where read receipts are on).
func (s *Service) MarkRead(ctx context.Context, conversationID string, newest csa.ChatMessage) error {
	if newest.Id == "" {
		return fmt.Errorf("no message to mark read")
	}
	auth, err := s.skypeAuthHeader()
	if err != nil {
		return err
	}

	endpoint := strings.TrimSuffix(conversationMessagesEndpoint(messageServiceHost, conversationID), "/messages") +
		"/properties?name=consumptionhorizon"
	bodyBytes, err := json.Marshal(map[string]string{"consumptionhorizon": consumptionHorizon(newest, time.Now())})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authentication", auth)
	req.Header.Set("Content-Type", "application/json")

	return s.doMessageSend(ctx, req, "read receipt")
}
