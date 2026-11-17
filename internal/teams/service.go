// Package teams is the Teams chat backend: the teams-api client, the
// signed-in profile, the conversation tree, and reading and sending
// messages.
package teams

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	teams_api "github.com/fossteams/teams-api"
	"github.com/fossteams/teams-api/pkg/csa"
	"github.com/fossteams/teams-api/pkg/models"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/sirupsen/logrus"
)

// Service holds the Teams session. The zero value is ready to use once a
// client is set.
type Service struct {
	// HTTPClient carries message, calendar, and image requests; nil means a
	// client with the default request timeout.
	HTTPClient *http.Client
	Logger     *logrus.Logger
	// Reauth refreshes the tokens after an auth failure and reports whether
	// the failed request should be retried. Nil disables the retry.
	Reauth func(ctx context.Context, cause error) bool

	clientMu sync.RWMutex
	client   *teams_api.TeamsClient

	dataMu sync.RWMutex
	me     *models.User
	data   *ConversationData

	// region caches the tenant's message-service region (thread replies);
	// it is re-probed after the client is replaced.
	regionMu sync.Mutex
	region   string

	// skypeAuthHeaderOverride replaces the token-derived header in tests.
	skypeAuthHeaderOverride func() (string, error)
}

// Conversation identifies a channel or chat.
type Conversation struct {
	ID    string
	Title string
}

// ConversationData is one fetch of the conversation tree, sorted for
// display.
type ConversationData struct {
	conversations *csa.ConversationResponse
	channelByID   map[string]channel
}

type channel struct {
	*csa.Channel
	parent *csa.Team
}

// NewClient constructs the teams-api client from the runtime tokens.
// teams-api panics on malformed token files (nil JWT inside the authz
// exchange), so the call is fenced into an error.
func NewClient() (client *teams_api.TeamsClient, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			client = nil
			err = fmt.Errorf("teams client construction failed: %v", recovered)
		}
	}()

	return teams_api.New()
}

// Client returns the current teams-api client. It is replaced after a
// token refresh, so callers must capture it per request.
func (s *Service) Client() *teams_api.TeamsClient {
	s.clientMu.RLock()
	defer s.clientMu.RUnlock()

	return s.client
}

// SetClient installs a (re)built client and drops the cached region, which
// was probed with the previous token.
func (s *Service) SetClient(client *teams_api.TeamsClient) {
	s.clientMu.Lock()
	s.client = client
	s.clientMu.Unlock()

	s.regionMu.Lock()
	s.region = ""
	s.regionMu.Unlock()
}

func (s *Service) logger() *logrus.Logger {
	if s.Logger != nil {
		return s.Logger
	}

	return logging.Discard
}

// HTTP returns the client for direct requests to Teams services.
func (s *Service) HTTP() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}

	return &http.Client{Timeout: requestTimeout}
}

// Init loads the signed-in profile and the conversation tree.
func (s *Service) Init() error {
	client := s.Client()
	if client == nil {
		return fmt.Errorf("client is nil")
	}

	me, err := client.GetMe()
	if err != nil {
		return fmt.Errorf("unable to get your profile: %v", err)
	}
	s.SetProfile(me)

	data, err := s.FetchConversations()
	if err != nil {
		return err
	}
	s.ApplyConversations(data)

	return nil
}

// FetchConversations loads and sorts the conversation tree without
// installing it. Pinned channels lead their team.
func (s *Service) FetchConversations() (*ConversationData, error) {
	client := s.Client()
	if client == nil {
		return nil, fmt.Errorf("teams client is nil")
	}

	pinnedChannels, err := client.GetPinnedChannels()
	if err != nil {
		return nil, fmt.Errorf("unable to get pinned channels: %v", err)
	}

	conversations, err := client.GetConversations()
	if err != nil {
		return nil, fmt.Errorf("unable to get conversations: %v", err)
	}

	pinnedChannelOrder := map[string]int{}
	for idx, channelID := range pinnedChannels {
		pinnedChannelOrder[string(channelID)] = idx
	}
	sortTeams(conversations.Teams)
	for teamIdx := range conversations.Teams {
		sortChannels(conversations.Teams[teamIdx].Channels, pinnedChannelOrder)
	}
	sortChats(conversations.Chats, s.SelfMri(), s.SelfDisplayName())

	return NewConversationData(conversations), nil
}

