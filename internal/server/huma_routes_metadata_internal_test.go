package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/service"
)

type statsSpyService struct {
	service.SessionService
	got  service.StatsFilter
	ctx  context.Context
	wait <-chan struct{}
}

func (s *statsSpyService) Stats(
	ctx context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	s.got = f
	s.ctx = ctx
	if s.wait != nil {
		select {
		case <-s.wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &service.SessionStats{}, nil
}

func TestSessionStatsOutlivesWriteTimeout(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		name := "complete"
		if cancelRequest {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			srv := testServer(t, 30*time.Second)
			srv.cfg.GithubToken = "server-token"
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				spy := &statsSpyService{wait: release}
				srv.sessions = spy
				ctx, cancel := context.WithCancel(t.Context())
				req := httptest.NewRequestWithContext(ctx, http.MethodGet,
					"/api/v1/session-stats?include_github_outcomes=true", nil)
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					srv.mux.ServeHTTP(response, req)
					close(done)
				}()
				defer func() { cancel(); <-done }()
				synctest.Wait()
				require.NotNil(t, spy.ctx, "the request must reach the stats service")
				time.Sleep(31 * time.Second)
				synctest.Wait()
				require.NoError(t, spy.ctx.Err(), "the write timeout must not cancel outcome lookups")
				assert.True(t, spy.got.IncludeGitHubOutcomes)
				if cancelRequest {
					cancel()
					<-done
					assert.ErrorIs(t, spy.ctx.Err(), context.Canceled)
				} else {
					close(release)
					<-done
					assert.Equal(t, http.StatusOK, response.Code, response.Body.String())
				}
			})
		})
	}
}

func TestHumaGetSessionStatsUsesServerGitHubToken(t *testing.T) {
	spy := &statsSpyService{}
	srv := &Server{
		cfg:      config.Config{GithubToken: "server-token"},
		sessions: spy,
	}

	_, err := srv.humaGetSessionStats(t.Context(), &sessionStatsInput{
		IncludeGitHubOutcomes: true,
	})

	require.NoError(t, err)
	assert.Equal(t, "server-token", spy.got.GHToken)
}
