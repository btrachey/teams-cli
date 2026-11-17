package tui

import (
	"context"
	"fmt"

	teams_api "github.com/fossteams/teams-api"
	"github.com/fossteams/teams-cli/internal/auth"
	"github.com/fossteams/teams-cli/internal/teams"
)

// refreshTokens runs the sign-in flow (or the test override).
func (s *AppState) refreshTokens(ctx context.Context) error {
	if !s.tokens.Enabled {
		return fmt.Errorf("automatic token refresh is disabled")
	}
	if s.refreshTokensOverride != nil {
		return s.refreshTokensOverride(ctx)
	}

	return s.tokens.Refresh(ctx)
}

// maybeRefreshTokens performs one refresh-and-rebuild cycle. A nil cause
// forces the attempt (proactive pre-flight refresh); otherwise only
// authentication failures qualify. Concurrent callers serialize inside the
// refresher.
func (s *AppState) maybeRefreshTokens(ctx context.Context, cause error) bool {
	if !s.tokens.Enabled {
		return false
	}
	if cause != nil && !auth.IsAuthFailure(cause) {
		return false
	}

	if err := s.refreshTokens(ctx); err != nil {
		s.appLogger().WithError(err).Warn("token refresh did not restore access")
		return false
	}
	if err := s.rebuildTeamsClient(); err != nil {
		s.appLogger().WithError(err).Warn("unable to rebuild teams client after token refresh")
		return false
	}

	return true
}

func (s *AppState) newTeamsClient() (*teams_api.TeamsClient, error) {
	if s.newTeamsClientOverride != nil {
		return s.newTeamsClientOverride()
	}

	return teams.NewClient()
}

func (s *AppState) rebuildTeamsClient() error {
	client, err := s.newTeamsClient()
	if err != nil {
		return err
	}

	s.teams.SetClient(client)

	// Image fetches may have been rejected by the media service with the
	// old credentials; give them a fresh chance.
	s.imageAuthBlocked.Store(false)

	s.appLogger().Info("teams client rebuilt with refreshed tokens")
	return nil
}
