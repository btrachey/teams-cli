package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	teams_api "github.com/fossteams/teams-api"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/rivo/tview"
	"github.com/sirupsen/logrus"
)

type AppState struct {
	app         *tview.Application
	pages       *tview.Pages
	logger      *logrus.Logger
	logFilePath string
	ctx         context.Context
	cancel      context.CancelFunc

	TeamsState
	components                   map[string]tview.Primitive
	loadSeq                      uint64
	activeLoadSeq                uint64
	messageLimit                 int
	focusedComponent             string
	previousFocus                string
	chatPaneFocusable            bool
	httpClient                   *http.Client
	liveRefreshDisabled          bool
	liveMessageRefreshEvery      time.Duration
	liveConversationRefreshEvery time.Duration
	stateMu                      sync.RWMutex
	currentConversation          ConversationTarget
	currentConversationSet       bool
	currentMessagesSignature     string
	lastMessageSyncAt            time.Time
	liveLoopStarted              uint32
	liveMessageRefreshActive     uint32
	liveTreeRefreshActive        uint32
	loadCancelMu                 sync.Mutex
	activeLoadCancel             context.CancelFunc
	activeLoadCancelSeq          uint64
	shutdownOnce                 sync.Once

	tokenDir                 string
	tokenRefresh             tokenRefreshConfig
	tokenRefreshMu           sync.Mutex
	lastTokenRefreshAt       time.Time
	lastTokenRefreshFailedAt time.Time
	refreshTokensOverride    func(ctx context.Context) error
	newTeamsClientOverride   func() (*teams_api.TeamsClient, error)
	skypeAuthHeaderOverride  func() (string, error)

	composerReply      replyContext
	conversationLoaded bool
	displayedMessages  []csa.ChatMessage
	messageRegion      string
	regionMu           sync.Mutex
	composerInput      *tview.InputField
	sendInFlight       uint32
}

const (
	loadingBarWidth = 18

	loadingPulseWidth                      = 5
	loadingTickInterval                    = 120 * time.Millisecond
	helpBarHeight                          = 1
	composerHeight                         = 3
	defaultLiveMessageRefreshInterval      = 5 * time.Second
	defaultLiveConversationRefreshInterval = 15 * time.Second
	messageRequestTimeout                  = 5 * time.Second
	messageFetchMaxAttempts                = 3
	messageSendTimeout                     = 15 * time.Second
	messageFetchRetryBaseDelay             = 250 * time.Millisecond
	messageFetchRetryMaxDelay              = time.Second
)

type replyContext struct {
	target    ConversationTarget
	messageID string
}

type treeViewState struct {
	expandedPaths    map[string]struct{}
	selectedPath     string
	selectedTargetID string
}

func newMessageHTTPClient() *http.Client {
	return &http.Client{
		Timeout: messageRequestTimeout,
	}
}

func (s *AppState) initRuntime(parent context.Context) {
	if parent == nil {
		parent = context.Background()
	}

	s.ctx, s.cancel = context.WithCancel(parent)
	if s.httpClient == nil {
		s.httpClient = newMessageHTTPClient()
	}
}

func (s *AppState) ensureRuntime() {
	if s.ctx != nil && s.cancel != nil && s.httpClient != nil {
		return
	}

	s.initRuntime(context.Background())
}

func (s *AppState) appContext() context.Context {
	if s.ctx != nil {
		return s.ctx
	}

	return context.Background()
}

func (s *AppState) appLogger() *logrus.Logger {
	if s != nil && s.logger != nil {
		return s.logger
	}

	return discardLogger
}

func (s *AppState) requestStop() {
	s.shutdownOnce.Do(func() {
		s.appLogger().WithField("log_file", s.logFilePath).Info("shutdown requested")
		s.cancelActiveLoad()
		if s.cancel != nil {
			s.cancel()
		}
		if s.app != nil {
			s.app.Stop()
		}
	})
}

func (s *AppState) replaceActiveLoadCancel(loadSeq uint64, cancel context.CancelFunc) {
	s.loadCancelMu.Lock()
	previousCancel := s.activeLoadCancel
	s.activeLoadCancel = cancel
	s.activeLoadCancelSeq = loadSeq
	s.loadCancelMu.Unlock()

	if previousCancel != nil {
		previousCancel()
	}
}

