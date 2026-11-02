package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	teams_api "github.com/fossteams/teams-api"
	"github.com/fossteams/teams-api/pkg/csa"
	teamsapierrors "github.com/fossteams/teams-api/pkg/errors"
)

func TestIsAuthFailureClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"fetch 401", &messageFetchStatusError{StatusCode: 401}, true},
		{"fetch 403", &messageFetchStatusError{StatusCode: 403}, true},
		{"fetch 500", &messageFetchStatusError{StatusCode: 500}, false},
		{"send 401", &messageSendError{StatusCode: 401}, true},
		{"send 404", &messageSendError{StatusCode: 404}, false},
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
			if got := isAuthFailure(test.err); got != test.want {
				t.Fatalf("isAuthFailure(%v) = %t, want %t", test.err, got, test.want)
			}
		})
	}
}

func TestTokenFilesNeedRefresh(t *testing.T) {
	dir := t.TempDir()
	fresh := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	expired := testJWT(t, map[string]any{"exp": float64(time.Now().Add(-time.Minute).Unix())})
	state := AppState{tokenDir: dir}

	if state.tokenFilesNeedRefresh(time.Now()) {
		t.Fatal("missing token files must not trigger refresh (fresh install)")
	}

	if err := os.WriteFile(tokenFilePath(dir, tokenTypeSkype), []byte(fresh), 0o600); err != nil {
		t.Fatalf("unable to write skype token: %v", err)
	}
	if !state.tokenFilesNeedRefresh(time.Now()) {
		t.Fatal("missing chatsvcagg file must trigger refresh")
	}

	if err := os.WriteFile(tokenFilePath(dir, tokenTypeChatSvcAgg), []byte(fresh), 0o600); err != nil {
		t.Fatalf("unable to write chatsvcagg token: %v", err)
	}
	if state.tokenFilesNeedRefresh(time.Now()) {
		t.Fatal("fresh token files must not trigger refresh")
	}

	if err := os.WriteFile(tokenFilePath(dir, tokenTypeChatSvcAgg), []byte(expired), 0o600); err != nil {
		t.Fatalf("unable to overwrite chatsvcagg token: %v", err)
	}
	if !state.tokenFilesNeedRefresh(time.Now()) {
		t.Fatal("expired token file must trigger refresh")
	}

	near := testJWT(t, map[string]any{"exp": float64(time.Now().Add(60 * time.Second).Unix())})
	if err := os.WriteFile(tokenFilePath(dir, tokenTypeChatSvcAgg), []byte(near), 0o600); err != nil {
		t.Fatalf("unable to overwrite chatsvcagg token: %v", err)
	}
	if !state.tokenFilesNeedRefresh(time.Now()) {
		t.Fatal("token expiring inside the grace window must trigger refresh")
	}
}

