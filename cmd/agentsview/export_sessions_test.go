package main

import (
	"bytes"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
)

type exportSessionsDocument struct {
	Type          string                 `json:"type"`
	SchemaVersion int                    `json:"schema_version"`
	ArchiveID     string                 `json:"archive_id"`
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

func TestExportSessionsPublishesArchiveIdentity(t *testing.T) {
	for _, format := range []string{"json", "ndjson"} {
		for _, empty := range []bool{false, true} {
			t.Run(format+"/empty="+strconv.FormatBool(empty), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				database := seedExportSessionsArchive(t)
				require.NoError(database.SetArchiveIdentityForTest(
					t.Context(), "stable-archive", strings.Repeat("a", 64),
				))
				args := []string{"export", "sessions", "--format", format, "--limit", "1"}
				if empty {
					args = append(args, "--project", "absent")
				}
				stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), args...)
				require.NoError(err)
				require.Empty(stderr)
				if format == "ndjson" {
					stdout, _, _ = strings.Cut(stdout, "\n")
				}
				doc := decodeExportSessionsDocument(t, stdout)
				assert.Equal("stable-archive", doc.ArchiveID)
				assert.Equal("export-sessions-test-db", doc.DatabaseID)
				if !empty {
					require.NotEmpty(doc.Cursor.Next)
					stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
						"export", "sessions", "--cursor", doc.Cursor.Next)
					require.NoError(err)
					require.Empty(stderr)
					resumed := decodeExportSessionsDocument(t, stdout)
					assert.Equal("stable-archive", resumed.ArchiveID)
					assert.Equal(doc.DatabaseID, resumed.DatabaseID)
				}
			})
		}
	}
}

func TestExportSessionsJSONEmitsOneDocument(t *testing.T) {
	assert := assert.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.NoError(t, err, "export sessions")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Equal(export.SessionSummarySchemaVersion, doc.SchemaVersion)
	assert.NotEmpty(doc.DatabaseID)
	assert.NotNil(doc.Pricing)
	assert.NotNil(doc.Projects)
	assert.Len(doc.Sessions, 2)
	assert.Empty(doc.Rows, "CLI output must use sessions, not rows")
	assert.Empty(strings.TrimSpace(decoderRemainder(t, stdout)),
		"stdout must contain exactly one JSON document")
}

func TestExportSessionsJSONAliasEmitsOneDocument(t *testing.T) {
	assert := assert.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--json",
	)
	require.NoError(t, err, "export sessions --json")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Equal(export.SessionSummarySchemaVersion, doc.SchemaVersion)
	assert.Len(doc.Sessions, 2)
	assert.Empty(strings.TrimSpace(decoderRemainder(t, stdout)),
		"--json must emit exactly one JSON document")
}

func TestExportSessionsJSONAliasRejectsConflictingFormat(t *testing.T) {
	assert := assert.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--json", "--format", "ndjson",
	)

	require.Error(t, err)
	assert.Empty(stdout)
	assert.Empty(stderr)
	assert.Contains(err.Error(), "--json cannot be combined with --format ndjson")
}

func TestExportSessionsJSONNoUsageKeepsClosedCostSource(t *testing.T) {
	assert := assert.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.NoError(t, err, "export sessions")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Equal("computed", doc.Pricing["cost_source"])
	assert.NotEmpty(doc.Pricing["cost_source"])
}

func TestExportSessionsNDJSONEmitsMetaThenRows(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "ndjson",
	)
	require.NoError(err, "export sessions")
	assert.Empty(stderr)

	lines := nonEmptyLines(stdout)
	require.Len(lines, 3)
	meta := decodeExportSessionsDocument(t, lines[0])
	assert.Equal("meta", meta.Type)
	assert.Equal(export.SessionSummarySchemaVersion, meta.SchemaVersion)
	assert.NotEmpty(meta.DatabaseID)
	assert.NotNil(meta.Pricing)
	assert.NotNil(meta.Projects)
	assert.Empty(meta.Sessions)

	for _, line := range lines[1:] {
		var row db.SessionSummaryRow
		require.NoError(json.Unmarshal([]byte(line), &row))
		assert.NotEmpty(row.ID)
	}
}

