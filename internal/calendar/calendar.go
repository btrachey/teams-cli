// Package calendar reads the signed-in user's Teams meetings.
package calendar

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
	"github.com/fossteams/teams-cli/internal/auth"
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
	requestTimeout = 8 * time.Second
	// attemptTimeout bounds one discovery+request cycle.
	attemptTimeout = 12 * time.Second
	partitionTTL   = 6 * time.Hour
	MaxEvents      = 500
)

// middleTierFormat is the calendarView URL template; a var so tests can
// point it at a local server.
var middleTierFormat = "https://teams.microsoft.com/api/mt/part/%s/v2.1/me/calendars/calendarView"

// partitionFallbacks are tried, in order, when authz does not yield a
// usable partition. They cover the partitions seen across the major regions;
// the correct one answers 200, the rest 404 quickly.
var partitionFallbacks = []string{
	"amer-01", "amer-02", "emea-01", "emea-02", "apac-01", "apac-02",
}

// selectFields is the projection requested from the calendar view.
// skypeTeamsMeetingUrl is included because the join link only appears when it
// is selected explicitly (the field is absent otherwise).
const selectFields = "cleanGlobalObjectId,endTime,eventTimeZone,eventType," +
	"hasAttachments,iCalUid,isAllDayEvent,isAppointment,isCancelled,isOnlineMeeting," +
	"isOrganizer,isPrivate,lastModifiedTime,location,myResponseType,objectId," +
	"organizerAddress,organizerName,showAs,skypeTeamsData,skypeTeamsMeetingUrl," +
	"startTime,subject"

