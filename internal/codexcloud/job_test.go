package codexcloud

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/codexcloudtest"
	"go.kenn.io/agentsview/internal/config"
)

func TestCodexCloudJobOffStates(t *testing.T) {
	cfgErr := &config.CodexCloudConfigError{Path: "/data/config.toml", Key: "endpoint", Reason: "expected a URL string"}
	for _, tt := range []struct {
		name    string
		noSync  bool
		gates   func(Gates) Gates
		state   State
		message string
	}{
		{
			name: "no sync", noSync: true, gates: func(g Gates) Gates { return g },
			state: StateOffNoSync, message: "Off: this daemon does not sync sources. A command run with --no-sync started it, " +
				"or its archive is archive-only. A daemon started with `agentsview serve` on a normal archive imports Codex cloud chats.",
		},
		{
			name: "disabled_agents wins over enabled",
			gates: func(g Gates) Gates {
				g.Disabled = true
				return g
			},
			state: StateOffDisabledAgents, message: "Off: listed in disabled_agents.",
		},
		{
			name: "config error",
			gates: func(g Gates) Gates {
				g.CodexCloud.Enabled, g.CodexCloud.Err = false, cfgErr
				return g
			},
			state: StateConfigError, message: cfgErr.Error(),
		},
		{
			name: "enabled is false",
			gates: func(g Gates) Gates {
				g.CodexCloud.Enabled = false
				return g
			},
			state: StateOffDisabled, message: "Off: enabled is false.",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := newPassEnv(t)
			env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
			env.run(t)
			require.NotEmpty(t, env.host.Requests())
			on := env.syncer.Gates
			before := len(env.host.Handshakes())
			env.syncer.Gates = func() Gates {
				g := on()
				g.CodexCloud.Present = true
				return tt.gates(g)
			}
			env.syncer.NoSync = tt.noSync

			env.run(t)

			assert.Len(t, env.host.Handshakes(), before, "an off source contacts nobody")
			assert.Equal(t, []HomeStatus{{Key: SourceRowKey, Homes: []string{}, State: tt.state, Message: tt.message}},
				env.syncer.Status.Snapshot(), "the off row replaces the account rows")
		})
	}
}

func TestUnconfiguredSourceHasNoStatusRow(t *testing.T) {
	env := newPassEnv(t)
	env.run(t)
	require.NotEmpty(t, env.syncer.Status.Snapshot())
	before := len(env.host.Handshakes())
	env.syncer.Gates = func() Gates { return Gates{} }
	env.syncer.NoSync = true

	env.run(t)

	assert.Empty(t, env.syncer.Status.Snapshot())
	assert.Len(t, env.host.Handshakes(), before)
}

func TestCodexCloudHandEditTakesEffect(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	enabled := false
	load := func() (config.Config, error) {
		return config.Config{CodexCloud: config.CodexCloudConfig{
			Present: true, Enabled: enabled, Endpoint: env.host.URL(),
		}}, nil
	}
	env.syncer.Gates = NewGateSource("/data/config.toml", config.Config{}, load, func(string) string { return env.homes[0] }, "/unused")

	env.run(t)
	assert.Empty(t, env.host.Handshakes())

	enabled = true
	env.run(t)
	_, err := os.Stat(env.mirrorPath(t, "a"))
	require.NoError(t, err, "the next run after the edit fetches")
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Equal(t, StateUpToDate, row.State)
	_, ok = env.syncer.Status.Row(SourceRowKey)
	assert.False(t, ok, "the off row goes once fetching starts")
}

func TestCodexCloudUnreadableConfigKeepsLastGood(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
	var readErr error
	load := func() (config.Config, error) {
		if readErr != nil {
			return config.Config{}, readErr
		}
		return config.Config{CodexCloud: config.CodexCloudConfig{
			Present: true, Enabled: true, Endpoint: env.host.URL(),
		}}, nil
	}
	env.syncer.Gates = NewGateSource("/data/config.toml", config.Config{}, load, func(string) string { return env.homes[0] }, "/unused")
	env.run(t)

	readErr = errors.New("toml: line 3: expected '='")
	env.threads.Set(finishedThread("b", ago(time.Minute), message("n", "two")))
	env.clock.Advance(time.Minute)
	env.run(t)

	_, err := os.Stat(env.mirrorPath(t, "b"))
	require.NoError(t, err, "sync keeps running on the last good settings")
	row, ok := env.syncer.Status.Row(SourceRowKey)
	require.True(t, ok)
	assert.Equal(t, StateConfigError, row.State)
	assert.Equal(t,
		"/data/config.toml could not be read: toml: line 3: expected '='. Codex cloud import keeps its last good settings.",
		row.Message)

	readErr = nil
	env.run(t)
	_, ok = env.syncer.Status.Row(SourceRowKey)
	assert.False(t, ok, "a good read clears the row")
}