func TestInvocationForTeamsTokenDirPrefersElectron(t *testing.T) {
	dir := t.TempDir()

	mainPath := filepath.Join(dir, "dist", "main.js")
	if err := os.MkdirAll(filepath.Dir(mainPath), 0o755); err != nil {
		t.Fatalf("unable to create dist: %v", err)
	}
	if err := os.WriteFile(mainPath, []byte("// built"), 0o644); err != nil {
		t.Fatalf("unable to write main.js: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("unable to write package.json: %v", err)
	}

	electronRel := filepath.Join("Electron.app", "Contents", "MacOS", "Electron")
	electronBase := filepath.Join(dir, "node_modules", "electron")
	if err := os.MkdirAll(filepath.Join(electronBase, "dist", filepath.Dir(electronRel)), 0o755); err != nil {
		t.Fatalf("unable to create electron dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(electronBase, "path.txt"), []byte(electronRel), 0o644); err != nil {
		t.Fatalf("unable to write path.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(electronBase, "dist", electronRel), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("unable to write electron binary: %v", err)
	}

	invocation, ok := invocationForTeamsTokenDir(dir)
	if !ok {
		t.Fatal("expected an invocation for a built checkout")
	}
	if invocation.label != "electron" {
		t.Fatalf("expected electron invocation, got %q", invocation.label)
	}
	if !strings.HasSuffix(invocation.binary, "Electron") {
		t.Fatalf("expected Electron binary, got %q", invocation.binary)
	}
	if len(invocation.args) != 1 || invocation.args[0] != mainPath {
		t.Fatalf("expected args [%s], got %v", mainPath, invocation.args)
	}
}

func TestInvocationForTeamsTokenDirFallsBackToYarn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("unable to write package.json: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatalf("unable to create node_modules: %v", err)
	}

	// No dist/main.js and no electron binary: yarn start (which builds) wins.
	invocation, ok := invocationForTeamsTokenDirWithYarn(dir, "sh")
	if !ok {
		t.Fatal("expected a yarn fallback invocation")
	}
	if invocation.label != "yarn start" {
		t.Fatalf("expected yarn fallback, got %q", invocation.label)
	}
	if len(invocation.args) != 1 || invocation.args[0] != "start" {
		t.Fatalf("expected args [start], got %v", invocation.args)
	}
	if invocation.dir != dir {
		t.Fatalf("expected working dir %s, got %s", dir, invocation.dir)
	}

	if _, ok := invocationForTeamsTokenDirWithYarn(dir, "definitely-not-a-real-binary"); ok {
		t.Fatal("expected no invocation without electron or yarn")
	}

	noDeps := t.TempDir()
	if err := os.WriteFile(filepath.Join(noDeps, "package.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("unable to write package.json: %v", err)
	}
	if _, ok := invocationForTeamsTokenDirWithYarn(noDeps, "sh"); ok {
		t.Fatal("expected a checkout without node_modules to be rejected")
	}
	if _, ok := invocationForTeamsTokenDir(filepath.Join(dir, "missing")); ok {
		t.Fatal("expected no invocation for a missing directory")
	}
}

func TestTeamsTokenRepoCandidatesPrefersVendoredCopy(t *testing.T) {
	t.Setenv("TEAMS_TOKEN_DIR", "/env/dir")

	candidates := teamsTokenRepoCandidatesFrom("/wd", "/exe")
	want := []string{
		"/env/dir",
		filepath.Join("/wd", "teams-token"),
		filepath.Join("/wd", "..", "teams-cli", "teams-token"),
		filepath.Join("/wd", "..", "teams-token"),
		filepath.Join("/exe", "teams-token"),
		filepath.Join("/exe", "..", "teams-cli", "teams-token"),
		filepath.Join("/exe", "..", "teams-token"),
	}
	if len(candidates) != len(want) {
		t.Fatalf("expected %d candidates, got %v", len(want), candidates)
	}
	for idx := range want {
		if candidates[idx] != want[idx] {
			t.Fatalf("candidate %d: expected %q, got %q", idx, want[idx], candidates[idx])
		}
	}
}

func TestDiscoverTeamsTokenInvocationNotFound(t *testing.T) {
	if _, err := discoverTeamsTokenInvocationFrom(nil); err == nil {
		t.Fatal("expected discovery to fail with no teams-token checkout")
	} else if !strings.Contains(err.Error(), "teams-token") {
		t.Fatalf("expected helpful error, got %v", err)
	}
}

func TestRefreshTokensWithTeamsTokenExportsFreshTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	token := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	command := fmt.Sprintf(
		"printf %%s '%s' > '%s' && printf %%s '%s' > '%s'",
		token, tokenFilePath(dir, tokenTypeSkype),
		token, tokenFilePath(dir, tokenTypeChatSvcAgg),
	)

	t.Setenv(tokenEnvName(tokenTypeSkype), "stale-skype")
	t.Setenv(tokenEnvName(tokenTypeChatSvcAgg), "stale-chatsvcagg")

	state := AppState{
		tokenDir: dir,
		tokenRefresh: tokenRefreshConfig{
			enabled: true,
			command: command,
			timeout: 20 * time.Second,
		},
	}
	if err := state.refreshTokensWithTeamsToken(context.Background()); err != nil {
		t.Fatalf("unable to refresh tokens: %v", err)
	}

	if got := os.Getenv(tokenEnvName(tokenTypeSkype)); got != token {
		t.Fatalf("expected exported skype token %q, got %q", token, got)
	}
	if got := os.Getenv(tokenEnvName(tokenTypeChatSvcAgg)); got != token {
		t.Fatalf("expected exported chatsvcagg token %q, got %q", token, got)
	}
	if err := tokenFilesFresh(dir, time.Now()); err != nil {
		t.Fatalf("expected fresh token files: %v", err)
	}
}

func TestSyncFreshTokensMirrorsDefaultDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	source := filepath.Join(home, ".config", "fossteams")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatalf("unable to create default token dir: %v", err)
	}
	target := t.TempDir()

	fresh := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	stale := testJWT(t, map[string]any{"exp": float64(time.Now().Add(-time.Hour).Unix())})

	if err := os.WriteFile(tokenFilePath(source, tokenTypeSkype), []byte(fresh), 0o600); err != nil {
		t.Fatalf("unable to write source skype token: %v", err)
	}
	if err := os.WriteFile(tokenFilePath(source, tokenTypeChatSvcAgg), []byte(stale), 0o600); err != nil {
		t.Fatalf("unable to write source chatsvcagg token: %v", err)
	}

	if err := syncFreshTokens(target); err != nil {
		t.Fatalf("unable to sync tokens: %v", err)
	}
	if raw, err := os.ReadFile(tokenFilePath(target, tokenTypeSkype)); err != nil || string(raw) != fresh {
		t.Fatalf("expected fresh skype token mirrored, got %q (err %v)", raw, err)
	}
	if _, err := os.Stat(tokenFilePath(target, tokenTypeChatSvcAgg)); !os.IsNotExist(err) {
		t.Fatal("stale source token must not be copied")
	}

	// A newer value already sitting in the target must not be overwritten.
	newer := testJWT(t, map[string]any{"exp": float64(time.Now().Add(2 * time.Hour).Unix())})
	if err := os.WriteFile(tokenFilePath(target, tokenTypeSkype), []byte(newer), 0o600); err != nil {
		t.Fatalf("unable to write target token: %v", err)
	}
	if err := syncFreshTokens(target); err != nil {
		t.Fatalf("unable to re-sync tokens: %v", err)
	}
	if raw, err := os.ReadFile(tokenFilePath(target, tokenTypeSkype)); err != nil || string(raw) != newer {
		t.Fatalf("expected target token to be kept, got %q (err %v)", raw, err)
	}

	if err := syncFreshTokens(source); err != nil {
		t.Fatalf("same-dir sync must be a no-op, got %v", err)
	}
}

