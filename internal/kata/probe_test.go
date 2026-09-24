package kata

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func readyConfig(endpoint string) Config {
	return Config{Enabled: true, Hub: true, Endpoint: endpoint, Project: "agentsview", Actor: "agentsview"}
}

func TestProbeStates(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*katatest.Server)
		mutate  func(*Config)
		want    State
		message string
	}{
		{name: "disabled", mutate: func(c *Config) { c.Enabled = false }, want: StateDisabled},
		{name: "ready", want: StateReady},
		{name: "schema metadata is hidden", setup: func(s *katatest.Server) {
			s.SetSchemaVersion("0.22.0+" + strings.Repeat("capabilitySecret123", 100))
		}, want: StateReady},
		{name: "old schema", setup: func(s *katatest.Server) { s.SetSchemaVersion("0.20.0") }, want: StateIncompatible, message: "0.21.0"},
		{name: "missing schema", setup: func(s *katatest.Server) { s.SetSchemaVersion("") }, want: StateIncompatible},
		{name: "malformed schema", setup: func(s *katatest.Server) { s.SetSchemaVersion("bad") }, want: StateIncompatible},
		{name: "schema with secret url", setup: func(s *katatest.Server) {
			s.SetSchemaVersion("https://example.test/capability/secret-schema-path")
		}, want: StateIncompatible, message: "0.21.0"},
		{name: "health missing", setup: func(s *katatest.Server) { s.Fail(http.MethodGet, "/api/v1/health", katatest.Fault{Status: 404}) }, want: StateIncompatible},
		{name: "health unavailable", setup: func(s *katatest.Server) { s.Fail(http.MethodGet, "/api/v1/health", katatest.Fault{Status: 503}) }, want: StateUnavailable},
		{name: "html health", setup: func(s *katatest.Server) {
			s.Fail(http.MethodGet, "/api/v1/health", katatest.Fault{Status: 200, RawBody: "<html>login</html>"})
		}, want: StateUnavailable},
		{name: "redirect", setup: func(s *katatest.Server) {
			s.Fail(http.MethodGet, "/api/v1/health", katatest.Fault{Status: 302, Location: "https://login.example.test/"})
		}, want: StateUnavailable, message: "redirect"},
		{name: "missing instance uid", setup: func(s *katatest.Server) { s.SetInstance("", "0.18.0") }, want: StateIncompatible},
		{name: "missing instance version", setup: func(s *katatest.Server) {
			s.SetInstance("01J00000000000000000000002", "")
		}, want: StateIncompatible},
		{name: "invalid instance uid", setup: func(s *katatest.Server) {
			s.SetInstance("https://example.test/capability/secret-instance-uid", "0.18.0")
		}, want: StateIncompatible},
		{name: "missing token", setup: func(s *katatest.Server) { s.SetToken("tok-1") }, want: StateUnauthenticated},
		{name: "wrong token", setup: func(s *katatest.Server) { s.SetToken("tok-1") }, mutate: func(c *Config) { c.Token = "tok-2" }, want: StateUnauthenticated},
		{name: "correct token", setup: func(s *katatest.Server) { s.SetToken("tok-1") }, mutate: func(c *Config) { c.Token = "tok-1" }, want: StateReady},
		{name: "empty token variable", mutate: func(c *Config) { c.TokenRequired = true }, want: StateUnauthenticated, message: "token_env"},
		{name: "wrong project", mutate: func(c *Config) { c.Project = "missing" }, want: StateWrongProject, message: "never creates"},
		{name: "inactive project", setup: func(s *katatest.Server) {
			s.SetProjects(katatest.Project{ID: 17, UID: "01J00000000000000000000001", Name: "agentsview", Active: false})
		}, want: StateWrongProject},
		{name: "deleted project", setup: func(s *katatest.Server) {
			s.SetProjects(katatest.Project{ID: 17, UID: "01J00000000000000000000001", Name: "agentsview", Active: true, Deleted: true})
		}, want: StateWrongProject},
		{name: "project with zero id", setup: func(s *katatest.Server) {
			s.SetProjects(katatest.Project{ID: 0, UID: "01J00000000000000000000001", Name: "agentsview", Active: true})
		}, want: StateIncompatible},
		{name: "project with empty uid", setup: func(s *katatest.Server) {
			s.SetProjects(katatest.Project{ID: 17, UID: "", Name: "agentsview", Active: true})
		}, want: StateIncompatible},
		{name: "project with invalid uid", setup: func(s *katatest.Server) {
			s.SetProjects(katatest.Project{ID: 17, UID: "https://example.test/capability/secret-project-uid", Name: "agentsview", Active: true})
		}, want: StateIncompatible},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			if tt.setup != nil {
				tt.setup(s)
			}
			cfg := readyConfig(s.Endpoint())
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			st, client := Probe(t.Context(), cfg)
			assert.Equal(t, tt.want, st.State, st.Message)
			assert.Equal(t, cfg.Project, st.Project)
			if tt.message != "" {
				assert.Contains(t, st.Message, tt.message)
			}
			if tt.name == "schema with secret url" {
				assert.NotContains(t, st.Message, "secret-schema-path")
				assert.NotContains(t, st.APISchemaVersion, "secret-schema-path")
			}
			if tt.name == "schema metadata is hidden" {
				assert.Equal(t, "0.22.0", st.APISchemaVersion)
				assert.NotContains(t, st.Message, "capabilitySecret123")
			}
			if tt.name == "invalid instance uid" {
				assert.Empty(t, st.InstanceUID)
				assert.NotContains(t, st.Message, "secret-instance-uid")
			}
			if tt.name == "project with invalid uid" {
				assert.NotContains(t, st.Message, "secret-project-uid")
			}
			if tt.want == StateReady {
				require.NotNil(t, client)
				assert.Equal(t, "01J00000000000000000000002", st.InstanceUID)
				if tt.name != "schema metadata is hidden" {
					assert.Equal(t, "0.22.0", st.APISchemaVersion)
				}
				uid, err := client.ProjectUID(t.Context())
				require.NoError(t, err)
				assert.Equal(t, "01J00000000000000000000001", uid)
			} else {
				assert.Nil(t, client)
			}
			if tt.name == "wrong project" {
				assert.Empty(t, s.RequestsMatching(http.MethodPost, "/api/v1/projects"))
			}
		})
	}
}

