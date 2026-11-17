package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/sirupsen/logrus"
)

const (
	DefaultRefreshTimeout       = 3 * time.Minute
	tokenRefreshMinInterval     = time.Minute
	tokenRefreshFailureCooldown = 5 * time.Minute
	tokenRefreshExpiryGrace     = 2 * time.Minute
	tokenRefreshOutputTailBytes = 1200
)

// Refresher recovers from expired token files by re-running the sign-in
// flow, which reuses the SSO cookies in its Electron profile to mint fresh
// token-*.jwt files without user interaction.
type Refresher struct {
	Enabled bool
	// Command replaces the embedded flow when set.
	Command string
	// Timeout bounds one refresh; DefaultRefreshTimeout when zero.
	Timeout time.Duration
	// TokenDir is where the flow writes tokens; DefaultTokenDir when empty.
	TokenDir string
	Logger   *logrus.Logger

	mu           sync.Mutex
	lastAt       time.Time
	lastFailedAt time.Time
}

func (r *Refresher) timeout() time.Duration {
	if r.Timeout <= 0 {
		return DefaultRefreshTimeout
	}

	return r.Timeout
}

func (r *Refresher) logger() *logrus.Logger {
	if r.Logger != nil {
		return r.Logger
	}

	return logging.Discard
}

// invocation describes how to launch the sign-in flow: the
// embedded flow on the pinned Electron runtime, or a user-configured shell
// command.
type invocation struct {
	label  string
	dir    string
	binary string
	args   []string
	shell  string
}

func (i invocation) String() string {
	if i.shell != "" {
		return i.label + " (" + i.shell + ")"
	}

	parts := append([]string{i.binary}, i.args...)
	return i.label + " (" + strings.Join(parts, " ") + ")"
}

// IsAuthFailure reports whether err looks like a rejected/expired Teams token
// rather than a transient transport error. The teams-api package wraps HTTP
// statuses into text-only GenericHTTPError values, so classification is
// partially textual.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}

	// The teams and calendar clients return typed status errors.
	var statusErr interface{ HTTPStatus() int }
	if errors.As(err, &statusErr) {
		status := statusErr.HTTPStatus()
		return status == http.StatusUnauthorized || status == http.StatusForbidden
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
	for _, tokenType := range RuntimeTokenTypes {
		path := TokenFilePath(tokenDir, tokenType)
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
	meta, err := ParseJWT(strings.TrimSpace(raw))
	if err != nil || !meta.HasExpiry {
		return false
	}

	return now.Before(meta.ExpiresAt.Add(-tokenRefreshExpiryGrace))
}

// TokenFilesPresent reports whether any runtime token file exists. A totally
// empty token directory is a fresh install: it needs the interactive
// sign-in (teams-cli login), not a silent refresh.
func TokenFilesPresent(tokenDir string) bool {
	for _, tokenType := range RuntimeTokenTypes {
		if _, err := os.Stat(TokenFilePath(tokenDir, tokenType)); err == nil {
			return true
		}
	}

	return false
}

// ResolveTokenDir returns dir, or the default token directory when dir is
// blank.
func ResolveTokenDir(dir string) (string, error) {
	if dir = strings.TrimSpace(dir); dir != "" {
		return dir, nil
	}

	return DefaultTokenDir()
}

// NeedsRefresh reports whether the stored token files are stale: at least
// one runtime token file exists but the set is not fresh (expired, expiring
// inside the grace window, unparseable, or partially missing).
func (r *Refresher) NeedsRefresh(now time.Time) bool {
	dir, err := ResolveTokenDir(r.TokenDir)
	if err != nil {
		return false
	}
	if !TokenFilesPresent(dir) {
		return false
	}

	return tokenFilesFresh(dir, now) != nil
}

// Refresh runs the sign-in flow, waits for it to rewrite the JWT files, and
// exports the fresh values so rebuilt clients pick them up. A cooldown keeps
// a broken/expired SSO session from re-launching Electron on every failed
// poll.
func (r *Refresher) Refresh(ctx context.Context) error {
	if !r.Enabled {
		return fmt.Errorf("automatic token refresh is disabled")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.lastAt.IsZero() && time.Since(r.lastAt) < tokenRefreshMinInterval {
		return fmt.Errorf("token refresh last attempted %s ago; waiting %s between attempts",
			time.Since(r.lastAt).Round(time.Second), tokenRefreshMinInterval)
	}
	if !r.lastFailedAt.IsZero() && time.Since(r.lastFailedAt) < tokenRefreshFailureCooldown {
		return fmt.Errorf("token refresh last failed %s ago; waiting for the %s failure cooldown",
			time.Since(r.lastFailedAt).Round(time.Second), tokenRefreshFailureCooldown)
	}

	flow, err := resolveTokenFlow(ctx, r.Command, r.logger())
	if err != nil {
		return err
	}

	tokenDir, err := ResolveTokenDir(r.TokenDir)
	if err != nil {
		return err
	}

	r.logger().WithFields(logrus.Fields{
		"command":   flow.String(),
		"timeout":   r.timeout().String(),
		"token_dir": tokenDir,
	}).Info("refreshing Teams tokens")

	r.lastAt = time.Now()
	if err := runTokenFlow(ctx, flow, tokenDir, r.timeout()); err != nil {
		r.lastFailedAt = time.Now()
		return err
	}
	if err := exportTokenFilesToEnv(tokenDir); err != nil {
		r.lastFailedAt = time.Now()
		return err
	}

	r.logger().WithField("token_dir", tokenDir).Info("Teams tokens refreshed successfully")
	return nil
}

// resolveTokenFlow resolves how to run the sign-in flow: the user-configured
// command, else the flow embedded in this binary on the pinned Electron
// runtime (bootstrapped into the user cache on first use).
func resolveTokenFlow(ctx context.Context, command string, logger interface{ Info(args ...any) }) (invocation, error) {
	if command = strings.TrimSpace(command); command != "" {
		// Run the user command verbatim through the shell so chained
		// commands (`a && b`) work; the process group kill covers children.
		return invocation{label: "command", shell: command}, nil
	}

	return ensureEmbeddedTokenFlow(ctx, logger)
}

// runTokenFlow runs the sign-in flow against tokenDir and verifies it left
// fresh token files behind. The flow's own output only surfaces on failure.
func runTokenFlow(ctx context.Context, invocation invocation, tokenDir string, timeout time.Duration) error {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	output := &lockedBuffer{}
	cmd, err := newTokenRefreshCommand(runCtx, invocation)
	if err != nil {
		return err
	}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Env = append(os.Environ(), "FOSSTEAMS_TOKEN_DIR="+tokenDir)

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("sign-in flow failed: %w (%s)", err, output.Tail(tokenRefreshOutputTailBytes))
	}
	if err := tokenFilesFresh(tokenDir, time.Now()); err != nil {
		return fmt.Errorf("sign-in flow did not produce usable tokens: %w (output: %s)", err, output.Tail(tokenRefreshOutputTailBytes))
	}
	return nil
}

func newTokenRefreshCommand(ctx context.Context, invocation invocation) (*exec.Cmd, error) {
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
	for _, tokenType := range RuntimeTokenTypes {
		path := TokenFilePath(tokenDir, tokenType)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("unable to read refreshed token from %s: %w", path, err)
		}

		value := strings.TrimSpace(string(raw))
		if value == "" {
			return fmt.Errorf("refreshed token %s is empty", path)
		}
		if err := os.Setenv(EnvName(tokenType), value); err != nil {
			return fmt.Errorf("unable to export %s: %w", EnvName(tokenType), err)
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
