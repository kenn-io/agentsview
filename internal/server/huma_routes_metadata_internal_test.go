package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/service"
)

type statsSpyService struct {
	service.SessionService
	got service.StatsFilter
}

func (s *statsSpyService) Stats(
	_ context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	s.got = f
	return &service.SessionStats{}, nil
}

func TestHumaGetSessionStatsUsesServerGitHubToken(t *testing.T) {
	spy := &statsSpyService{}
	srv := &Server{
		cfg:      config.Config{GithubToken: "server-token"},
		sessions: spy,
	}

	_, err := srv.humaGetSessionStats(context.Background(), &sessionStatsInput{
		IncludeGitHubOutcomes: true,
	})

	require.NoError(t, err)
	assert.Equal(t, "server-token", spy.got.GHToken)
}