// NewConversationData indexes a conversation tree for lookup. The tree is
// used as-is; FetchConversations sorts it for display first.
func NewConversationData(conversations *csa.ConversationResponse) *ConversationData {
	data := &ConversationData{
		conversations: conversations,
		channelByID:   map[string]channel{},
	}

	for teamIdx := range conversations.Teams {
		team := &conversations.Teams[teamIdx]
		for channelIdx := range team.Channels {
			ch := &team.Channels[channelIdx]
			data.channelByID[ch.Id] = channel{Channel: ch, parent: team}
		}
	}

	return data
}

// ApplyConversations installs a fetched conversation tree.
func (s *Service) ApplyConversations(data *ConversationData) {
	if data == nil {
		return
	}

	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	s.data = data
}

// Conversations returns the installed tree (nil before Init). The response
// is replaced, never mutated, so callers may read it without locking.
func (s *Service) Conversations() *csa.ConversationResponse {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	if s.data == nil {
		return nil
	}
	return s.data.conversations
}

// Channel returns the channel with the given conversation ID.
func (s *Service) Channel(id string) (*csa.Channel, bool) {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	if s.data == nil {
		return nil, false
	}
	ch, ok := s.data.channelByID[id]
	if !ok || ch.Channel == nil {
		return nil, false
	}
	return ch.Channel, true
}

// IsChannel reports whether a conversation ID names a channel (as opposed
// to a chat).
func (s *Service) IsChannel(id string) bool {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	if s.data == nil {
		return false
	}
	_, ok := s.data.channelByID[id]
	return ok
}

// teamOf maps a channel ID to the team ID the CSA v2 thread-reply endpoints
// need. Channel IDs are unique across the loaded conversation data.
func (s *Service) teamOf(channelID string) (string, bool) {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	if s.data == nil {
		return "", false
	}
	ch, ok := s.data.channelByID[channelID]
	if !ok || ch.Channel == nil || ch.parent == nil {
		return "", false
	}

	return ch.parent.Id, true
}

// SetProfile installs the signed-in user's profile (Init loads it).
func (s *Service) SetProfile(me *models.User) {
	s.dataMu.Lock()
	defer s.dataMu.Unlock()
	s.me = me
}

func (s *Service) SelfMri() string {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	if s.me == nil {
		return ""
	}

	return s.me.Mri
}

func (s *Service) SelfDisplayName() string {
	s.dataMu.RLock()
	defer s.dataMu.RUnlock()

	if s.me == nil {
		return ""
	}

	return s.me.DisplayName
}

// Link is the Teams web/desktop deep link for a conversation, or for one of
// its messages when messageID is set.
func (s *Service) Link(conversationID, messageID string) string {
	id := url.PathEscape(conversationID)
	if messageID != "" {
		return "https://teams.microsoft.com/l/message/" + id + "/" + url.PathEscape(messageID)
	}
	if ch, ok := s.Channel(conversationID); ok {
		return "https://teams.microsoft.com/l/channel/" + id + "/" + url.PathEscape(ch.DisplayName)
	}
	return "https://teams.microsoft.com/l/chat/" + id + "/0"
}

// ChatTitle names a chat from the signed-in user's point of view.
func (s *Service) ChatTitle(chat csa.Chat) string {
	return resolveChatTitle(chat, s.SelfMri(), s.SelfDisplayName())
}

func sortTeams(teams []csa.Team) {
	sort.SliceStable(teams, func(i, j int) bool {
		return lessTeam(teams[i], teams[j])
	})
}

