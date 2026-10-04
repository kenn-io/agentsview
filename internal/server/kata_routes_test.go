package server_test

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
	"go.kenn.io/agentsview/internal/server"
)

func TestKataHubEligibilityLoggedAtStartup(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		pgURL   string
		conn    *kata.Conn
		want    string
	}{
		{name: "standalone hub", enabled: true, want: "kata: hub eligibility hub"},
		{name: "PG pusher", enabled: true, pgURL: "postgres://pg.example.test/agentsview", want: "kata: hub eligibility not_hub"},
		{name: "disabled", want: "kata: hub eligibility disabled"},
		{name: "explicit PG serve hub", enabled: true, pgURL: "postgres://pg.example.test/agentsview", conn: kata.NewConn(kata.Config{Enabled: true, Hub: true, Project: "agentsview"}), want: "kata: hub eligibility hub"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			original := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(original) })

			var opts []server.Option
			if tt.conn != nil {
				opts = append(opts, server.WithKataConn(tt.conn))
			}
			setupWithServerOpts(t, opts, func(c *config.Config) {
				c.PG = config.PGConfig{URL: tt.pgURL}
				c.Kata = config.KataConfig{Enabled: tt.enabled, Endpoint: "https://kata.example.test/?token=secret", Project: "agentsview"}
			})
			log.SetOutput(original)

			var eligibility []string
			for line := range strings.SplitSeq(logs.String(), "\n") {
				if strings.Contains(line, "kata: hub eligibility ") {
					eligibility = append(eligibility, line)
				}
			}
			require.Len(t, eligibility, 1, "startup must report one eligibility decision before any request")
			assert.Contains(t, eligibility[0], tt.want)
			assert.NotContains(t, logs.String(), "token=secret", "startup logs must not expose the configured endpoint")
		})
	}
}

func TestKataStatusRoute(t *testing.T) {
	tests := []struct {
		name          string
		conn          func(t *testing.T) *kata.Conn
		wantState     kata.State
		wantProject   string
		wantAvailable bool
	}{
		{name: "default_is_disabled", conn: func(t *testing.T) *kata.Conn {
			t.Helper()
			return nil
		}, wantState: kata.StateDisabled, wantProject: "agentsview"},
		{name: "ready", conn: func(t *testing.T) *kata.Conn {
			t.Helper()
			s := katatest.New(t)
			return kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "agentsview", Actor: "agentsview"})
		}, wantState: kata.StateReady, wantProject: "agentsview", wantAvailable: true},
		{name: "wrong_project", conn: func(t *testing.T) *kata.Conn {
			t.Helper()
			s := katatest.New(t)
			return kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "other"})
		}, wantState: kata.StateWrongProject, wantProject: "other"},
		{name: "unauthenticated_empty_env", conn: func(t *testing.T) *kata.Conn {
			t.Helper()
			s := katatest.New(t)
			return kata.NewConn(kata.Config{Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "agentsview", TokenRequired: true})
		}, wantState: kata.StateUnauthenticated, wantProject: "agentsview"},
		{name: "not_hub", conn: func(t *testing.T) *kata.Conn {
			t.Helper()
			s := katatest.New(t)
			return kata.NewConn(kata.Config{Enabled: true, Hub: false, Endpoint: s.Endpoint(), Project: "agentsview"})
		}, wantState: kata.StateNotHub, wantProject: "agentsview"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []server.Option
			if c := tt.conn(t); c != nil {
				opts = append(opts, server.WithKataConn(c))
			}
			te := setupWithServerOpts(t, opts)

			w := te.get(t, "/api/v1/kata/status")
			assertStatus(t, w, http.StatusOK)
			st := decode[kata.Status](t, w)
			assert.Equal(t, tt.wantState, st.State, st.Message)
			assert.Equal(t, tt.wantProject, st.Project)

			w = te.get(t, "/api/v1/version")
			assertStatus(t, w, http.StatusOK)
			v := decode[server.VersionInfo](t, w)
			assert.Equal(t, tt.wantAvailable, v.KataAvailable)
		})
	}
}