func TestExportSessionsAllJSONEmitsOneDocument(t *testing.T) {
	assert := assert.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--all", "--format", "json",
	)
	require.NoError(t, err, "export all sessions")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Len(doc.Sessions, 2)
	assert.Empty(doc.Cursor.Next)
	assert.Empty(strings.TrimSpace(decoderRemainder(t, stdout)),
		"--all must not concatenate JSON documents")
}

func TestExportSessionsAllJSONMergesPricingAcrossPages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	setupExportGoldenDataDir(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(),
		"export", "sessions",
		"--all",
		"--format", "json",
		"--limit", "1",
	)
	require.NoError(err, "export all sessions")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(doc.Sessions, 4)
	models, ok := doc.Pricing["models"].(map[string]any)
	require.True(ok, "pricing.models must be an object")
	assert.Contains(models, goldenComputedModel)
	assert.Contains(models, goldenReportedModel)
	assert.Empty(doc.Cursor.Next)
}

func TestExportSessionsAllJSONPreservesCostOnlyReportedPricingAcrossPages(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(database.SetDatabaseIDForTest(
		t.Context(), "cost-only-reported-export-db"))
	require.NoError(database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "computed-model", InputPerMTok: money.MustParseDollars("1"),
	}}))
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "computed", Project: "alpha", Machine: "local", Agent: "codex",
		StartedAt:    dbtest.Ptr("2026-06-16T11:00:00Z"),
		EndedAt:      dbtest.Ptr("2026-06-16T11:10:00Z"),
		MessageCount: 2, UserMessageCount: 2,
	})
	require.NoError(database.InsertMessages([]db.Message{{
		SessionID: "computed", Ordinal: 0, Role: "assistant",
		Timestamp: "2026-06-16T11:05:00Z", Model: "computed-model",
		TokenUsage: jsontext.Value(`{"input_tokens":1000000}`),
	}}))
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "cost-only-reported", Project: "alpha", Machine: "local",
		Agent: "copilot", StartedAt: dbtest.Ptr("2026-06-16T10:00:00Z"),
		EndedAt:      dbtest.Ptr("2026-06-16T10:10:00Z"),
		MessageCount: 2, UserMessageCount: 2,
	})
	reportedCost := money.MustParseDollars("0.03")
	require.NoError(database.ReplaceSessionUsageEvents(
		"cost-only-reported", []db.UsageEvent{{
			Source: "shutdown", Model: "copilot-cost-only",
			Cost: &reportedCost, CostStatus: "exact",
			CostSource: db.CopilotReportedCostSource,
			OccurredAt: "2026-06-16T10:10:00Z", DedupKey: "final",
		}},
	))
	require.NoError(database.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--all", "--format", "json",
		"--limit", "1",
	)
	require.NoError(err, "export all sessions")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(doc.Sessions, 2)
	assert.Equal(string(export.CostSourceMixed), doc.Pricing["cost_source"])
	require.NotNil(doc.Sessions[1].ModelUsage)
	assert.Equal("cost-only-reported", doc.Sessions[1].ID)
	assert.Equal(reportedCost, doc.Sessions[1].ModelUsage.Cost)
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

	got := buildExportSessionsOutput([]db.SessionExportResult{
		page("p1:sha256:first"), page("p1:sha256:second"),
	})

	require.Contains(t, got.Projects, projectKey)
	assert.Equal(t, export.ProjectResolutionAmbiguous,
		got.Projects[projectKey].Resolution)
	assert.Nil(t, got.Projects[projectKey].Identity)
}

