package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// tokenflow/ holds the bundled teams-token Electron flow (dist/main.js plus
// its transitive pure-JS dependencies). It is embedded into the teams-cli
// binary so a standalone build needs no teams-token checkout, no node, and
// no yarn; refresh materializes it into the user cache directory and runs it
// on a pinned Electron runtime that teams-cli downloads once.
//
//go:embed all:tokenflow
var embeddedTokenFlow embed.FS

const (
	electronFlowVersion = "12.0.2"

	electronFlowMarkerFile  = ".teams-cli-flow-ready"
	electronDownloadTimeout = 10 * time.Minute
	flowDownloadTailBytes   = 200
)

// electronArchiveSpec pins the Electron release archive per platform.
type electronArchiveSpec struct {
	asset  string
	sha256 string
}

func electronArchiveFor(goos, goarch string) (electronArchiveSpec, error) {
	platformArch := ""
	switch goarch {
	case "amd64":
		platformArch = "x64"
	case "arm64":
		platformArch = "arm64"
	default:
		return electronArchiveSpec{}, fmt.Errorf("no Electron runtime build for architecture %q", goarch)
	}
	key := goos + "/" + platformArch
	archives := map[string]electronArchiveSpec{
		"darwin/arm64": {"electron-v12.0.2-darwin-arm64.zip", "d4f0f73e0a5a723ef7f3f1e6ca3743b6267eef385cf79aa63a2fb3f698a7931d"},
		"darwin/x64":   {"electron-v12.0.2-darwin-x64.zip", "766ca8f8adc4535db3069665ea8983979ea79dd5ec376e1c298f858b420ec58f"},
		"linux/arm64":  {"electron-v12.0.2-linux-arm64.zip", "9ef70ab9347be63555784cac99efbaff1ef2d02dcc79070d7bccd18c38de87ef"},
		"linux/x64":    {"electron-v12.0.2-linux-x64.zip", "fc3ff888d8cd4ada8368420c8951ed1b5ad78919bdcb688abe698d00e12a2e0a"},
	}

	spec, ok := archives[key]
	if !ok {
		return electronArchiveSpec{}, fmt.Errorf("no Electron runtime build for platform %s/%s", goos, goarch)
	}

	return spec, nil
}

// flowCacheRoot returns the versioned cache directory holding the Electron
// runtime and the materialized flow.
func flowCacheRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return "", fmt.Errorf("cannot resolve a cache directory")
		}
		base = filepath.Join(home, ".cache")
	}

	return filepath.Join(base, "teams-cli", "tokenflow", electronFlowVersion), nil
}

func flowElectronBinary(cacheDir string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(cacheDir, "electron", "Electron.app", "Contents", "MacOS", "Electron")
	}

	return filepath.Join(cacheDir, "electron", "electron")
}

func flowMainScript(cacheDir string) string {
	return filepath.Join(cacheDir, "flow", "main.js")
}

// ensureEmbeddedTokenFlow downloads/extracts the pinned Electron runtime and
// materializes the embedded flow, returning an invocation ready to run.
func ensureEmbeddedTokenFlow(ctx context.Context, logger interface {
	Info(args ...any)
}) (tokenRefreshInvocation, error) {
	spec, err := electronArchiveFor(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return tokenRefreshInvocation{}, err
	}

	cacheDir, err := flowCacheRoot()
	if err != nil {
		return tokenRefreshInvocation{}, err
	}

	binary := flowElectronBinary(cacheDir)
	marker := filepath.Join(cacheDir, electronFlowMarkerFile)
	if _, err := os.Stat(marker); err != nil || statFileHasSize(binary) != nil {
		if err := installElectronRuntime(ctx, logger, cacheDir, spec); err != nil {
			return tokenRefreshInvocation{}, err
		}
	}

	if err := materializeEmbeddedFlow(cacheDir); err != nil {
		return tokenRefreshInvocation{}, err
	}

	return tokenRefreshInvocation{
		label:  "embedded",
		dir:    filepath.Join(cacheDir, "flow"),
		binary: binary,
		args:   []string{flowMainScript(cacheDir)},
	}, nil
}

func statFileHasSize(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("%s is empty", path)
	}

	return nil
}

func installElectronRuntime(ctx context.Context, logger interface {
	Info(args ...any)
}, cacheDir string, spec electronArchiveSpec) error {
	url := "https://github.com/electron/electron/releases/download/v" + electronFlowVersion + "/" + spec.asset

	dlCtx, cancel := context.WithTimeout(ctx, electronDownloadTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	if logger != nil {
		logger.Info("downloading Electron runtime " + electronFlowVersion + " for token refresh")
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("unable to download Electron runtime: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("unable to download Electron runtime: %s", response.Status)
	}

	payload, err := io.ReadAll(io.LimitReader(response.Body, 512<<20))
	if err != nil {
		return fmt.Errorf("unable to read Electron runtime download: %w", err)
	}

	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != spec.sha256 {
		return errors.New("downloaded Electron runtime failed checksum verification")
	}

	staging := cacheDir + ".staging"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(staging, "electron"), 0o755); err != nil {
		return err
	}

	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return fmt.Errorf("unable to open Electron runtime archive: %w", err)
	}

	for _, entry := range reader.File {
		if err := extractZipEntry(staging, "electron", entry); err != nil {
			return fmt.Errorf("unable to extract Electron runtime: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		return err
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		return err
	}
	if err := os.Rename(staging, cacheDir); err != nil {
		return fmt.Errorf("unable to install Electron runtime: %w", err)
	}

	markerPath := filepath.Join(cacheDir, electronFlowMarkerFile)

	return os.WriteFile(markerPath, []byte(electronFlowVersion+"\n"), 0o644)
}

func extractZipEntry(root, subdir string, entry *zip.File) error {
	target := filepath.Join(root, subdir, filepath.FromSlash(entry.Name))
	cleanRoot := filepath.Clean(filepath.Join(root, subdir))
	if target != cleanRoot && !strings.HasPrefix(target, cleanRoot+string(os.PathSeparator)) {
		return fmt.Errorf("unsafe archive path %q", entry.Name)
	}

	mode := entry.Mode()

	switch {
	case entry.FileInfo().IsDir():
		return os.MkdirAll(target, 0o755)
	case mode&os.ModeSymlink != 0:
		link, err := entry.Open()
		if err != nil {
			return err
		}
		defer link.Close()
		payload, err := io.ReadAll(link)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		_ = os.Remove(target)

		return os.Symlink(string(payload), target)
	default:
		source, err := entry.Open()
		if err != nil {
			return err
		}
		defer source.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		payload, err := io.ReadAll(source)
		if err != nil {
			return err
		}
		perm := mode.Perm()
		if perm == 0 {
			perm = 0o644
		}

		return os.WriteFile(target, payload, perm)
	}
}

// materializeEmbeddedFlow writes the embedded flow into the cache directory
// unless it is already current.
func materializeEmbeddedFlow(cacheDir string) error {
	flowDir := filepath.Join(cacheDir, "flow")

	return fs.WalkDir(embeddedTokenFlow, "tokenflow", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel("tokenflow", path)
		if err != nil {
			return err
		}

		payload, err := embeddedTokenFlow.ReadFile(path)
		if err != nil {
			return err
		}

		target := filepath.Join(flowDir, rel)
		if existing, err := os.ReadFile(target); err == nil && bytes.Equal(existing, payload) {
			return nil
		}

		mode := os.FileMode(0o644)
		if info, statErr := d.Info(); statErr == nil {
			mode = info.Mode().Perm()
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		return os.WriteFile(target, payload, mode)
	})
}
