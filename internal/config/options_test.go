package config

import (
	"strings"
	"testing"
	"time"

	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/sirupsen/logrus"
)

func TestParseDefaults(t *testing.T) {
	options, err := Parse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if options.MessageLimit != DefaultMessageLimit {
		t.Fatalf("expected default message limit %d, got %d", DefaultMessageLimit, options.MessageLimit)
	}
	if options.LogLevel != logrus.InfoLevel {
		t.Fatalf("expected default log level info, got %s", options.LogLevel)
	}
	if !options.LiveRefresh {
		t.Fatal("expected live refresh to be enabled by default")
	}
	if options.RefreshMessagesInterval != DefaultMessageRefreshInterval {
		t.Fatalf("expected default message refresh interval %s, got %s", DefaultMessageRefreshInterval, options.RefreshMessagesInterval)
	}
	if options.RefreshConversationInterval != DefaultConversationRefreshInterval {
		t.Fatalf("expected default tree refresh interval %s, got %s", DefaultConversationRefreshInterval, options.RefreshConversationInterval)
	}
	if !options.AutoRefresh {
		t.Fatal("expected automatic token refresh to be enabled by default")
	}
	if options.TokenRefreshTimeout != auth.DefaultRefreshTimeout {
		t.Fatalf("expected default token refresh timeout %s, got %s", auth.DefaultRefreshTimeout, options.TokenRefreshTimeout)
	}
}

