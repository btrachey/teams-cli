package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-api/pkg/models"
	"github.com/rivo/tview"
)

func TestWrapMessageContentEscapesHTMLAndWrapsParagraph(t *testing.T) {
	got := wrapMessageContent("hi & bye")
	want := "<p>hi &amp; bye</p>"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestWrapMessageContentEscapesAngleBrackets(t *testing.T) {
	got := wrapMessageContent("<b>bold</b>")
	want := "<p>&lt;b&gt;bold&lt;/b&gt;</p>"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestWrapMessageContentTurnsNewlinesIntoBreaks(t *testing.T) {
	got := wrapMessageContent("one\ntwo\r\nthree")
	want := "<p>one<br>two<br>three</p>"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestConversationMessagesEndpointTargetsMessageServiceHost(t *testing.T) {
	got := conversationMessagesEndpoint("https://emea.ng.msg.teams.microsoft.com/", "19:abc@thread.v2")
	want := "https://emea.ng.msg.teams.microsoft.com/v1/users/ME/conversations/19:abc@thread.v2/messages"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
	// The host is accepted with or without a trailing slash so it can be
	// swapped for the csa.MessagesHost constant or an httptest URL.
	if noSlash := conversationMessagesEndpoint("https://emea.ng.msg.teams.microsoft.com", "19:abc@thread.v2"); noSlash != want {
		t.Fatalf("expected trailing slash to be normalized, got %q", noSlash)
	}
	if got := conversationMessagesEndpoint("https://emea.ng.msg.teams.microsoft.com/", "a/b"); !strings.Contains(got, "a%2Fb") {
		t.Fatalf("expected slash in conversation id to be path-escaped, got %q", got)
	}
}

func TestThreadReplyEndpointUsesCSARoute(t *testing.T) {
	got := threadReplyEndpoint("emea", "team-1", "chan-2", "msg-3")
	want := "https://teams.microsoft.com/api/csa/emea/api/v2/teams/team-1/channels/chan-2/messages/msg-3/replies"
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

func TestNewConversationMessageBodyContainsRequiredKeys(t *testing.T) {
	body := newConversationMessageBody("hello", "client-id-1", "Brian Tracey")

	if body["content"] != "<p>hello</p>" {
		t.Fatalf("expected wrapped content, got %q", body["content"])
	}
	if body["messagetype"] != "RichText/Html" {
		t.Fatalf("expected messagetype RichText/Html, got %q", body["messagetype"])
	}
	if body["contenttype"] != "text" {
		t.Fatalf("expected contenttype text, got %q", body["contenttype"])
	}
	if body["clientmessageid"] != "client-id-1" {
		t.Fatalf("expected clientmessageid client-id-1, got %q", body["clientmessageid"])
	}
	// The message service stores an empty sender name when imdisplayname is
	// omitted, which made CLI sends invisible to name-keyed readers.
	if body["imdisplayname"] != "Brian Tracey" {
		t.Fatalf("expected imdisplayname to carry the sender name, got %q", body["imdisplayname"])
	}
	if len(body) != 5 {
		t.Fatalf("expected exactly 5 body keys, got %d: %v", len(body), body)
	}

	anonymous := newConversationMessageBody("hello", "client-id-2", "   ")
	if _, ok := anonymous["imdisplayname"]; ok {
		t.Fatalf("expected no imdisplayname key for a blank display name, got %q", anonymous["imdisplayname"])
	}
	if len(anonymous) != 4 {
		t.Fatalf("expected exactly 4 body keys without a display name, got %d: %v", len(anonymous), anonymous)
	}
}

func TestNewThreadReplyBodyContainsParentMessage(t *testing.T) {
	body := newThreadReplyBody("reply", "parent-9")

	if body["content"] != "<p>reply</p>" {
		t.Fatalf("expected wrapped content, got %q", body["content"])
	}
	if body["parentMessageId"] != "parent-9" {
		t.Fatalf("expected parentMessageId parent-9, got %q", body["parentMessageId"])
	}
	if len(body) != 2 {
		t.Fatalf("expected exactly 2 body keys, got %d: %v", len(body), body)
	}
}

func TestNewClientMessageIDIsNineteenDigits(t *testing.T) {
	for range 64 {
		id := newClientMessageID()
		if len(id) != 19 {
			t.Fatalf("expected 19-digit client message id, got %q (len %d)", id, len(id))
		}
		for _, r := range id {
			if r < '0' || r > '9' {
				t.Fatalf("expected digits only in client message id, got %q", id)
			}
		}
	}
}

func TestNewClientMessageIDVaries(t *testing.T) {
	seen := map[string]bool{}
	for range 128 {
		seen[newClientMessageID()] = true
	}
	if len(seen) == 1 {
		t.Fatalf("client message ids should vary between calls")
	}
}

func TestIsChannelConversation(t *testing.T) {
	state := AppState{
		TeamsState: TeamsState{
			channelById: map[string]Channel{"chan-1": {}},
		},
	}
	if !state.isChannelConversation("chan-1") {
		t.Fatalf("expected chan-1 to be a channel")
	}
	if state.isChannelConversation("19:chat") {
		t.Fatalf("expected a chat conversation id to not be a channel")
	}
}

func TestHelpBarTextForComposer(t *testing.T) {
	text := helpBarText(ViComposer)
	if !strings.Contains(text, "Enter send") {
		t.Fatalf("expected composer help to mention send, got %q", text)
	}
	// The global 'q'/'?' shortcuts are suppressed while the composer is
	// focused, so they must not be advertised on its help line.
	if strings.Contains(text, " q") || strings.Contains(text, "?") {
		t.Fatalf("composer help should not advertise the global q/? keys, got %q", text)
	}
}

func TestResolveMessageLocation(t *testing.T) {
	state := AppState{
		TeamsState: TeamsState{
			channelById: map[string]Channel{"chan-1": {
				Channel: &csa.Channel{Id: "chan-1"},
				parent:  &csa.Team{Id: "team-1"},
			}},
		},
	}

	teamID, ok := state.resolveMessageLocation("chan-1")
	if !ok {
		t.Fatalf("expected chan-1 to resolve")
	}
	if teamID != "team-1" {
		t.Fatalf("expected team-1, got %q", teamID)
	}

	if _, ok := state.resolveMessageLocation("unknown"); ok {
		t.Fatalf("expected unknown channel to not resolve")
	}
}

func TestProbeMessageRegionUsesCachedRegion(t *testing.T) {
	state := AppState{messageRegion: "emea"}
	region, err := state.probeMessageRegion(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if region != "emea" {
		t.Fatalf("expected cached region emea, got %q", region)
	}
}

func TestProbeMessageRegionRequiresTeamsClient(t *testing.T) {
	state := AppState{TeamsState: TeamsState{}}
	if _, err := state.probeMessageRegion(context.Background()); err == nil {
		t.Fatal("expected an error when the teams client is nil")
	}
}

// TestSendConversationMessageAuthenticatesAgainstMessageServiceHost pins the
// root cause of the spurious forced sign-in: the POST must carry the
// Authentication header explicitly and target the host the read path uses.
// The teams-api AuthenticatedRequest helper only attaches the skype token
// for csa.MessagesHost URLs, so the old probed-region path silently sent
// unauthenticated POSTs that came back 401.
func TestSendConversationMessageAuthenticatesAgainstMessageServiceHost(t *testing.T) {
	var got *http.Request
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		bodyBytes, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(bodyBytes, &gotBody); err != nil {
			t.Errorf("request body is not valid JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	originalHost := messageServiceHost
	messageServiceHost = server.URL + "/"
	defer func() { messageServiceHost = originalHost }()

	state := &AppState{
		logger:     discardLogger,
		httpClient: server.Client(),
		TeamsState: TeamsState{me: &models.User{DisplayName: "Brian Tracey"}},
		skypeAuthHeaderOverride: func() (string, error) {
			return "skypetoken=test-token", nil
		},
	}

	if err := state.sendConversationMessage(context.Background(), ConversationTarget{ID: "19:abc@thread.v2"}, "hello"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got.Method != http.MethodPost {
		t.Fatalf("expected POST, got %s", got.Method)
	}
	if want := "/v1/users/ME/conversations/19:abc@thread.v2/messages"; got.URL.Path != want {
		t.Fatalf("expected path %q, got %q", want, got.URL.Path)
	}
	if auth := got.Header.Get("Authentication"); auth != "skypetoken=test-token" {
		t.Fatalf("expected explicit Authentication header, got %q", auth)
	}
	if contentType := got.Header.Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("expected JSON content type, got %q", contentType)
	}
	if gotBody["content"] != "<p>hello</p>" || gotBody["messagetype"] != "RichText/Html" {
		t.Fatalf("unexpected body: %v", gotBody)
	}
	// The stored sender name must be carried in the POST: without
	// imdisplayname the service records an empty name and the message
	// renders/reads as anonymous.
	if gotBody["imdisplayname"] != "Brian Tracey" {
		t.Fatalf("expected imdisplayname in the POST body, got %v", gotBody["imdisplayname"])
	}

	failureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer failureServer.Close()
	messageServiceHost = failureServer.URL + "/"
	err := state.sendConversationMessage(context.Background(), ConversationTarget{ID: "19:abc@thread.v2"}, "hello")
	if err == nil {
		t.Fatal("expected an error for a 401 response")
	}
	if !isAuthFailure(err) {
		t.Fatalf("expected 401 to classify as an auth failure, got %v", err)
	}
}

func TestProbeMessageRegionAcceptsOnlyAuthorizedRegions(t *testing.T) {
	seenAuth := map[string]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/deny-401"):
			seenAuth["deny-401"] = r.Header.Get("Authentication")
			w.WriteHeader(http.StatusUnauthorized)
		case strings.HasPrefix(r.URL.Path, "/deny-403"):
			seenAuth["deny-403"] = r.Header.Get("Authentication")
			w.WriteHeader(http.StatusForbidden)
		case strings.HasPrefix(r.URL.Path, "/accept-200"):
			seenAuth["accept-200"] = r.Header.Get("Authentication")
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	originalRegions := csaMessageRegions
	csaMessageRegions = []string{"deny-401", "deny-403", "accept-200"}
	defer func() { csaMessageRegions = originalRegions }()

	probeFormat := messageRegionProbeFormat
	messageRegionProbeFormat = server.URL + "/%s/properties"
	defer func() { messageRegionProbeFormat = probeFormat }()

	state := &AppState{
		logger:     discardLogger,
		httpClient: server.Client(),
		skypeAuthHeaderOverride: func() (string, error) {
			return "skypetoken=test-token", nil
		},
	}

	region, err := state.probeMessageRegion(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 401/403 answers mean the token was rejected: accepting them (the old
	// non-403 rule) picked amer on a 401 and sent every later request there
	// unauthenticated.
	if region != "accept-200" {
		t.Fatalf("expected 2xx-only acceptance to select accept-200, got %q", region)
	}
	for _, region := range []string{"deny-401", "deny-403", "accept-200"} {
		if seenAuth[region] != "skypetoken=test-token" {
			t.Fatalf("expected probe of %s to set the Authentication header, got %q", region, seenAuth[region])
		}
	}
}

// TestProbeMessageRegionFailsWhenNoRegionAuthorizes pins that a token
// rejected everywhere yields an error rather than a bogus cached region.
func TestProbeMessageRegionFailsWhenNoRegionAuthorizes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	originalRegions := csaMessageRegions
	csaMessageRegions = []string{"deny-a", "deny-b"}
	defer func() { csaMessageRegions = originalRegions }()

	probeFormat := messageRegionProbeFormat
	messageRegionProbeFormat = server.URL + "/%s/properties"
	defer func() { messageRegionProbeFormat = probeFormat }()

	state := &AppState{
		logger:     discardLogger,
		httpClient: server.Client(),
		skypeAuthHeaderOverride: func() (string, error) {
			return "skypetoken=test-token", nil
		},
	}

	if _, err := state.probeMessageRegion(context.Background()); err == nil {
		t.Fatal("expected an error when every region rejects the token")
	}
	if state.messageRegion != "" {
		t.Fatalf("expected no cached region, got %q", state.messageRegion)
	}
}

func TestApplySendOutcomePreservesDraftOnFailure(t *testing.T) {
	composer := tview.NewInputField()
	status := tview.NewTextView()
	state := &AppState{
		logger:        discardLogger,
		composerInput: composer,
		components:    map[string]tview.Primitive{TvComposerStatus: status},
	}
	target := ConversationTarget{ID: "19:abc@thread.v2"}
	state.setCurrentConversation(target)
	state.composerReply = replyContext{target: target, messageID: "msg-1"}

	composer.SetText("my draft")

	if state.applySendOutcome(target, errors.New("message send failed: 500")) {
		t.Fatal("expected applySendOutcome to report failure")
	}

	if composer.GetText() != "my draft" {
		t.Fatalf("expected the draft to survive a failed send, got %q", composer.GetText())
	}
	if state.composerReply.messageID != "msg-1" {
		t.Fatal("expected the reply context to survive a failed send")
	}
	if !strings.Contains(status.GetText(false), "Send failed") {
		t.Fatalf("expected a failure status, got %q", status.GetText(false))
	}
}

func TestApplySendOutcomeClearsDraftAndReplyOnSuccess(t *testing.T) {
	composer := tview.NewInputField()
	status := tview.NewTextView()
	state := &AppState{
		logger:        discardLogger,
		composerInput: composer,
		components:    map[string]tview.Primitive{TvComposerStatus: status},
	}
	target := ConversationTarget{ID: "19:abc@thread.v2"}
	state.setCurrentConversation(target)
	state.composerReply = replyContext{target: target, messageID: "msg-1"}

	composer.SetText("my draft")

	if !state.applySendOutcome(target, nil) {
		t.Fatal("expected applySendOutcome to report success")
	}

	if composer.GetText() != "" {
		t.Fatalf("expected the draft to clear after success, got %q", composer.GetText())
	}
	if state.composerReply.messageID != "" {
		t.Fatal("expected the reply context to clear after success")
	}
	if !strings.Contains(status.GetText(false), "Sent") {
		t.Fatalf("expected a sent status, got %q", status.GetText(false))
	}
}

func TestApplySendOutcomeIgnoresStaleConversation(t *testing.T) {
	composer := tview.NewInputField()
	status := tview.NewTextView()
	state := &AppState{
		logger:        discardLogger,
		composerInput: composer,
		components:    map[string]tview.Primitive{TvComposerStatus: status},
	}
	target := ConversationTarget{ID: "19:old@thread.v2"}
	state.setCurrentConversation(ConversationTarget{ID: "19:new@thread.v2"})

	composer.SetText("draft for old conversation")

	if state.applySendOutcome(target, nil) {
		t.Fatal("expected applySendOutcome to ignore a conversation that is no longer open")
	}
	if composer.GetText() != "draft for old conversation" {
		t.Fatalf("expected the other conversation's draft to stay untouched, got %q", composer.GetText())
	}
	if status.GetText(false) != "" {
		t.Fatalf("expected no status change, got %q", status.GetText(false))
	}
}
