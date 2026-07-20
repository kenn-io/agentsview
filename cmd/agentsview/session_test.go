package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/server"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
)

// decodeCLIJSON unmarshals CLI stdout into T, failing the test with the
// raw output when the bytes are not valid JSON. Centralizes the
// "stdout should be valid JSON" diagnostic used across the CLI tests.
func decodeCLIJSON[T any](t *testing.T, out string) T {
	t.Helper()
	var got T
	require.NoError(t, json.Unmarshal([]byte(out), &got),
		"stdout should be valid JSON: %q", out)
	return got
}

// cliSessionList mirrors the JSON shape emitted by `session list`. Unused
// fields stay at their zero value, so tests that only inspect Sessions or
// NextCursor can decode into the same type.
type cliSessionList struct {
	Sessions   []map[string]any `json:"sessions"`
	NextCursor string           `json:"next_cursor"`
	Total      int              `json:"total"`
}

// cliMessageList mirrors the JSON shape emitted by `session messages`.
type cliMessageList struct {
	Messages []map[string]any `json:"messages"`
	Count    int              `json:"count"`
}

// cliToolCallList mirrors the JSON shape emitted by `session tool-calls`.
type cliToolCallList struct {
	ToolCalls []map[string]any `json:"tool_calls"`
	Count     int              `json:"count"`
}

// sessionUsageCommand resolves the `session usage` cobra command for the
// given full argument slice and parses the flags that follow the session
// id (args[3:]). Callers then pass the command to
// sessionUsageDataForCommand.
func sessionUsageCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	root := newRootCommand()
	cmd, _, err := root.Find(args)
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args[3:]))
	return cmd
}

// writeTestConfig writes body to config.toml under dataDir. Used to set up
// auth-token and intentionally-invalid configuration cases.
func writeTestConfig(t *testing.T, dataDir, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "config.toml"), []byte(body), 0o600,
	))
}

// seedUsageSession upserts a session carrying total-output-token data so
// the offline `session usage` path has something to report.
func seedUsageSession(
	t *testing.T, d *db.DB, id, project, agent string, outputTokens int,
) {
	t.Helper()
	require.NoError(t, d.UpsertSession(db.Session{
		ID:                   id,
		Project:              project,
		Machine:              "usage-host",
		Agent:                agent,
		MessageCount:         2,
		UserMessageCount:     2,
		TotalOutputTokens:    outputTokens,
		HasTotalOutputTokens: true,
	}))
}

// pgReadStoreStub records how the patched openPGReadStore was exercised.
type pgReadStoreStub struct {
	Opened        bool
	CleanupCalled bool
	PG            config.PGConfig
}

// stubPGReadStore replaces openPGReadStore with one that returns store,
// records the PGConfig it was called with, and restores the original on
// cleanup. The caller owns the lifecycle of store; the stub's cleanup only
// flags CleanupCalled so tests can assert the service released the store.
func stubPGReadStore(t *testing.T, store db.Store) *pgReadStoreStub {
	t.Helper()
	stub := &pgReadStoreStub{}
	orig := openPGReadStore
	openPGReadStore = func(
		_ config.Config, pgCfg config.PGConfig,
	) (db.Store, func(), error) {
		stub.Opened = true
		stub.PG = pgCfg
		return store, func() { stub.CleanupCalled = true }, nil
	}
	t.Cleanup(func() { openPGReadStore = orig })
	return stub
}

// forbidPGReadStore replaces openPGReadStore with one that fails the test
// if called, asserting the SQLite path never reaches for PostgreSQL.
func forbidPGReadStore(t *testing.T) {
	t.Helper()
	orig := openPGReadStore
	openPGReadStore = func(
		config.Config, config.PGConfig,
	) (db.Store, func(), error) {
		t.Fatal("openPGReadStore should not be called without --pg")
		return nil, nil, nil
	}
	t.Cleanup(func() { openPGReadStore = orig })
}

// remoteUsageSpec configures newRemoteUsageServer. Zero values fall back to
// the common defaults (codex agent, remote-project, 42 output tokens).
type remoteUsageSpec struct {
	canonicalID   string        // id whose detail and usage routes return 200
	agent         string        // defaults to "codex"
	project       string        // defaults to "remote-project"
	outputTokens  int           // defaults to 42
	bearer        string        // if set, asserts Authorization: Bearer <bearer>
	serverRunning bool          // include server_running:true in the usage body
	usageDelay    time.Duration // optional sleep before serving /usage
}

// remoteUsageRequests records what the fake usage server observed.
type remoteUsageRequests struct {
	UsagePath  string
	UsageQuery string
}