// Event is one meeting/appointment row as returned by the calendar
// view. Only the fields the UI reads are decoded; the wire record carries many
// more.
type Event struct {
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

type viewResponse struct {
	Count int     `json:"count"`
	Value []Event `json:"value"`
}

// ResponseError carries a non-200 status so the fetch loop can
// distinguish an auth failure (worth a token refresh) from a bad partition
// (worth trying the next candidate).
type ResponseError struct {
	StatusCode int
	Status     string
	Body       string
}

// Error includes the bare status code for logging; auth.IsAuthFailure
// classifies through HTTPStatus, not the message.
func (e *ResponseError) Error() string {
	return fmt.Sprintf("calendar request failed: %d %s", e.StatusCode, e.Status)
}

func (e *ResponseError) HTTPStatus() int { return e.StatusCode }

// Query bounds one calendarView request.
type Query struct {
	Start time.Time
	End   time.Time
	// Limit caps the result; MaxEvents when zero.
	Limit int
}

// regionCache resolves the {region}-{partition} segment used in the
// calendar host. It is discovered once via authz and cached; token refreshes
// do not change the tenant partition, so the cache survives client swaps.
type regionCache struct {
	mu        sync.Mutex
	segment   string
	expiresAt time.Time
}

// Client fetches calendar views. The zero value reads the token files and
// uses a default HTTP client.
type Client struct {
	HTTPClient *http.Client
	// RootToken returns the login (root) skype token; nil reads the runtime
	// token files.
	RootToken func() (string, error)
	// Reauth refreshes the tokens after an auth failure and reports whether
	// to retry. Nil disables the retry.
	Reauth func(ctx context.Context, cause error) bool

	region regionCache
}

// partitionFromAuthz is a seam so tests can drive partition discovery
// without a live authz round trip.
var partitionFromAuthz = defaultPartitionFromAuthz

// discoverPartition returns a validated {region}-{partition} segment.
// When every candidate is rejected with an auth error, the first auth error
// is returned (wrapped) rather than the generic message: the caller's
// refresh-and-retry classifies through auth.IsAuthFailure, and a token-expired
// run must reach it or the Electron refresh never fires.
func (c *Client) discoverPartition(ctx context.Context) (string, error) {
	c.region.mu.Lock()
	defer c.region.mu.Unlock()

	if c.region.segment != "" && time.Now().Before(c.region.expiresAt) {
		return c.region.segment, nil
	}

	var firstAuthErr error
	if seg, ok := partitionFromAuthz(ctx, c); ok && partitionIsUsable(seg) {
		usable, authErr := c.probePartition(ctx, seg)
		if usable {
			c.region.segment = seg
			c.region.expiresAt = time.Now().Add(partitionTTL)
			return seg, nil
		}
		if authErr != nil && firstAuthErr == nil {
			firstAuthErr = authErr
		}
	}

	for _, candidate := range partitionFallbacks {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		usable, authErr := c.probePartition(ctx, candidate)
		if usable {
			c.region.segment = candidate
			c.region.expiresAt = time.Now().Add(partitionTTL)
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

// probePartition issues a minimal calendar request to decide whether
// the segment is a real partition. Only a 200 marks it usable: a wrong
// partition 404s; a genuine auth rejection (401/403) must not be cached as
// usable, because the discovery loop runs inside a fetch that has not done
// its refresh-and-retry yet and an unrefreshed expired token would otherwise
// poison the partition cache for the whole session; and a transport error is
// no evidence of a real partition either, so it must not be cached either.
// The bool-only shape cannot express the auth case, so the auth error is
// returned for discovery to propagate.
func (c *Client) probePartition(ctx context.Context, segment string) (usable bool, authErr error) {
	probeCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	probeStart := time.Now().Add(-time.Hour)
	probeEnd := time.Now().Add(time.Hour)
	_, err := c.request(probeCtx, segment, Query{
		Start: probeStart,
		End:   probeEnd,
		Limit: 1,
	})
	if err == nil {
		return true, nil
	}
	if auth.IsAuthFailure(err) {
		return false, err
	}
	return false, nil
}

// Events loads meetings for [start, end] and returns them sorted
// ascending by start time. A single token refresh-and-retry mirrors the
// message fetch path.
func (c *Client) Events(ctx context.Context, params Query) ([]Event, error) {
	events, err := c.eventsForWindow(ctx, params)
	if err != nil {
		return nil, err
	}

	return Normalize(events, params.Limit), nil
}

// eventsForWindow performs the network fetch, with a single token
// refresh-and-retry on an auth failure.
//
// Each network attempt runs under attemptTimeout, but the refresh
// itself runs on the parent ctx: the refresh derives its own
// timeout from the refresh config, and the Electron flow needs more than a
// request budget — clipping it here would kill a ~16s refresh at 12s and arm
// the shared failure cooldown that also gates the chat path's reactive
// refresh.
func (c *Client) eventsForWindow(ctx context.Context, params Query) ([]Event, error) {
	events, segment, err := c.attempt(ctx, "", params)
	if err != nil && c.Reauth != nil && auth.IsAuthFailure(err) && c.Reauth(ctx, err) {
		// A refresh rotates the skype token. Reuse the segment when the
		// failed attempt validated one (validation needs a 200, so a
		// later 401 is the token, not the partition); rediscover
		// otherwise — every candidate was rejected under the old token.
		events, _, err = c.attempt(ctx, segment, params)
	}
	if err != nil {
		return nil, err
	}
	return events, nil
}

// attempt performs one discovery+view-request cycle under the load
// budget, optionally reusing an already-validated segment (empty means
// discover). The validated segment is returned so the retry path can skip
// discovery.
func (c *Client) attempt(ctx context.Context, knownSegment string, params Query) ([]Event, string, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, attemptTimeout)
	defer cancel()

	segment := knownSegment
	if segment == "" {
		var err error
		segment, err = c.discoverPartition(attemptCtx)
		if err != nil {
			return nil, "", err
		}
	}

	events, err := c.request(attemptCtx, segment, params)
	if err != nil {
		return nil, segment, err
	}
	return events, segment, nil
}

// request performs one authenticated calendarView GET.
func (c *Client) request(ctx context.Context, segment string, params Query) ([]Event, error) {
	bearer, err := c.rootToken()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf(middleTierFormat, url.PathEscape(segment))
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	query := request.URL.Query()
	query.Set("startDate", formatTime(params.Start))
	query.Set("endDate", formatTime(params.End))
	query.Set("$filter", "isCancelled eq false")
	query.Set("$orderby", "startTime asc")
	limit := params.Limit
	if limit <= 0 || limit > MaxEvents {
		limit = MaxEvents
	}
	query.Set("$top", fmt.Sprint(limit))
	query.Set("$count", "true")
	query.Set("$select", selectFields)
	request.URL.RawQuery = query.Encode()

	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Origin", "https://teams.microsoft.com")
	request.Header.Set("Referer", "https://teams.microsoft.com/")

	client := c.http()
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, &ResponseError{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Body:       strings.TrimSpace(string(body)),
		}
	}

	var decoded viewResponse
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("unable to decode calendar events: %w", err)
	}

	events := decoded.Value
	for i := range events {
		events[i].Start = parseTime(events[i].StartTime)
		events[i].End = parseTime(events[i].EndTime)
	}
	return events, nil
}

// rootToken returns the raw login/root skype token from the runtime
// token files.
func (c *Client) rootToken() (string, error) {
	if c.RootToken != nil {
		return c.RootToken()
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

func (c *Client) http() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}

	return &http.Client{Timeout: requestTimeout}
}

// Normalize sorts events ascending (what the view assumes) and caps them at
// limit when positive.
func Normalize(events []Event, limit int) []Event {
	sortEvents(events)
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events
}

// formatTime renders a UTC time in the ISO-8601 shape the calendar
// view expects for its startDate/endDate bounds.
func formatTime(t time.Time) string {
	return t.UTC().Truncate(time.Second).Format("2006-01-02T15:04:05.000Z")
}

// parseTime parses a calendar timestamp, tolerating the offsets the
// service emits (e.g. "2026-11-05T16:30:00+00:00").
func parseTime(raw string) time.Time {
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

// sortEvents orders events ascending by start time, breaking ties on
// subject so the ordering is deterministic.
func sortEvents(events []Event) {
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

// partitionIsUsable rejects empty or obviously malformed segments
// (whitespace, path traversal) before they reach the probe.
func partitionIsUsable(segment string) bool {
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

// authzResponse is the subset of the authz body needed to derive the
// calendar host segment. The teams-api Authz helper decodes only the token,
// so the region/partition are read directly here.
type authzResponse struct {
	Region     string `json:"region"`
	Partition  string `json:"partition"`
	RegionGtms struct {
		MiddleTier string `json:"middleTier"`
	} `json:"regionGtms"`
}

// authzEndpoint is a var so tests keep partition discovery on the
// local server instead of the live authz service.
var authzEndpoint = "https://teams.microsoft.com/api/authsvc/v1.0/authz"

// defaultPartitionFromAuthz derives {region}-{partition} from the
// authz response (region "amer" + partition "amer01" -> "amer-01"). It is
// fenced against teams-api's panic-on-malformed-token behavior because it can
// run from a background goroutine.
func defaultPartitionFromAuthz(ctx context.Context, c *Client) (segment string, ok bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			segment, ok = "", false
		}
	}()

	bearer, err := c.rootToken()
	if err != nil || bearer == "" {
		return "", false
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, authzEndpoint, bytes.NewReader(nil))
	if err != nil {
		return "", false
	}
	request.Header.Set("ms-teams-authz-type", "TokenRefresh")
	request.Header.Set("Authorization", "Bearer "+bearer)

	response, err := c.http().Do(request)
	if err != nil {
		return "", false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", false
	}

	var authz authzResponse
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
	if !partitionIsUsable(derived) {
		return "", false
	}
	return derived, true
}