func TestRefreshTokensThrottlesRepeatAttempts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	marker := filepath.Join(dir, "attempts")
	command := fmt.Sprintf("printf x >> '%s'; echo boom >&2; exit 1", marker)

	state := AppState{
		tokenDir: dir,
		tokenRefresh: tokenRefreshConfig{
			enabled: true,
			command: command,
			timeout: 20 * time.Second,
		},
	}

	if err := state.refreshTokensWithTeamsToken(context.Background()); err == nil {
		t.Fatal("expected failing refresh command to error")
	}
	second := state.refreshTokensWithTeamsToken(context.Background())
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

func TestRefreshTokensHonorsTimeout(t *testing.T) {
	dir := t.TempDir()
	state := AppState{
		tokenDir: dir,
		tokenRefresh: tokenRefreshConfig{
			enabled: true,
			command: "sleep 30",
			timeout: 300 * time.Millisecond,
		},
	}

	started := time.Now()
	err := state.refreshTokensWithTeamsToken(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("refresh command was not killed promptly: %s", elapsed)
	}
}

func TestMaybeRefreshTokensRebuildsOnAuthFailure(t *testing.T) {
	original := newTestTeamsClient(t)
	rebuilt := newTestTeamsClient(t)

	state := AppState{
		tokenRefresh: tokenRefreshConfig{enabled: true},
	}
	state.setTeamsClient(original)
	state.refreshTokensOverride = func(context.Context) error { return nil }
	state.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) { return rebuilt, nil }

	if !state.maybeRefreshTokens(context.Background(), &messageFetchStatusError{StatusCode: 401}) {
		t.Fatal("expected refresh on 401")
	}
	if state.client() != rebuilt {
		t.Fatal("expected teams client to be rebuilt")
	}

	state2 := AppState{tokenRefresh: tokenRefreshConfig{enabled: true}}
	state2.setTeamsClient(original)
	state2.refreshTokensOverride = func(context.Context) error { return nil }
	state2.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) { return rebuilt, nil }
	if state2.maybeRefreshTokens(context.Background(), &messageFetchStatusError{StatusCode: 503}) {
		t.Fatal("expected no refresh on non-auth failure")
	}
	if state2.client() != original {
		t.Fatal("expected teams client to stay in place")
	}

	state3 := AppState{tokenRefresh: tokenRefreshConfig{enabled: false}}
	if state3.maybeRefreshTokens(context.Background(), nil) {
		t.Fatal("expected disabled refresh to be a no-op")
	}
}

