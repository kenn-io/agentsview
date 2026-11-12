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
	"go.kenn.io/agentsview/internal/service"
)

type searchSpyService struct {
	service.SessionService
	ctx  context.Context
	wait <-chan struct{}
}

func (s *searchSpyService) Search(ctx context.Context, _ service.SearchRequest) (*service.SessionSearchResult, error) {
	s.ctx = ctx
	select {
	case <-s.wait:
		return &service.SessionSearchResult{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSessionSearchOutlivesWriteTimeout(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		name := "complete"
		if cancelRequest {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			srv := testServer(t, 30*time.Second)
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				spy := &searchSpyService{wait: release}
				srv.sessions = spy
				ctx, cancel := context.WithCancel(t.Context())
				req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/search?q=needle", nil)
				response := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					srv.mux.ServeHTTP(response, req)
					close(done)
				}()
				defer func() { cancel(); <-done }()
				synctest.Wait()
				require.NotNil(t, spy.ctx, "the request must reach the search service")
				time.Sleep(31 * time.Second)
				synctest.Wait()
				require.NoError(t, spy.ctx.Err(), "the write timeout must not cancel search")
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
