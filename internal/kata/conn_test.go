package kata

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func TestConnCachesStatus(t *testing.T) {
	s := katatest.New(t)
	c := NewConn(readyConfig(s.Endpoint()))
	assert.True(t, c.Ready(t.Context()))
	assert.True(t, c.Ready(t.Context()))
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 1, "second Ready uses the cache")
	assert.Equal(t, StateReady, c.Status(t.Context(), true).State)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 2, "fresh bypasses the cache")
}

func TestConnVersionReadyDoesNotWaitForStatusProbe(t *testing.T) {
	c := NewConn(readyConfig("http://127.0.0.1:1"))
	started := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	c.probe = func(context.Context, Config) (Status, *Client) {
		close(started)
		<-release
		return Status{State: StateReady}, nil
	}
	done := make(chan Status, 1)
	go func() { done <- c.Status(t.Context(), false) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "status probe did not start")
	}
	startedAt := time.Now()
	assert.False(t, c.VersionReady(t.Context()))
	assert.Less(t, time.Since(startedAt), time.Second, "version must not wait behind a status probe")
	close(release)
	select {
	case st := <-done:
		assert.Equal(t, StateReady, st.State)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "status probe did not finish")
	}
}

func TestConnVersionReadyReprobesWhenTokenAppears(t *testing.T) {
	const tokenEnv = "AGENTSVIEW_TEST_KATA_VERSION_TOKEN"
	t.Setenv(tokenEnv, "")
	s := katatest.New(t)
	s.SetToken("synthetic-token")
	c := NewConn(ConfigFrom(config.KataConfig{
		Enabled: true, Endpoint: s.Endpoint(), Project: "agentsview", TokenEnv: tokenEnv,
	}, true))
	assert.False(t, c.VersionReady(t.Context()))
	assert.Empty(t, s.Requests())

	t.Setenv(tokenEnv, "synthetic-token")
	assert.True(t, c.VersionReady(t.Context()))
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 1)
}

func TestConnVersionReadyCanceledCallerDoesNotCacheFailure(t *testing.T) {
	s := katatest.New(t)
	c := NewConn(readyConfig(s.Endpoint()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.False(t, c.VersionReady(ctx))
	assert.True(t, c.VersionReady(t.Context()))
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 1)
}

func TestConnCanceledFreshStatusDoesNotCacheUnavailable(t *testing.T) {
	c := NewConn(readyConfig("http://127.0.0.1:1"))
	calls := 0
	c.probe = func(ctx context.Context, _ Config) (Status, *Client) {
		calls++
		if ctx.Err() != nil {
			return Status{State: StateUnavailable}, nil
		}
		return Status{State: StateReady}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Equal(t, StateUnavailable, c.Status(ctx, true).State)
	assert.True(t, c.VersionReady(t.Context()), "a canceled status request cannot hide a healthy Kata")
	assert.True(t, c.VersionReady(t.Context()), "a successful version probe uses the normal cache")
	assert.Equal(t, 2, calls)
}

func TestConnCanceledFreshStatusKeepsTerminalStatesCached(t *testing.T) {
	tests := []struct {
		name   string
		config Config
		want   State
	}{
		{name: "disabled", config: Config{Enabled: false}, want: StateDisabled},
		{name: "not_hub", config: Config{Enabled: true, Hub: false}, want: StateNotHub},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewConn(tt.config)
			calls := 0
			c.probe = func(ctx context.Context, cfg Config) (Status, *Client) {
				calls++
				return Probe(ctx, cfg)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			assert.Equal(t, tt.want, c.Status(ctx, true).State)
			assert.False(t, c.VersionReady(t.Context()))
			assert.Equal(t, 1, calls)
		})
	}
}

func TestVersionFollowupTimeoutCoversConfiguredRequests(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{name: "default", want: 35 * time.Second},
		{name: "larger_configured_timeout", timeout: 20 * time.Second, want: 65 * time.Second},
		{name: "overflow_saturates", timeout: time.Duration(1<<63 - 1), want: time.Duration(1<<63 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, versionFollowupTimeout(tt.timeout))
		})
	}
}

func TestConnReprobesWhenUnavailable(t *testing.T) {
	s := katatest.New(t)
	s.Fail(http.MethodGet, "/api/v1/health", katatest.Fault{Status: 503})
	c := NewConn(readyConfig(s.Endpoint()))
	assert.Equal(t, StateUnavailable, c.Status(t.Context(), false).State)
	cl, err := c.Client(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, cl)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 2)
}

func TestConnReadsTokenEnvAtUse(t *testing.T) {
	const tokenEnv = "AGENTSVIEW_TEST_KATA_TOKEN_DYNAMIC"
	t.Setenv(tokenEnv, "")
	s := katatest.New(t)
	s.SetToken("tok-one")
	c := NewConn(ConfigFrom(config.KataConfig{Enabled: true, Endpoint: s.Endpoint(), TokenEnv: tokenEnv}, true))

	assert.Equal(t, StateUnauthenticated, c.Status(t.Context(), false).State)
	assert.Empty(t, s.Requests(), "a missing configured token must not make any request")

	t.Setenv(tokenEnv, "tok-one")
	require.True(t, c.Ready(t.Context()), "the connection must recover when the variable is set")
	instanceReqs := s.RequestsMatching(http.MethodGet, "/api/v1/instance")
	require.Len(t, instanceReqs, 1)
	assert.Equal(t, "Bearer tok-one", instanceReqs[0].Authorization)

	s.SetToken("tok-two")
	t.Setenv(tokenEnv, "tok-two")
	_, err := c.FindByMetadata(t.Context(), "k", "v")
	require.NoError(t, err)
	issueReqs := s.RequestsMatching(http.MethodGet, "/api/v1/projects/17/issues")
	require.Len(t, issueReqs, 1)
	assert.Equal(t, "Bearer tok-two", issueReqs[0].Authorization)
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), 1, "rotation uses the ready client")
	readyClient, err := c.Client(t.Context())
	require.NoError(t, err)

	t.Setenv(tokenEnv, "")
	before := len(s.Requests())
	_, err = readyClient.GetIssue(t.Context(), "f001")
	require.ErrorIs(t, err, ErrNotReady)
	assert.Len(t, s.Requests(), before, "a previously returned client must also fail closed")
	_, err = c.GetIssue(t.Context(), "f001")
	require.ErrorIs(t, err, ErrNotReady)
	assert.Len(t, s.Requests(), before, "removed token must fail before a request")
	assert.Equal(t, StateUnauthenticated, c.Status(t.Context(), false).State)
	assert.Len(t, s.Requests(), before)
}