// newRemoteUsageServer stands in for a remote agentsview server answering
// the session-detail and session-usage routes that `session usage --server`
// calls. Any unregistered path 404s, which lets bare/raw session ids fall
// through to the canonical lookup the CLI retries.
func newRemoteUsageServer(
	t *testing.T, spec remoteUsageSpec,
) (*httptest.Server, *remoteUsageRequests) {
	t.Helper()
	if spec.agent == "" {
		spec.agent = "codex"
	}
	if spec.project == "" {
		spec.project = "remote-project"
	}
	if spec.outputTokens == 0 {
		spec.outputTokens = 42
	}
	reqs := &remoteUsageRequests{}
	detailPath := "/api/v1/sessions/" + spec.canonicalID
	usagePath := detailPath + "/usage"
	detailJSON := fmt.Sprintf(`{"id":%q,"agent":%q,"project":%q}`,
		spec.canonicalID, spec.agent, spec.project)
	usageJSON := remoteUsageJSON(spec)
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		if spec.bearer != "" {
			assert.Equal(t, "Bearer "+spec.bearer,
				r.Header.Get("Authorization"))
		} else {
			assert.Empty(t, r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case detailPath:
			writeJSONResponse(w, detailJSON)
		case usagePath:
			reqs.UsagePath = r.URL.Path
			reqs.UsageQuery = r.URL.RawQuery
			if spec.usageDelay > 0 {
				time.Sleep(spec.usageDelay)
			}
			writeJSONResponse(w, usageJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)
	return ts, reqs
}

// remoteUsageJSON builds the session-usage response body for a
// remoteUsageSpec.
func remoteUsageJSON(spec remoteUsageSpec) string {
	server := ""
	if spec.serverRunning {
		server = `,"server_running":true`
	}
	return fmt.Sprintf(`{
		"session_id": %q,
		"agent": %q,
		"project": %q,
		"total_output_tokens": %d,
		"peak_context_tokens": 2048,
		"has_token_data": true,
		"cost_usd": 0.5,
		"has_cost": true,
		"models": ["gpt-5.1"],
		"unpriced_models": []%s
	}`, spec.canonicalID, spec.agent, spec.project, spec.outputTokens, server)
}

func TestSessionHelp_ShowsSubcommands(t *testing.T) {
	t.Parallel()
	cmd := newRootCommand()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs([]string{"session", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	help := buf.String()
	for _, name := range []string{
		"get", "usage", "list", "messages", "tool-calls",
		"export", "sync", "watch",
	} {
		assert.Contains(t, help, name,
			"expected subcommand %q in help", name)
	}
	assert.Contains(t, help, "--format",
		"expected --format persistent flag in help")
	assert.Contains(t, help, "--pg",
		"expected --pg persistent flag in help")
}

// seedSession opens the SQLite DB at dataDir/sessions.db, inserts
// one session with the given id+project (plus sane defaults), and
// closes the DB. Each subtest gets its own dataDir so parallel
// runs don't step on each other.
func seedSession(t *testing.T, dataDir, id, project string) {
	t.Helper()
	seedSessionWithOpts(t, dataDir, id, project, nil)
}

type sessionSeed struct {
	id      string
	project string
	mut     func(*db.Session)
}

func seedEmptyArchive(t *testing.T, dataDir string) {
	t.Helper()
	dbtest.EnsureTestDBAt(t, sessionsDBPath(dataDir))
	registerSQLiteDaemonRuntime(t, dataDir)
}

// seedSessionWithOpts is like seedSession but allows mutation of
// the db.Session before insert via the optional mut callback.
// Use this when a test needs to set signal counts or other
// non-default fields (e.g. ToolFailureSignalCount = 0 to
// exercise the --min-tool-failures flag's *int handling).
func seedSessionWithOpts(
	t *testing.T, dataDir, id, project string,
	mut func(*db.Session),
) {
	t.Helper()
	seedSessionsWithOpts(t, dataDir, sessionSeed{
		id:      id,
		project: project,
		mut:     mut,
	})
}

func seedSessionsWithOpts(t *testing.T, dataDir string, seeds ...sessionSeed) {
	t.Helper()
	seedSessionArchiveRows(t, dataDir, seeds...)
	registerSQLiteDaemonRuntime(t, dataDir)
}

func seedSessionArchiveRows(t *testing.T, dataDir string, seeds ...sessionSeed) {
	t.Helper()
	dbPath := sessionsDBPath(dataDir)
	dbtest.EnsureTestDBAt(t, dbPath)
	d, err := db.Open(dbPath)
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = d.Close()
		}
	})
	for _, seed := range seeds {
		// UserMessageCount >= 2 so seeded sessions pass the default
		// ExcludeOneShot filter in `session list` (one-shot means
		// user_message_count <= 1). See internal/db/analytics.go.
		s := db.Session{
			ID:               seed.id,
			Project:          seed.project,
			Machine:          "m",
			Agent:            "claude",
			MessageCount:     4,
			UserMessageCount: 2,
		}
		if seed.mut != nil {
			seed.mut(&s)
		}
		require.NoError(t, d.UpsertSession(s))
	}
	err = d.Close()
	closed = true
	require.NoError(t, err)
}

func registerSQLiteDaemonRuntime(t *testing.T, dataDir string) {
	t.Helper()
	registerSQLiteDaemonRuntimeWithEngine(t, dataDir, false)
}

func registerSQLiteWritableDaemonRuntime(t *testing.T, dataDir string) {
	t.Helper()
	registerSQLiteDaemonRuntimeWithEngine(t, dataDir, true)
}

func registerSQLiteDaemonRuntimeWithEngine(
	t *testing.T, dataDir string, writable bool,
) {
	t.Helper()
	cfg, err := config.LoadMinimal()
	require.NoError(t, err)
	if cfg.DataDir != dataDir {
		cfg.DataDir = dataDir
		cfg.DBPath = sessionsDBPath(dataDir)
	}
	database, err := openDB(cfg)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.WriteTimeout = 30 * time.Second
	var engine *agentsync.Engine
	if writable {
		engine = agentsync.NewEngine(database, agentsync.EngineConfig{
			Ephemeral: true,
		})
	}
	srv := server.New(cfg, database, engine)
	ts := httptest.NewUnstartedServer(srv.Handler())
	ts.Listener = ln
	ts.Start()
	t.Cleanup(func() {
		ts.Close()
		database.Close()
		RemoveDaemonRuntime(dataDir)
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)
}

func TestSessionGetVariants(t *testing.T) {
	dataDir := newAgentDataDir(t)
	bareID := "019da6a6-8c67-7c23-b102-ef48502852d0"
	seedSessionsWithOpts(t, dataDir,
		sessionSeed{id: "s-1", project: "proj"},
		sessionSeed{id: "s-json", project: "proj"},
		sessionSeed{id: "s-2", project: "proj"},
		sessionSeed{
			id:      "codex:" + bareID,
			project: "proj",
			mut:     func(s *db.Session) { s.Agent = "codex" },
		},
		sessionSeed{
			id:      "trae:workspaceStorage:rewrite",
			project: "proj",
			mut:     func(s *db.Session) { s.Agent = string(parser.AgentTrae) },
		},
		sessionSeed{
			id:      "trae:workspaceStorage:collision",
			project: "proj",
			mut:     func(s *db.Session) { s.Agent = string(parser.AgentTrae) },
		},
		sessionSeed{
			id:      "trae:globalStorage:collision",
			project: "proj",
			mut:     func(s *db.Session) { s.Agent = string(parser.AgentTrae) },
		},
		sessionSeed{
			id:      "amp:collision",
			project: "proj",
			mut:     func(s *db.Session) { s.Agent = "amp" },
		},
	)

	t.Run("json format", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "get", "s-1", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[map[string]any](t, out)
		assert.Equal(t, "s-1", got["id"])
		assert.Equal(t, "proj", got["project"])
	})

	t.Run("json alias", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "get", "s-json", "--json")
		require.NoError(t, err)

		got := decodeCLIJSON[map[string]any](t, out)
		assert.Equal(t, "s-json", got["id"])
		assert.Equal(t, "proj", got["project"])
	})

	t.Run("missing", func(t *testing.T) {
		_, err := executeCommand(newRootCommand(),
			"session", "get", "missing", "--format", "json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "missing")
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("human", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "get", "s-2")
		require.NoError(t, err)
		assert.True(t, strings.Contains(out, "s-2"),
			"human output should contain session id, got: %q", out)
		assert.True(t, strings.Contains(out, "proj"),
			"human output should contain project, got: %q", out)
	})

	// Covers the case where a user passes a bare UUID (e.g. copied from a
	// Codex session file name) for a session whose stored ID carries an
	// agent prefix. The CLI retries the lookup with each registered IDPrefix.
	t.Run("bare id finds prefixed", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "get", bareID, "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[map[string]any](t, out)
		assert.Equal(t, "codex:"+bareID, got["id"])
	})

	t.Run("bare trae id finds single namespaced session", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "get", "rewrite", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[map[string]any](t, out)
		assert.Equal(t, "trae:workspaceStorage:rewrite", got["id"])
	})

	t.Run("bare trae id rejects ambiguous namespaces", func(t *testing.T) {
		_, err := executeCommand(newRootCommand(),
			"session", "get", "collision", "--format", "json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ambiguous")
		assert.Contains(t, err.Error(), "trae:workspaceStorage:collision")
		assert.Contains(t, err.Error(), "trae:globalStorage:collision")
	})
}