func TestProbeProjectListEnvelope(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		want       State
	}{
		{name: "missing projects", body: `{}`, want: StateIncompatible},
		{name: "null projects", body: `{"projects":null}`, want: StateIncompatible},
		{name: "legacy items", body: `{"items":[]}`, want: StateIncompatible},
		{name: "empty projects", body: `{"projects":[]}`, want: StateWrongProject},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := katatest.New(t)
			s.Fail(http.MethodGet, "/api/v1/projects", katatest.Fault{Status: http.StatusOK, RawBody: tt.body})
			st, client := Probe(t.Context(), readyConfig(s.Endpoint()))
			assert.Equal(t, tt.want, st.State, st.Message)
			assert.Nil(t, client)
			assert.Len(t, s.RequestsMatching(http.MethodGet, "/api/v1/projects"), 1)
		})
	}
}

func TestProbeDoesNotExposeServerErrorDetails(t *testing.T) {
	s := katatest.New(t)
	s.Fail(http.MethodGet, "/api/v1/health", katatest.Fault{
		Status:  503,
		Code:    "capabilitySecret123",
		Message: "upstream https://example.test/capability/secret-path-token failed",
	})
	st, client := Probe(t.Context(), readyConfig(s.Endpoint()))
	assert.Equal(t, StateUnavailable, st.State)
	assert.Nil(t, client)
	assert.Contains(t, st.Message, "503")
	assert.NotContains(t, st.Message, "capabilitySecret123")
	assert.NotContains(t, st.Message, "secret-path-token")
	assert.NotContains(t, st.Message, "https://")
}

func TestProbeNotHubMakesNoRequests(t *testing.T) {
	for _, endpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "discovery", true: "configured"}[endpoint], func(t *testing.T) {
			previous := locateCommand
			t.Cleanup(func() { locateCommand = previous })
			locateCommand = func(context.Context) ([]byte, error) {
				require.FailNow(t, "pusher attempted discovery")
				return nil, nil
			}
			s := katatest.New(t)
			cfg := readyConfig("")
			if endpoint {
				cfg.Endpoint = s.Endpoint()
			}
			cfg.Hub = false
			st, client := Probe(t.Context(), cfg)
			assert.Equal(t, StateNotHub, st.State)
			assert.Contains(t, st.Message, "only the agentsview hub files")
			assert.Nil(t, client)
			assert.Empty(t, s.Requests())
		})
	}
}

func TestProbeTransport(t *testing.T) {
	_, unixEndpoint := katatest.NewUnix(t)
	for _, tt := range []struct {
		name, endpoint string
		want           State
	}{
		{"unix", unixEndpoint, StateReady},
		{"https without token", "https://kata.example.test", StateUnauthenticated},
		{"unreachable", "http://127.0.0.1:1", StateUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := readyConfig(tt.endpoint)
			cfg.Timeout = time.Second
			st, client := Probe(t.Context(), cfg)
			assert.Equal(t, tt.want, st.State, st.Message)
			assert.Equal(t, tt.want == StateReady, client != nil)
		})
	}
}

func TestCompatibleAPISchema(t *testing.T) {
	for _, tt := range []struct {
		version string
		want    bool
	}{{"0.21.0", true}, {"v0.22.0", true}, {"1.0.0", true}, {"0.20.9", false}, {"", false}, {"garbage", false}} {
		t.Run(tt.version, func(t *testing.T) { assert.Equal(t, tt.want, compatibleAPISchema(tt.version)) })
	}
}

