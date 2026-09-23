package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/kata"
	"go.kenn.io/agentsview/internal/kata/katatest"
)

func runKataCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout bytes.Buffer
	root := newRootCommand()
	root.SetOut(&stdout)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs(args)
	_, err := root.ExecuteC()
	return stdout.String(), err
}

func TestKataStatusCommandLocalProbe(t *testing.T) {
	fake := katatest.New(t)
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
		"[kata]\nenabled = true\nendpoint = \""+fake.Endpoint()+"\"\n"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_PG_URL", "")

	tests := []struct {
		name  string
		args  []string
		check func(t *testing.T, out string)
	}{
		{name: "json", args: []string{"kata", "status", "--json"}, check: func(t *testing.T, out string) {
			t.Helper()
			var st kata.Status
			require.NoError(t, json.Unmarshal([]byte(out), &st))
			assert.Equal(t, kata.StateReady, st.State)
			assert.Equal(t, "agentsview", st.Project)
		}},
		{name: "format json", args: []string{"kata", "status", "--format", "json"}, check: func(t *testing.T, out string) {
			t.Helper()
			var st kata.Status
			require.NoError(t, json.Unmarshal([]byte(out), &st))
			assert.Equal(t, kata.StateReady, st.State)
			assert.Equal(t, "agentsview", st.Project)
		}},
		{name: "format human", args: []string{"kata", "status", "--format", "human"}, check: func(t *testing.T, out string) {
			t.Helper()
			assert.Contains(t, out, "State:    ready\n")
			assert.Contains(t, out, "Project:  agentsview\n")
		}},
		{name: "human", args: []string{"kata", "status"}, check: func(t *testing.T, out string) {
			t.Helper()
			assert.Contains(t, out, "State:    ready\n")
			assert.Contains(t, out, "Project:  agentsview\n")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runKataCommand(t, tt.args...)
			require.NoError(t, err)
			tt.check(t, out)
		})
	}
}

func TestKataStatusCommandDisabledByDefault(t *testing.T) {
	t.Setenv("AGENTSVIEW_DATA_DIR", t.TempDir())
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_PG_URL", "")
	out, err := runKataCommand(t, "kata", "status")
	require.NoError(t, err, "a non-ready state is information, not a command failure")
	assert.Contains(t, out, "State:    disabled\n")
}

func TestKataStatusCommandLocalProbeDoesNotStartDaemon(t *testing.T) {
	fake := katatest.New(t)
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
		"[kata]\nenabled = true\nendpoint = \""+fake.Endpoint()+"\"\n"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "0")
	t.Setenv("AGENTSVIEW_PG_URL", "")
	started := false
	stubStartBackgroundServeForTransport(t, func(
		context.Context, *config.Config, time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		return nil, errors.New("unexpected daemon start")
	})

	out, err := runKataCommand(t, "kata", "status")
	require.NoError(t, err)
	assert.False(t, started, "status must not start or restart a daemon")
	assert.Contains(t, out, "State:    ready\n")
}

func TestKataStatusCommandUnreachableOwnerDoesNotProbeLocally(t *testing.T) {
	fake := katatest.New(t)
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
		"[kata]\nenabled = true\nendpoint = \""+fake.Endpoint()+"\"\n"), 0o600))
	writeUnreachableDaemonRuntime(t, dataDir, false)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_PG_URL", "")

	out, err := runKataCommand(t, "kata", "status")
	require.ErrorIs(t, err, errLocalDaemonUnreachable)
	assert.Empty(t, out)
	assert.Empty(t, fake.Requests(), "local config must not replace the owning daemon's status")
}

