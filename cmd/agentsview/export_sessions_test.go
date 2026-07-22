package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/pricing"
)

type exportSessionsDocument struct {
	Type          string                 `json:"type"`
	SchemaVersion int                    `json:"schema_version"`
	DatabaseID    string                 `json:"database_id"`
	Cursor        exportSessionsCursor   `json:"cursor"`
	Pricing       map[string]any         `json:"pricing"`
	Projects      map[string]any         `json:"projects"`
	Sessions      []db.SessionSummaryRow `json:"sessions"`
	Rows          []db.SessionSummaryRow `json:"rows"`
	Error         string                 `json:"error"`
	Message       string                 `json:"message"`
}

type exportSessionsCursor struct {
	Next string `json:"next"`
}

func TestExportSessionsJSONEmitsOneDocument(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.NoError(t, err, "export sessions")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Equal(t, export.SessionSummarySchemaVersion, doc.SchemaVersion)
	assert.NotEmpty(t, doc.DatabaseID)
	assert.NotNil(t, doc.Pricing)
	assert.NotNil(t, doc.Projects)
	assert.Len(t, doc.Sessions, 2)
	assert.Empty(t, doc.Rows, "CLI output must use sessions, not rows")
	assert.Empty(t, strings.TrimSpace(decoderRemainder(t, stdout)),
		"stdout must contain exactly one JSON document")
}

func TestExportSessionsJSONAliasEmitsOneDocument(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--json",
	)
	require.NoError(t, err, "export sessions --json")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Equal(t, export.SessionSummarySchemaVersion, doc.SchemaVersion)
	assert.Len(t, doc.Sessions, 2)
	assert.Empty(t, strings.TrimSpace(decoderRemainder(t, stdout)),
		"--json must emit exactly one JSON document")
}

func TestExportSessionsJSONAliasRejectsConflictingFormat(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--json", "--format", "ndjson",
	)

	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	assert.Contains(t, err.Error(), "--json cannot be combined with --format ndjson")
}

func TestExportSessionsJSONNoUsageKeepsClosedCostSource(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.NoError(t, err, "export sessions")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Equal(t, "computed", doc.Pricing["cost_source"])
	assert.NotEqual(t, "", doc.Pricing["cost_source"])
}

func TestExportSessionsNDJSONEmitsMetaThenRows(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "ndjson",
	)
	require.NoError(t, err, "export sessions")
	assert.Empty(t, stderr)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 3)
	meta := decodeExportSessionsDocument(t, lines[0])
	assert.Equal(t, "meta", meta.Type)
	assert.Equal(t, export.SessionSummarySchemaVersion, meta.SchemaVersion)
	assert.NotEmpty(t, meta.DatabaseID)
	assert.NotNil(t, meta.Pricing)
	assert.NotNil(t, meta.Projects)
	assert.Empty(t, meta.Sessions)

	for _, line := range lines[1:] {
		var row db.SessionSummaryRow
		require.NoError(t, json.Unmarshal([]byte(line), &row))
		assert.NotEmpty(t, row.ID)
	}
}

func TestExportSessionsAllJSONEmitsOneDocument(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--all", "--format", "json",
	)
	require.NoError(t, err, "export all sessions")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Len(t, doc.Sessions, 2)
	assert.Empty(t, doc.Cursor.Next)
	assert.Empty(t, strings.TrimSpace(decoderRemainder(t, stdout)),
		"--all must not concatenate JSON documents")
}

func TestExportSessionsAllJSONMergesPricingAcrossPages(t *testing.T) {
	setupExportGoldenDataDir(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(),
		"export", "sessions",
		"--all",
		"--format", "json",
		"--limit", "1",
	)
	require.NoError(t, err, "export all sessions")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(t, doc.Sessions, 4)
	models, ok := doc.Pricing["models"].(map[string]any)
	require.True(t, ok, "pricing.models must be an object")
	assert.Contains(t, models, goldenComputedModel)
	assert.Contains(t, models, goldenReportedModel)
	assert.Empty(t, doc.Cursor.Next)
}