func TestParseExtendedFlags(t *testing.T) {
	options, err := Parse([]string{
		"--msg", "25",
		"--debug",
		"--token-dir", "/tmp/tokens",
		"--refresh-messages", "10",
		"--refresh-tree=45s",
		"--no-live",
		"doctor",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if options.MessageLimit != 25 {
		t.Fatalf("expected message limit 25, got %d", options.MessageLimit)
	}
	if options.LogLevel != logrus.DebugLevel {
		t.Fatalf("expected debug log level, got %s", options.LogLevel)
	}
	if options.TokenDir != "/tmp/tokens" {
		t.Fatalf("expected token dir /tmp/tokens, got %q", options.TokenDir)
	}
	if options.RefreshMessagesInterval != 10*time.Second {
		t.Fatalf("expected message refresh 10s, got %s", options.RefreshMessagesInterval)
	}
	if options.RefreshConversationInterval != 45*time.Second {
		t.Fatalf("expected tree refresh 45s, got %s", options.RefreshConversationInterval)
	}
	if options.LiveRefresh {
		t.Fatal("expected live refresh to be disabled")
	}
	if !options.DoctorMode {
		t.Fatal("expected doctor mode to be enabled")
	}
}

func TestParseLegacyMessageLimit(t *testing.T) {
	options, err := Parse([]string{"msg=25"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if options.MessageLimit != 25 {
		t.Fatalf("expected message limit 25, got %d", options.MessageLimit)
	}
}

func TestParseHelpAndVersion(t *testing.T) {
	options, err := Parse([]string{"--help", "--version"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !options.ShowHelp {
		t.Fatal("expected help mode")
	}
	if !options.ShowVersion {
		t.Fatal("expected version mode")
	}
}

func TestParseDebugFlagCanBeOverridden(t *testing.T) {
	options, err := Parse([]string{"--debug", "--log-level", "error"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if options.LogLevel != logrus.ErrorLevel {
		t.Fatalf("expected error log level, got %s", options.LogLevel)
	}
}

func TestParseSupportsEqualsForms(t *testing.T) {
	options, err := Parse([]string{
		"--msg=12",
		"--log-level=warn",
		"--token-dir=/tmp/teams-cli-tokens",
		"--refresh-messages=12s",
		"--refresh-tree=30",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if options.MessageLimit != 12 {
		t.Fatalf("expected message limit 12, got %d", options.MessageLimit)
	}
	if options.LogLevel != logrus.WarnLevel {
		t.Fatalf("expected warn level, got %s", options.LogLevel)
	}
	if options.TokenDir != "/tmp/teams-cli-tokens" {
		t.Fatalf("expected token dir to be parsed, got %q", options.TokenDir)
	}
	if options.RefreshMessagesInterval != 12*time.Second {
		t.Fatalf("expected 12s message refresh interval, got %s", options.RefreshMessagesInterval)
	}
	if options.RefreshConversationInterval != 30*time.Second {
		t.Fatalf("expected 30s tree refresh interval, got %s", options.RefreshConversationInterval)
	}
}

func TestParseFlexibleDurationNumericSeconds(t *testing.T) {
	got, err := parseFlexibleDuration("30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 30*time.Second {
		t.Fatalf("expected 30s, got %s", got)
	}
}

func TestParseRejectsInvalidMessageLimit(t *testing.T) {
	_, err := Parse([]string{"msg=0"})
	if err == nil {
		t.Fatal("expected an error for msg=0")
	}
}

func TestParseRejectsInvalidRefreshInterval(t *testing.T) {
	_, err := Parse([]string{"--refresh-messages", "0"})
	if err == nil {
		t.Fatal("expected an error for zero refresh interval")
	}
}

func TestParseRejectsMissingFlagValues(t *testing.T) {
	testCases := []string{
		"--msg",
		"--log-level",
		"--token-dir",
		"--refresh-messages",
		"--refresh-tree",
	}

	for _, arg := range testCases {
		if _, err := Parse([]string{arg}); err == nil {
			t.Fatalf("expected an error for %s without a value", arg)
		}
	}
}

func TestParseRejectsUnknownArgument(t *testing.T) {
	_, err := Parse([]string{"foo=bar"})
	if err == nil {
		t.Fatal("expected an error for an unknown argument")
	}
}

func TestUsageTextIncludesDiagnosticsAndRefreshFlags(t *testing.T) {
	text := Usage("teams-cli")

	for _, needle := range []string{"--doctor", "--no-live", "--refresh-messages", "--refresh-tree", "--no-auto-refresh", "--token-refresh-command", "--token-refresh-timeout"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("expected usage text to mention %s", needle)
		}
	}
}

func TestParseLoginCommand(t *testing.T) {
	options, err := Parse([]string{"login", "--switch-account", "--token-dir", "/tmp/tok"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.LoginMode || !options.SwitchAccount || options.TokenDir != "/tmp/tok" {
		t.Fatalf("unexpected options: %+v", options)
	}
}

func TestParseTokenRefreshFlags(t *testing.T) {
	options, err := Parse([]string{"--no-auto-refresh"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if options.AutoRefresh {
		t.Fatal("expected --no-auto-refresh to disable automatic refresh")
	}

	options, err = Parse([]string{"--token-refresh-command", "my-tool --renew", "--token-refresh-timeout", "90"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if options.TokenRefreshCommand != "my-tool --renew" {
		t.Fatalf("expected custom refresh command to be stored, got %q", options.TokenRefreshCommand)
	}
	if options.TokenRefreshTimeout != 90*time.Second {
		t.Fatalf("expected 90s refresh timeout, got %s", options.TokenRefreshTimeout)
	}

	options, err = Parse([]string{"--token-refresh-command=my-tool", "--token-refresh-timeout=45s"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if options.TokenRefreshCommand != "my-tool" || options.TokenRefreshTimeout != 45*time.Second {
		t.Fatalf("equals forms not parsed: %+v", options)
	}

	if _, err := Parse([]string{"--token-refresh-command", " "}); err == nil {
		t.Fatal("expected empty refresh command to be rejected")
	}
	if _, err := Parse([]string{"--token-refresh-timeout", "0"}); err == nil {
		t.Fatal("expected non-positive refresh timeout to be rejected")
	}
	if _, err := Parse([]string{"--token-refresh-command"}); err == nil {
		t.Fatal("expected missing refresh command value to be rejected")
	}
	if _, err := Parse([]string{"--token-refresh-timeout"}); err == nil {
		t.Fatal("expected missing refresh timeout value to be rejected")
	}
}

func TestParseReadReceiptsFlag(t *testing.T) {
	options, err := Parse(nil)
	if err != nil || !options.ReadReceipts {
		t.Fatalf("read receipts must default on: %+v %v", options, err)
	}
	options, err = Parse([]string{"--no-read-receipts"})
	if err != nil || options.ReadReceipts {
		t.Fatalf("--no-read-receipts must turn them off: %+v %v", options, err)
	}
}