func TestKataStatusCommandIncompatibleOwnerReportsVersionsWithoutLocalProbe(t *testing.T) {
	tests := []struct {
		name         string
		writeRuntime func(t *testing.T, dir, host string, port int)
		wantReason   string
		wantHint     string
	}{
		{
			name: "older daemon API",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				writeIncompatibleDaemonRuntime(t, dir, host, port, "old", false)
			},
			wantReason: fmt.Sprintf("daemon API version 0 is incompatible with client API version %d", daemonAPIVersion),
			wantHint:   "CLI: run `agentsview daemon restart`",
		},
		{
			name: "newer daemon data",
			writeRuntime: func(t *testing.T, dir, host string, port int) {
				t.Helper()
				writeNewerDataVersionDaemonRuntime(t, dir, host, port, "new")
			},
			wantReason: fmt.Sprintf("daemon data version %d is incompatible with client data version %d", db.CurrentDataVersion()+1, db.CurrentDataVersion()),
			wantHint:   "Upgrade this agentsview install",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := katatest.New(t)
			dataDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
				"[kata]\nenabled = true\nendpoint = \""+fake.Endpoint()+"\"\n"), 0o600))
			host, port := testPingServer(t)
			tt.writeRuntime(t, dataDir, host, port)
			t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
			t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
			t.Setenv("AGENTSVIEW_PG_URL", "")

			out, err := runKataCommand(t, "kata", "status")
			require.Error(t, err)
			assert.Empty(t, out)
			assert.Contains(t, err.Error(), tt.wantReason)
			assert.Contains(t, err.Error(), tt.wantHint)
			assert.Empty(t, fake.Requests(), "incompatible daemon must not trigger a local Kata probe")
		})
	}
}

func TestKataStatusCommandTransportErrorDoesNotProbeLocally(t *testing.T) {
	fake := katatest.New(t)
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
		"[kata]\nenabled = true\nendpoint = \""+fake.Endpoint()+"\"\n"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	t.Setenv("AGENTSVIEW_PG_URL", "")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	root := newRootCommand()
	root.SetContext(ctx)
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"kata", "status"})
	_, err := root.ExecuteC()
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, out.String())
	assert.Empty(t, fake.Requests(), "a failed daemon lookup must not become a local probe")
}

func TestKataStatusCommandPusherIsNotHub(t *testing.T) {
	fake := katatest.New(t)
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(
		"[pg]\nurl = \"postgres://pg.example.test/agentsview\"\n\n[kata]\nenabled = true\nendpoint = \""+fake.Endpoint()+"\"\n"), 0o600))
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	out, err := runKataCommand(t, "kata", "status")
	require.NoError(t, err)
	assert.Contains(t, out, "State:    not_hub\n")
	assert.Empty(t, fake.Requests(), "a pusher never contacts Kata")
}

func TestKataStatusCommandUsesServer(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/kata/status", r.URL.Path)
		assert.Equal(t, "Bearer srv-token", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"wrong_project","project":"agentsview","message":"no active project"}`))
	}))
	t.Cleanup(ts.Close)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "srv-token")
	out, err := runKataCommand(t, "kata", "status", "--server", ts.URL, "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"state":"wrong_project"`)
}

type kataStatusRoundTripper func(*http.Request) (*http.Response, error)

func (f kataStatusRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestKataStatusRequestBudget(t *testing.T) {
	tests := []struct {
		name        string
		perRequest  time.Duration
		wantTimeout time.Duration
	}{
		{name: "default", wantTimeout: 40 * time.Second},
		{name: "configured", perRequest: 20 * time.Second, wantTimeout: 70 * time.Second},
		{name: "large timeout saturates", perRequest: time.Duration(1<<63 - 1), wantTimeout: time.Duration(1<<63 - 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.wantTimeout, kataStatusRequestBudget(tt.perRequest))
		})
	}
}