func TestBuildExportSessionsOutputMarksCrossPageProjectConflictAmbiguous(t *testing.T) {
	const projectKey = "pl1:sha256:project"
	page := func(identityKey string) db.SessionExportResult {
		return db.SessionExportResult{Projects: map[string]export.ProjectMapEntry{
			projectKey: {
				ProjectKey: projectKey,
				Resolution: export.ProjectResolutionResolved,
				Identity: &export.ProjectIdentity{
					Key: identityKey, Kind: export.ProjectKindGitRemote,
				},
			},
		}}
	}

	got := buildExportSessionsOutput("database", []db.SessionExportResult{
		page("p1:sha256:first"), page("p1:sha256:second"),
	})

	require.Contains(t, got.Projects, projectKey)
	assert.Equal(t, export.ProjectResolutionAmbiguous,
		got.Projects[projectKey].Resolution)
	assert.Nil(t, got.Projects[projectKey].Identity)
}

func TestMergeExportSessionsPricingTreatsNoModelPagesAsNeutral(t *testing.T) {
	noModels := &export.PricingBlock{
		CostSource: export.CostSourceComputed,
		Models:     map[string]export.EffectiveModelRate{},
	}
	reported := &export.PricingBlock{
		CostSource: export.CostSourceReported,
		Models: map[string]export.EffectiveModelRate{
			"reported-model": {
				CostSource: export.CostSourceReported,
			},
		},
	}

	got := mergeExportSessionsPricing(noModels, reported)
	assert.Equal(t, export.CostSourceReported, got.CostSource)

	got = mergeExportSessionsPricing(reported, noModels)
	assert.Equal(t, export.CostSourceReported, got.CostSource)

	got = mergeExportSessionsPricing(noModels, noModels)
	assert.Equal(t, export.CostSourceComputed, got.CostSource)
}

func TestExportSessionsAllNDJSONCursorNextEmpty(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--all", "--format", "ndjson",
	)
	require.NoError(t, err, "export all sessions")
	assert.Empty(t, stderr)

	lines := nonEmptyLines(stdout)
	require.Len(t, lines, 3)
	meta := decodeExportSessionsDocument(t, lines[0])
	assert.Empty(t, meta.Cursor.Next)
}

func TestExportSessionsInvalidCursorWritesStructuredResetError(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--cursor", "not-a-cursor",
	)
	require.Error(t, err, "invalid cursor")
	assert.Equal(t, 4, exitCodeFromError(err))
	assert.Empty(t, stdout)

	var got exportSessionsDocument
	require.NoError(t, json.Unmarshal([]byte(stderr), &got))
	assert.Equal(t, "cursor_reset", got.Error)
	assert.Equal(t,
		"session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(t, got.DatabaseID)
}

func TestExportSessionsWrongDatabaseCursorWritesStructuredResetError(t *testing.T) {
	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))
	cursor := firstExportSessionsCursor(t)

	otherDir := t.TempDir()
	other := seedExportSessionsArchiveAt(t, filepath.Join(otherDir, "sessions.db"))
	require.NoError(t, other.SetDatabaseIDForTest(
		context.Background(), "other-export-sessions-test-db"))
	t.Setenv("AGENTSVIEW_DATA_DIR", otherDir)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--cursor", cursor,
	)
	require.Error(t, err, "wrong database cursor")
	assert.Equal(t, 4, exitCodeFromError(err))
	assert.Empty(t, stdout)

	var got exportSessionsDocument
	require.NoError(t, json.Unmarshal([]byte(stderr), &got))
	assert.Equal(t, "cursor_reset", got.Error)
	assert.Equal(t,
		"session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(t, got.DatabaseID)
}

func TestExportSessionsCursorResetMainStderrIsOnlyStructuredJSON(t *testing.T) {
	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestExportSessionsCursorResetMainHelperProcess$",
		"--",
		"export", "sessions", "--cursor", "not-a-cursor",
	)
	cmd.Env = append(os.Environ(),
		"AGENTSVIEW_EXPORT_SESSIONS_MAIN_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)
	stdout, err := cmd.Output()
	require.Error(t, err, "cursor reset should exit non-zero")
	assert.Empty(t, stdout)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, sessionExportCursorResetExitCode, exitErr.ExitCode())
	stderr := string(exitErr.Stderr)
	assert.NotContains(t, stderr, "fatal:")

	var got exportSessionsDocument
	require.NoError(t, json.Unmarshal([]byte(stderr), &got))
	assert.Equal(t, "cursor_reset", got.Error)
	assert.Equal(t,
		"session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(t, got.DatabaseID)
}

