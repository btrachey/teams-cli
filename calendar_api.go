package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	api "github.com/fossteams/teams-api/pkg"
)

// Calendar read path. Teams' own calendar lives behind the middle-tier
// service the chat client already talks to (teams.microsoft.com/api/mt), not
// Microsoft Graph: Graph rejects this app's tokens (wrong audience) while the
// middle-tier calendar view accepts the login/root skype token. Note this is
// NOT the token the ng.msg message service uses: that one is the
// authz-exchanged skype token, which the middle tier rejects with 401. The
// verified request is:
//
//	GET https://teams.microsoft.com/api/mt/part/{region}-{partition}/v2.1/me/calendars/calendarView
//	Authorization: Bearer <root skype token>
//
// {region}-{partition} is the tenant home (e.g. amer-01). Any valid partition
// returns the same data, but a bogus one 404s, so the segment must be a real
// provisioning partition. It is discovered from the authz response and cached,
// falling back to a short probe list of the common partitions.

const (
	calendarRequestTimeout = 8 * time.Second
	calendarPartitionTTL   = 6 * time.Hour
	calendarMaxEvents      = 500
)

// calendarMiddleTierFormat is the calendarView URL template. A var (like
// messageServiceHost) so tests can point it at a local server.
var calendarMiddleTierFormat = "https://teams.microsoft.com/api/mt/part/%s/v2.1/me/calendars/calendarView"

// calendarRootTokenOverride lets tests supply the skype token without the
// runtime token files; mirrors skypeAuthHeaderOverride.
var calendarRootTokenOverride func() (string, error)

// calendarPartitionFallbacks are tried, in order, when authz does not yield a
// usable partition. They cover the partitions seen across the major regions;
// the correct one answers 200, the rest 404 quickly.
var calendarPartitionFallbacks = []string{
	"amer-01", "amer-02", "emea-01", "emea-02", "apac-01", "apac-02",
}

// calendarSelectFields is the projection requested from the calendar view.
// skypeTeamsMeetingUrl is included because the join link only appears when it
// is selected explicitly (the field is absent otherwise).
const calendarSelectFields = "cleanGlobalObjectId,endTime,eventTimeZone,eventType," +
	"hasAttachments,iCalUid,isAllDayEvent,isAppointment,isCancelled,isOnlineMeeting," +
	"isOrganizer,isPrivate,lastModifiedTime,location,myResponseType,objectId," +
	"organizerAddress,organizerName,showAs,skypeTeamsData,skypeTeamsMeetingUrl," +
	"startTime,subject"

// CalendarEvent is one meeting/appointment row as returned by the calendar
// view. Only the fields the UI reads are decoded; the wire record carries many
// more.
type CalendarEvent struct {
	ObjectID             string    `json:"objectId"`
	Subject              string    `json:"subject"`
	StartTime            string    `json:"startTime"`
	EndTime              string    `json:"endTime"`
	LastModifiedTime     string    `json:"lastModifiedTime"`
	EventTimeZone        string    `json:"eventTimeZone"`
	EventType            string    `json:"eventType"`
	Location             string    `json:"location"`
	OrganizerName        string    `json:"organizerName"`
	OrganizerAddress     string    `json:"organizerAddress"`
	ShowAs               string    `json:"showAs"`
	MyResponseType       string    `json:"myResponseType"`
	IsAllDayEvent        bool      `json:"isAllDayEvent"`
	IsAppointment        bool      `json:"isAppointment"`
	IsCancelled          bool      `json:"isCancelled"`
	IsOnlineMeeting      bool      `json:"isOnlineMeeting"`
	IsOrganizer          bool      `json:"isOrganizer"`
	IsPrivate            bool      `json:"isPrivate"`
	HasAttachments       bool      `json:"hasAttachments"`
	SkypeTeamsMeetingURL string    `json:"skypeTeamsMeetingUrl"`
	Start                time.Time `json:"-"`
	End                  time.Time `json:"-"`
}

type calendarViewResponse struct {
	Count int             `json:"count"`
	Value []CalendarEvent `json:"value"`
}

// calendarResponseError carries a non-200 status so the fetch loop can
// distinguish an auth failure (worth a token refresh) from a bad partition
// (worth trying the next candidate).
type calendarResponseError struct {
	StatusCode int
	Status     string
	Body       string
}

