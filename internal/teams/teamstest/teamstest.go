// Package teamstest provides fakes for tests that exercise the Teams
// backend without the network.
package teamstest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/dgrijalva/jwt-go"
	teams_api "github.com/fossteams/teams-api"
	api "github.com/fossteams/teams-api/pkg"
	"github.com/fossteams/teams-api/pkg/csa"
)

// NewClient returns a teams-api client with fake tokens and no network
// setup; its chat service signs requests with a fake skype token.
func NewClient(t *testing.T) *teams_api.TeamsClient {
	t.Helper()

	chatSvc, err := csa.NewCSAService(
		&api.TeamsToken{
			Inner: &jwt.Token{Raw: "chat-service-token"},
			Type:  api.TokenBearer,
		},
		&api.SkypeToken{
			Inner: &jwt.Token{Raw: "skype-token"},
			Type:  api.TokenSkype,
		},
	)
	if err != nil {
		t.Fatalf("unable to create CSA service: %v", err)
	}

	client := &teams_api.TeamsClient{}
	setUnexportedField(t, client, "chatSvc", chatSvc)
	return client
}

func setUnexportedField(t *testing.T, target any, field string, value any) {
	t.Helper()

	targetValue := reflect.ValueOf(target)
	if targetValue.Kind() != reflect.Pointer || targetValue.IsNil() {
		t.Fatal("target must be a non-nil pointer")
	}

	fieldValue := targetValue.Elem().FieldByName(field)
	if !fieldValue.IsValid() {
		t.Fatalf("field %q does not exist", field)
	}

	reflect.NewAt(fieldValue.Type(), unsafe.Pointer(fieldValue.UnsafeAddr())).Elem().Set(reflect.ValueOf(value))
}

// RoundTripperFunc adapts a function to http.RoundTripper.
type RoundTripperFunc func(*http.Request) (*http.Response, error)

func (f RoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Response builds a canned HTTP response.
func Response(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// MessagesJSON encodes messages as a message-service response body.
func MessagesJSON(t *testing.T, messages []csa.ChatMessage) string {
	t.Helper()

	bodyBytes, err := json.Marshal(csa.MessagesResponse{Messages: messages})
	if err != nil {
		t.Fatalf("unable to marshal test messages: %v", err)
	}

	return string(bodyBytes)
}

// ChatMessage builds a message with the fields the client reads.
func ChatMessage(id, author, content string, composedAt time.Time) csa.ChatMessage {
	return csa.ChatMessage{
		Id:            id,
		Version:       "1",
		ImDisplayName: author,
		Content:       content,
		ComposeTime:   api.RFC3339Time(composedAt),
	}
}