func TestSessionList_ReadOnlyFixture(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionsWithOpts(t, dataDir,
		sessionSeed{id: "s-a", project: "shape"},
		sessionSeed{id: "s-b", project: "shape"},
		sessionSeed{id: "s-c", project: "shape"},
		sessionSeed{id: "p1-only", project: "p1"},
		sessionSeed{id: "lo", project: "sort-count", mut: func(s *db.Session) {
			s.MessageCount = 2
		}},
		sessionSeed{id: "mid", project: "sort-count", mut: func(s *db.Session) {
			s.MessageCount = 5
		}},
		sessionSeed{id: "hi", project: "sort-count", mut: func(s *db.Session) {
			s.MessageCount = 9
		}},
		sessionSeed{id: "a", project: "sort-multi", mut: func(s *db.Session) {
			s.MessageCount = 1
			s.StartedAt = new("2024-03-01T00:00:00Z")
		}},
		sessionSeed{id: "b", project: "sort-multi", mut: func(s *db.Session) {
			s.MessageCount = 1
			s.StartedAt = new("2024-01-01T00:00:00Z")
		}},
		sessionSeed{id: "c", project: "sort-multi", mut: func(s *db.Session) {
			s.MessageCount = 2
			s.StartedAt = new("2024-02-01T00:00:00Z")
		}},
		sessionSeed{id: "old", project: "sort-empty", mut: func(s *db.Session) {
			s.EndedAt = new("2024-01-01T00:00:00Z")
		}},
		sessionSeed{id: "new", project: "sort-empty", mut: func(s *db.Session) {
			s.EndedAt = new("2024-03-01T00:00:00Z")
		}},
	)

	t.Run("json shape", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "shape", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[cliSessionList](t, out)
		assert.Equal(t, 3, got.Total)
		assert.Len(t, got.Sessions, 3)
	})

	t.Run("cold read-only cursor round trip", func(t *testing.T) {
		t.Setenv("AGENTSVIEW_NO_DAEMON", "1")

		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "shape",
			"--format", "json", "--limit", "1")
		require.NoError(t, err)

		first := decodeCLIJSON[cliSessionList](t, out)
		require.NotEmpty(t, first.NextCursor)

		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "shape", "--format", "json",
			"--limit", "1", "--cursor", first.NextCursor)
		require.NoError(t, err)

		second := decodeCLIJSON[cliSessionList](t, out)
		assert.Len(t, second.Sessions, 1)
	})

	t.Run("filter by project", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "p1", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[cliSessionList](t, out)
		require.Len(t, got.Sessions, 1)
		assert.Equal(t, "p1-only", got.Sessions[0]["id"])
	})

	t.Run("sort and reverse", func(t *testing.T) {
		// --sort messages defaults to ascending.
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "messages", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"lo", "mid", "hi"},
			sessionListIDs(t, out))

		// --reverse flips it to descending.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "messages", "--reverse", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"hi", "mid", "lo"},
			sessionListIDs(t, out))

		// -r is the shorthand for --reverse.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "messages", "-r", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"hi", "mid", "lo"},
			sessionListIDs(t, out))
	})

	t.Run("multi-key sort", func(t *testing.T) {
		// Per-key directions: messages asc, then started desc.
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-multi",
			"--sort", "messages:asc,started:desc", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"a", "b", "c"},
			sessionListIDs(t, out))

		// --reverse flips only the unsuffixed key (messages -> desc); the
		// explicit started:asc is left untouched.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-multi",
			"--sort", "messages,started:asc", "-r", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"c", "b", "a"},
			sessionListIDs(t, out))
	})

	t.Run("empty sort reverse", func(t *testing.T) {
		// Default recent is newest-first.
		out, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-empty",
			"--sort", "", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"new", "old"}, sessionListIDs(t, out))

		// --reverse on the empty (default) sort flips recent to
		// oldest-first.
		out, err = executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-empty",
			"--sort", "", "--reverse", "--format", "json")
		require.NoError(t, err)
		assert.Equal(t, []string{"old", "new"}, sessionListIDs(t, out))
	})

	t.Run("invalid sort", func(t *testing.T) {
		_, err := executeCommand(newRootCommand(),
			"session", "list", "--project", "sort-count",
			"--sort", "bogus", "--format", "json")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid sort")
	})
}

func TestSessionList_ServerFlagUsesHTTP(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "local-session", "local")

	var gotPath, gotProject string
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotProject = r.URL.Query().Get("project")
			assert.Equal(t, http.MethodGet, r.Method)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"sessions": [
					{"id":"remote-session","project":"remote","agent":"claude"}
				],
				"total": 1
			}`))
		}))
	defer ts.Close()

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--project", "remote",
		"--json")
	require.NoError(t, err)

	got := decodeCLIJSON[cliSessionList](t, out)
	assert.Equal(t, "/api/v1/sessions", gotPath)
	assert.Equal(t, "remote", gotProject)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "remote-session", got.Sessions[0]["id"])
}

func TestSessionList_ServerFlagDoesNotSendConfigAuthToken(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
	writeTestConfig(t, dataDir, `auth_token = "local-secret"`)

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
		}))
	defer ts.Close()

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--json")
	require.NoError(t, err)
}

func TestSessionList_ServerFlagDoesNotLoadLocalConfig(t *testing.T) {
	dataDir := newAgentDataDir(t)
	writeTestConfig(t, dataDir, `not = valid = toml`)

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
		}))
	defer ts.Close()

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--json")
	require.NoError(t, err)
}

func TestSessionList_ServerTokenSendsBearer(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "remote-secret")

	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer remote-secret",
				r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"sessions":[],"total":0}`))
		}))
	defer ts.Close()

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--server", ts.URL, "--json")
	require.NoError(t, err)
}

func TestSessionList_PGFlagUsesPGReadStore(t *testing.T) {
	localDir := newAgentDataDir(t)
	remoteDir := t.TempDir()
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	t.Setenv("AGENTSVIEW_PG_SCHEMA", "custom_schema")

	seedSessionArchiveRows(t, localDir,
		sessionSeed{id: "local-session", project: "local"})
	seedSessionArchiveRows(t, remoteDir,
		sessionSeed{id: "pg-session", project: "remote"})

	remoteDB := dbtest.OpenTestDBAt(t, sessionsDBPath(remoteDir))
	stub := stubPGReadStore(t, remoteDB)

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--pg", "--format", "json")
	require.NoError(t, err)

	got := decodeCLIJSON[cliSessionList](t, out)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "pg-session", got.Sessions[0]["id"])
	assert.Equal(t, "postgres://example.test/agentsview", stub.PG.URL)
	assert.Equal(t, "custom_schema", stub.PG.Schema)
	assert.True(t, stub.CleanupCalled, "expected PG store cleanup")
}

func TestSessionList_ConfiguredPGWithoutFlagUsesSQLite(t *testing.T) {
	localDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/from-env")

	seedSession(t, localDir, "local-session", "local")

	forbidPGReadStore(t)

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--format", "json")
	require.NoError(t, err)

	got := decodeCLIJSON[cliSessionList](t, out)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "local-session", got.Sessions[0]["id"])
}