// Error includes the bare status code for logging. isAuthFailure classifies
// this error through its typed calendarResponseError branch, not by string
// matching; the message stays human-readable here.
func (e *calendarResponseError) Error() string {
	return fmt.Sprintf("calendar request failed: %d %s", e.StatusCode, e.Status)
}

// calendarErrorIsStatus reports whether err is a calendarResponseError with
// the given HTTP status.
func calendarErrorIsStatus(err error, status int) bool {
	var statusErr *calendarResponseError
	if errors.As(err, &statusErr) {
		return statusErr.StatusCode == status
	}
	return false
}

// FetchCalendarEventsOverride lets tests replace the network call, mirroring
// fetchMessagesOverride.
type calendarFetchParams struct {
	startTime time.Time
	endTime   time.Time
	limit     int
}

// calendarRegion resolves the {region}-{partition} segment used in the
// calendar host. It is discovered once via authz and cached; token refreshes
// do not change the tenant partition, so the cache survives client swaps.
type calendarRegion struct {
	mu        sync.Mutex
	segment   string
	expiresAt time.Time
}

// calendarState groups the calendar feature's server-side state so it never
// collides with the chat fields on AppState.
type calendarState struct {
	region calendarRegion

	eventsMu     sync.Mutex
	events       []CalendarEvent
	loaded       bool
	loadedAt     time.Time
	rangeStart   time.Time
	rangeEnd     time.Time
	fetching     uint32
	loadingSince time.Time
	// windowOffset shifts the visible range by whole days for paging
	// ("today", "+1d", "-1d"). 0 is today.
	windowOffset int
	// override replaces the network fetch in tests.
	override func(ctx context.Context, params calendarFetchParams) ([]CalendarEvent, error)
}

// calendarPartitionFromAuthz is a seam so tests can drive partition discovery
// without a live authz round trip.
var calendarPartitionFromAuthz = defaultCalendarPartitionFromAuthz

// discoverCalendarPartition returns a validated {region}-{partition} segment.
// When every candidate is rejected with an auth error, the first auth error
// is returned (wrapped) rather than the generic message: the caller's
// refresh-and-retry classifies through isAuthFailure, and a token-expired
// run must reach it or the Electron refresh never fires.
func (s *AppState) discoverCalendarPartition(ctx context.Context) (string, error) {
	st := &s.calendar
	st.region.mu.Lock()
	defer st.region.mu.Unlock()

	if st.region.segment != "" && time.Now().Before(st.region.expiresAt) {
		return st.region.segment, nil
	}

	var firstAuthErr error
	if seg, ok := calendarPartitionFromAuthz(ctx, s); ok && calendarPartitionIsUsable(seg) {
		usable, authErr := s.probeCalendarPartition(ctx, seg)
		if usable {
			st.region.segment = seg
			st.region.expiresAt = time.Now().Add(calendarPartitionTTL)
			return seg, nil
		}
		if authErr != nil && firstAuthErr == nil {
			firstAuthErr = authErr
		}
	}

	for _, candidate := range calendarPartitionFallbacks {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		usable, authErr := s.probeCalendarPartition(ctx, candidate)
		if usable {
			st.region.segment = candidate
			st.region.expiresAt = time.Now().Add(calendarPartitionTTL)
			return candidate, nil
		}
		if authErr != nil && firstAuthErr == nil {
			firstAuthErr = authErr
		}
	}

	if firstAuthErr != nil {
		return "", fmt.Errorf("calendar partition probes rejected the token: %w", firstAuthErr)
	}
	return "", errors.New("unable to determine the Teams calendar region")
}

// probeCalendarPartition issues a minimal calendar request to decide whether
// the segment is a real partition. Only a 200 marks it usable: a wrong
// partition 404s; a genuine auth rejection (401/403) must not be cached as
// usable, because the discovery loop runs inside a fetch that has not done
// its refresh-and-retry yet and an unrefreshed expired token would otherwise
// poison the partition cache for the whole session; and a transport error is
// no evidence of a real partition either, so it must not be cached either.
// The bool-only shape cannot express the auth case, so the auth error is
// returned for discovery to propagate.
func (s *AppState) probeCalendarPartition(ctx context.Context, segment string) (usable bool, authErr error) {
	probeCtx, cancel := context.WithTimeout(ctx, calendarRequestTimeout)
	defer cancel()

	probeStart := time.Now().Add(-time.Hour)
	probeEnd := time.Now().Add(time.Hour)
	_, err := s.doCalendarRequest(probeCtx, segment, calendarFetchParams{
		startTime: probeStart,
		endTime:   probeEnd,
		limit:     1,
	})
	if err == nil {
		return true, nil
	}
	if isAuthFailure(err) {
		return false, err
	}
	return false, nil
}

