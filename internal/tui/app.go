// Package tui is the terminal UI: the Bubble Tea model, its panes, and the
// background loads and live refresh that feed it.
package tui

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/fossteams/teams-cli/internal/config"
	"github.com/sirupsen/logrus"
)

// Run runs the TUI until the user quits or ctx is cancelled (SIGTERM, or an
// external SIGINT; in raw mode Ctrl+C arrives as a key).
func Run(ctx context.Context, options config.Options, logger *logrus.Logger, logFilePath string) error {
	state := &AppState{
		logger:                       logger,
		logFilePath:                  logFilePath,
		messageLimit:                 options.MessageLimit,
		liveRefreshDisabled:          !options.LiveRefresh,
		liveMessageRefreshEvery:      options.RefreshMessagesInterval,
		liveConversationRefreshEvery: options.RefreshConversationInterval,
		readReceipts:                 options.ReadReceipts,
		tokens: auth.Refresher{
			Enabled:  options.AutoRefresh,
			Command:  options.TokenRefreshCommand,
			Timeout:  options.TokenRefreshTimeout,
			TokenDir: options.TokenDir,
			Logger:   logger,
		},
	}
	state.initRuntime(ctx)
	defer state.requestStop()

	program := tea.NewProgram(newChatModel(state), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
	state.createUI(program)

	go state.start()

	if _, err := program.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return err
	}
	return nil
}

// createUI wires the Bubble Tea program to the app state: background work
// delivers tea.Msgs through sendUIMsg. Quitting flows through tea.Quit
// commands; AppState never reaches back into the program.
func (s *AppState) createUI(program *tea.Program) {
	s.ensureRuntime()
	s.send = program.Send
}

// setLoginStatus updates the login page text so long-running startup work
// (like an automatic token refresh) is visible to the user.
func (s *AppState) setLoginStatus(message string) {
	s.sendUIMsg(loginStatusMsg(message))
}

func (s *AppState) start() {
	if s.tokens.Enabled && s.tokens.NeedsRefresh(time.Now()) {
		s.appLogger().Info("stored Teams tokens are expired or missing expiry; attempting automatic refresh")
		s.setLoginStatus("Your stored Teams tokens are expired.\nRefreshing them…")
		if !s.maybeRefreshTokens(s.appContext(), nil) {
			s.appLogger().Warn("automatic token refresh did not restore usable tokens")
		}
	}

	client, err := s.newTeamsClient()
	if err != nil && auth.IsAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
		client, err = s.newTeamsClient()
	}
	if err != nil {
		s.appLogger().WithError(err).Error("unable to initialize teams client")
		s.showError(err)
		return
	}
	s.teams.SetClient(client)

	err = s.teams.Init()
	if err != nil && auth.IsAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
		err = s.teams.Init()
	}
	if err != nil {
		s.appLogger().WithError(err).Error("unable to initialize teams state")
		s.showError(err)
		return
	}

	conversations := s.teams.Conversations()
	s.appLogger().WithFields(logrus.Fields{
		"teams_count": len(conversations.Teams),
		"chat_count":  len(conversations.Chats),
	}).Info("teams state initialized")

	select {
	case <-s.appContext().Done():
		return
	default:
	}

	s.fillMainWindow()
}

func (s *AppState) showError(err error) {
	s.appLogger().WithError(err).WithField("log_file", s.logFilePath).Error("showing error view")

	message := err.Error()
	if strings.TrimSpace(s.logFilePath) != "" {
		message += "\n\nSee log: " + s.logFilePath
	}
	s.sendUIMsg(errorPageMsg{message: message})
}

// fillMainWindow builds the initial conversation tree, switches to the main
// page, and starts the live refresh loop when enabled.
func (s *AppState) fillMainWindow() {
	select {
	case <-s.appContext().Done():
		return
	default:
	}

	s.sendUIMsg(mainReadyMsg{})

	if s.liveRefreshEnabled() && atomic.CompareAndSwapUint32(&s.liveLoopStarted, 0, 1) {
		s.appLogger().WithFields(logrus.Fields{
			"message_refresh":      s.messageRefreshInterval().String(),
			"conversation_refresh": s.conversationRefreshInterval().String(),
		}).Info("starting live refresh loop")
		go s.runLiveRefreshLoop()
	}
}
