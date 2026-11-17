package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/sirupsen/logrus"
)

// FetchError is a non-200 response from the message service.
type FetchError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *FetchError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("unable to fetch messages: unexpected status %s", e.Status)
	}

	return fmt.Sprintf("unable to fetch messages: unexpected status %s: %s", e.Status, strings.TrimSpace(e.Body))
}

func (e *FetchError) HTTPStatus() int { return e.StatusCode }

const (
	requestTimeout      = 5 * time.Second
	fetchMaxAttempts    = 3
	fetchRetryBaseDelay = 250 * time.Millisecond
	fetchRetryMaxDelay  = time.Second
)

func shouldRetryMessageFetch(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var statusErr *FetchError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == http.StatusTooManyRequests || statusErr.StatusCode >= http.StatusInternalServerError
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}

	return false
}

func messageFetchBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}

	backoff := fetchRetryBaseDelay * time.Duration(1<<(attempt-1))
	if backoff > fetchRetryMaxDelay {
		return fetchRetryMaxDelay
	}

	return backoff
}

func waitForMessageFetchRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(messageFetchBackoff(attempt))
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// FetchMessages returns the newest limit messages of a conversation, oldest
// first. Throttling, server errors and timeouts are retried with backoff; an
// auth failure triggers one Reauth and an immediate retry.
func (s *Service) FetchMessages(ctx context.Context, target Conversation, limit int) ([]csa.ChatMessage, error) {
	if s.Client() == nil {
		return nil, fmt.Errorf("teams client is nil")
	}

	baseURL, err := url.Parse(csa.MessagesHost)
	if err != nil {
		return nil, fmt.Errorf("unable to parse messages host: %v", err)
	}

	endpointURL, err := baseURL.Parse("v1/users/ME/conversations/" + url.QueryEscape(target.ID) + "/messages")
	if err != nil {
		return nil, fmt.Errorf("unable to parse messages endpoint: %v", err)
	}

	values := endpointURL.Query()
	values.Add("view", "msnp24Equivalent|supportsMessageProperties")
	values.Add("pageSize", strconv.Itoa(limit))
	values.Add("startTime", "1")
	endpointURL.RawQuery = values.Encode()

	s.logger().WithFields(logrus.Fields{
		"conversation_id":    target.ID,
		"conversation_title": target.Title,
		"message_limit":      limit,
	}).Debug("fetching conversation messages")

	var lastErr error
	authRetryUsed := false
	maxAttempts := fetchMaxAttempts
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		messages, err := s.fetchMessagesOnce(attemptCtx, endpointURL.String(), limit)
		cancel()
		if err == nil {
			return messages, nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, err
		}

		lastErr = err
		if !authRetryUsed && s.Reauth != nil && auth.IsAuthFailure(err) && s.Reauth(ctx, err) {
			// One refresh-and-retry per fetch: extend the budget and skip
			// the backoff so the retry hits the service immediately.
			authRetryUsed = true
			maxAttempts++
			continue
		}
		if attempt == maxAttempts || !shouldRetryMessageFetch(err) {
			return nil, err
		}

		s.logger().WithFields(logrus.Fields{
			"conversation_id": target.ID,
			"attempt":         attempt,
			"max_attempts":    maxAttempts,
			"backoff":         messageFetchBackoff(attempt).String(),
		}).WithError(err).Warn("retrying message fetch")
		if err := waitForMessageFetchRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

func (s *Service) fetchMessagesOnce(ctx context.Context, endpoint string, limit int) ([]csa.ChatMessage, error) {
	teamsClient := s.Client()
	if teamsClient == nil {
		return nil, fmt.Errorf("teams client is nil")
	}

	req, err := teamsClient.ChatSvc().AuthenticatedRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req = req.WithContext(ctx)

	resp, err := s.HTTP().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, &FetchError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(bodyBytes)),
		}
	}

	var msgResponse csa.MessagesResponse
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&msgResponse); err != nil {
		return nil, fmt.Errorf("unable to decode messages: %v", err)
	}

	messages := msgResponse.Messages
	sort.Sort(csa.SortMessageByTime(messages))
	if len(messages) > limit {
		messages = messages[len(messages)-limit:]
	}

	return messages, nil
}