func TestSessionList_PGFlagRequiresURL(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "")

	_, err := executeCommand(newRootCommand(),
		"session", "list", "--pg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg url not configured")
	assert.Contains(t, err.Error(), "AGENTSVIEW_PG_URL")
}

func TestSessionList_DefaultDoesNotOpenPGStore(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "")
	seedSession(t, dataDir, "local-session", "local")

	forbidPGReadStore(t)

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--format", "json")
	require.NoError(t, err)

	got := decodeCLIJSON[cliSessionList](t, out)
	assert.Equal(t, 1, got.Total)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "local-session", got.Sessions[0]["id"])
}

func TestPGReadServiceClosesStoreWhenOpenFailsAfterCleanupProvided(t *testing.T) {
	closed := false
	orig := openPGReadStore
	openPGReadStore = func(
		config.Config, config.PGConfig,
	) (db.Store, func(), error) {
		return nil, func() { closed = true },
			errors.New("schema check failed")
	}
	t.Cleanup(func() {
		openPGReadStore = orig
	})

	_, _, err := newPGReadService(config.Config{}, config.PGConfig{
		URL:    "postgres://example.test/agentsview",
		Schema: "agentsview",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "opening pg store")
	assert.True(t, closed, "expected cleanup on error-after-open path")
}

// TestSessionList_MinToolFailuresZero verifies that passing
// --min-tool-failures 0 is treated as an explicit filter value
// (sessions with >=0 failures) rather than skipped as the int
// zero value. This exercises the cmd.Flags().Changed() guard
// that converts the int flag into a *int on ListFilter.
func TestSessionList_MinToolFailuresZero(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSessionWithOpts(t, dataDir, "s-a", "proj",
		func(s *db.Session) { s.ToolFailureSignalCount = 0 })

	out, err := executeCommand(newRootCommand(),
		"session", "list", "--min-tool-failures", "0",
		"--format", "json")
	require.NoError(t, err)

	got := decodeCLIJSON[cliSessionList](t, out)
	require.Len(t, got.Sessions, 1)
	assert.Equal(t, "s-a", got.Sessions[0]["id"])
}

// seedMessages inserts n message rows for sessionID with alternating
// user/assistant roles, ordinals starting at 1, and RFC3339
// timestamps one minute apart starting at 2026-04-01T00:00:00Z.
func seedMessages(t *testing.T, dataDir, sessionID string, n int) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	base := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	msgs := make([]db.Message, 0, n)
	for i := range n {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		content := fmt.Sprintf("msg-%d", i+1)
		msgs = append(msgs, db.Message{
			SessionID:     sessionID,
			Ordinal:       i + 1,
			Role:          role,
			Content:       content,
			ContentLength: len(content),
			Timestamp:     base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
		})
	}
	require.NoError(t, d.InsertMessages(msgs))
	require.NoError(t, d.Close())
}

func TestSessionMessagesVariants(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-msgs", "proj")
	seedMessages(t, dataDir, "s-msgs", 5)

	t.Run("json shape", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "messages", "s-msgs", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[cliMessageList](t, out)
		assert.Equal(t, 5, got.Count)
		require.Len(t, got.Messages, 5)
		assert.Equal(t, float64(1), got.Messages[0]["ordinal"])
	})

	t.Run("from and limit", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "messages", "s-msgs",
			"--from", "3", "--limit", "2", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[cliMessageList](t, out)
		assert.Equal(t, 2, got.Count)
		require.Len(t, got.Messages, 2)
		assert.Equal(t, float64(3), got.Messages[0]["ordinal"])
	})

	t.Run("direction desc", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "messages", "s-msgs",
			"--direction", "desc", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[cliMessageList](t, out)
		assert.Equal(t, 5, got.Count)
		require.Len(t, got.Messages, 5)
		assert.Equal(t, float64(5), got.Messages[0]["ordinal"])
		assert.Equal(t, float64(4), got.Messages[1]["ordinal"])
		assert.Equal(t, float64(3), got.Messages[2]["ordinal"])
		assert.Equal(t, float64(2), got.Messages[3]["ordinal"])
		assert.Equal(t, float64(1), got.Messages[4]["ordinal"])
	})
}

// seedMessagesWithToolCalls inserts one assistant message for sessionID
// carrying n tool_use blocks, numbered 1..n with ToolName "Bash<i>".
// Ordinals start at 1. Timestamp is fixed for determinism.
func seedMessagesWithToolCalls(
	t *testing.T, dataDir, sessionID string, n int,
) {
	t.Helper()
	d, err := db.Open(filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, err)
	t.Cleanup(func() { d.Close() })

	calls := make([]db.ToolCall, 0, n)
	for i := range n {
		calls = append(calls, db.ToolCall{
			SessionID: sessionID,
			ToolName:  fmt.Sprintf("Bash%d", i+1),
			Category:  "shell",
			ToolUseID: fmt.Sprintf("tu-%d", i+1),
			InputJSON: `{"command":"echo hi"}`,
		})
	}
	msg := db.Message{
		SessionID:     sessionID,
		Ordinal:       1,
		Role:          "assistant",
		Content:       "",
		ContentLength: 0,
		Timestamp:     "2026-04-01T00:00:00Z",
		HasToolUse:    true,
		ToolCalls:     calls,
	}
	require.NoError(t, d.InsertMessages([]db.Message{msg}))
	require.NoError(t, d.Close())
}

func TestSessionToolCallsVariants(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-tc", "proj")
	seedMessagesWithToolCalls(t, dataDir, "s-tc", 2)

	t.Run("json shape", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "tool-calls", "s-tc", "--format", "json")
		require.NoError(t, err)

		got := decodeCLIJSON[cliToolCallList](t, out)
		assert.Equal(t, 2, got.Count)
		require.Len(t, got.ToolCalls, 2)
		assert.NotEmpty(t, got.ToolCalls[0]["tool_name"])
		assert.NotEmpty(t, got.ToolCalls[0]["timestamp"])
	})

	t.Run("human table", func(t *testing.T) {
		out, err := executeCommand(newRootCommand(),
			"session", "tool-calls", "s-tc")
		require.NoError(t, err)

		for _, token := range []string{
			"ORDINAL", "TIMESTAMP", "TOOL", "CATEGORY",
			"Bash1", "Bash2",
		} {
			assert.Contains(t, out, token,
				"human output should contain %q, got: %q", token, out)
		}
	})
}

func TestSessionExport_StreamsFromDisk(t *testing.T) {
	dataDir := newAgentDataDir(t)

	src := filepath.Join(t.TempDir(), "session.jsonl")
	body := "{\"type\":\"user\",\"content\":\"hello\"}\n" +
		"{\"type\":\"assistant\",\"content\":\"hi\"}\n"
	require.NoError(t, os.WriteFile(src, []byte(body), 0o600))

	seedSessionWithOpts(t, dataDir, "s-1", "proj",
		func(s *db.Session) { s.FilePath = &src })

	out, err := executeCommand(newRootCommand(),
		"session", "export", "s-1")
	require.NoError(t, err)
	assert.Equal(t, body, out)
}