func lessTeam(left, right csa.Team) bool {
	if left.IsDeleted != right.IsDeleted {
		return !left.IsDeleted && right.IsDeleted
	}
	if left.IsFavorite != right.IsFavorite {
		return left.IsFavorite && !right.IsFavorite
	}
	if left.IsFollowed != right.IsFollowed {
		return left.IsFollowed && !right.IsFollowed
	}
	if left.IsArchived != right.IsArchived {
		return !left.IsArchived && right.IsArchived
	}

	return normalizedSortText(left.DisplayName) < normalizedSortText(right.DisplayName)
}

func sortChannels(channels []csa.Channel, pinnedChannelOrder map[string]int) {
	sort.SliceStable(channels, func(i, j int) bool {
		return lessChannel(channels[i], channels[j], pinnedChannelOrder)
	})
}

func lessChannel(left, right csa.Channel, pinnedChannelOrder map[string]int) bool {
	leftPinnedIndex, leftPinned := pinnedChannelOrder[left.Id]
	rightPinnedIndex, rightPinned := pinnedChannelOrder[right.Id]
	if leftPinned != rightPinned {
		return leftPinned && !rightPinned
	}
	if leftPinned && rightPinned && leftPinnedIndex != rightPinnedIndex {
		return leftPinnedIndex < rightPinnedIndex
	}
	if left.IsGeneral != right.IsGeneral {
		return left.IsGeneral && !right.IsGeneral
	}
	if left.IsFavorite != right.IsFavorite {
		return left.IsFavorite && !right.IsFavorite
	}
	if left.IsPinned != right.IsPinned {
		return left.IsPinned && !right.IsPinned
	}
	if left.IsArchived != right.IsArchived {
		return !left.IsArchived && right.IsArchived
	}

	return normalizedSortText(left.DisplayName) < normalizedSortText(right.DisplayName)
}

func sortChats(chats []csa.Chat, selfMri, selfDisplayName string) {
	sort.SliceStable(chats, func(i, j int) bool {
		return lessChat(chats[i], chats[j], selfMri, selfDisplayName)
	})
}

func lessChat(left, right csa.Chat, selfMri, selfDisplayName string) bool {
	if left.Hidden != right.Hidden {
		return !left.Hidden && right.Hidden
	}
	if left.IsSticky != right.IsSticky {
		return left.IsSticky && !right.IsSticky
	}

	leftTime := ChatActivityTime(left)
	rightTime := ChatActivityTime(right)
	if !leftTime.Equal(rightTime) {
		return leftTime.After(rightTime)
	}

	leftTitle := normalizedSortText(resolveChatTitle(left, selfMri, selfDisplayName))
	rightTitle := normalizedSortText(resolveChatTitle(right, selfMri, selfDisplayName))
	if leftTitle != rightTitle {
		return leftTitle < rightTitle
	}

	return left.Id < right.Id
}

// ChatActivityTime is when a chat last saw activity (zero when unknown).
func ChatActivityTime(chat csa.Chat) time.Time {
	if ts := time.Time(chat.LastMessage.ComposeTime); !ts.IsZero() {
		return ts
	}
	if ts := time.Time(chat.LastMessage.OriginalArrivalTime); !ts.IsZero() {
		return ts
	}
	if !chat.LastJoinAt.IsZero() {
		return chat.LastJoinAt
	}
	if !chat.LastLeaveAt.IsZero() {
		return chat.LastLeaveAt
	}
	if ts := parseConversationTime(chat.CreatedAt); !ts.IsZero() {
		return ts
	}

	return time.Time{}
}

func parseConversationTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}

	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05.9999999Z07:00",
	}
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed
		}
	}

	return time.Time{}
}

func normalizedSortText(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
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
		if member.Mri != selfMri {
			participantCount++
		}
	}

	switch {
	case strings.EqualFold(chat.ChatType, "meeting"):
		return "Meeting chat"
	case chat.IsOneOnOne || participantCount <= 1:
		return "Direct chat"
	default:
		return fmt.Sprintf("Group chat (%d people)", participantCount)
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
