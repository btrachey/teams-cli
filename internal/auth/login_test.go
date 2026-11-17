package auth

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRunLoginWritesTokens drives login through a custom command standing in
// for the Electron window: it must receive the token directory and leave
// fresh tokens behind.
func TestRunLoginWritesTokens(t *testing.T) {
	dir := t.TempDir()
	fresh := testJWT(t, map[string]any{"exp": float64(time.Now().Add(time.Hour).Unix())})
	if err := os.WriteFile(dir+"/fresh.jwt", []byte(fresh), 0o600); err != nil {
		t.Fatal(err)
	}
	command := `for t in teams skype chatsvcagg; do cp "$FOSSTEAMS_TOKEN_DIR/fresh.jwt" "$FOSSTEAMS_TOKEN_DIR/token-$t.jwt"; done`

	var out bytes.Buffer
	err := Login(context.Background(), &out, dir, command, false)
	if err != nil {
		t.Fatalf("login failed: %v", err)
	}
	if err := tokenFilesFresh(dir, time.Now()); err != nil {
		t.Fatalf("expected fresh tokens after login: %v", err)
	}
	if !strings.Contains(out.String(), "Signed in") {
		t.Fatalf("expected a confirmation, got %q", out.String())
	}

	// A flow that exits without writing tokens is a failed sign-in.
	empty := t.TempDir()
	if err := Login(context.Background(), &out, empty, "true", false); err == nil {
		t.Fatal("expected login to fail when no tokens were written")
	}
}