func createTraeExportStateDB(t *testing.T, root string) string {
	t.Helper()
	dbPath := filepath.Join(root, "workspaceStorage", "hash", "state.vscdb")
	require.NoError(t, os.MkdirAll(filepath.Dir(dbPath), 0o755))

	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, err = conn.Exec(`
		CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT);
		INSERT INTO ItemTable(key, value) VALUES (
			'memento/icube-ai-agent-storage',
			'{"list":[{"sessionId":"session-1","messages":[{"role":"user","content":"target trae message"}]}]}'
		);
	`)
	require.NoError(t, err)
	return dbPath
}

func TestSessionExportTraeStateDB(t *testing.T) {
	dataDir := newAgentDataDir(t)
	root := t.TempDir()
	dbPath := createTraeExportStateDB(t, root)
	virtualPath := dbPath + "#session-1"

	seedSessionWithOpts(t, dataDir, "trae:session-1", "proj",
		func(s *db.Session) {
			s.Agent = string(parser.AgentTrae)
			s.SourceSessionID = "session-1"
			s.FilePath = &virtualPath
		})

	out, err := executeCommand(newRootCommand(),
		"session", "export", "trae:session-1")
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &doc))
	assert.Equal(t, "session-1", doc["sessionId"])
	messages, ok := doc["messages"].([]any)
	require.True(t, ok)
	require.Len(t, messages, 1)
	first, ok := messages[0].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "user", first["role"])
	assert.Equal(t, "target trae message", first["content"])
}

func createHermesExportStateDB(t *testing.T, root string) string {
	t.Helper()
	sessionsDir := filepath.Join(root, "sessions")
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))

	dbPath := filepath.Join(root, "state.db")
	conn, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	_, err = conn.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			source TEXT NOT NULL,
			model TEXT,
			parent_session_id TEXT,
			started_at REAL NOT NULL,
			ended_at REAL,
			message_count INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			estimated_cost_usd REAL,
			actual_cost_usd REAL,
			cost_status TEXT,
			cost_source TEXT,
			title TEXT,
			api_call_count INTEGER DEFAULT 0
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			content TEXT,
			tool_call_id TEXT,
			tool_calls TEXT,
			timestamp REAL NOT NULL,
			finish_reason TEXT,
			reasoning TEXT,
			reasoning_content TEXT,
			reasoning_details TEXT,
			codex_reasoning_items TEXT,
			codex_message_items TEXT
		);
		INSERT INTO sessions (
			id, source, model, started_at, ended_at, message_count, title
		) VALUES
			('child', 'profile', 'gpt-5.4', 1778767200.0, 1778767800.0, 1, 'Child Session'),
			('sibling', 'profile', 'gpt-5.4', 1778767200.0, 1778767800.0, 1, 'Sibling Session');
		INSERT INTO messages (
			session_id, role, content, timestamp
		) VALUES
			('child', 'user', 'target hermes message', 1778767210.0),
			('sibling', 'user', 'sibling hermes message', 1778767211.0);
	`)
	require.NoError(t, err)
	return dbPath
}

func TestSessionExportHermesStateDB(t *testing.T) {
	dataDir := newAgentDataDir(t)

	wrongRoot := t.TempDir()
	_ = createHermesExportStateDB(t, wrongRoot)
	root := t.TempDir()
	dbPath := createHermesExportStateDB(t, root)
	t.Setenv("HERMES_SESSIONS_DIR", filepath.Join(wrongRoot, "sessions"))

	seedSessionWithOpts(t, dataDir, "hermes:child", "proj",
		func(s *db.Session) {
			s.Agent = string(parser.AgentHermes)
			s.SourceSessionID = "child"
			s.SourceVersion = "hermes-state-db"
			s.FilePath = &dbPath
		})

	out, err := executeCommand(newRootCommand(),
		"session", "export", "hermes:child")
	require.NoError(t, err)
	assert.NotContains(t, out, "SQLite format 3")
	assert.Contains(t, out, `"role":"session_meta"`)
	assert.Contains(t, out, "target hermes message")
	assert.NotContains(t, out, "sibling hermes message")
	assert.NotContains(t, out, "wrong root message")

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		assert.JSONEq(t, line, line)
	}
}

func TestSessionExportHermesStateDBWithoutSourceVersion(t *testing.T) {
	dataDir := newAgentDataDir(t)

	root := t.TempDir()
	dbPath := createHermesExportStateDB(t, root)

	seedSessionWithOpts(t, dataDir, "hermes:child", "proj",
		func(s *db.Session) {
			s.Agent = string(parser.AgentHermes)
			s.FilePath = &dbPath
		})

	out, err := executeCommand(newRootCommand(),
		"session", "export", "hermes:child")
	require.NoError(t, err)
	assert.NotContains(t, out, "SQLite format 3")
	assert.Contains(t, out, `"role":"session_meta"`)
	assert.Contains(t, out, "target hermes message")
	assert.NotContains(t, out, "sibling hermes message")

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		assert.JSONEq(t, line, line)
	}
}

func TestSessionExport_AiderVirtualPathStreamsOnlySelectedRun(t *testing.T) {
	dataDir := newAgentDataDir(t)

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	history := filepath.Join(repo, parser.AiderHistoryFileName())
	run0 := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n"
	run1 := "# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	run2 := "# aider chat started at 2026-06-09 16:45:00\n" +
		"#### third prompt\nanswer three\n"
	require.NoError(t, os.WriteFile(
		history, []byte("ignored preamble\n"+run0+run1+run2), 0o600,
	))
	rawID, ok := parser.AiderRawIDAt(history, 1)
	require.True(t, ok, "run 1 raw ID")

	seedSessionWithOpts(t, dataDir, "aider:"+rawID, "repo",
		func(s *db.Session) {
			s.Agent = string(parser.AgentAider)
			vp := parser.AiderVirtualPath(history, 1)
			s.FilePath = &vp
		})

	out, err := executeCommand(newRootCommand(),
		"session", "export", "aider:"+rawID)
	require.NoError(t, err)
	assert.Equal(t, run1, out)
	assert.NotContains(t, out, "first prompt")
	assert.NotContains(t, out, "third prompt")
}

func TestSessionExport_AiderStaleIndexReResolvesBySessionID(t *testing.T) {
	dataDir := newAgentDataDir(t)

	repo := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	history := filepath.Join(repo, parser.AiderHistoryFileName())
	run0 := "# aider chat started at 2026-06-09 14:01:00\n" +
		"#### first prompt\nanswer one\n"
	run1 := "# aider chat started at 2026-06-09 15:30:00\n" +
		"#### second prompt\nanswer two\n"
	require.NoError(t, os.WriteFile(history, []byte(run0+run1), 0o600))
	rawID, ok := parser.AiderRawIDAt(history, 1)
	require.True(t, ok, "run 1 raw ID")

	seedSessionWithOpts(t, dataDir, "aider:"+rawID, "repo",
		func(s *db.Session) {
			s.Agent = string(parser.AgentAider)
			vp := parser.AiderVirtualPath(history, 1)
			s.FilePath = &vp
		})

	inserted := "# aider chat started at 2026-06-09 13:00:00\n" +
		"#### inserted prompt\ninserted answer\n"
	require.NoError(t, os.WriteFile(
		history, []byte(inserted+run0+run1), 0o600,
	))

	out, err := executeCommand(newRootCommand(),
		"session", "export", "aider:"+rawID)
	require.NoError(t, err)
	assert.Equal(t, run1, out)
	assert.NotContains(t, out, "inserted prompt")
	assert.NotContains(t, out, "first prompt")
}

func TestSessionExport_FailsWhenSourceMissing(t *testing.T) {
	dataDir := newAgentDataDir(t)

	nonExistent := filepath.Join(t.TempDir(), "gone.jsonl")
	seedSessionWithOpts(t, dataDir, "s-1", "proj",
		func(s *db.Session) { s.FilePath = &nonExistent })

	_, err := executeCommand(newRootCommand(),
		"session", "export", "s-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "source file not found")
}

func TestSessionExport_FailsWhenNotInLocalArchive(t *testing.T) {
	newAgentDataDir(t)

	_, err := executeCommand(newRootCommand(),
		"session", "export", "unknown-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not in local archive")
	assert.Contains(t, err.Error(), "unknown-id")
}

// Export inherits --format/--json from the session group but streams raw
// bytes, so it must reject both rather than silently ignore them.
func TestSessionExport_RejectsFormatFlag(t *testing.T) {
	for _, flag := range [][]string{{"--format", "json"}, {"--json"}} {
		t.Run(strings.Join(flag, " "), func(t *testing.T) {
			newAgentDataDir(t)

			args := append([]string{"session", "export", "some-id"}, flag...)
			_, err := executeCommand(newRootCommand(), args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(),
				"--format/--json not supported")
		})
	}
}

func TestSessionExport_RejectsPGFlag(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	_, err := executeCommand(newRootCommand(),
		"session", "export", "some-id", "--pg")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local-only command")
	assert.Contains(t, err.Error(), "--pg not supported")
}

func TestSessionUsage_ServerFlagUsesHTTP(t *testing.T) {
	newAgentDataDir(t)

	ts, reqs := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID:   "remote-session",
		serverRunning: true,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", "remote-session", "--server", ts.URL)

	out, code, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "/api/v1/sessions/remote-session/usage", reqs.UsagePath)
	assert.Equal(t, "breakdown=true", reqs.UsageQuery,
		"remote CLI must request full breakdown rows")
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "remote-session", out.SessionID)
	assert.Equal(t, "remote-project", out.Project)
	assert.Equal(t, 42, out.TotalOutputTokens)
	assert.True(t, out.ServerRunning)
}

func TestSessionUsage_UsesDiscoveredDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotUsagePath string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/remote-session":
			_, _ = w.Write([]byte(`{
				"id": "remote-session",
				"agent": "codex",
				"project": "remote-project"
			}`))
		case "/api/v1/sessions/remote-session/usage":
			gotUsagePath = r.URL.Path
			_, _ = w.Write([]byte(`{
				"session_id": "remote-session",
				"agent": "codex",
				"project": "remote-project",
				"total_output_tokens": 42,
				"peak_context_tokens": 2048,
				"has_token_data": true,
				"cost_usd": 0.5,
				"has_cost": true,
				"models": ["gpt-5.1"],
				"unpriced_models": []
			}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	cmd := sessionUsageCommand(t, "session", "usage", "remote-session")

	out, code, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "/api/v1/sessions/remote-session/usage", gotUsagePath)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "remote-session", out.SessionID)
	assert.True(t, out.ServerRunning)
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