func TestFetchMessagesRefreshesTokensAndRetriesOn401(t *testing.T) {
	var calls int32
	var refreshes int32

	state := AppState{
		messageLimit: 3,
		tokenRefresh: tokenRefreshConfig{enabled: true},
		httpClient: &http.Client{
			Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				if atomic.AddInt32(&calls, 1) == 1 {
					return testHTTPResponse(401, "token expired"), nil
				}
				messages := []csa.ChatMessage{testChatMessage("1", "author", "hello", time.Now())}
				return testHTTPResponse(200, testMessagesJSON(t, messages)), nil
			}),
		},
	}
	state.setTeamsClient(newTestTeamsClient(t))
	state.refreshTokensOverride = func(context.Context) error {
		atomic.AddInt32(&refreshes, 1)
		return nil
	}
	state.newTeamsClientOverride = func() (*teams_api.TeamsClient, error) {
		return newTestTeamsClient(t), nil
	}

	messages, err := state.fetchMessages(context.Background(), ConversationTarget{ID: "19:chat-1"})
	if err != nil {
		t.Fatalf("expected fetch to succeed after refresh: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if got := atomic.LoadInt32(&refreshes); got != 1 {
		t.Fatalf("expected exactly one refresh, got %d", got)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected exactly two HTTP attempts, got %d", got)
	}
}

func TestFetchMessagesDoesNotRefreshOnTransientErrors(t *testing.T) {
	var calls int32
	var refreshes int32

	state := AppState{
		messageLimit: 3,
		tokenRefresh: tokenRefreshConfig{enabled: true},
		httpClient: &http.Client{
			Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				atomic.AddInt32(&calls, 1)
				return testHTTPResponse(503, "unavailable"), nil
			}),
		},
	}
	state.setTeamsClient(newTestTeamsClient(t))
	state.refreshTokensOverride = func(context.Context) error {
		atomic.AddInt32(&refreshes, 1)
		return nil
	}

	if _, err := state.fetchMessages(context.Background(), ConversationTarget{ID: "19:chat-1"}); err == nil {
		t.Fatal("expected fetch to fail")
	}
	if got := atomic.LoadInt32(&refreshes); got != 0 {
		t.Fatalf("expected no refresh on 503, got %d", got)
	}
	if got := atomic.LoadInt32(&calls); got != int32(messageFetchMaxAttempts) {
		t.Fatalf("expected %d HTTP attempts, got %d", messageFetchMaxAttempts, got)
	}
}

func TestDoctorTokenRefreshCheckStates(t *testing.T) {
	disabled := doctorTokenRefreshCheck(AppOptions{AutoRefresh: false})
	if disabled.Status != doctorWarn || !strings.Contains(disabled.Details, "disabled") {
		t.Fatalf("expected disabled warning, got %+v", disabled)
	}

	custom := doctorTokenRefreshCheck(AppOptions{
		AutoRefresh:         true,
		TokenRefreshCommand: "my-token-tool",
		TokenRefreshTimeout: time.Minute,
	})
	if custom.Status != doctorPass || !strings.Contains(custom.Details, "my-token-tool") {
		t.Fatalf("expected custom command pass, got %+v", custom)
	}
}

func TestTeamsTokenElectronBinaryResolvesPathTxt(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "node_modules", "electron")
	if err := os.MkdirAll(filepath.Join(base, "dist"), 0o755); err != nil {
		t.Fatalf("unable to create dirs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(base, "path.txt"), []byte("electron\n"), 0o644); err != nil {
		t.Fatalf("unable to write path.txt: %v", err)
	}

	if got := teamsTokenElectronBinary(dir); got != "" {
		t.Fatalf("expected empty result for missing binary, got %q", got)
	}

	binary := filepath.Join(base, "dist", "electron")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("unable to write binary: %v", err)
	}
	if got := teamsTokenElectronBinary(dir); got != binary {
		t.Fatalf("expected %s, got %s", binary, got)
	}
}

