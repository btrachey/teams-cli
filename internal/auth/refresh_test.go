package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	teamsapierrors "github.com/fossteams/teams-api/pkg/errors"
)

func TestIsAuthFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"status 401", statusError(401), true},
		{"status 403", statusError(403), true},
		{"status 500", statusError(500), false},
		{
			"wrapped library status error",
			fmt.Errorf("unable to get conversations: %w", teamsapierrors.NewHTTPError(200, 401, nil)),
			true,
		},
		{
			"library status text without wrap",
			fmt.Errorf("unable to get conversations: %v", teamsapierrors.NewHTTPError(200, 403, nil)),
			true,
		},
		{
			"swapped-args library status error",
			fmt.Errorf("unable to get tenants: %v", teamsapierrors.NewHTTPError(401, 200, nil)),
			true,
		},
		{
			"region probe rejection",
			errors.New("unable to determine message service region: amer rejected the skype token (403 Forbidden)"),
			true,
		},
		{"canceled", context.Canceled, false},
		{"network", errors.New("dial tcp: lookup failed"), false},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := IsAuthFailure(test.err); got != test.want {
				t.Fatalf("IsAuthFailure(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestTokenFilesNeedRefresh(t *testing.T) {
	dir := t.TempDir()
	fresh := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	expired := testJWT(t, map[string]any{"exp": float64(time.Now().Add(-time.Minute).Unix())})
	state := Refresher{TokenDir: dir}

	if state.NeedsRefresh(time.Now()) {
		t.Fatal("missing token files must not trigger refresh (fresh install)")
	}

	if err := os.WriteFile(TokenFilePath(dir, TokenTypeSkype), []byte(fresh), 0o600); err != nil {
		t.Fatalf("unable to write skype token: %v", err)
	}
	if !state.NeedsRefresh(time.Now()) {
		t.Fatal("missing chatsvcagg file must trigger refresh")
	}

	if err := os.WriteFile(TokenFilePath(dir, TokenTypeChatSvcAgg), []byte(fresh), 0o600); err != nil {
		t.Fatalf("unable to write chatsvcagg token: %v", err)
	}
	if state.NeedsRefresh(time.Now()) {
		t.Fatal("fresh token files must not trigger refresh")
	}

	if err := os.WriteFile(TokenFilePath(dir, TokenTypeChatSvcAgg), []byte(expired), 0o600); err != nil {
		t.Fatalf("unable to overwrite chatsvcagg token: %v", err)
	}
	if !state.NeedsRefresh(time.Now()) {
		t.Fatal("expired token file must trigger refresh")
	}

	near := testJWT(t, map[string]any{"exp": float64(time.Now().Add(60 * time.Second).Unix())})
	if err := os.WriteFile(TokenFilePath(dir, TokenTypeChatSvcAgg), []byte(near), 0o600); err != nil {
		t.Fatalf("unable to overwrite chatsvcagg token: %v", err)
	}
	if !state.NeedsRefresh(time.Now()) {
		t.Fatal("token expiring inside the grace window must trigger refresh")
	}
}

func TestRefreshExportsFreshTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	token := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	command := fmt.Sprintf(
		"printf %%s '%s' > '%s' && printf %%s '%s' > '%s'",
		token, TokenFilePath(dir, TokenTypeSkype),
		token, TokenFilePath(dir, TokenTypeChatSvcAgg),
	)

	t.Setenv(EnvName(TokenTypeSkype), "stale-skype")
	t.Setenv(EnvName(TokenTypeChatSvcAgg), "stale-chatsvcagg")

	state := Refresher{
		Enabled:  true,
		TokenDir: dir,
		Command:  command,
		Timeout:  20 * time.Second,
	}
	if err := state.Refresh(context.Background()); err != nil {
		t.Fatalf("unable to refresh tokens: %v", err)
	}

	if got := os.Getenv(EnvName(TokenTypeSkype)); got != token {
		t.Fatalf("expected exported skype token %q, got %q", token, got)
	}
	if got := os.Getenv(EnvName(TokenTypeChatSvcAgg)); got != token {
		t.Fatalf("expected exported chatsvcagg token %q, got %q", token, got)
	}
	if err := tokenFilesFresh(dir, time.Now()); err != nil {
		t.Fatalf("expected fresh token files: %v", err)
	}
}

func TestRefreshThrottlesRepeatAttempts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	marker := filepath.Join(dir, "attempts")
	command := fmt.Sprintf("printf x >> '%s'; echo boom >&2; exit 1", marker)

	state := Refresher{
		Enabled:  true,
		TokenDir: dir,
		Command:  command,
		Timeout:  20 * time.Second,
	}

	if err := state.Refresh(context.Background()); err == nil {
		t.Fatal("expected failing refresh command to error")
	}
	second := state.Refresh(context.Background())
	if second == nil || !strings.Contains(second.Error(), "waiting") {
		t.Fatalf("expected throttle error, got %v", second)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("unable to read attempts marker: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected one command attempt, got %d", len(data))
	}
}

func TestRefreshHonorsTimeout(t *testing.T) {
	dir := t.TempDir()
	state := Refresher{
		Enabled:  true,
		TokenDir: dir,
		Command:  "sleep 30",
		Timeout:  300 * time.Millisecond,
	}

	started := time.Now()
	err := state.Refresh(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("refresh command was not killed promptly: %s", elapsed)
	}
}

func TestExportTokenFilesToEnv(t *testing.T) {
	dir := t.TempDir()
	skype := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	chatsvc := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})

	if err := os.WriteFile(TokenFilePath(dir, TokenTypeSkype), []byte(skype), 0o600); err != nil {
		t.Fatalf("unable to write skype token: %v", err)
	}
	if err := os.WriteFile(TokenFilePath(dir, TokenTypeChatSvcAgg), []byte(chatsvc), 0o600); err != nil {
		t.Fatalf("unable to write chatsvcagg token: %v", err)
	}
	t.Setenv(EnvName(TokenTypeSkype), "")
	t.Setenv(EnvName(TokenTypeChatSvcAgg), "")

	if err := exportTokenFilesToEnv(dir); err != nil {
		t.Fatalf("unable to export tokens: %v", err)
	}
	if got := os.Getenv(EnvName(TokenTypeSkype)); got != skype {
		t.Fatalf("expected skype env %q, got %q", skype, got)
	}
	if got := os.Getenv(EnvName(TokenTypeChatSvcAgg)); got != chatsvc {
		t.Fatalf("expected chatsvcagg env %q, got %q", chatsvc, got)
	}
}

// statusError stands in for the typed HTTP errors of the teams and calendar
// clients.
type statusError int

func (e statusError) Error() string   { return fmt.Sprintf("status %d", int(e)) }
func (e statusError) HTTPStatus() int { return int(e) }