// fetchCalendarEvents loads meetings for [start, end] and returns them sorted
// ascending by start time. A single token refresh-and-retry mirrors the
// message fetch path.
func (s *AppState) fetchCalendarEvents(ctx context.Context, params calendarFetchParams) ([]CalendarEvent, error) {
	events, err := s.calendarEventsForWindow(ctx, params)
	if err != nil {
		return nil, err
	}

	// Normalize on every path (real and override): the view assumes ascending
	// order and a bounded length.
	sortCalendarEvents(events)
	if params.limit > 0 && len(events) > params.limit {
		events = events[:params.limit]
	}
	return events, nil
}

// calendarEventsForWindow performs the network fetch (or the test override),
// with a single token refresh-and-retry on an auth failure.
//
// Each network attempt runs under calendarLoadTimeout, but the refresh
// itself runs on the parent ctx: refreshTokensWithTeamsToken derives its own
// timeout from the refresh config, and the Electron flow needs more than a
// request budget — clipping it here would kill a ~16s refresh at 12s and arm
// the shared failure cooldown that also gates the chat path's reactive
// refresh.
func (s *AppState) calendarEventsForWindow(ctx context.Context, params calendarFetchParams) ([]CalendarEvent, error) {
	if s.calendar.override != nil {
		return s.calendar.override(ctx, params)
	}

	if s.client() == nil && s.skypeAuthHeaderOverride == nil {
		return nil, errors.New("teams client is nil")
	}

	events, segment, err := s.calendarAttempt(ctx, "", params)
	if err != nil && isAuthFailure(err) && s.maybeRefreshTokens(ctx, err) {
		// A refresh rotates the skype token. Reuse the segment when the
		// failed attempt validated one (validation needs a 200, so a
		// later 401 is the token, not the partition); rediscover
		// otherwise — every candidate was rejected under the old token.
		events, _, err = s.calendarAttempt(ctx, segment, params)
	}
	if err != nil {
		return nil, err
	}
	return events, nil
}

// calendarAttempt performs one discovery+view-request cycle under the load
// budget, optionally reusing an already-validated segment (empty means
// discover). The validated segment is returned so the retry path can skip
// discovery.
func (s *AppState) calendarAttempt(ctx context.Context, knownSegment string, params calendarFetchParams) ([]CalendarEvent, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, calendarLoadTimeout)
	defer cancel()

	segment := knownSegment
	if segment == "" {
		var err error
		segment, err = s.discoverCalendarPartition(attemptCtx)
		if err != nil {
			return nil, "", err
		}
	}

	events, err := s.doCalendarRequest(attemptCtx, segment, params)
	if err != nil {
		return nil, segment, err
	}
	return events, segment, nil
}

// doCalendarRequest performs one authenticated calendarView GET.
func (s *AppState) doCalendarRequest(ctx context.Context, segment string, params calendarFetchParams) ([]CalendarEvent, error) {
	bearer, err := s.rootSkypeToken()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf(calendarMiddleTierFormat, url.PathEscape(segment))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	query := request.URL.Query()
	query.Set("startDate", formatCalendarTime(params.startTime))
	query.Set("endDate", formatCalendarTime(params.endTime))
	query.Set("$filter", "isCancelled eq false")
	query.Set("$orderby", "startTime asc")
	limit := params.limit
	if limit <= 0 {
		limit = calendarMaxEvents
	}
	if limit > calendarMaxEvents {
		limit = calendarMaxEvents
	}
	query.Set("$top", fmt.Sprint(limit))
	query.Set("$count", "true")
	query.Set("$select", calendarSelectFields)
	request.URL.RawQuery = query.Encode()

	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", "https://teams.microsoft.com")
	request.Header.Set("Referer", "https://teams.microsoft.com/")

	client := s.messageHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, &calendarResponseError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       strings.TrimSpace(string(body)),
		}
	}

	var decoded calendarViewResponse
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("unable to decode calendar events: %w", err)
	}

	events := decoded.Value
	for i := range events {
		events[i].Start = parseCalendarTime(events[i].StartTime)
		events[i].End = parseCalendarTime(events[i].EndTime)
	}
	return events, nil
}