func TestResolveEndpoint(t *testing.T) {
	for _, tt := range []struct {
		name, output    string
		failure         error
		want, wantError string
	}{
		{"unix", `{"network":"unix","address":"unix:///tmp/k.sock"}`, nil, "unix:///tmp/k.sock", ""},
		{"tcp", `{"network":"tcp","address":"127.0.0.1:7777","request_base_url":"http://127.0.0.1:7777"}`, nil, "http://127.0.0.1:7777", ""},
		{"remote", `{"network":"tcp","request_base_url":"https://kata.example.test","extra":1}`, nil, "https://kata.example.test", ""},
		{"command failure", "", errors.New("missing"), "", "kata daemon locate failed"},
		{"not json", "unix:///tmp/k.sock", nil, "", "drifted"},
		{"unknown network", `{"network":"pipe","address":"x"}`, nil, "", "unsupported network"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			previous := locateCommand
			t.Cleanup(func() { locateCommand = previous })
			locateCommand = func(ctx context.Context) ([]byte, error) {
				_, bounded := ctx.Deadline()
				assert.True(t, bounded)
				return []byte(tt.output), tt.failure
			}
			got, err := resolveEndpoint(t.Context(), "")
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestResolveEndpointHangingCommandIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires an executable shell script")
	}
	previous := locateCommand
	t.Cleanup(func() { locateCommand = previous })
	locateCommand = func(ctx context.Context) ([]byte, error) {
		return previous(ctx)
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kata"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := resolveEndpoint(ctx, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kata daemon locate failed")
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestResolveEndpointChildHoldingStdoutIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires an executable shell script")
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kata"), []byte("#!/bin/sh\nsleep 2 &\nexit 0\n"), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := resolveEndpoint(ctx, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kata daemon locate failed")
	assert.Less(t, time.Since(start), time.Second)
}

func TestResolveEndpointAcceptsCompleteOutputFromChildHoldingStdout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires an executable shell script")
	}
	dir := t.TempDir()
	output := `{"network":"tcp","request_base_url":"http://127.0.0.1:7777"}`
	script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\nsleep 2 &\nexit 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kata"), []byte(script), 0o700))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	endpoint, err := resolveEndpoint(ctx, "")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:7777", endpoint)
	assert.Less(t, time.Since(start), time.Second)
}

func TestResolveEndpointRejectsCanceledLocateOutput(t *testing.T) {
	previous := locateCommand
	t.Cleanup(func() { locateCommand = previous })
	locateCommand = func(ctx context.Context) ([]byte, error) {
		require.ErrorIs(t, ctx.Err(), context.Canceled)
		return []byte(`{"network":"tcp","request_base_url":"http://127.0.0.1:7777"}`), exec.ErrWaitDelay
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	endpoint, err := resolveEndpoint(ctx, "")
	require.Error(t, err)
	assert.Empty(t, endpoint)
	assert.Contains(t, err.Error(), "kata daemon locate failed")
}

func TestWithProjectResolvesStaleIDOnce(t *testing.T) {
	s := katatest.New(t)
	st, client := Probe(t.Context(), readyConfig(s.Endpoint()))
	require.Equal(t, StateReady, st.State)
	require.NotNil(t, client)
	s.SetProjects(katatest.Project{ID: 23, UID: "01J00000000000000000000023", Name: "agentsview", Active: true})
	var ids []int64
	err := client.withProject(t.Context(), func(id int64) error {
		ids = append(ids, id)
		if id == 17 {
			return &APIError{Status: 404, Code: "project_not_found"}
		}
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{17, 23}, ids)
	uid, err := client.ProjectUID(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "01J00000000000000000000023", uid)
}

func TestProbeDiscoveryFailureIsUnavailable(t *testing.T) {
	previous := locateCommand
	t.Cleanup(func() { locateCommand = previous })
	locateCommand = func(context.Context) ([]byte, error) { return nil, errors.New("boom") }
	st, client := Probe(t.Context(), readyConfig(""))
	assert.Equal(t, StateUnavailable, st.State)
	assert.Nil(t, client)
}

func TestConfigFrom(t *testing.T) {
	t.Setenv("AGENTSVIEW_TEST_KATA_TOKEN", "tok-1")
	got := ConfigFrom(config.KataConfig{Enabled: true, Endpoint: " https://kata.example.test ", TokenEnv: "AGENTSVIEW_TEST_KATA_TOKEN", Project: "p", Actor: "a", AllowInsecure: true, Timeout: 3 * time.Second}, true)
	assert.Equal(t, Config{Enabled: true, Hub: true, Endpoint: "https://kata.example.test", TokenEnv: "AGENTSVIEW_TEST_KATA_TOKEN", TokenRequired: true, Project: "p", Actor: "a", AllowInsecure: true, Timeout: 3 * time.Second}, got)
	assert.False(t, ConfigFrom(config.KataConfig{Enabled: true}, false).Hub)
	assert.Equal(t, "agentsview", ConfigFrom(config.KataConfig{}, true).Project)
	assert.Equal(t, "agentsview", ConfigFrom(config.KataConfig{}, true).Actor)
	assert.Equal(t, DefaultTimeout, ConfigFrom(config.KataConfig{}, true).Timeout)
}
