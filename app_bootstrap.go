package main

import (
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sirupsen/logrus"
)

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
	if s.tokenFilesNeedRefresh(time.Now()) {
		s.appLogger().Info("stored Teams tokens are expired or missing expiry; attempting automatic refresh via teams-token")
		s.setLoginStatus("Your stored Teams tokens are expired.\nRefreshing via teams-token…")
		if !s.maybeRefreshTokens(s.appContext(), nil) {
			s.appLogger().Warn("automatic token refresh did not restore usable tokens")
		}
	}

	client, err := s.newTeamsClient()
	if err != nil && isAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
		client, err = s.newTeamsClient()
	}
	if err != nil {
		s.appLogger().WithError(err).Error("unable to initialize teams client")
		s.showError(err)
		return
	}
	s.setTeamsClient(client)

	err = s.TeamsState.init(client)
	if err != nil && isAuthFailure(err) && s.maybeRefreshTokens(s.appContext(), err) {
		err = s.TeamsState.init(s.client())
	}
	if err != nil {
		s.appLogger().WithError(err).Error("unable to initialize teams state")
		s.showError(err)
		return
	}

	s.appLogger().WithFields(logrus.Fields{
		"teams_count": len(s.conversations.Teams),
		"chat_count":  len(s.conversations.Chats),
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
