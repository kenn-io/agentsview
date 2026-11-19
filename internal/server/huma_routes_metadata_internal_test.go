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

type longRouteSpyService struct {
	service.SessionService
	got  service.StatsFilter
	ctx  context.Context
	wait <-chan struct{}
}

func (s *longRouteSpyService) Stats(
	ctx context.Context, f service.StatsFilter,
) (*service.SessionStats, error) {
	s.got = f
	if err := s.block(ctx); err != nil {
		return nil, err
	}
	return &service.SessionStats{}, nil
}

func (s *longRouteSpyService) block(ctx context.Context) error {
	s.ctx = ctx
	if s.wait != nil {
		select {
		case <-s.wait:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *longRouteSpyService) Search(ctx context.Context, _ service.SearchRequest) (*service.SessionSearchResult, error) {
	if err := s.block(ctx); err != nil {
		return nil, err
	}
	return &service.SessionSearchResult{}, nil
}

func TestLongRoutesOutliveWriteTimeout(t *testing.T) {
	for _, route := range []struct{ name, path string }{
		{"stats", "/api/v1/session-stats?include_github_outcomes=true"},
		{"search", "/api/v1/search?q=needle"},
	} {
		for _, cancelRequest := range []bool{false, true} {
			name := "complete"
			if cancelRequest {
				name = "cancel"
			}
			t.Run(route.name+"/"+name, func(t *testing.T) {
				srv := testServer(t, 30*time.Second)
				if route.name == "stats" {
					srv.cfg.GithubToken = "server-token"
				}
				synctest.Test(t, func(t *testing.T) {
					release := make(chan struct{})
					spy := &longRouteSpyService{wait: release}
					srv.sessions = spy
					ctx, cancel := context.WithCancel(t.Context())
					req := httptest.NewRequestWithContext(ctx, http.MethodGet, route.path, nil)
					response := httptest.NewRecorder()
					done := make(chan struct{})
					go func() { srv.mux.ServeHTTP(response, req); close(done) }()
					defer func() { cancel(); <-done }()
					synctest.Wait()
					require.NotNil(t, spy.ctx, "the request must reach the service")
					time.Sleep(31 * time.Second)
					synctest.Wait()
					require.NoError(t, spy.ctx.Err(), "the write timeout must not cancel the request")
					if route.name == "stats" {
						assert.True(t, spy.got.IncludeGitHubOutcomes)
					}
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
}

func TestHumaGetSessionStatsUsesServerGitHubToken(t *testing.T) {
	spy := &longRouteSpyService{}
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

func TestHumaGetSessionStatsOutcomesOnly(t *testing.T) {
	srv := testServer(t, 30*time.Second)
	spy := &longRouteSpyService{}
	srv.sessions = spy
	response := httptest.NewRecorder()
	srv.mux.ServeHTTP(response, httptest.NewRequestWithContext(t.Context(), http.MethodGet,
		"/api/v1/session-stats?outcomes_only=true", nil))
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.True(t, spy.got.OutcomesOnly)
}