func TestExportSessionsMainStillPrintsFatalForNonCursorErrors(t *testing.T) {
	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))

	cmd := exec.Command(
		os.Args[0],
		"-test.run=^TestExportSessionsCursorResetMainHelperProcess$",
		"--",
		"export", "sessions", "--format", "xml",
	)
	cmd.Env = append(os.Environ(),
		"AGENTSVIEW_EXPORT_SESSIONS_MAIN_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)
	stdout, err := cmd.Output()
	require.Error(t, err, "invalid format should exit non-zero")
	assert.Empty(t, stdout)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 1, exitErr.ExitCode())
	assert.Contains(t, string(exitErr.Stderr), "fatal:")
	assert.Contains(t, string(exitErr.Stderr), "invalid argument \"xml\"")
}

func TestExportSessionsCursorResetMainHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_EXPORT_SESSIONS_MAIN_HELPER") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"agentsview"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	t.Fatal("missing helper args")
}

func TestExportSessionsExitCode4ReservedForCursorReset(t *testing.T) {
	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "xml",
	)
	require.Error(t, err, "invalid format")
	assert.NotEqual(t, 4, exitCodeFromError(err))
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
}

func TestExportSessionsRunsWhileWriteOwnerLockHeld(t *testing.T) {
	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))
	holdWriteOwnerLockForTest(t, dataDir)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.NoError(t, err, "read-only export should not need writer lock")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Len(t, doc.Sessions, 2)
	assert.Equal(t, "export-sessions-test-db", doc.DatabaseID)
}

func TestExportSessionsRequiresExistingDatabaseID(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	insertExportSessionsTestSession(t, database, db.Session{
		ID:               "missing-db-id",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "codex",
		StartedAt:        dbtest.Ptr("2026-06-01T10:00:00Z"),
		EndedAt:          dbtest.Ptr("2026-06-01T10:10:00Z"),
		MessageCount:     2,
		UserMessageCount: 2,
	})
	require.NoError(t, database.Close())
	removeArchiveDatabaseIDForTest(t, dbPath)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
	)
	require.Error(t, err, "export sessions should not initialize metadata")
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	assert.Contains(t, err.Error(), "database id")

	readonly, openErr := db.OpenReadOnly(dbPath)
	require.NoError(t, openErr)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	_, idErr := readonly.GetDatabaseID(context.Background())
	require.ErrorIs(t, idErr, db.ErrDatabaseIDMissing)
}