// rootSkypeToken returns the raw login/root skype token from the runtime
// token files. Tests override calendarRootTokenOverride to stay offline.
func (s *AppState) rootSkypeToken() (string, error) {
	if calendarRootTokenOverride != nil {
		return calendarRootTokenOverride()
	}
	root, err := api.GetRootToken()
	if err != nil {
		return "", fmt.Errorf("unable to read the skype token: %w", err)
	}
	if root == nil || root.Inner == nil || root.Inner.Raw == "" {
		return "", errors.New("skype token is not a valid JWT")
	}
	return root.Inner.Raw, nil
}

// formatCalendarTime renders a UTC time in the ISO-8601 shape the calendar
// view expects for its startDate/endDate bounds.
func formatCalendarTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05.000Z")
}

// parseCalendarTime parses a calendar timestamp, tolerating the offsets the
// service emits (e.g. "2026-11-05T16:30:00+00:00").
func parseCalendarTime(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05Z",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// sortCalendarEvents orders events ascending by start time, breaking ties on
// subject so the ordering is deterministic.
func sortCalendarEvents(events []CalendarEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		left, right := events[i], events[j]
		if !left.Start.Equal(right.Start) {
			if left.Start.IsZero() {
				return false
			}
			if right.Start.IsZero() {
				return true
			}
			return left.Start.Before(right.Start)
		}
		return left.Subject < right.Subject
	})
}

// calendarPartitionIsUsable rejects empty or obviously malformed segments
// (whitespace, path traversal) before they reach the probe.
func calendarPartitionIsUsable(segment string) bool {
	segment = strings.TrimSpace(segment)
	if segment == "" || segment != strings.ToLower(segment) {
		return false
	}
	if strings.ContainsAny(segment, "/ \t\n\\") {
		return false
	}
	// Must look like region-partition (at least one letter, a dash, a
	// partition suffix).
	return strings.Contains(segment, "-")
}

// calendarAuthzResponse is the subset of the authz body needed to derive the
// calendar host segment. The teams-api Authz helper decodes only the token,
// so the region/partition are read directly here.
type calendarAuthzResponse struct {
	Region     string `json:"region"`
	Partition  string `json:"partition"`
	RegionGtms struct {
		MiddleTier string `json:"middleTier"`
	} `json:"regionGtms"`
}

// calendarAuthzEndpoint is a var so tests keep partition discovery on the
// local server instead of the live authz service.
var calendarAuthzEndpoint = "https://teams.microsoft.com/api/authsvc/v1.0/authz"

// defaultCalendarPartitionFromAuthz derives {region}-{partition} from the
// authz response (region "amer" + partition "amer01" -> "amer-01"). It is
// fenced against teams-api's panic-on-malformed-token behavior because it can
// run from a background goroutine.
func defaultCalendarPartitionFromAuthz(ctx context.Context, s *AppState) (segment string, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			segment, ok = "", false
		}
	}()

	bearer, err := s.rootSkypeToken()
	if err != nil || bearer == "" {
		return "", false
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, calendarAuthzEndpoint, bytes.NewReader(nil))
	if err != nil {
		return "", false
	}
	request.Header.Set("ms-teams-authz-type", "TokenRefresh")
	request.Header.Set("Authorization", "Bearer "+bearer)

	response, err := s.messageHTTPClient().Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}

	var authz calendarAuthzResponse
	if err := json.NewDecoder(response.Body).Decode(&authz); err != nil {
		return "", false
	}

	region := strings.ToLower(strings.TrimSpace(authz.Region))
	partition := strings.ToLower(strings.TrimSpace(authz.Partition))
	if region == "" || partition == "" {
		return "", false
	}

	// partition typically repeats the region prefix ("amer" + "amer01");
	// the calendar host wants them joined with a single dash.
	suffix := strings.TrimPrefix(partition, region)
	suffix = strings.TrimLeft(suffix, "-")
	if suffix == "" {
		suffix = partition
	}
	derived := region + "-" + suffix
	if !calendarPartitionIsUsable(derived) {
		return "", false
	}
	return derived, true
}