func TestConnFailedDiscoveryRunsLocateOnce(t *testing.T) {
	previous := locateCommand
	t.Cleanup(func() { locateCommand = previous })
	calls := 0
	locateCommand = func(context.Context) ([]byte, error) {
		calls++
		return nil, errors.New("synthetic locate failure")
	}
	cfg := readyConfig("")
	c := NewConn(cfg)
	assert.Equal(t, StateUnavailable, c.Status(t.Context(), false).State)
	assert.Equal(t, 1, calls)
}

func TestConnMissingTokenSkipsDiscovery(t *testing.T) {
	const tokenEnv = "AGENTSVIEW_TEST_KATA_TOKEN_MISSING"
	t.Setenv(tokenEnv, "")
	previous := locateCommand
	t.Cleanup(func() { locateCommand = previous })
	locateCommand = func(context.Context) ([]byte, error) {
		require.FailNow(t, "missing token must fail before discovery can start Kata")
		return nil, nil
	}
	c := NewConn(ConfigFrom(config.KataConfig{Enabled: true, TokenEnv: tokenEnv}, true))
	assert.Equal(t, StateUnauthenticated, c.Status(t.Context(), false).State)
}

func TestConnNeverContactsKataWhenOffOrNotHub(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(c *Config)
		endpoint bool
	}{
		{name: "disabled", mutate: func(c *Config) { c.Enabled = false }, endpoint: true},
		{name: "not_hub", mutate: func(c *Config) { c.Hub = false }, endpoint: true},
		{name: "not_hub_with_discovery", mutate: func(c *Config) { c.Hub = false }, endpoint: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := locateCommand
			t.Cleanup(func() { locateCommand = prev })
			locateCommand = func(context.Context) ([]byte, error) {
				require.FailNow(t, "kata daemon locate must not run")
				return nil, nil
			}
			s := katatest.New(t)
			cfg := readyConfig(s.Endpoint())
			if !tt.endpoint {
				cfg.Endpoint = ""
			}
			tt.mutate(&cfg)
			c := NewConn(cfg)
			assert.False(t, c.Ready(t.Context()))
			_, err := c.FindByMetadata(t.Context(), "k", "v")
			require.ErrorIs(t, err, ErrNotReady)
			assert.Empty(t, s.Requests())
		})
	}
}

func TestConnInvalidatesOnTransportFailure(t *testing.T) {
	s := katatest.New(t)
	c := NewConn(readyConfig(s.Endpoint()))
	require.True(t, c.Ready(t.Context()))
	s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 503})
	_, err := c.FindByMetadata(t.Context(), "k", "v")
	require.Error(t, err)
	before := len(s.RequestsMatching(http.MethodGet, "/api/v1/health"))
	assert.True(t, c.Ready(t.Context()))
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), before+1, "failure forced a re-probe")
}

func TestConnInvalidatesOnAuthorizationFailure(t *testing.T) {
	s := katatest.New(t)
	c := NewConn(readyConfig(s.Endpoint()))
	require.True(t, c.Ready(t.Context()))
	s.Fail(http.MethodGet, "/api/v1/projects/17/issues", katatest.Fault{Status: 403})
	_, err := c.FindByMetadata(t.Context(), "k", "v")
	require.Error(t, err)
	assert.Equal(t, http.StatusForbidden, StatusOf(err))
	assert.Empty(t, c.InstanceUID(), "invalidated connection must not expose stale identity")
	before := len(s.RequestsMatching(http.MethodGet, "/api/v1/health"))
	assert.True(t, c.Ready(t.Context()))
	assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/health"), before+1)
}

func TestConnNeverLogsToken(t *testing.T) {
	const secret = "tok-SECRET-9f2c"
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := katatest.New(t)
	s.SetToken("the-real-token")
	cfg := readyConfig(s.Endpoint())
	cfg.Token = secret
	c := NewConn(cfg)
	st := c.Status(t.Context(), true)
	assert.Equal(t, StateUnauthenticated, st.State)

	s.SetToken(secret)
	st = c.Status(t.Context(), true)
	assert.Equal(t, StateReady, st.State)
	_, err := c.GetIssue(t.Context(), "missing")
	require.Error(t, err)

	require.NotEmpty(t, s.Requests())
	assert.Equal(t, "Bearer "+secret, s.Requests()[len(s.Requests())-1].Authorization, "token is actually sent")
	assert.Contains(t, buf.String(), "kata:", "state transitions are logged")
	assert.NotContains(t, buf.String(), secret)
	assert.NotContains(t, st.Message, secret)
	assert.NotContains(t, err.Error(), secret)
}