func TestMergeExportSessionsPricingTreatsOnlyComputedNoModelPagesAsNeutral(
	t *testing.T,
) {
	assert := assert.New(t)

	noModels := &export.PricingBlock{
		CostSource: export.CostSourceComputed,
		Models:     map[string]export.ModelPricingProvenance{},
	}
	mixedNoModels := &export.PricingBlock{
		CostSource: export.CostSourceMixed,
		Models:     map[string]export.ModelPricingProvenance{},
	}
	reported := &export.PricingBlock{
		CostSource: export.CostSourceReported,
		Models: map[string]export.ModelPricingProvenance{
			"reported-model": {
				CostSource: export.CostSourceReported,
			},
		},
	}

	got := mergeExportSessionsPricing(noModels, reported)
	assert.Equal(export.CostSourceReported, got.CostSource)

	got = mergeExportSessionsPricing(reported, noModels)
	assert.Equal(export.CostSourceReported, got.CostSource)

	got = mergeExportSessionsPricing(noModels, noModels)
	assert.Equal(export.CostSourceComputed, got.CostSource)

	got = mergeExportSessionsPricing(noModels, mixedNoModels)
	assert.Equal(export.CostSourceMixed, got.CostSource)

	got = mergeExportSessionsPricing(mixedNoModels, noModels)
	assert.Equal(export.CostSourceMixed, got.CostSource)
}

func TestMergeExportSessionsPricingCombinesReportedModelResolutions(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	base := &export.PricingBlock{
		CostSource: export.CostSourceComputed,
		Models: map[string]export.ModelPricingProvenance{
			"kimi-for-coding": {
				CostSource: export.CostSourceComputed,
				Resolutions: []export.EffectiveModelRate{{
					PricedModel: "kimi-k3",
					CostSource:  export.CostSourceComputed,
				}},
			},
		},
	}
	next := &export.PricingBlock{
		CostSource: export.CostSourceMixed,
		Models: map[string]export.ModelPricingProvenance{
			"kimi-for-coding": {
				CostSource: export.CostSourceMixed,
				Resolutions: []export.EffectiveModelRate{
					{
						PricedModel: "moonshot/kimi-k2.6",
						CostSource:  export.CostSourceComputed,
					},
					{
						PricedModel: "kimi-k3",
						CostSource:  export.CostSourceReported,
					},
				},
			},
		},
	}

	got := mergeExportSessionsPricing(base, next)

	require.Contains(got.Models, "kimi-for-coding")
	provenance := got.Models["kimi-for-coding"]
	assert.Equal(export.CostSourceMixed, provenance.CostSource)
	require.Len(provenance.Resolutions, 2)
	assert.Equal("kimi-k3", provenance.Resolutions[0].PricedModel)
	assert.Equal(export.CostSourceMixed,
		provenance.Resolutions[0].CostSource)
	assert.Equal("moonshot/kimi-k2.6",
		provenance.Resolutions[1].PricedModel)
	assert.Equal(export.CostSourceComputed,
		provenance.Resolutions[1].CostSource)
}

func TestMergeExportSessionsModelRateSumsPricingApplications(t *testing.T) {
	base := export.EffectiveModelRate{
		Bands: []export.PricingBand{{AboveInputTokens: 200_000}},
		Application: export.PricingApplication{
			BaseRequestCount:  1,
			AggregateRowCount: 2,
			Bands: []export.AppliedPricingBand{{
				AboveInputTokens: 200_000,
				RequestCount:     3,
			}},
		},
	}
	next := export.EffectiveModelRate{
		Bands: []export.PricingBand{{AboveInputTokens: 200_000}},
		Application: export.PricingApplication{
			BaseRequestCount:  4,
			AggregateRowCount: 5,
			Bands: []export.AppliedPricingBand{
				{AboveInputTokens: 200_000, RequestCount: 6},
				{AboveInputTokens: 272_000, RequestCount: 7},
			},
		},
	}

	got := mergeExportSessionsModelRate(base, next)
	next.Bands[0].AboveInputTokens = 1
	next.Application.Bands[0].RequestCount = 99

	assert.Equal(t, []export.PricingBand{{AboveInputTokens: 200_000}}, got.Bands)
	assert.Equal(t, export.PricingApplication{
		BaseRequestCount:  5,
		AggregateRowCount: 7,
		Bands: []export.AppliedPricingBand{
			{AboveInputTokens: 200_000, RequestCount: 9},
			{AboveInputTokens: 272_000, RequestCount: 7},
		},
	}, got.Application)
}