func TestStatusMessages(t *testing.T) {
	t.Run("401 says codex login", func(t *testing.T) {
		env := newPassEnv(t)
		env.host.RefuseHandshake(http.StatusUnauthorized, nil, "")
		env.run(t)
		row, ok := env.syncer.Status.Row("/homes/a")
		require.True(t, ok)
		assert.Contains(t, row.Message, "`codex login`")
	})
	t.Run("401 in auto mode is the keyring", func(t *testing.T) {
		env := newPassEnv(t)
		login := env.logins["/homes/a"]
		login.Store = "auto"
		env.logins["/homes/a"] = login
		env.host.RefuseHandshake(http.StatusUnauthorized, nil, "")
		env.run(t)
		row, ok := env.syncer.Status.Row("/homes/a")
		require.True(t, ok)
		assert.Contains(t, row.Message, "OpenAI did not accept the ChatGPT login")
		assert.Contains(t, row.Message, "Either that saved login expired")
		assert.Contains(t, row.Message, "set `cli_auth_credentials_store = \"file\"`")
		assert.Equal(t, 1, strings.Count(row.Message, "codex login"), "the login command is named once")
		assert.Contains(t, row.Message, "plain file that any program running as you can read")
	})
	t.Run("403 does not", func(t *testing.T) {
		env := newPassEnv(t)
		env.host.RefuseHandshake(http.StatusForbidden, nil, "")
		env.run(t)
		row, ok := env.syncer.Status.Row(string(passKey))
		require.True(t, ok)
		assert.Equal(t, StateError, row.State)
		assert.Equal(t, Message(FailRefused, time.Time{}), row.Message)
		assert.NotContains(t, row.Message, "codex login")
	})
	t.Run("retrying shows count and message, never up to date", func(t *testing.T) {
		env := newPassEnv(t)
		env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))
		env.threads.Set(finishedThread("b", ago(2*time.Hour), message("n", "two")))
		fail, mu := true, &sync.Mutex{}
		env.failThread("a", &fail, mu)
		env.run(t)
		row, ok := env.syncer.Status.Row(string(passKey))
		require.True(t, ok)
		assert.Equal(t, StateRetrying, row.State)
		assert.Equal(t, 1, row.RetryCount)
		lines := StatusLines(row)
		assert.Contains(t, lines[0], "1 chat could not be imported")
		assert.Contains(t, lines[0], Message(FailThread, time.Time{}))
		assert.Contains(t, lines[0], "retries a failed chat for 7 days; a change to the chat in OpenAI restarts the 7 days",
			"retries end, and the user should know when")
		assert.NotContains(t, lines[0], "Up to date")
	})
}

func TestStatusLinesRetrying(t *testing.T) {
	first := time.Date(2027, 1, 15, 9, 30, 0, 0, time.UTC)
	row := HomeStatus{
		Key: "k", Homes: []string{"/homes/a"}, State: StateRetrying, RetryCount: 2,
		OldestFailure: &first, Message: Message(FailThread, time.Time{}),
	}
	lines := StatusLines(row)
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "2 chats could not be imported")
	assert.Contains(t, lines[0], first.Local().Format("Jan 2 15:04"))
	assert.Contains(t, lines[0], Message(FailThread, time.Time{}))
	assert.NotContains(t, lines[0], "Up to date")
}