func TestSessionUsage_DefaultRefusesReadOnlyDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotUsagePath string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			gotUsagePath = r.URL.Path
		}
		http.NotFound(w, r)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	cmd := sessionUsageCommand(t, "session", "usage", "remote-session")

	out, code, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, tokenUseExitErr, code)
	assert.Contains(t, err.Error(), "read-only")
	assert.Contains(t, err.Error(), "use --pg")
	assert.Empty(t, gotUsagePath)
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

func TestTokenUse_UsesDiscoveredDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotUsagePath string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/remote-session":
			_, _ = w.Write([]byte(`{
				"id": "remote-session",
				"agent": "codex",
				"project": "remote-project"
			}`))
		case "/api/v1/sessions/remote-session/usage":
			gotUsagePath = r.URL.Path
			_, _ = w.Write([]byte(`{
				"session_id": "remote-session",
				"agent": "codex",
				"project": "remote-project",
				"total_output_tokens": 42,
				"peak_context_tokens": 2048,
				"has_token_data": true,
				"cost_usd": 0.5,
				"has_cost": true,
				"models": ["gpt-5.1"],
				"unpriced_models": []
			}`))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	registerSyncRouteTestRuntime(t, dataDir, ts.URL)

	out, code, err := sessionUsageData("remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "/api/v1/sessions/remote-session/usage", gotUsagePath)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "remote-session", out.SessionID)
	assert.True(t, out.ServerRunning)
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

func TestTokenUse_RefusesReadOnlyDaemon(t *testing.T) {
	dataDir := newAgentDataDir(t)

	var gotUsagePath string
	ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			gotUsagePath = r.URL.Path
		}
		http.NotFound(w, r)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	out, code, err := sessionUsageData("remote-session")
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, tokenUseExitErr, code)
	assert.Contains(t, err.Error(), "read-only")
	assert.Contains(t, err.Error(), "use --pg")
	assert.Empty(t, gotUsagePath)
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