func TestExportSessionsAllNDJSONCursorNextEmpty(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--all", "--format", "ndjson",
	)
	require.NoError(err, "export all sessions")
	assert.Empty(stderr)

	lines := nonEmptyLines(stdout)
	require.Len(lines, 3)
	meta := decodeExportSessionsDocument(t, lines[0])
	assert.Empty(meta.Cursor.Next)
}

func TestExportSessionsInvalidCursorWritesStructuredResetError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--cursor", "not-a-cursor",
	)
	require.Error(err, "invalid cursor")
	assert.Equal(4, exitCodeFromError(err))
	assert.Empty(stdout)

	var got exportSessionsDocument
	require.NoError(json.Unmarshal([]byte(stderr), &got))
	assert.Equal("cursor_reset", got.Error)
	assert.Equal("session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(got.DatabaseID)
}

func TestExportSessionsWrongDatabaseCursorWritesStructuredResetError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))
	cursor := firstExportSessionsCursor(t)

	otherDir := t.TempDir()
	other := seedExportSessionsArchiveAt(t, filepath.Join(otherDir, "sessions.db"))
	require.NoError(other.SetDatabaseIDForTest(
		t.Context(), "other-export-sessions-test-db"))
	t.Setenv("AGENTSVIEW_DATA_DIR", otherDir)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--cursor", cursor,
	)
	require.Error(err, "wrong database cursor")
	assert.Equal(4, exitCodeFromError(err))
	assert.Empty(stdout)

	var got exportSessionsDocument
	require.NoError(json.Unmarshal([]byte(stderr), &got))
	assert.Equal("cursor_reset", got.Error)
	assert.Equal("session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(got.DatabaseID)
}

func TestExportSessionsCursorResetMainStderrIsOnlyStructuredJSON(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))

	cmd := exec.CommandContext(t.Context(),
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
	require.Error(err, "cursor reset should exit non-zero")
	assert.Empty(stdout)

	var exitErr *exec.ExitError
	require.ErrorAs(err, &exitErr)
	assert.Equal(sessionExportCursorResetExitCode, exitErr.ExitCode())
	stderr := string(exitErr.Stderr)
	assert.NotContains(stderr, "fatal:")

	var got exportSessionsDocument
	require.NoError(json.Unmarshal([]byte(stderr), &got))
	assert.Equal("cursor_reset", got.Error)
	assert.Equal("session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(got.DatabaseID)
}

func TestExportSessionsMainStillPrintsFatalForNonCursorErrors(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))

	cmd := exec.CommandContext(t.Context(),
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
	require.Error(err, "invalid format should exit non-zero")
	assert.Empty(stdout)

	var exitErr *exec.ExitError
	require.ErrorAs(err, &exitErr)
	assert.Equal(1, exitErr.ExitCode())
	assert.Contains(string(exitErr.Stderr), "fatal:")
	assert.Contains(string(exitErr.Stderr), "invalid argument \"xml\"")
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
	assert := assert.New(t)

	seedExportSessionsArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "xml",
	)
	require.Error(t, err, "invalid format")
	assert.NotEqual(4, exitCodeFromError(err))
	assert.Empty(stdout)
	assert.Empty(stderr)
}

func TestExportSessionsRunsWhileWriteOwnerLockHeld(t *testing.T) {
	assert := assert.New(t)

	dataDir := testDataDir(t)
	seedExportSessionsArchiveAt(t, filepath.Join(dataDir, "sessions.db"))
	holdWriteOwnerLockForTest(t, dataDir)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.NoError(t, err, "read-only export should not need writer lock")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	assert.Len(doc.Sessions, 2)
	assert.Equal("export-sessions-test-db", doc.DatabaseID)
}