// TestTokenCanary sends a unique token through every path that writes:
// a good sync with plan usage, a refresh request, and refused handshakes
// and plan usage requests whose bodies echo the token. No file, log line, or status output may contain it.
func TestTokenCanary(t *testing.T) {
	const access = "canary-access-9d27e1c4"
	home := writeHome(t, homeSpec{
		store: "file", authJSON: true, access: access,
		claim: map[string]any{"chatgpt_user_id": "canary-user-51b0"},
	})
	idToken := fakeIDToken(t, map[string]any{"chatgpt_user_id": "canary-user-51b0"})
	secrets := []string{access, idToken, "test-refresh-token"}

	var logs bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(prev) })

	env := newPassEnv(t)
	env.syncer.Log = log.New(&logs, "", 0)
	env.syncer.ReadLogin = ReadLogin
	usage := codexcloudtest.NewUsageHost(t, fetchNow)
	usage.SetRow("a", codexcloudtest.Row("a", "available", nil, nil))
	endpoint, usageEndpoint := env.host.URL(), usage.URL()
	env.syncer.Gates = func() Gates {
		return Gates{
			CodexCloud: config.CodexCloudConfig{
				Present: true, Enabled: true, Endpoint: endpoint, UsageEndpoint: usageEndpoint,
			},
			Homes: []string{home},
		}
	}
	env.threads.Set(finishedThread("a", ago(time.Hour), message("m", "one")))

	var outputs []string
	capture := func() {
		data, err := json.Marshal(env.syncer.Status.Snapshot())
		require.NoError(t, err)
		outputs = append(outputs, string(data))
		for _, row := range env.syncer.Status.Snapshot() {
			outputs = append(outputs, StatusLines(row)...)
		}
	}

	env.run(t)
	capture()
	require.Len(t, usage.Requests(), 1)
	assert.Equal(t, "Bearer "+access, usage.Requests()[0].Header.Get("Authorization"))

	// A warm join through the same login, ended by a refresh request.
	monitor := &Monitor{
		Store: env.syncer.Store, Waits: env.syncer.Waits, Status: env.syncer.Status, Now: env.syncer.Now,
		Dial: Dial, ReadLogin: ReadLogin, Version: "test", Log: env.syncer.Log,
		Gates: func() Gates {
			gates := env.syncer.Gates()
			gates.CodexCloud.LiveTokens = true
			return gates
		},
	}
	env.threads.Set(runningThread("live", "warm"))
	require.NoError(t, monitor.Discover(t.Context()))
	require.Len(t, monitor.Joined(passKeyFor(t, home)), 1)
	require.NoError(t, env.host.NotifyThread(t.Context(), "live", tokenUsageMethod,
		tokenParams("live", "live-turn-2", [6]float64{10, 0, 0, 5, 0, 15}, [6]float64{10, 0, 0, 5, 0, 15})))
	require.NoError(t, env.host.ServerRequestThread(t.Context(), "live", refreshMethod, map[string]any{}))
	require.Eventually(t, func() bool { return len(monitor.Joined(passKeyFor(t, home))) == 0 },
		10*time.Second, 5*time.Millisecond)
	monitor.CloseAll()
	capture()

	env.clock.Advance(25 * time.Hour)
	env.threads.Set(finishedThread("b", ago(time.Minute), message("n", "two")))
	env.host.Handle("thread/turns/list", func(params jsontext.Value) (any, *codexcloudtest.Error) {
		_ = env.host.ServerRequest(t.Context(), "account/chatgptAuthTokens/refresh", map[string]any{})
		return env.threads.TurnsList(params)
	})
	env.run(t)
	capture()

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		// A new auth.json mtime ends a signed-out or refresh wait.
		now := env.clock.Now().Add(25 * time.Hour)
		env.clock.Set(now)
		require.NoError(t, os.Chtimes(filepath.Join(home, "auth.json"), now, now))
		env.host.RefuseHandshake(status, http.Header{"X-Echo": {access}}, "bad token "+access)
		usage.Refuse(status, http.Header{"X-Echo": {access}}, "bad token "+access)
		env.run(t)
		capture()
	}

	all := strings.Join(outputs, "\n")
	assert.Greater(t, len(usage.Requests()), 1, "refused plan usage requests ran")
	assert.Contains(t, all, "Plan usage as of")
	for _, f := range []Failure{FailRefreshRequested, FailSignedOut, FailRefused, FailServer} {
		assert.Contains(t, all, Message(f, time.Time{}), "the canary reached the %s path", f)
	}

	files := 0
	require.NoError(t, filepath.WalkDir(env.root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		files++
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for _, secret := range secrets {
			assert.NotContains(t, string(data), secret, "file %s", path)
		}
		return nil
	}))
	require.NotZero(t, files)
	require.NotEmpty(t, logs.String(), "the refused passes log their errors")
	for _, secret := range secrets {
		assert.NotContains(t, logs.String(), secret, "log")
		for _, out := range outputs {
			assert.NotContains(t, out, secret, "status")
		}
	}
}

// passKeyFor reads the account key of the login in home.
func passKeyFor(t *testing.T, home string) AccountKey {
	t.Helper()
	login, state, err := ReadLogin(home)
	require.NoError(t, err)
	require.Equal(t, HomeOK, state)
	return login.Key
}

func TestSignedInHomeDropsItsErrorRow(t *testing.T) {
	env := newPassEnv(t)
	signedOut := true
	env.syncer.ReadLogin = func(home string) (Login, HomeState, error) {
		if signedOut {
			return Login{Home: home}, HomeSignedOut, nil
		}
		return env.logins[home], HomeOK, nil
	}
	env.run(t)
	_, ok := env.syncer.Status.Row("/homes/a")
	require.True(t, ok)

	signedOut = false
	env.run(t)

	_, ok = env.syncer.Status.Row("/homes/a")
	assert.False(t, ok, "the signed-out row goes once the home signs in")
	row, ok := env.syncer.Status.Row(string(passKey))
	require.True(t, ok)
	assert.Equal(t, StateUpToDate, row.State)
}
