package main

import (
	"os"
	"testing"

	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/fossteams/teams-cli/internal/config"
)

// TestEnsureSignedInOnlyOnFreshInstall pins that startup signs in only when
// the token directory is empty and automatic sign-in is enabled.
func TestEnsureSignedInOnlyOnFreshInstall(t *testing.T) {
	failing := "exit 3"

	if err := ensureSignedIn(config.Options{TokenDir: t.TempDir(), TokenRefreshCommand: failing}); err != nil {
		t.Fatal("auto refresh off must skip sign-in")
	}

	present := t.TempDir()
	if err := os.WriteFile(auth.TokenFilePath(present, auth.TokenTypeSkype), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureSignedIn(config.Options{AutoRefresh: true, TokenDir: present, TokenRefreshCommand: failing}); err != nil {
		t.Fatalf("existing tokens must skip sign-in, got %v", err)
	}

	if err := ensureSignedIn(config.Options{AutoRefresh: true, TokenDir: t.TempDir(), TokenRefreshCommand: failing}); err == nil {
		t.Fatal("an empty token directory must run the sign-in flow")
	}
}