func TestExportTokenFilesToEnv(t *testing.T) {
	dir := t.TempDir()
	skype := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	chatsvc := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})

	if err := os.WriteFile(tokenFilePath(dir, tokenTypeSkype), []byte(skype), 0o600); err != nil {
		t.Fatalf("unable to write skype token: %v", err)
	}
	if err := os.WriteFile(tokenFilePath(dir, tokenTypeChatSvcAgg), []byte(chatsvc), 0o600); err != nil {
		t.Fatalf("unable to write chatsvcagg token: %v", err)
	}
	t.Setenv(tokenEnvName(tokenTypeSkype), "")
	t.Setenv(tokenEnvName(tokenTypeChatSvcAgg), "")

	if err := exportTokenFilesToEnv(dir); err != nil {
		t.Fatalf("unable to export tokens: %v", err)
	}
	if got := os.Getenv(tokenEnvName(tokenTypeSkype)); got != skype {
		t.Fatalf("expected skype env %q, got %q", skype, got)
	}
	if got := os.Getenv(tokenEnvName(tokenTypeChatSvcAgg)); got != chatsvc {
		t.Fatalf("expected chatsvcagg env %q, got %q", chatsvc, got)
	}
}

func TestNewTeamsClientRecoversFromMalformedTokenPanic(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv(tokenEnvName(tokenTypeSkype), "not-a-jwt")
	t.Setenv(tokenEnvName(tokenTypeChatSvcAgg), "also-not-a-jwt")

	// teams-api dereferences a nil JWT inside the authz exchange for
	// malformed tokens; newTeamsClient must surface that as an error.
	state := AppState{}
	client, err := state.newTeamsClient()
	if err == nil {
		t.Fatalf("expected an error from malformed token input, got client %v", client)
	}
	if client != nil {
		t.Fatal("expected nil client alongside the error")
	}
}

func TestRebuildTeamsClientClearsCachedRegion(t *testing.T) {
	state := AppState{newTeamsClientOverride: func() (*teams_api.TeamsClient, error) {
		return newTestTeamsClient(t), nil
	}}
	state.messageRegion = "emea"

	if err := state.rebuildTeamsClient(); err != nil {
		t.Fatalf("unable to rebuild client: %v", err)
	}
	if state.messageRegion != "" {
		t.Fatalf("expected cached region to be cleared, got %q", state.messageRegion)
	}
}

func TestParseAppOptionsTokenRefreshFlags(t *testing.T) {
	options, err := parseAppOptions([]string{"--no-auto-refresh"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if options.AutoRefresh {
		t.Fatal("expected --no-auto-refresh to disable automatic refresh")
	}

	options, err = parseAppOptions([]string{"--token-refresh-command", "my-tool --renew", "--token-refresh-timeout", "90"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if options.TokenRefreshCommand != "my-tool --renew" {
		t.Fatalf("expected custom refresh command to be stored, got %q", options.TokenRefreshCommand)
	}
	if options.TokenRefreshTimeout != 90*time.Second {
		t.Fatalf("expected 90s refresh timeout, got %s", options.TokenRefreshTimeout)
	}

	options, err = parseAppOptions([]string{"--token-refresh-command=my-tool", "--token-refresh-timeout=45s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if options.TokenRefreshCommand != "my-tool" || options.TokenRefreshTimeout != 45*time.Second {
		t.Fatalf("equals forms not parsed: %+v", options)
	}

	if _, err := parseAppOptions([]string{"--token-refresh-command", " "}); err == nil {
		t.Fatal("expected empty refresh command to be rejected")
	}
	if _, err := parseAppOptions([]string{"--token-refresh-timeout", "0"}); err == nil {
		t.Fatal("expected non-positive refresh timeout to be rejected")
	}
	if _, err := parseAppOptions([]string{"--token-refresh-command"}); err == nil {
		t.Fatal("expected missing refresh command value to be rejected")
	}
	if _, err := parseAppOptions([]string{"--token-refresh-timeout"}); err == nil {
		t.Fatal("expected missing refresh timeout value to be rejected")
	}
}
