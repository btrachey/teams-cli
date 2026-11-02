package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	teams_api "github.com/fossteams/teams-api"
	"github.com/sirupsen/logrus"
)

const (
	defaultTokenRefreshTimeout  = 3 * time.Minute
	tokenRefreshMinInterval     = time.Minute
	tokenRefreshFailureCooldown = 5 * time.Minute
	tokenRefreshExpiryGrace     = 2 * time.Minute
	tokenRefreshOutputTailBytes = 300

	teamsTokenCheckoutMissing = "unable to locate a usable teams-token checkout (no vendored teams-token/ directory, $TEAMS_TOKEN_DIR, or sibling checkout with installed dependencies); run yarn install in the vendored copy or set --token-refresh-command"
)

// tokenRefreshConfig controls how teams-cli recovers from expired teams-token
// JWT files. When enabled, teams-cli launches the teams-token Electron flow,
// which reuses the SSO cookies in its own profile to mint fresh token-*.jwt
// files without user interaction.
type tokenRefreshConfig struct {
	enabled bool
	command string
	timeout time.Duration
}

func (c tokenRefreshConfig) refreshTimeout() time.Duration {
	if c.timeout <= 0 {
		return defaultTokenRefreshTimeout
	}

	return c.timeout
}

// tokenRefreshInvocation describes one way to launch the teams-token flow.
type tokenRefreshInvocation struct {
	label  string
	dir    string
	binary string
	args   []string
	shell  string
}

func (i tokenRefreshInvocation) String() string {
	if i.shell != "" {
		return i.label + " (" + i.shell + ")"
	}

	parts := append([]string{i.binary}, i.args...)
	return i.label + " (" + strings.Join(parts, " ") + ")"
}

// isAuthFailure reports whether err looks like a rejected/expired Teams token
// rather than a transient transport error. The teams-api package wraps HTTP
// statuses into text-only GenericHTTPError values, so classification is
// partially textual.
func isAuthFailure(err error) bool {
	if err == nil {
		return false
	}

	var fetchErr *messageFetchStatusError
	if errors.As(err, &fetchErr) {
		return fetchErr.StatusCode == http.StatusUnauthorized || fetchErr.StatusCode == http.StatusForbidden
	}

	var sendErr *messageSendError
	if errors.As(err, &sendErr) {
		return sendErr.StatusCode == http.StatusUnauthorized || sendErr.StatusCode == http.StatusForbidden
	}

	message := err.Error()
	if strings.Contains(message, "invalid status code returned:") {
		// covers both argument orders the library uses ("expected but 401
		// returned" and the swapped skypespaces form) including %v-wrapped
		// copies where errors.As can no longer see the type.
		return strings.Contains(message, " 401 ") || strings.Contains(message, " 403 ")
	}
	if strings.Contains(message, "rejected the skype token") {
		return strings.Contains(message, "401") || strings.Contains(message, "403")
	}
	if strings.Contains(message, "invalid status code 401") || strings.Contains(message, "invalid status code 403") {
		return true
	}

	return false
}

// tokenFilesFresh verifies the runtime token files are present, parseable,
// and unexpired. The grace window makes tokens that are about to expire
// trigger a refresh instead of a failed request.
func tokenFilesFresh(tokenDir string, now time.Time) error {
	for _, tokenType := range runtimeTokenTypes {
		path := tokenFilePath(tokenDir, tokenType)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("unable to read %s: %w", path, err)
		}
		if !tokenValueFresh(string(raw), now) {
			return fmt.Errorf("%s is expired or unparseable", path)
		}
	}

	return nil
}

// tokenValueFresh reports whether a raw JWT value carries an expiry beyond
// the grace window.
func tokenValueFresh(raw string, now time.Time) bool {
	meta, err := parseJWTMetadata(strings.TrimSpace(raw))
	if err != nil || !meta.HasExpiry {
		return false
	}

	return now.Before(meta.ExpiresAt.Add(-tokenRefreshExpiryGrace))
}

// tokenFilesPresent reports whether any runtime token file exists. A totally
// empty token directory is a fresh install: it needs the interactive
// teams-token login, so it must not trigger an automatic refresh.
func tokenFilesPresent(tokenDir string) bool {
	for _, tokenType := range runtimeTokenTypes {
		if _, err := os.Stat(tokenFilePath(tokenDir, tokenType)); err == nil {
			return true
		}
	}

	return false
}

func (s *AppState) tokenFileDir() (string, error) {
	if dir := strings.TrimSpace(s.tokenDir); dir != "" {
		return dir, nil
	}

	return defaultTokenDir()
}

