package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestDaemonArchiveQueryBackendDefaultsMappedLocalTimezone(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	t.Setenv("TZ", "America/New_York")
	oldLocal := time.Local                                         //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.
	time.Local = time.FixedZone("Eastern Standard Time", -5*60*60) //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.
	t.Cleanup(func() { time.Local = oldLocal })                    //nolint:forbidigo // Exercise report formatting and date buckets in the local calendar timezone.

	var queries []url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.Query())
		writeJSONResponse(w, `{"timezone":"America/New_York"}`)
	}))
	t.Cleanup(ts.Close)
	backend := daemonArchiveQueryBackend{tr: transport{URL: ts.URL}}

	_, err := backend.ActivityReport(t.Context(), ActivityReportConfig{
		Preset: "day",
	})
	require.NoError(err)
	require.Len(queries, 1)
	assert.Equal("America/New_York", queries[0].Get("timezone"))
	assert.Equal(todayIn("America/New_York"), queries[0].Get("date"))

	_, err = backend.ActivityReport(t.Context(), ActivityReportConfig{
		Preset: "day", Date: "2026-03-09", Timezone: "Europe/Berlin",
	})
	require.NoError(err)
	require.Len(queries, 2)
	assert.Equal("Europe/Berlin", queries[1].Get("timezone"))
	assert.Equal("2026-03-09", queries[1].Get("date"))
}

func TestResolveArchiveQueryBackendNoSyncStartsNoSyncDaemon(t *testing.T) {
	testDataDir(t)
	var started bool
	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		started = true
		assert.True(t, cfg.NoSync)
		return &DaemonRuntime{Host: "127.0.0.1", Port: 12345}, nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) { p.NoSync = true },
	))
	assert.True(t, started)
	assert.IsType(t, daemonArchiveQueryBackend{}, backend)
}

func TestResolveArchiveQueryBackendRefusesReadOnlyDaemonForFreshQueries(t *testing.T) {
	assert := assert.New(t)

	dataDir := testDataDir(t)

	var called bool
	ts := sessionUsageRuntimeServer(t, func(
		w http.ResponseWriter, r *http.Request,
	) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	})
	registerTestRuntime(t, dataDir, ts.URL, true)

	_, cleanup, err := resolveArchiveQueryBackend(
		t.Context(), defaultArchiveQueryPolicy(nil),
	)
	if cleanup != nil {
		t.Cleanup(cleanup)
	}
	require.Error(t, err)
	assert.Contains(err.Error(), "read-only")
	assert.NotContains(err.Error(), "--pg")
	assert.False(called)
}

func TestResolveArchiveQueryBackendUsesGeneratedAutostartToken(t *testing.T) {
	testDataDir(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer generated-token", r.Header.Get("Authorization"))
		assert.Equal(t, "/api/v1/sync", r.URL.Path)
		assert.Equal(t, "true", r.URL.Query().Get("startup_only"))
		writeJSONResponse(w, `{}`)
	}))
	t.Cleanup(ts.Close)

	stubStartBackgroundServeForTransport(t, func(
		_ context.Context, cfg *config.Config, _ time.Duration,
	) (*DaemonRuntime, error) {
		cfg.AuthToken = "generated-token"
		return daemonRuntimeFromTestURL(t, ts.URL), nil
	})

	backend := resolveTestArchiveQueryBackend(t, defaultArchiveQueryPolicy(
		func(p *archiveQueryPolicy) {
			p.AutoStart = true
			p.ReadOnlyDaemon = archiveQueryRejectReadOnlyDaemon
		},
	))

	daemonBackend, ok := backend.(daemonArchiveQueryBackend)
	require.True(t, ok)
	assert.Equal(t, "generated-token", daemonBackend.authToken)
}