func TestKataStatusRouteDefaultUsesConfig(t *testing.T) {
	tests := []struct {
		name      string
		pgURL     string
		wantState kata.State
		wantReady bool
	}{
		{name: "standalone_is_hub", wantState: kata.StateReady, wantReady: true},
		{name: "pusher_is_not_hub", pgURL: "postgres://pg.example.test/agentsview", wantState: kata.StateNotHub},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			te := setup(t, func(c *config.Config) {
				c.PG = config.PGConfig{URL: tt.pgURL}
				c.Kata = config.KataConfig{Enabled: true, Endpoint: s.Endpoint(), Project: "agentsview", Actor: "agentsview", Timeout: time.Second}
			})
			w := te.get(t, "/api/v1/kata/status")
			assertStatus(t, w, http.StatusOK)
			require.Equal(t, tt.wantState, decode[kata.Status](t, w).State)
			w = te.get(t, "/api/v1/version")
			assert.Equal(t, tt.wantReady, decode[server.VersionInfo](t, w).KataAvailable)
			assert.Equal(t, tt.wantReady, len(s.Requests()) > 0, "a pusher never contacts Kata")
		})
	}
}

func TestKataStatusRouteReprobes(t *testing.T) {
	s := katatest.New(t)
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "agentsview",
	}))})
	w := te.get(t, "/api/v1/kata/status")
	assertStatus(t, w, http.StatusOK)
	require.Equal(t, kata.StateReady, decode[kata.Status](t, w).State)

	s.SetSchemaVersion("0.20.0")
	w = te.get(t, "/api/v1/kata/status")
	assertStatus(t, w, http.StatusOK)
	assert.Equal(t, kata.StateIncompatible, decode[kata.Status](t, w).State)
	w = te.get(t, "/api/v1/version")
	assertStatus(t, w, http.StatusOK)
	assert.False(t, decode[server.VersionInfo](t, w).KataAvailable)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 2)
}

func TestKataVersionRouteProbesOnFirstRequest(t *testing.T) {
	s := katatest.New(t)
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "agentsview",
	}))})
	w := te.get(t, "/api/v1/version")
	assertStatus(t, w, http.StatusOK)
	assert.True(t, decode[server.VersionInfo](t, w).KataAvailable)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 1)
}

func TestKataVersionRouteBoundsSlowProbe(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(slow.Close)
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: slow.URL, Project: "agentsview", Timeout: 4 * time.Second,
	}))})

	started := time.Now()
	w := te.get(t, "/api/v1/version")
	elapsed := time.Since(started)
	assertStatus(t, w, http.StatusOK)
	assert.False(t, decode[server.VersionInfo](t, w).KataAvailable)
	assert.Less(t, elapsed, 3*time.Second, "version should not wait for the Kata request timeout")
}

type delayedKataTransport struct {
	next  http.RoundTripper
	delay time.Duration
}

func (d delayedKataTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	timer := time.NewTimer(d.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return d.next.RoundTrip(r)
}

func TestKataVersionRouteEventuallyReportsSlowHealthyKata(t *testing.T) {
	s := katatest.New(t)
	c := kata.NewConn(kata.Config{
		Enabled: true, Hub: true, Endpoint: s.Endpoint(), Project: "agentsview", Timeout: 5 * time.Second,
		HTTPClient: &http.Client{Transport: delayedKataTransport{next: http.DefaultTransport, delay: 600 * time.Millisecond}},
	})
	te := setupWithServerOpts(t, []server.Option{server.WithKataConn(c)})
	started := time.Now()
	w := te.get(t, "/api/v1/version")
	assertStatus(t, w, http.StatusOK)
	assert.False(t, decode[server.VersionInfo](t, w).KataAvailable)
	assert.Less(t, time.Since(started), 3*time.Second)

	require.Eventually(t, func() bool { return c.VersionReady(t.Context()) }, 6*time.Second, 100*time.Millisecond)
	w = te.get(t, "/api/v1/version")
	assertStatus(t, w, http.StatusOK)
	assert.True(t, decode[server.VersionInfo](t, w).KataAvailable)
}