func sessionUsageRuntimeServer(
	t *testing.T,
	sessionHandler http.HandlerFunc,
) *httptest.Server {
	t.Helper()
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: daemonService,
		Version: "test",
	})
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		r *http.Request,
	) {
		if r.URL.Path == "/api/ping" {
			ping.ServeHTTP(w, r)
			return
		}
		sessionHandler(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestSessionUsage_ServerFlagResolvesBareID(t *testing.T) {
	newAgentDataDir(t)
	const bareID = "019da6a6-8c67-7c23-b102-ef48502852d0"
	const canonicalID = "codex:" + bareID

	ts, reqs := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID:   canonicalID,
		serverRunning: true,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", bareID, "--server", ts.URL)

	out, code, err := sessionUsageDataForCommand(cmd, bareID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, canonicalID, out.SessionID)
	assert.Equal(t, "/api/v1/sessions/"+canonicalID+"/usage", reqs.UsagePath)
}

func TestSessionUsage_ServerFlagResolvesKimiRawID(t *testing.T) {
	newAgentDataDir(t)
	const rawID = "project-hash:session-uuid"
	const canonicalID = "kimi:" + rawID

	ts, reqs := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID:   canonicalID,
		agent:         "kimi",
		outputTokens:  84,
		serverRunning: true,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", rawID, "--server", ts.URL)

	out, code, err := sessionUsageDataForCommand(cmd, rawID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, canonicalID, out.SessionID)
	assert.Equal(t, "/api/v1/sessions/"+canonicalID+"/usage", reqs.UsagePath)
}

func TestSessionUsage_ServerFlagDoesNotSendConfigAuthToken(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_SERVER_TOKEN", "")
	writeTestConfig(t, dataDir, `auth_token = "local-secret"`)

	ts, _ := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID:   "remote-session",
		serverRunning: true,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", "remote-session", "--server", ts.URL)

	out, _, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestSessionUsage_ServerFlagDoesNotLoadLocalConfig(t *testing.T) {
	dataDir := newAgentDataDir(t)
	writeTestConfig(t, dataDir, `not = valid = toml`)

	ts, _ := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID:   "remote-session",
		serverRunning: true,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", "remote-session", "--server", ts.URL)

	out, _, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestSessionUsage_ServerTokenSendsBearer(t *testing.T) {
	dataDir := newAgentDataDir(t)
	tokenFile := filepath.Join(dataDir, "remote-token")
	require.NoError(t, os.WriteFile(
		tokenFile, []byte("remote-secret\n"), 0o600,
	))

	ts, _ := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID:   "remote-session",
		bearer:        "remote-secret",
		serverRunning: true,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", "remote-session",
		"--server", ts.URL,
		"--server-token-file", tokenFile)

	out, _, err := sessionUsageDataForCommand(cmd, "remote-session")
	require.NoError(t, err)
	require.NotNil(t, out)
}

func TestSessionUsage_ServerHTTPClientHasTimeout(t *testing.T) {
	oldClient := sessionUsageHTTPClient
	sessionUsageHTTPClient = &http.Client{Timeout: 20 * time.Millisecond}
	t.Cleanup(func() { sessionUsageHTTPClient = oldClient })

	newAgentDataDir(t)

	ts, _ := newRemoteUsageServer(t, remoteUsageSpec{
		canonicalID: "remote-session",
		usageDelay:  200 * time.Millisecond,
	})

	cmd := sessionUsageCommand(t,
		"session", "usage", "remote-session", "--server", ts.URL)

	start := time.Now()
	out, code, err := sessionUsageDataForCommand(cmd, "remote-session")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, tokenUseExitErr, code)
	assert.Less(t, elapsed, 150*time.Millisecond)
}

func TestSessionUsage_ConfiguredPGWithoutFlagUsesSQLite(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")

	localDB := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	seedUsageSession(t, localDB, "local-session", "local-project", "codex", 24)

	forbidPGReadStore(t)

	cmd := sessionUsageCommand(t, "session", "usage", "local-session")

	out, code, err := sessionUsageDataForCommand(cmd, "local-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "local-session", out.SessionID)
	assert.Equal(t, "local-project", out.Project)
	assert.Equal(t, 24, out.TotalOutputTokens)
	assert.False(t, out.ServerRunning)
}

func TestSessionUsage_PGFlagUsesPGStore(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	pgDB := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "pg.db"))
	seedUsageSession(t, pgDB, "pg-session", "pg-project", "codex", 42)

	stub := stubPGReadStore(t, pgDB)

	cmd := sessionUsageCommand(t, "session", "usage", "pg-session", "--pg")

	out, code, err := sessionUsageDataForCommand(cmd, "pg-session")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.True(t, stub.Opened, "expected session usage --pg to open PG store")
	assert.Equal(t, "postgres://example.test/agentsview", stub.PG.URL)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, "pg-session", out.SessionID)
	assert.Equal(t, "pg-project", out.Project)
	assert.Equal(t, 42, out.TotalOutputTokens)
	assert.False(t, out.ServerRunning)
}

func TestSessionUsage_PGFlagResolvesBareSessionID(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	bareID := "019da6a6-8c67-7c23-b102-ef48502852d0"
	storedID := "codex:" + bareID
	pgDB := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "pg.db"))
	seedUsageSession(t, pgDB, storedID, "pg-project", "codex", 42)

	stubPGReadStore(t, pgDB)

	cmd := sessionUsageCommand(t, "session", "usage", bareID, "--pg")

	out, code, err := sessionUsageDataForCommand(cmd, bareID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, storedID, out.SessionID)
	assert.Equal(t, "pg-project", out.Project)
	assert.Equal(t, 42, out.TotalOutputTokens)
}

func TestSessionUsage_PGFlagResolvesColonBearingRawSessionID(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	rawID := "project-hash:session-uuid"
	storedID := "kimi:" + rawID
	pgDB := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "pg.db"))
	seedUsageSession(t, pgDB, storedID, "pg-project", "kimi", 84)

	stubPGReadStore(t, pgDB)

	cmd := sessionUsageCommand(t, "session", "usage", rawID, "--pg")

	out, code, err := sessionUsageDataForCommand(cmd, rawID)
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, tokenUseExitOK, code)
	assert.Equal(t, storedID, out.SessionID)
	assert.Equal(t, "pg-project", out.Project)
	assert.Equal(t, 84, out.TotalOutputTokens)
}

func TestSessionUsage_PGFlagRejectsAmbiguousTraeNamespaces(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	rawID := "collision"
	pgDB := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "pg.db"))
	seedUsageSession(t, pgDB,
		"trae:workspaceStorage:"+rawID,
		"pg-project", string(parser.AgentTrae), 84,
	)
	seedUsageSession(t, pgDB,
		"trae:globalStorage:"+rawID,
		"pg-project", string(parser.AgentTrae), 84,
	)
	seedUsageSession(t, pgDB,
		"amp:"+rawID,
		"pg-project", "amp", 84,
	)

	stubPGReadStore(t, pgDB)

	cmd := sessionUsageCommand(t, "session", "usage", rawID, "--pg")

	out, code, err := sessionUsageDataForCommand(cmd, rawID)
	require.Error(t, err)
	assert.Nil(t, out)
	assert.Equal(t, tokenUseExitErr, code)
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Contains(t, err.Error(), "trae:workspaceStorage:"+rawID)
	assert.Contains(t, err.Error(), "trae:globalStorage:"+rawID)
}

// TestSessionSync_UnknownID_ReportsNoFilePath verifies that the
// sync engine is plumbed in direct mode. No daemon running, no
// sessions in DB — Execute returns an error whose message contains
// "no file_path recorded" AND the missing id. Critically the
// error must NOT be db.ErrReadOnly (that would mean the engine
// was nil, i.e. direct-backend constructed without a real
// sync.Engine as in the default newService path).
func TestSessionSync_UnknownID_ReportsNoFilePath(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")

	_, err := executeCommand(newRootCommand(),
		"session", "sync", "missing-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-id")
	assert.Contains(t, err.Error(), "no file_path recorded",
		"error should come from directBackend.Sync validation, not ErrReadOnly")
	assert.NotContains(t, err.Error(), "read-only",
		"engine must be plumbed; got ErrReadOnly-style message: %v", err)
}

func TestSessionSync_PGFlagRefusesWrite(t *testing.T) {
	newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")

	_, err := executeCommand(newRootCommand(),
		"session", "sync", "--pg", "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--pg is read-only")
	assert.Contains(t, err.Error(), "write commands")
}