func TestExportSessionsUpgradeRequiresBackgroundEvidenceBackfill(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "legacy", Project: "agentsview", Machine: "local",
		Agent: "codex", Cwd: "/work/agentsview", MessageCount: 2,
		UserMessageCount: 1,
	})
	require.NoError(t, database.Close())
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec(`DROP TABLE session_project_identity_snapshots`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "status",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t, "project identity evidence: pending (0/1)\n", stdout)

	stdout, stderr, err = executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.Error(t, err)
	assert.Empty(t, stderr)
	assert.Empty(t, stdout)
	assert.Contains(t, err.Error(), "project identity evidence backfill is pending")
	assert.Contains(t, err.Error(), "agentsview export status")

	raw, err = sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer raw.Close()
	var exists int
	require.NoError(t, raw.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'session_project_identity_snapshots'
	`).Scan(&exists))
	assert.Equal(t, 1, exists)
	var state string
	require.NoError(t, raw.QueryRow(`
		SELECT state FROM background_migrations
		WHERE name = 'session_project_identity_snapshots_v1'
	`).Scan(&state))
	assert.Equal(t, "pending", state)
}

func TestExportStatusReportsBackfillFailure(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	_, err = raw.Exec(`
		INSERT INTO background_migrations (
			name, state, total_items, completed_items, last_error
		) VALUES (
			'session_project_identity_snapshots_v1', 'failed', 3, 1,
			'git metadata unavailable'
		)
	`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	require.NoError(t, database.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "status",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Equal(t,
		"project identity evidence: failed (1/3)\n"+
			"last error: git metadata unavailable\n",
		stdout)
}

func TestExportSessionsDoesNotUpgradeUnrelatedReadOnlyOpenFailure(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	require.NoError(t, os.WriteFile(dbPath, nil, 0o600))

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)

	info, statErr := os.Stat(dbPath)
	require.NoError(t, statErr)
	assert.Zero(t, info.Size(),
		"a non-schema read failure must not initialize or rebuild the archive")
}

func removeArchiveDatabaseIDForTest(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	_, err = raw.Exec(`DELETE FROM archive_metadata WHERE key = 'database_id'`)
	require.NoError(t, err)
}

func TestExportSessionsCursorConflictingFilterIsUsageError(t *testing.T) {
	seedExportSessionsArchive(t)
	cursor := firstExportSessionsCursor(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--cursor", cursor,
		"--project", "alpha",
	)
	require.Error(t, err, "cursor with filter should fail as usage error")
	assert.NotEqual(t, 4, exitCodeFromError(err))
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	assert.Contains(t, err.Error(), "--cursor cannot be combined with --project")
}

func TestExportSessionsCursorAllowsFormatAndLimit(t *testing.T) {
	seedExportSessionsArchive(t)
	cursor := firstExportSessionsCursor(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--cursor", cursor,
		"--format", "json",
		"--limit", "1",
	)
	require.NoError(t, err, "cursor with format and limit")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(t, doc.Sessions, 1)
	assert.Equal(t, "alpha-old", doc.Sessions[0].ID)
}

func TestExportSessionsCursorResumesFilteredExport(t *testing.T) {
	database := seedExportSessionsArchive(t)
	insertExportSessionsTestSession(t, database, db.Session{
		ID:               "beta-middle",
		Project:          "beta",
		Machine:          "local",
		Agent:            "codex",
		StartedAt:        dbtest.Ptr("2026-06-01T09:30:00Z"),
		EndedAt:          dbtest.Ptr("2026-06-01T09:40:00Z"),
		MessageCount:     2,
		UserMessageCount: 2,
	})
	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--project", "alpha",
		"--limit", "1",
	)
	require.NoError(t, err, "first filtered export page")
	require.Empty(t, stderr)
	first := decodeExportSessionsDocument(t, stdout)
	require.Len(t, first.Sessions, 1)
	assert.Equal(t, "alpha-new", first.Sessions[0].ID)
	require.NotEmpty(t, first.Cursor.Next)

	stdout, stderr, err = executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--cursor", first.Cursor.Next,
	)
	require.NoError(t, err, "filtered cursor resume")
	require.Empty(t, stderr)
	second := decodeExportSessionsDocument(t, stdout)
	require.Len(t, second.Sessions, 1)
	assert.Equal(t, "alpha-old", second.Sessions[0].ID)
	assert.Empty(t, second.Cursor.Next)
}

func TestExportSessionsJSONGolden(t *testing.T) {
	setupExportGoldenDataDir(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(),
		"export", "sessions",
		"--format", "json",
		"--limit", "2",
	)
	require.NoError(t, err, "export sessions json golden")
	require.Empty(t, stderr)
	assert.NotContains(t, stdout, "/fixtures/")
	assert.NotContains(t, stdout, `"cwd"`)
	assert.NotContains(t, stdout, `"machine":"golden-host"`)
	assert.NotContains(t, stdout, `"root_path":"/`)

	assertGoldenBytes(t, "session_export_v3.json", []byte(stdout))
}

func TestExportSessionsNDJSONGolden(t *testing.T) {
	setupExportGoldenDataDir(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(),
		"export", "sessions",
		"--format", "ndjson",
		"--limit", "2",
	)
	require.NoError(t, err, "export sessions ndjson golden")
	require.Empty(t, stderr)

	assertGoldenBytes(t, "session_export_v3.ndjson", []byte(stdout))
}

func firstExportSessionsCursor(t *testing.T) string {
	t.Helper()
	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--limit", "1",
	)
	require.NoError(t, err, "first export page")
	require.Empty(t, stderr)
	doc := decodeExportSessionsDocument(t, stdout)
	require.NotEmpty(t, doc.Cursor.Next)
	return doc.Cursor.Next
}