// tokenFilesNeedRefresh reports whether the stored token files are stale:
// at least one runtime token file exists but the set is not fresh (expired,
// expiring inside the grace window, unparseable, or partially missing).
func (s *AppState) tokenFilesNeedRefresh(now time.Time) bool {
	dir, err := s.tokenFileDir()
	if err != nil {
		return false
	}
	if !tokenFilesPresent(dir) {
		return false
	}

	return tokenFilesFresh(dir, now) != nil
}

// teamsTokenRepoCandidates lists directories that may hold a teams-token
// checkout, in priority order: explicit env, the copy vendored into this
// repository (cwd and executable siblings), then standalone checkouts next
// to the repository.
func teamsTokenRepoCandidates() []string {
	var wd, exeDir string
	if dir, err := os.Getwd(); err == nil {
		wd = dir
	}
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}

	return teamsTokenRepoCandidatesFrom(wd, exeDir)
}

// teamsTokenRepoCandidatesFrom builds the search list; the two base
// directories (working dir, executable dir) are injectable so tests are not
// polluted by the repository's own vendored teams-token copy.
func teamsTokenRepoCandidatesFrom(wd, exeDir string) []string {
	var candidates []string
	if dir := strings.TrimSpace(os.Getenv("TEAMS_TOKEN_DIR")); dir != "" {
		candidates = append(candidates, dir)
	}
	if wd != "" {
		candidates = append(candidates,
			filepath.Join(wd, "teams-token"),
			filepath.Join(wd, "..", "teams-cli", "teams-token"),
			filepath.Join(wd, "..", "teams-token"),
		)
	}
	if exeDir != "" {
		candidates = append(candidates,
			filepath.Join(exeDir, "teams-token"),
			filepath.Join(exeDir, "..", "teams-cli", "teams-token"),
			filepath.Join(exeDir, "..", "teams-token"),
		)
	}

	return candidates
}

// discoverTeamsTokenInvocation finds the teams-token checkout and returns the
// cheapest way to run its saved flow: the bundled Electron binary straight
// against the compiled dist/main.js, falling back to `yarn start` (which
// compiles first) when the checkout has not been built yet.
func discoverTeamsTokenInvocation() (tokenRefreshInvocation, error) {
	return discoverTeamsTokenInvocationFrom(teamsTokenRepoCandidates())
}

func discoverTeamsTokenInvocationFrom(candidates []string) (tokenRefreshInvocation, error) {
	for _, candidate := range candidates {
		if invocation, ok := invocationForTeamsTokenDir(candidate); ok {
			return invocation, nil
		}
	}

	return tokenRefreshInvocation{}, errors.New(teamsTokenCheckoutMissing)
}

func invocationForTeamsTokenDir(dir string) (tokenRefreshInvocation, bool) {
	return invocationForTeamsTokenDirWithYarn(dir, "yarn")
}

func invocationForTeamsTokenDirWithYarn(dir, yarnName string) (tokenRefreshInvocation, bool) {
	if _, err := os.Stat(filepath.Join(dir, "package.json")); err != nil {
		return tokenRefreshInvocation{}, false
	}

	mainPath := filepath.Join(dir, "dist", "main.js")
	if info, err := os.Stat(mainPath); err == nil && !info.IsDir() {
		if electron := teamsTokenElectronBinary(dir); electron != "" {
			return tokenRefreshInvocation{label: "electron", dir: dir, binary: electron, args: []string{mainPath}}, true
		}
	}

	// `yarn start` compiles with the checkout's own tsc, so it only works
	// once dependencies are installed; without node_modules the checkout is
	// not usable and discovery moves on (the error names the fix).
	if info, err := os.Stat(filepath.Join(dir, "node_modules")); err == nil && info.IsDir() {
		if yarn, err := exec.LookPath(yarnName); err == nil {
			return tokenRefreshInvocation{label: "yarn start", dir: dir, binary: yarn, args: []string{"start"}}, true
		}
	}

	return tokenRefreshInvocation{}, false
}

// teamsTokenElectronBinary resolves node_modules/electron/path.txt the way
// Electron's own bin wrapper does.
func teamsTokenElectronBinary(repoDir string) string {
	base := filepath.Join(repoDir, "node_modules", "electron")
	rel, err := os.ReadFile(filepath.Join(base, "path.txt"))
	if err != nil {
		return ""
	}

	candidate := filepath.Join(base, "dist", strings.TrimSpace(string(rel)))
	if info, err := os.Stat(candidate); err != nil || info.IsDir() {
		return ""
	}

	return candidate
}

