package auth

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestElectronArchiveForSupportedPlatforms(t *testing.T) {
	for _, platform := range []struct{ goos, goarch string }{
		{"darwin", "arm64"},
		{"darwin", "amd64"},
		{"linux", "arm64"},
		{"linux", "amd64"},
	} {
		spec, err := electronArchiveFor(platform.goos, platform.goarch)
		if err != nil {
			t.Fatalf("%s/%s: %v", platform.goos, platform.goarch, err)
		}
		if !strings.Contains(spec.asset, platform.goos) {
			t.Fatalf("%s/%s: asset %q mismatches platform", platform.goos, platform.goarch, spec.asset)
		}
		if len(spec.sha256) != 64 {
			t.Fatalf("%s/%s: bad checksum %q", platform.goos, platform.goarch, spec.sha256)
		}
	}

	if _, err := electronArchiveFor("darwin", "riscv64"); err == nil {
		t.Fatal("expected unsupported architecture to error")
	}
	if _, err := electronArchiveFor("plan9", "amd64"); err == nil {
		t.Fatal("expected unsupported platform to error")
	}
}

func TestMaterializeEmbeddedFlow(t *testing.T) {
	cacheDir := t.TempDir()

	if err := materializeEmbeddedFlow(cacheDir); err != nil {
		t.Fatalf("unable to materialize flow: %v", err)
	}

	mainPath := flowMainScript(cacheDir)
	raw, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("expected %s: %v", mainPath, err)
	}
	if !strings.Contains(string(raw), "FOSSTEAMS_TOKEN_DIR") {
		t.Fatal("embedded flow must honor FOSSTEAMS_TOKEN_DIR")
	}

	// The flow ships without node_modules, so it may only require Electron
	// and Node built-ins.
	builtins := map[string]bool{"electron": true, "crypto": true, "os": true, "fs": true, "https": true, "path": true}
	for _, match := range regexp.MustCompile(`require\("([^"]+)"\)`).FindAllStringSubmatch(string(raw), -1) {
		if !builtins[match[1]] {
			t.Fatalf("embedded flow requires %q, which is not shipped", match[1])
		}
	}

	// Second pass must be a no-op that leaves files intact.
	if err := materializeEmbeddedFlow(cacheDir); err != nil {
		t.Fatalf("re-materialize failed: %v", err)
	}
}

func TestTokenRefreshInvocationUsesEmbeddedFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	// Pre-seed the cache so the fallback takes the fast path: a marker and
	// a fake Electron binary stand in for the ~90MB download.
	cacheDir, err := flowCacheRoot()
	if err != nil {
		t.Fatalf("unable to resolve cache root: %v", err)
	}
	binary := flowElectronBinary(cacheDir)
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatalf("unable to seed cache: %v", err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("unable to seed fake electron: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, electronFlowMarkerFile), []byte(ElectronVersion+"\n"), 0o644); err != nil {
		t.Fatalf("unable to seed marker: %v", err)
	}

	invocation, err := resolveTokenFlow(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("expected the embedded flow with a seeded cache: %v", err)
	}

	if invocation.label != "embedded" {
		t.Fatalf("expected the embedded flow, got %q", invocation.label)
	}
	if invocation.binary != binary {
		t.Fatalf("expected seeded binary %q, got %q", binary, invocation.binary)
	}
	if _, err := os.Stat(invocation.args[0]); err != nil {
		t.Fatalf("expected materialized flow for invocation %v: %v", invocation, err)
	}
}

func TestEnsureEmbeddedTokenFlowReportsUnsupportedPlatform(t *testing.T) {
	if _, err := electronArchiveFor(runtime.GOOS, runtime.GOARCH); err == nil {
		t.Skip("platform is supported; nothing to assert")
	}
	if _, err := ensureEmbeddedTokenFlow(context.Background(), nil); err == nil {
		t.Fatal("expected unsupported platform to error before any download")
	}
}
