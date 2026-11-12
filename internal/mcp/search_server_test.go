package mcp

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/servicehttp"
)

type delayedSearchStore struct {
	db.Store
	started chan struct{}
	stopped chan struct{}
}

func (s *delayedSearchStore) Search(ctx context.Context, f db.SearchFilter) (db.SearchPage, error) {
	switch f.Query {
	case "cancelterm":
		close(s.started)
		<-ctx.Done()
		close(s.stopped)
		return db.SearchPage{}, ctx.Err()
	case "errorterm":
		return db.SearchPage{}, &db.SearchInputError{Msg: "search fixture error"}
	}
	select {
	case <-time.After(150 * time.Millisecond): //nolint:kennlint // deadlinetest: search must outlast the server write timeout
	case <-ctx.Done():
		return db.SearchPage{}, ctx.Err()
	}
	return s.Store.Search(ctx, f)
}

func TestServer_SearchSessionsThroughHTTP(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	if !d.HasFTS(t.Context()) {
		t.Skip("FTS not available")
	}
	for i := range 4 {
		seedFTSSession(t, d, fmt.Sprintf("p%d", i), "proj", "pageterm body", fmt.Sprintf("2024-06-1%dT10:00:00Z", i))
	}
	store := &delayedSearchStore{Store: d, started: make(chan struct{}), stopped: make(chan struct{})}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := server.New(config.Config{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, DataDir: t.TempDir(), WriteTimeout: 50 * time.Millisecond}, store, nil) //nolint:kennlint // deadlinetest: the search must outlast this timeout
	httpServer := httptest.NewUnstartedServer(srv.Handler())
	require.NoError(t, httpServer.Listener.Close())
	httpServer.Listener = ln
	httpServer.Start()
	t.Cleanup(httpServer.Close)

	direct := newSessionUsageClient(t, service.NewDirectBackend(d, nil))
	remote := newSessionUsageClient(t, servicehttp.NewHTTPBackend(httpServer.URL, "", false, ""))
	args := map[string]any{"query": "pageterm", "sort": "recency", "limit": 2, "include_active": true}
	for page := range 2 {
		want, err := direct.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
		got, err := remote.CallTool(t.Context(), callParams(ToolSearchSessions, args))
		require.NoError(t, err)
		wantPage := decodeStructured[searchSessionsOut](t, want)
		gotPage := decodeStructured[searchSessionsOut](t, got)
		require.Len(t, gotPage.Results, 2)
		for i := range gotPage.Results {
			assert.NotEmpty(t, gotPage.Results[i].WebURL)
			gotPage.Results[i].WebURL = ""
			wantPage.Results[i].WebURL = ""
		}
		assert.Equal(t, wantPage, gotPage)
		if page == 0 {
			require.NotNil(t, wantPage.NextCursor)
			args["cursor"] = *wantPage.NextCursor
		}
	}

	t.Run("error", func(t *testing.T) {
		res, err := remote.CallTool(t.Context(), callParams(ToolSearchSessions, map[string]any{"query": "errorterm"}))
		require.NoError(t, err)
		require.True(t, res.IsError)
		require.NotEmpty(t, res.Content)
		message, ok := res.Content[0].(*mcp.TextContent)
		require.True(t, ok)
		assert.Contains(t, message.Text, "search fixture error")
	})
	t.Run("cancel", func(t *testing.T) {
		st, ct := newInMemoryPair(t, newServer(ServeOptions{Service: servicehttp.NewHTTPBackend(httpServer.URL, "", false, "")}))
		t.Cleanup(func() {
			_ = ct.Close()
			_ = st.Wait()
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := ct.CallTool(ctx, callParams(ToolSearchSessions, map[string]any{"query": "cancelterm"}))
			done <- err
		}()
		<-store.started
		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		<-store.stopped
	})
}