func TestExportSessionsRequiresExistingDatabaseID(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(database.Close())
	removeArchiveDatabaseIDForTest(t, dbPath)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
	)
	require.Error(err, "export sessions should not initialize metadata")
	assert.Empty(stdout)
	assert.Empty(stderr)
	assert.Contains(err.Error(), "database id")

	readonly, openErr := db.OpenReadOnly(dbPath)
	require.NoError(openErr)
	t.Cleanup(func() { require.NoError(readonly.Close()) })
	_, idErr := readonly.GetDatabaseID(t.Context())
	require.ErrorIs(idErr, db.ErrDatabaseIDMissing)
}

func TestExportSessionsUpgradeRequiresBackgroundEvidenceBackfill(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "legacy", Project: "agentsview", Machine: "local",
		Agent: "codex", Cwd: "/work/agentsview", MessageCount: 2,
		UserMessageCount: 1,
	})
	require.NoError(database.Close())
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = raw.ExecContext(t.Context(), `DROP TABLE session_project_identity_snapshots`)
	require.NoError(err)
	require.NoError(raw.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "status",
	)
	require.NoError(err)
	assert.Empty(stderr)
	assert.Equal("project identity evidence: pending (0/1)\n", stdout)

	stdout, stderr, err = executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.Error(err)
	assert.Empty(stderr)
	assert.Empty(stdout)
	assert.Contains(err.Error(), "project identity evidence backfill is pending")
	assert.Contains(err.Error(), "agentsview export status")

	raw, err = sql.Open("sqlite3", dbPath)
	require.NoError(err)
	defer raw.Close()
	var exists int
	require.NoError(raw.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'session_project_identity_snapshots'
	`).Scan(&exists))
	assert.Equal(1, exists)
	var state string
	require.NoError(raw.QueryRowContext(t.Context(), `
		SELECT state FROM background_migrations
		WHERE name = 'session_project_identity_snapshots_v1'
	`).Scan(&state))
	assert.Equal("pending", state)
}

func TestExportStatusReportsBackfillFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	database := dbtest.OpenTestDBAt(t, dbPath)
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(err)
	_, err = raw.ExecContext(t.Context(), `
		INSERT INTO background_migrations (
			name, state, total_items, completed_items, last_error
		) VALUES (
			'session_project_identity_snapshots_v1', 'failed', 3, 1,
			'git metadata unavailable'
		)
	`)
	require.NoError(err)
	require.NoError(raw.Close())
	require.NoError(database.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "status",
	)
	require.NoError(err)
	assert.Empty(stderr)
	assert.Equal("project identity evidence: failed (1/3)\n"+
		"last error: git metadata unavailable\n",
		stdout)
}

func TestExportSessionsDoesNotUpgradeUnrelatedReadOnlyOpenFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")
	require.NoError(os.WriteFile(dbPath, nil, 0o600))

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--format", "json",
	)
	require.Error(err)
	assert.Empty(stdout)
	assert.Empty(stderr)

	info, statErr := os.Stat(dbPath)
	require.NoError(statErr)
	assert.Zero(info.Size(),
		"a non-schema read failure must not initialize or rebuild the archive")
}

func removeArchiveDatabaseIDForTest(t *testing.T, dbPath string) {
	t.Helper()
	raw, err := sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	_, err = raw.ExecContext(t.Context(), `DELETE FROM archive_metadata WHERE key = 'database_id'`)
	require.NoError(t, err)
}

func TestExportSessionsCursorConflictingFilterIsUsageError(t *testing.T) {
	assert := assert.New(t)

	seedExportSessionsArchive(t)
	cursor := firstExportSessionsCursor(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--cursor", cursor,
		"--project", "alpha",
	)
	require.Error(t, err, "cursor with filter should fail as usage error")
	assert.NotEqual(4, exitCodeFromError(err))
	assert.Empty(stdout)
	assert.Empty(stderr)
	assert.Contains(err.Error(), "--cursor cannot be combined with --project")
}

func TestExportSessionsCursorAllowsFormatAndLimit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	seedExportSessionsArchive(t)
	cursor := firstExportSessionsCursor(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--cursor", cursor,
		"--format", "json",
		"--limit", "1",
	)
	require.NoError(err, "cursor with format and limit")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(doc.Sessions, 1)
	assert.Equal("alpha-old", doc.Sessions[0].ID)
}

func TestExportSessionsCursorResumesFilteredExport(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err, "first filtered export page")
	require.Empty(stderr)
	first := decodeExportSessionsDocument(t, stdout)
	require.Len(first.Sessions, 1)
	assert.Equal("alpha-new", first.Sessions[0].ID)
	require.NotEmpty(first.Cursor.Next)

	stdout, stderr, err = executeExportSessionsCommand(
		newRootCommand(), "export", "sessions",
		"--cursor", first.Cursor.Next,
	)
	require.NoError(err, "filtered cursor resume")
	require.Empty(stderr)
	second := decodeExportSessionsDocument(t, stdout)
	require.Len(second.Sessions, 1)
	assert.Equal("alpha-old", second.Sessions[0].ID)
	assert.Empty(second.Cursor.Next)
}

func TestExportSessionsJSONGolden(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	setupExportGoldenDataDir(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(),
		"export", "sessions",
		"--format", "json",
		"--limit", "2",
	)
	require.NoError(err, "export sessions json golden")
	require.Empty(stderr)
	assert.NotContains(stdout, "/fixtures/")
	assert.NotContains(stdout, `"cwd"`)
	assert.NotContains(stdout, `"machine":"golden-host"`)
	assert.NotContains(stdout, `"root_path":"/`)

	assertGoldenBytes(t, "session_export_v6.json", []byte(stdout))
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

	assertGoldenBytes(t, "session_export_v6.ndjson", []byte(stdout))
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
	assert := assert.New(t)
	require := require.New(t)

	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(database.SetDatabaseIDForTest(
		t.Context(), "fallback-pricing-test-db"))

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
	require.NoError(database.InsertMessages([]db.Message{
		{
			SessionID: "fallback-priced", Ordinal: 0, Role: "user",
			Content: "question", ContentLength: len("question"),
			Timestamp: "2026-06-01T10:00:00Z",
		},
		{
			SessionID: "fallback-priced", Ordinal: 1, Role: "assistant",
			Content: "answer", ContentLength: len("answer"),
			Timestamp: "2026-06-01T10:05:00Z", Model: model,
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`),
		},
		{
			SessionID: "fallback-priced", Ordinal: 2, Role: "user",
			Content: "follow up", ContentLength: len("follow up"),
			Timestamp: "2026-06-01T10:06:00Z",
		},
	}), "insert messages")
	require.NoError(database.Close(), "close seeded archive")

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions")
	require.NoError(err, "export sessions on unseeded archive")
	assert.Empty(stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(doc.Sessions, 1, "exported sessions")
	usage := doc.Sessions[0].ModelUsage
	require.NotNil(usage, "model usage")
	assert.True(usage.HasCost,
		"fallback-priced model %s should have cost", model)
	assert.Positive(usage.Cost.Microdollars, "fallback-priced cost")

	fallback, ok := doc.Pricing["fallback"].(map[string]any)
	require.True(ok, "pricing fallback block")
	assert.Equal(true, fallback["used"], "fallback used")
	assert.Contains(doc.Pricing["source"], "embedded",
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
		t.Context(), "export-sessions-test-db"))
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
		t.Context(), export.ProjectIdentityObservation{
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
	reader := strings.NewReader(input)
	dec := jsontext.NewDecoder(reader)
	var doc any
	require.NoError(t, json.UnmarshalDecode(dec, &doc))
	rest, err := io.ReadAll(io.MultiReader(
		bytes.NewReader(dec.UnreadBuffer()), reader,
	))
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
