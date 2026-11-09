package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sirupsen/logrus"
)

func main() {
	program := filepath.Base(os.Args[0])
	options, err := parseAppOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\nerror: %v\n", usageText(program), err)
		os.Exit(2)
	}
	if options.ShowHelp {
		printUsage(os.Stdout, program)
		return
	}
	if options.ShowVersion {
		printVersion(os.Stdout)
		return
	}
	if options.DoctorMode {
		if err := runDoctor(os.Stdout, options); err != nil {
			fmt.Fprintf(os.Stderr, "doctor: %v\n", err)
			os.Exit(1)
		}
		return
	}

	logSetup, err := setupLogger(options)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logging setup failed: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		if err := logSetup.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "log shutdown failed: %v\n", err)
		}
	}()

	logger := logSetup.Logger
	logger.WithFields(logrus.Fields{
		"version":       version,
		"log_file":      logSetup.Path,
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
	if err := applyTokenDirToEnv(options.TokenDir); err != nil {
		logger.WithError(err).WithField("token_dir", options.TokenDir).Error("token setup failed")
		fmt.Fprintf(os.Stderr, "token setup failed: %v\nSee log: %s\n", err, logSetup.Path)
		os.Exit(1)
	}

	rootCtx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	state := AppState{
		logger:                       logger,
		logFilePath:                  logSetup.Path,
		messageLimit:                 options.MessageLimit,
		liveRefreshDisabled:          !options.LiveRefresh,
		liveMessageRefreshEvery:      options.RefreshMessagesInterval,
		liveConversationRefreshEvery: options.RefreshConversationInterval,
		tokenDir:                     options.TokenDir,
		tokenRefresh: tokenRefreshConfig{
			enabled: options.AutoRefresh,
			command: options.TokenRefreshCommand,
			timeout: options.TokenRefreshTimeout,
		},
	}
	state.initRuntime(rootCtx)
	defer state.requestStop()

	teaProgram := tea.NewProgram(newChatModel(&state), tea.WithAltScreen(), tea.WithMouseCellMotion())
	state.createUI(teaProgram)

	go state.start()

	if _, err := teaProgram.Run(); err != nil {
		logger.WithError(err).WithField("log_file", logSetup.Path).Error("tui runtime failed")
		fmt.Fprintf(os.Stderr, "teams-cli failed: %v\nSee log: %s\n", err, logSetup.Path)
		os.Exit(1)
	}

	logger.WithField("log_file", logSetup.Path).Info("teams-cli exited cleanly")
}

func printUsage(out io.Writer, program string) {
	fmt.Fprint(out, usageText(program))
}

func printVersion(out io.Writer) {
	fmt.Fprintf(out, "teams-cli %s\n", version)
}