func (s *AppState) clearActiveLoadCancel(loadSeq uint64) {
	s.loadCancelMu.Lock()
	defer s.loadCancelMu.Unlock()

	if s.activeLoadCancelSeq != loadSeq {
		return
	}

	s.activeLoadCancel = nil
	s.activeLoadCancelSeq = 0
}

func (s *AppState) cancelActiveLoad() {
	s.loadCancelMu.Lock()
	cancel := s.activeLoadCancel
	s.activeLoadCancel = nil
	s.activeLoadCancelSeq = 0
	s.loadCancelMu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (s *AppState) setCurrentConversation(target ConversationTarget) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	s.currentConversation = target
	s.currentConversationSet = true
	s.currentMessagesSignature = ""
	s.lastMessageSyncAt = time.Time{}
}

func (s *AppState) clearCurrentConversation() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	s.currentConversation = ConversationTarget{}
	s.currentConversationSet = false
	s.currentMessagesSignature = ""
	s.lastMessageSyncAt = time.Time{}
}

func (s *AppState) currentConversationSnapshot() (ConversationTarget, bool, string, time.Time) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	return s.currentConversation, s.currentConversationSet, s.currentMessagesSignature, s.lastMessageSyncAt
}

func (s *AppState) currentConversationIs(id string) bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	return s.currentConversationSet && s.currentConversation.ID == id
}

func (s *AppState) updateCurrentConversationTarget(target ConversationTarget) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if s.currentConversationSet && s.currentConversation.ID == target.ID {
		s.currentConversation = target
	}
}

func (s *AppState) rememberConversationSync(target ConversationTarget, signature string, syncedAt time.Time) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	if !s.currentConversationSet || s.currentConversation.ID != target.ID {
		return
	}

	s.currentConversation = target
	s.currentMessagesSignature = signature
	s.lastMessageSyncAt = syncedAt
}

func (s *AppState) liveRefreshEnabled() bool {
	return !s.liveRefreshDisabled
}

func (s *AppState) messageRefreshInterval() time.Duration {
	if s.liveMessageRefreshEvery > 0 {
		return s.liveMessageRefreshEvery
	}

	return defaultLiveMessageRefreshInterval
}

func (s *AppState) conversationRefreshInterval() time.Duration {
	if s.liveConversationRefreshEvery > 0 {
		return s.liveConversationRefreshEvery
	}

	return defaultLiveConversationRefreshInterval
}

func (s *AppState) conversationSyncStatus(syncedAt time.Time) string {
	if syncedAt.IsZero() {
		return ""
	}
	if s.liveRefreshEnabled() {
		return "live " + syncedAt.Format("15:04:05")
	}

	return "loaded " + syncedAt.Format("15:04:05")
}

// resolveMessageLocation maps a conversation ID to the team ID needed by the
// CSA v2 thread-reply endpoints. Channel IDs are unique across the loaded
// conversation data.
func (s *TeamsState) resolveMessageLocation(channelID string) (string, bool) {
	channel, ok := s.channelById[channelID]
	if !ok || channel.Channel == nil || channel.parent == nil {
		return "", false
	}

	return channel.parent.Id, true
}

func (s *AppState) isChannelConversation(conversationID string) bool {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()

	_, ok := s.channelById[conversationID]
	return ok
}

func (s *AppState) currentMessageList() *tview.List {
	list, ok := s.components[ViChat].(*tview.List)
	if !ok {
		return nil
	}

	return list
}

func (s *AppState) messageAtCurrentListIndex() (*csa.ChatMessage, bool) {
	list := s.currentMessageList()
	if list == nil {
		return nil, false
	}

	if list.GetItemCount() != len(s.displayedMessages) {
		// The list holds a hint or error placeholder, not real messages.
		return nil, false
	}

	index := list.GetCurrentItem()
	if index < 0 || index >= len(s.displayedMessages) {
		return nil, false
	}

	return &s.displayedMessages[index], true
}