func TestLocalArchiveQuerySessionUsageNoSyncSkipsSingleSessionSync(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := t.TempDir()
	dbPath := filepath.Join(dataDir, "sessions.db")
	writer := dbtest.OpenTestDBAt(t, dbPath)
	started := "2026-06-23T12:00:00Z"
	require.NoError(writer.UpsertSession(db.Session{
		ID:                   "codex:no-sync-usage",
		Project:              "proj",
		Machine:              "local",
		Agent:                "codex",
		StartedAt:            &started,
		MessageCount:         1,
		TotalOutputTokens:    42,
		HasTotalOutputTokens: true,
	}))
	require.NoError(writer.Close())

	readonly, err := db.OpenReadOnly(dbPath)
	require.NoError(err)
	t.Cleanup(func() { readonly.Close() })

	backend := localArchiveQueryBackend{
		cfg:           config.Config{DBPath: dbPath},
		database:      readonly,
		offline:       true,
		skipFreshData: true,
	}
	stderr := captureStderr(t, func() {
		out, exitCode, err := backend.SessionUsage(
			t.Context(),
			sessionUsageQuery{SessionID: "codex:no-sync-usage"},
		)
		require.NoError(err)
		require.NotNil(out)
		assert.Equal(tokenUseExitOK, exitCode)
		assert.Equal(42, out.TotalOutputTokens)
	})
	assert.NotContains(stderr, "warning: sync failed")
	assert.NotContains(stderr, "warning: pricing seed failed")
}

// TestLocalSessionUsageRefreshesSubagentTranscripts covers the freshness
// half of the subagent rollup: SyncSingleSession only knows about the named
// session's file, so the backend must also ingest the agent-*.jsonl files
// beside it. Without that, a session that just finished would report a
// combined cost missing its most recent subagents.
func TestLocalSessionUsageRefreshesSubagentTranscripts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar,
				filepath.Join(home, "agent-dirs", string(def.Type)))
		}
	}
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_PROJECTS_DIR", claudeDir)

	projDir := filepath.Join(claudeDir, "-home-proj")
	require.NoError(os.MkdirAll(
		filepath.Join(projDir, "parent-uuid", "subagents"), 0o755))
	parentPath := filepath.Join(projDir, "parent-uuid.jsonl")
	require.NoError(os.WriteFile(parentPath, []byte(
		testjsonl.NewSessionBuilder().
			AddClaudeUser("2026-05-20T10:00:00Z", "delegate this").
			AddClaudeAssistant("2026-05-20T10:00:05Z", "on it").
			String(),
	), 0o644))

	dbPath := sessionsDBPath(dataDir)
	database := dbtest.OpenTestDBAt(t, dbPath)
	backend := localArchiveQueryBackend{
		cfg: config.Config{
			DBPath: dbPath,
			AgentDirs: map[parser.AgentType][]string{
				parser.AgentClaude: {claudeDir},
			},
		},
		database: database,
		offline:  true,
	}
	ctx := t.Context()

	// Ingest the parent, then write a subagent transcript the way Claude
	// Code does after the parent's own file was last synced.
	_, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid"})
	require.NoError(err)

	require.NoError(os.WriteFile(
		filepath.Join(projDir, "parent-uuid", "subagents",
			"agent-worker1.jsonl"),
		[]byte(testjsonl.NewSessionBuilder().
			AddClaudeUserWithSessionID(
				"2026-05-20T10:01:00Z", "do the subtask", "parent-uuid").
			AddClaudeAssistant("2026-05-20T10:01:30Z", "subtask done").
			String()),
		0o644))

	// Passive reads must leave newly written source transcripts unindexed.
	archived, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid", NoSync: true})
	require.NoError(err)
	require.NotNil(archived)
	assert.Zero(archived.SubagentCount)
	unsynced, err := database.GetSession(ctx, "agent-worker1")
	require.NoError(err)
	assert.Nil(unsynced)

	out, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid"})
	require.NoError(err)
	require.NotNil(out)
	assert.Equal(1, out.SubagentCount,
		"the new subagent transcript must be ingested before the query")

	child, err := database.GetSession(ctx, "agent-worker1")
	require.NoError(err)
	require.NotNil(child, "subagent session was not synced")
	require.NotNil(child.ParentSessionID)
	assert.Equal("parent-uuid", *child.ParentSessionID)

	archived, _, err = backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid", NoSync: true})
	require.NoError(err)
	require.NotNil(archived)
	assert.Equal(out.SessionUsage, archived.SessionUsage)

	// --own-only skips the subagent refresh and the combined view.
	own, _, err := backend.SessionUsage(
		ctx, sessionUsageQuery{SessionID: "parent-uuid", OwnOnly: true})
	require.NoError(err)
	require.NotNil(own)
	assert.Zero(own.SubagentCount)
}