func TestSessionSync_ServerFlagTreatsPathShapedArgAsRemotePath(t *testing.T) {
	dataDir := newAgentDataDir(t)
	writeTestConfig(t, dataDir, `not = valid = toml`)

	var got struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	ts := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/api/v1/sessions/sync", r.URL.Path)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&got))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": "remote-session",
				"agent": "codex",
				"project": "remote-project"
			}`))
		}))
	defer ts.Close()

	out, err := executeCommand(newRootCommand(),
		"session", "sync", "--server", ts.URL,
		"/remote/session.jsonl", "--json")
	require.NoError(t, err)
	assert.Contains(t, out, `"id":"remote-session"`)
	assert.Empty(t, got.ID)
	assert.Equal(t, "/remote/session.jsonl", got.Path)
}

// TestSessionSync_AgainstReadOnlyDaemon_Refuses verifies the CLI
// refuses to sync when a pg serve (ReadOnly=true) daemon owns
// the runtime record. Discovery uses the shared daemon ping
// endpoint, so the fixture must answer /api/ping.
func TestSessionSync_AgainstReadOnlyDaemon_Refuses(t *testing.T) {
	dataDir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dataDir, host, port, "test", true)

	_, err := executeCommand(newRootCommand(),
		"session", "sync", "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read-only",
		"should refuse against pg serve daemon")
}

// TestSessionSync_WhenDaemonRuntimeUnprobeable_Refuses verifies that
// an unprobeable writable runtime record still suppresses direct writes.
func TestSessionSync_WhenDaemonRuntimeUnprobeable_Refuses(t *testing.T) {
	dataDir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	// Bind then immediately close so the port is guaranteed
	// free and no TCP listener is accepting.
	ln, port := freeTCPListener(t)
	ln.Close()

	_, err := WriteDaemonRuntime(
		dataDir, "127.0.0.1", port, "test", false,
	)
	require.NoError(t, err)
	t.Cleanup(func() { RemoveDaemonRuntime(dataDir) })

	_, err = executeCommand(newRootCommand(),
		"session", "sync", "some-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not responding",
		"should refuse against unreachable active daemon")
	assert.NotContains(t, err.Error(), "no file_path",
		"must not fall through to direct-write engine")
}

func TestSessionSync_ColdArchiveWriteAutoStartsDaemon(t *testing.T) {
	dataDir := daemonRuntimeDir(t)
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)

	var syncCalled bool
	ts := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		require.Equal(t, "/api/v1/sessions/sync", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		syncCalled = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"daemon-synced"}`))
	}))
	t.Cleanup(ts.Close)

	rt := daemonRuntimeFromTestURL(t, ts.URL)
	oldStart := startBackgroundServeForTransport
	startBackgroundServeForTransport = func(
		_ context.Context, cfg *config.Config, timeout time.Duration,
	) (*DaemonRuntime, error) {
		assert.Equal(t, dataDir, cfg.DataDir)
		assert.Equal(t, backgroundAutoStartReadyTimeout, timeout)
		return rt, nil
	}
	t.Cleanup(func() { startBackgroundServeForTransport = oldStart })

	out, err := executeCommand(newRootCommand(),
		"session", "sync", "some-id")
	require.NoError(t, err)
	assert.True(t, syncCalled)
	assert.Contains(t, out, "synced: daemon-synced")
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

func daemonRuntimeFromTestURL(t *testing.T, rawURL string) *DaemonRuntime {
	t.Helper()
	u, err := url.Parse(rawURL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	return &DaemonRuntime{Host: host, Port: port}
}

// TestSessionWatch_ExitsOnCancel verifies that `session watch`
// exits cleanly when the cobra Command's context is cancelled,
// without hanging on the upstream channel. Any NDJSON emitted
// to stdout must parse as one JSON object per line. We don't
// drive DB changes here (poll interval is 1.5s) — this test
// only asserts the plumbing: service resolution, channel wiring,
// and the shutdown path.
//
// To distinguish a real Watch call from an early-return stub, we
// also assert the command runs past a short delay: any stub that
// returns synchronously would complete in single-digit ms.
func TestSessionWatch_ExitsOnCancel(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "s-watch", "proj")

	root := newRootCommand()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(buf)
	root.SetArgs([]string{"session", "watch", "s-watch"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	var execErr error
	select {
	case execErr = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("session watch did not exit within 3s after ctx cancel")
	}
	elapsed := time.Since(start)

	// Clean cancellation must surface as either nil (upstream channel
	// closed on ctx cancel) or an error that wraps context.Canceled.
	// Anything else indicates a regression that earlier versions of
	// this test swallowed by discarding execErr.
	if execErr != nil && !errors.Is(execErr, context.Canceled) {
		t.Fatalf("expected nil or context.Canceled, got %v", execErr)
	}

	// A stub that returns immediately would complete far faster
	// than the cancel delay. Require the command to actually
	// wait on the Watch channel.
	assert.GreaterOrEqual(t, elapsed, 30*time.Millisecond,
		"session watch returned too quickly (%v) — "+
			"likely a stub, not a real Watch", elapsed)

	// Any output must be valid NDJSON. Empty output is fine.
	for line := range bytes.SplitSeq(buf.Bytes(), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var ev map[string]any
		require.NoError(t, json.Unmarshal(line, &ev),
			"non-NDJSON line: %q", line)
	}
}

// TestSessionWatch_UnknownID_FailsFast verifies that `session
// watch` against an unknown session id fails fast with a clear
// "session not found" error rather than returning an indefinitely
// live heartbeat stream. Slow-failure mode would be a contract
// footgun for automation scripts.
func TestSessionWatch_UnknownID_FailsFast(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedEmptyArchive(t, dataDir)

	_, err := executeCommand(newRootCommand(),
		"session", "watch", "unknown-id")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found",
		"expected 'session not found' error; got: %v", err)
	assert.Contains(t, err.Error(), "unknown-id",
		"error should name the missing session id")
}

// Watch streams a fixed NDJSON format, so it rejects --format/--json
// inherited from the session group. The guard fires before any service
// resolution, so no archive setup is needed.
func TestSessionWatch_RejectsFormatFlag(t *testing.T) {
	for _, flag := range [][]string{{"--format", "json"}, {"--json"}} {
		t.Run(strings.Join(flag, " "), func(t *testing.T) {
			newAgentDataDir(t)

			args := append([]string{"session", "watch", "some-id"}, flag...)
			_, err := executeCommand(newRootCommand(), args...)
			require.Error(t, err)
			assert.Contains(t, err.Error(),
				"--format/--json not supported")
		})
	}
}

// TestLooksLikePath covers both POSIX and Windows-style separators
// so "./session.jsonl" works on Windows and bare session IDs stay
// classified as IDs regardless of platform.
func TestLooksLikePath(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"abc-123", false},
		{"550e8400-e29b-41d4-a716-446655440000", false},
		{"codex:my-session", false},
		{".", true},
		{"..", true},
		{"./session.jsonl", true},
		{"../parent/session.jsonl", true},
		{`.\session.jsonl`, true},
		{`..\parent\session.jsonl`, true},
		{"subdir/session.jsonl", true},
		{`subdir\session.jsonl`, true},
		{"/abs/path.jsonl", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := looksLikePath(tc.in); got != tc.want {
				t.Fatalf("looksLikePath(%q) = %v, want %v",
					tc.in, got, tc.want)
			}
		})
	}
}