func TestExportSessionsFallbackPricingOnUnseededArchive(t *testing.T) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.SetDatabaseIDForTest(
		context.Background(), "fallback-pricing-test-db"))

	model := exactFallbackPricedModel(t)
	insertExportSessionsTestSession(t, database, db.Session{
		ID:               "fallback-priced",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "claude",
		StartedAt:        dbtest.Ptr("2026-06-01T10:00:00Z"),
		EndedAt:          dbtest.Ptr("2026-06-01T10:10:00Z"),
		MessageCount:     3,
		UserMessageCount: 2,
	})
	require.NoError(t, database.InsertMessages([]db.Message{
		{
			SessionID: "fallback-priced", Ordinal: 0, Role: "user",
			Content: "question", ContentLength: len("question"),
			Timestamp: "2026-06-01T10:00:00Z",
		},
		{
			SessionID: "fallback-priced", Ordinal: 1, Role: "assistant",
			Content: "answer", ContentLength: len("answer"),
			Timestamp: "2026-06-01T10:05:00Z", Model: model,
			TokenUsage: json.RawMessage(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		{
			SessionID: "fallback-priced", Ordinal: 2, Role: "user",
			Content: "follow up", ContentLength: len("follow up"),
			Timestamp: "2026-06-01T10:06:00Z",
		},
	}), "insert messages")
	require.NoError(t, database.Close(), "close seeded archive")

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions")
	require.NoError(t, err, "export sessions on unseeded archive")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(t, doc.Sessions, 1, "exported sessions")
	usage := doc.Sessions[0].ModelUsage
	require.NotNil(t, usage, "model usage")
	assert.True(t, usage.HasCost,
		"fallback-priced model %s should have cost", model)
	assert.Positive(t, usage.Cost.Microdollars, "fallback-priced cost")

	fallback, ok := doc.Pricing["fallback"].(map[string]any)
	require.True(t, ok, "pricing fallback block")
	assert.Equal(t, true, fallback["used"], "fallback used")
	assert.Contains(t, doc.Pricing["source"], "embedded",
		"pricing source provenance")
}

// exactFallbackPricedModel returns an embedded fallback model pattern with
// non-wildcard name and nonzero input/output rates, so lookups resolve
// deterministically regardless of snapshot contents.
func exactFallbackPricedModel(t *testing.T) string {
	t.Helper()
	for _, p := range pricing.FallbackPricing() {
		if strings.ContainsAny(p.ModelPattern, "*/_") {
			continue
		}
		if p.InputPerMTok.Microdollars > 0 && p.OutputPerMTok.Microdollars > 0 {
			return p.ModelPattern
		}
	}
	t.Fatal("no exact fallback-priced model in embedded snapshot")
	return ""
}

func executeExportSessionsCommand(
	root *cobra.Command, args ...string,
) (string, string, error) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	_, err := root.ExecuteC()
	return stdout.String(), stderr.String(), err
}

func seedExportSessionsArchive(t *testing.T) *db.DB {
	t.Helper()
	dataDir := testDataDir(t)
	return seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))
}

func seedExportSessionsArchiveAt(t *testing.T, path string) *db.DB {
	t.Helper()
	database := dbtest.OpenTestDBAt(t, path)
	require.NoError(t, database.SetDatabaseIDForTest(
		context.Background(), "export-sessions-test-db"))
	insertExportSessionsTestSession(t, database, db.Session{
		ID:               "alpha-new",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "codex",
		StartedAt:        dbtest.Ptr("2026-06-01T10:00:00Z"),
		EndedAt:          dbtest.Ptr("2026-06-01T10:10:00Z"),
		MessageCount:     2,
		UserMessageCount: 2,
	})
	insertExportSessionsTestSession(t, database, db.Session{
		ID:               "alpha-old",
		Project:          "alpha",
		Machine:          "local",
		Agent:            "codex",
		StartedAt:        dbtest.Ptr("2026-06-01T09:00:00Z"),
		EndedAt:          dbtest.Ptr("2026-06-01T09:10:00Z"),
		MessageCount:     2,
		UserMessageCount: 2,
	})
	return database
}

func insertExportSessionsTestSession(
	t *testing.T, database *db.DB, session db.Session,
) {
	t.Helper()
	require.NoError(t, database.UpsertSession(session),
		"upsert session %s", session.ID)
	require.NoError(t, database.UpsertProjectIdentityObservation(
		context.Background(), export.ProjectIdentityObservation{
			SessionID: session.ID, Project: session.Project,
			Machine: session.Machine, RootPath: session.Cwd,
			GitBranch: session.GitBranch, ObservedAt: time.Now().UTC(),
		}), "upsert session project identity %s", session.ID)
}

func decodeExportSessionsDocument(
	t *testing.T, input string,
) exportSessionsDocument {
	t.Helper()
	var doc exportSessionsDocument
	require.NoError(t, json.Unmarshal([]byte(input), &doc))
	return doc
}

func decoderRemainder(t *testing.T, input string) string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(input))
	var doc any
	require.NoError(t, dec.Decode(&doc))
	rest, err := io.ReadAll(dec.Buffered())
	require.NoError(t, err)
	return string(rest)
}

func nonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	lines := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