func TestKataStatusCommandDaemonRequestTimeout(t *testing.T) {
	tests := []struct {
		name        string
		config      string
		wantTimeout time.Duration
	}{
		{name: "explicit server uses default probe budget", wantTimeout: 40 * time.Second},
		{name: "owning daemon uses configured probe budget", config: "[kata]\ntimeout = \"20s\"\n", wantTimeout: 70 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seenTimeout time.Duration
			var hasDeadline bool
			var seenPath string
			originalClient := kataDaemonHTTPClient
			kataDaemonHTTPClient = &http.Client{
				CheckRedirect: originalClient.CheckRedirect,
				Transport: kataStatusRoundTripper(func(req *http.Request) (*http.Response, error) {
					deadline, ok := req.Context().Deadline()
					hasDeadline = ok
					seenTimeout = time.Until(deadline)
					seenPath = req.URL.Path
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": {"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"state":"ready","project":"agentsview"}`)),
						Request:    req,
					}, nil
				}),
			}
			t.Cleanup(func() { kataDaemonHTTPClient = originalClient })

			args := []string{"kata", "status", "--server", "http://127.0.0.1:1"}
			if tt.config != "" {
				dataDir := t.TempDir()
				require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(tt.config), 0o600))
				host, port := testPingServer(t)
				writeDaemonRuntimeForTest(t, dataDir, host, port, "test", false)
				t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
				args = []string{"kata", "status"}
			}
			out, err := runKataCommand(t, args...)
			require.NoError(t, err)
			assert.Contains(t, out, "State:    ready\n")
			assert.Equal(t, "/api/v1/kata/status", seenPath)
			require.True(t, hasDeadline, "the daemon status request must be bounded")
			assert.InDelta(t, tt.wantTimeout.Seconds(), seenTimeout.Seconds(), 1)
		})
	}
}

func TestKataStatusCommandExplicitServerRespectsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var out bytes.Buffer
	root := newRootCommand()
	root.SetContext(ctx)
	root.SetOut(&out)
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"kata", "status", "--server", "http://127.0.0.1:1"})
	_, err := root.ExecuteC()
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, out.String())
}

func TestKataStatusCommandDoesNotFollowServerRedirect(t *testing.T) {
	var targetRequests atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"ready","project":"agentsview"}`))
	}))
	t.Cleanup(target.Close)
	var sourceRequests atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceRequests.Add(1)
		assert.Equal(t, "Bearer srv-token", r.Header.Get("Authorization"))
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(source.Close)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "srv-token")

	out, err := runKataCommand(t, "kata", "status", "--server", source.URL)
	assert.EqualValues(t, 1, sourceRequests.Load())
	assert.Zero(t, targetRequests.Load(), "redirect target must never receive the bearer request")
	require.Error(t, err)
	assert.Empty(t, out)
	assert.Equal(t, "kata status request failed: HTTP 302", err.Error())
}

func TestKataStatusCommandServerErrorDoesNotEchoBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("secret=synthetic-token"))
	}))
	t.Cleanup(ts.Close)

	out, err := runKataCommand(t, "kata", "status", "--server", ts.URL)
	require.Error(t, err)
	assert.Empty(t, out)
	assert.Equal(t, "kata status request failed: HTTP 503", err.Error())
	assert.NotContains(t, err.Error(), "synthetic-token")
}

func TestKataStatusCommandRejectsInvalidSuccessBody(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "empty body", body: ""},
		{name: "empty object", body: `{}`},
		{name: "null", body: `null`},
		{name: "unknown state", body: `{"state":"unknown","project":"agentsview"}`},
		{name: "empty project", body: `{"state":"ready","project":""}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(ts.Close)

			out, err := runKataCommand(t, "kata", "status", "--server", ts.URL)
			require.Error(t, err)
			assert.Empty(t, out)
			assert.Equal(t, "kata status request failed: invalid HTTP 200 response", err.Error())
		})
	}
}

func TestKataStatusCommandServerErrorsDoNotEchoURL(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if assert.NoError(t, err) {
			assert.NoError(t, conn.Close())
		}
	}))
	t.Cleanup(closed.Close)
	tests := []struct {
		name string
		url  string
	}{
		{name: "invalid URL", url: "http://[::1?token=synthetic-parse"},
		{name: "connection closed", url: closed.URL + "/private?token=synthetic-transport"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := runKataCommand(t, "kata", "status", "--server", tt.url)
			require.Error(t, err)
			assert.Empty(t, out)
			assert.Contains(t, err.Error(), "kata status request failed")
			assert.NotContains(t, err.Error(), "synthetic-")
			assert.NotContains(t, err.Error(), "/private")
		})
	}
}

func TestKataStatusCommandHumanOutputSanitizesDaemonFields(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"ready","project":"ag\u0007entsview","instance_uid":"id\u0085","api_schema_version":"0.21.\u001b0","message":"a\u000db"}`))
	}))
	t.Cleanup(ts.Close)

	out, err := runKataCommand(t, "kata", "status", "--server", ts.URL)
	require.NoError(t, err)
	assert.Equal(t, "State:    ready\nProject:  agentsview\nInstance: id\nAPI:      0.21.0\nMessage:  ab\n", out)
}