// refreshTokensWithTeamsToken launches the teams-token flow, waits for it to
// rewrite the JWT files, and exports the fresh values so rebuilt clients pick
// them up. A cooldown keeps a broken/expired SSO session from re-launching
// Electron on every failed poll.
func (s *AppState) refreshTokensWithTeamsToken(ctx context.Context) error {
	if !s.tokenRefresh.enabled {
		return fmt.Errorf("automatic token refresh is disabled")
	}

	if s.refreshTokensOverride != nil {
		return s.refreshTokensOverride(ctx)
	}

	s.tokenRefreshMu.Lock()
	defer s.tokenRefreshMu.Unlock()

	if !s.lastTokenRefreshAt.IsZero() && time.Since(s.lastTokenRefreshAt) < tokenRefreshMinInterval {
		return fmt.Errorf("token refresh last attempted %s ago; waiting %s between attempts",
			time.Since(s.lastTokenRefreshAt).Round(time.Second), tokenRefreshMinInterval)
	}
	if !s.lastTokenRefreshFailedAt.IsZero() && time.Since(s.lastTokenRefreshFailedAt) < tokenRefreshFailureCooldown {
		return fmt.Errorf("token refresh last failed %s ago; waiting for the %s failure cooldown",
			time.Since(s.lastTokenRefreshFailedAt).Round(time.Second), tokenRefreshFailureCooldown)
	}

	invocation, err := s.tokenRefreshInvocation(ctx)
	if err != nil {
		return err
	}

	runCtx, cancel := context.WithTimeout(ctx, s.tokenRefresh.refreshTimeout())
	defer cancel()

	output := &lockedBuffer{}
	cmd, err := newTokenRefreshCommand(runCtx, invocation)
	if err != nil {
		return err
	}
	cmd.Stdout = output
	cmd.Stderr = output

	tokenDir, err := s.tokenFileDir()
	if err != nil {
		return err
	}

	cmd.Env = append(os.Environ(), "FOSSTEAMS_TOKEN_DIR="+tokenDir)

	s.appLogger().WithFields(logrus.Fields{
		"command":   invocation.String(),
		"label":     invocation.label,
		"timeout":   s.tokenRefresh.refreshTimeout().String(),
		"token_dir": tokenDir,
	}).Info("refreshing Teams tokens via teams-token")

	s.lastTokenRefreshAt = time.Now()
	if err := cmd.Run(); err != nil {
		s.lastTokenRefreshFailedAt = time.Now()
		return fmt.Errorf("teams-token refresh failed: %w (%s)", err, output.Tail(tokenRefreshOutputTailBytes))
	}
	if err := syncFreshTokens(tokenDir); err != nil {
		s.lastTokenRefreshFailedAt = time.Now()
		return err
	}
	if err := tokenFilesFresh(tokenDir, time.Now()); err != nil {
		s.lastTokenRefreshFailedAt = time.Now()
		return fmt.Errorf("teams-token did not produce usable tokens: %w (output: %s)", err, output.Tail(tokenRefreshOutputTailBytes))
	}
	if err := exportTokenFilesToEnv(tokenDir); err != nil {
		s.lastTokenRefreshFailedAt = time.Now()
		return err
	}

	s.appLogger().WithField("token_dir", tokenDir).Info("Teams tokens refreshed successfully")
	return nil
}

// syncFreshTokens mirrors token files from teams-token's historical output
// directory (~/.config/fossteams) into the directory this app reads. The
// vendored flow writes the effective directory directly (teams-cli passes
// FOSSTEAMS_TOKEN_DIR), so this only matters for unmodified external
// checkouts pointed at via TEAMS_TOKEN_DIR or --token-refresh-command. Only
// files that are themselves fresh are copied, and a target file that is
// already fresh is left alone.
func syncFreshTokens(targetDir string) error {
	sourceDir, err := teamsTokenDefaultOutputDir()
	if err != nil {
		return nil
	}
	if filepath.Clean(sourceDir) == filepath.Clean(targetDir) {
		return nil
	}
	if err := os.MkdirAll(targetDir, 0o700); err != nil {
		return fmt.Errorf("unable to create token directory %s: %w", targetDir, err)
	}

	now := time.Now()
	for _, tokenType := range append(append([]string{}, runtimeTokenTypes...), optionalTokenTypes...) {
		target := tokenFilePath(targetDir, tokenType)
		if raw, err := os.ReadFile(target); err == nil && tokenValueFresh(string(raw), now) {
			continue
		}

		source := tokenFilePath(sourceDir, tokenType)
		raw, err := os.ReadFile(source)
		if err != nil || !tokenValueFresh(string(raw), now) {
			continue
		}
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			return fmt.Errorf("unable to mirror %s into %s: %w", source, targetDir, err)
		}
	}

	return nil
}

