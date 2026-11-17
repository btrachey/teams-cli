package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/fossteams/teams-cli/internal/config"
	"github.com/fossteams/teams-cli/internal/doctor"
	"github.com/fossteams/teams-cli/internal/logging"
	"github.com/fossteams/teams-cli/internal/tui"
	"github.com/sirupsen/logrus"
)

func main() {
	program := filepath.Base(os.Args[0])
	options, err := config.Parse(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\nerror: %v\n", config.Usage(program), err)
		os.Exit(2)
	}
	if options.ShowHelp {
		fmt.Fprint(os.Stdout, config.Usage(program))
		return
	}
	if options.ShowVersion {
		printVersion(os.Stdout)
		return
	}
	if options.LoginMode {
		if err := auth.Login(context.Background(), os.Stdout, options.TokenDir, options.TokenRefreshCommand, options.SwitchAccount); err != nil {
			fmt.Fprintf(os.Stderr, "login: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if options.DoctorMode {
		if err := doctor.Run(os.Stdout, options, version); err != nil {
			fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logFile, err := logging.Open(options.LogLevel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging setup failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := logFile.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "log shutdown failed: %v\n", err)
		}
	}()

	logger := logFile.Logger
	logger.WithFields(logrus.Fields{
		"version":       version,
		"log_file":      logFile.Path,
		"log_level":     options.LogLevel.String(),
		"message_limit": options.MessageLimit,
		"live_refresh":  options.LiveRefresh,
		"refresh_msg":   options.RefreshMessagesInterval.String(),
		"refresh_tree":  options.RefreshConversationInterval.String(),
		"token_dir":     options.TokenDir,
		"auto_refresh":  options.AutoRefresh,
		"refresh_cmd":   options.TokenRefreshCommand,
		"refresh_wait":  options.TokenRefreshTimeout.String(),
		"debug_enabled": options.LogLevel == logrus.DebugLevel,
		"process_id":    os.Getpid(),
	}).Info("starting teams-cli")
	if err := ensureSignedIn(options); err != nil {
		logger.WithError(err).Error("first sign-in failed")
		fmt.Fprintf(os.Stderr, "sign-in failed: %v\nRun `%s login` to try again.\n", err, program)
		os.Exit(1)
	}
	if err := auth.ApplyTokenDirToEnv(options.TokenDir); err != nil {
		logger.WithError(err).WithField("token_dir", options.TokenDir).Error("token setup failed")
		fmt.Fprintf(os.Stderr, "token setup failed: %v\nSee log: %s\n", err, logFile.Path)
		os.Exit(1)
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	if err := tui.Run(ctx, options, logger, logFile.Path); err != nil {
		logger.WithError(err).WithField("log_file", logFile.Path).Error("tui runtime failed")
		fmt.Fprintf(os.Stderr, "teams-cli failed: %v\nSee log: %s\n", err, logFile.Path)
		os.Exit(1)
	}

	logger.WithField("log_file", logFile.Path).Info("teams-cli exited cleanly")
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "teams-cli %s\n", version)
}

// ensureSignedIn runs the interactive sign-in before the TUI starts when the
// token directory holds no tokens at all (a fresh install). Expired tokens
// are left to the silent refresh inside the app.
func ensureSignedIn(options config.Options) error {
	if !options.AutoRefresh {
		return nil
	}
	tokenDir, err := auth.ResolveTokenDir(options.TokenDir)
	if err != nil {
		return err
	}
	if auth.TokenFilesPresent(tokenDir) {
		return nil
	}
	fmt.Fprintln(os.Stdout, "No Teams tokens found; signing in.")
	return auth.Login(context.Background(), os.Stdout, tokenDir, options.TokenRefreshCommand, false)
}