// teamsTokenDefaultOutputDir is the literal upstream default directory
// (never the FOSSTEAMS_TOKEN_DIR override): what unmodified teams-token
// checkouts always write.
func teamsTokenDefaultOutputDir() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot retrieve user home directory")
	}

	return filepath.Join(homeDir, ".config", "fossteams"), nil
}

func (s *AppState) tokenRefreshInvocation(ctx context.Context) (tokenRefreshInvocation, error) {
	if command := strings.TrimSpace(s.tokenRefresh.command); command != "" {
		// Run the user command verbatim through the shell so chained
		// commands (`a && b`) work; the process group kill covers children.
		return tokenRefreshInvocation{label: "command", shell: command}, nil
	}

	invocation, err := discoverTeamsTokenInvocation()
	if err == nil {
		return invocation, nil
	}

	// No checkout anywhere: fall back to the flow embedded in this binary,
	// bootstrapping the pinned Electron runtime into the user cache.
	embedded, embeddedErr := ensureEmbeddedTokenFlow(ctx, s.appLogger())
	if embeddedErr != nil {
		return tokenRefreshInvocation{}, fmt.Errorf("%w (embedded flow: %v)", err, embeddedErr)
	}

	return embedded, nil
}

func newTokenRefreshCommand(ctx context.Context, invocation tokenRefreshInvocation) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	if invocation.shell != "" {
		// The shell wrapper only ever runs the user-configured refresh command.
		cmd = exec.CommandContext(ctx, "/bin/sh", "-c", invocation.shell)
	} else {
		if invocation.binary == "" {
			return nil, fmt.Errorf("token refresh invocation is empty")
		}
		cmd = exec.CommandContext(ctx, invocation.binary, invocation.args...)
	}

	if invocation.dir != "" {
		cmd.Dir = invocation.dir
	}

	// Run in its own process group so killing the wrapper also kills the
	// Electron tree it spawns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	return cmd, nil
}

func exportTokenFilesToEnv(tokenDir string) error {
	for _, tokenType := range runtimeTokenTypes {
		path := tokenFilePath(tokenDir, tokenType)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("unable to read refreshed token from %s: %w", path, err)
		}

		value := strings.TrimSpace(string(raw))
		if value == "" {
			return fmt.Errorf("refreshed token %s is empty", path)
		}
		if err := os.Setenv(tokenEnvName(tokenType), value); err != nil {
			return fmt.Errorf("unable to export %s: %w", tokenEnvName(tokenType), err)
		}
	}

	return nil
}

// lockedBuffer collects command output from the stdout/stderr copy goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Tail(max int) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	text := strings.TrimSpace(b.buf.String())
	if max > 0 && len(text) > max {
		text = "..." + text[len(text)-max:]
	}

	return text
}

// maybeRefreshTokens performs one refresh-and-rebuild cycle. A nil cause
// forces the attempt (proactive pre-flight refresh); otherwise only
// authentication failures qualify. Concurrent callers serialize on the mutex
// inside refreshTokensWithTeamsToken.
func (s *AppState) maybeRefreshTokens(ctx context.Context, cause error) bool {
	if !s.tokenRefresh.enabled {
		return false
	}
	if cause != nil && !isAuthFailure(cause) {
		return false
	}

	if err := s.refreshTokensWithTeamsToken(ctx); err != nil {
		s.appLogger().WithError(err).Warn("teams-token refresh did not restore access")
		return false
	}
	if err := s.rebuildTeamsClient(); err != nil {
		s.appLogger().WithError(err).Warn("unable to rebuild teams client after token refresh")
		return false
	}

	return true
}

// newTeamsClient constructs the teams-api client, honoring the test
// override. teams-api panics on malformed token files (nil JWT inside the
// authz exchange), so the call is fenced into an error.
func (s *AppState) newTeamsClient() (client *teams_api.TeamsClient, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			client = nil
			err = fmt.Errorf("teams client construction failed: %v", recovered)
		}
	}()

	if s.newTeamsClientOverride != nil {
		return s.newTeamsClientOverride()
	}

	return teams_api.New()
}

func (s *AppState) rebuildTeamsClient() error {
	client, err := s.newTeamsClient()
	if err != nil {
		return err
	}

	s.setTeamsClient(client)

	// The cached region was probed with the previous token; re-probe it.
	s.regionMu.Lock()
	s.messageRegion = ""
	s.regionMu.Unlock()

	s.appLogger().Info("teams client rebuilt with refreshed tokens")
	return nil
}
