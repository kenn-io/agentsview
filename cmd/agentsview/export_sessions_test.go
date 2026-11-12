package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	"go.kenn.io/agentsview/internal/pricing"
	agentsync "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/kit/daemon"
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
				database := seedExportSessionsArchive(t)
				require.NoError(t, database.SetArchiveIdentityForTest(
					t.Context(), "stable-archive", strings.Repeat("a", 64),
				))
				args := []string{"export", "sessions", "--format", format, "--limit", "1"}
				if empty {
					args = append(args, "--project", "absent")
				}
				stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), args...)
				require.NoError(t, err)
				require.Empty(t, stderr)
				if format == "ndjson" {
					stdout, _, _ = strings.Cut(stdout, "\n")
				}
				doc := decodeExportSessionsDocument(t, stdout)
				assert.Equal(t, "stable-archive", doc.ArchiveID)
				assert.Equal(t, "export-sessions-test-db", doc.DatabaseID)
				if !empty {
					require.NotEmpty(t, doc.Cursor.Next)
					stdout, stderr, err = executeExportSessionsCommand(newRootCommand(),
						"export", "sessions", "--cursor", doc.Cursor.Next)
					require.NoError(t, err)
					require.Empty(t, stderr)
					resumed := decodeExportSessionsDocument(t, stdout)
					assert.Equal(t, "stable-archive", resumed.ArchiveID)
					assert.Equal(t, doc.DatabaseID, resumed.DatabaseID)
				}
			})
		}
	}
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
	assert.NotEmpty(t, doc.Pricing["cost_source"])
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

func TestExportSessionsAllJSONPreservesCostOnlyReportedPricingAcrossPages(
	t *testing.T,
) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.SetDatabaseIDForTest(
		t.Context(), "cost-only-reported-export-db"))
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{{
		ModelPattern: "computed-model", InputPerMTok: money.MustParseDollars("1"),
	}}))
	insertExportSessionsTestSession(t, database, db.Session{
		ID: "computed", Project: "alpha", Machine: "local", Agent: "codex",
		StartedAt:    dbtest.Ptr("2026-06-16T11:00:00Z"),
		EndedAt:      dbtest.Ptr("2026-06-16T11:10:00Z"),
		MessageCount: 2, UserMessageCount: 2,
	})
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{{
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
	require.NoError(t, database.ReplaceSessionUsageEvents(t.Context(),
		"cost-only-reported", []db.UsageEvent{{
			Source: "shutdown", Model: "copilot-cost-only",
			Cost: &reportedCost, CostStatus: "exact",
			CostSource: db.CopilotReportedCostSource,
			OccurredAt: "2026-06-16T10:10:00Z", DedupKey: "final",
		}},
	))
	require.NoError(t, database.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newRootCommand(), "export", "sessions", "--all", "--format", "json",
		"--limit", "1",
	)
	require.NoError(t, err, "export all sessions")
	assert.Empty(t, stderr)

	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(t, doc.Sessions, 2)
	assert.Equal(t, string(export.CostSourceMixed), doc.Pricing["cost_source"])
	require.NotNil(t, doc.Sessions[1].ModelUsage)
	assert.Equal(t, "cost-only-reported", doc.Sessions[1].ID)
	assert.Equal(t, reportedCost, doc.Sessions[1].ModelUsage.Cost)
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
	assert.Equal(t, export.CostSourceReported, got.CostSource)

	got = mergeExportSessionsPricing(reported, noModels)
	assert.Equal(t, export.CostSourceReported, got.CostSource)

	got = mergeExportSessionsPricing(noModels, noModels)
	assert.Equal(t, export.CostSourceComputed, got.CostSource)

	got = mergeExportSessionsPricing(noModels, mixedNoModels)
	assert.Equal(t, export.CostSourceMixed, got.CostSource)

	got = mergeExportSessionsPricing(mixedNoModels, noModels)
	assert.Equal(t, export.CostSourceMixed, got.CostSource)
}

func TestMergeExportSessionsPricingCombinesReportedModelResolutions(t *testing.T) {
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

	require.Contains(t, got.Models, "kimi-for-coding")
	provenance := got.Models["kimi-for-coding"]
	assert.Equal(t, export.CostSourceMixed, provenance.CostSource)
	require.Len(t, provenance.Resolutions, 2)
	assert.Equal(t, "kimi-k3", provenance.Resolutions[0].PricedModel)
	assert.Equal(t, export.CostSourceMixed,
		provenance.Resolutions[0].CostSource)
	assert.Equal(t, "moonshot/kimi-k2.6",
		provenance.Resolutions[1].PricedModel)
	assert.Equal(t, export.CostSourceComputed,
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
	assert.Equal(t, "session export cursor is no longer valid; restart the export",
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
		t.Context(), "other-export-sessions-test-db"))
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
	assert.Equal(t, "session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(t, got.DatabaseID)
}

func TestExportSessionsCursorResetMainStderrIsOnlyStructuredJSON(t *testing.T) {
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
	assert.Equal(t, "session export cursor is no longer valid; restart the export",
		got.Message,
	)
	assert.NotEmpty(t, got.DatabaseID)
}

func TestExportSessionsMainPrintsValidationHintForInvalidFormat(t *testing.T) {
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
	require.Error(t, err, "invalid format should exit non-zero")
	assert.Empty(t, stdout)

	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 65, exitErr.ExitCode())
	assert.Contains(t, string(exitErr.Stderr), "fatal:")
	assert.Contains(t, string(exitErr.Stderr), "\nhint:")
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
	require.FailNow(t, "missing helper args")
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

	readonly, openErr := db.OpenReadOnly(t.Context(), dbPath)
	require.NoError(t, openErr)
	t.Cleanup(func() { require.NoError(t, readonly.Close()) })
	_, idErr := readonly.GetDatabaseID(t.Context())
	require.ErrorIs(t, idErr, db.ErrDatabaseIDMissing)
}

func TestExportSessionsUpgradesRecordedRepositoryColumn(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("parser_resync_%t", stale), func(t *testing.T) {
			dataDir := testDataDir(t)
			dbPath := filepath.Join(dataDir, "sessions.db")
			seedExportSessionsArchiveAt(t, dbPath)
			raw, err := sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			if stale {
				_, err = raw.ExecContext(t.Context(), "PRAGMA user_version=137")
				require.NoError(t, err)
			}
			var versionBefore int
			require.NoError(t, raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&versionBefore))
			_, err = raw.ExecContext(t.Context(), "ALTER TABLE session_project_identity_snapshots DROP COLUMN recorded_git_remote")
			require.NoError(t, err)
			require.NoError(t, raw.Close())

			stdout, stderr, err := executeExportSessionsCommand(
				newRootCommand(), "export", "sessions", "--format", "json",
			)
			assert.Empty(t, stderr)
			if stale {
				require.ErrorContains(t, err, "archive data needs resync")
				assert.Empty(t, stdout)
			} else {
				require.NoError(t, err)
				doc := decodeExportSessionsDocument(t, stdout)
				assert.Len(t, doc.Sessions, 2)
				assert.Equal(t, "export-sessions-test-db", doc.DatabaseID)
			}
			// The schema is readable, but export must not bypass parser upgrades.
			readonly, err := db.OpenReadOnly(t.Context(), dbPath)
			require.NoError(t, err)
			assert.Equal(t, stale, readonly.NeedsResync())
			require.NoError(t, readonly.Close())

			raw, err = sql.Open("sqlite3", dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, raw.Close()) })
			var versionAfter int
			require.NoError(t, raw.QueryRowContext(t.Context(), "PRAGMA user_version").Scan(&versionAfter))
			assert.Equal(t, versionBefore, versionAfter, "an export must not mark unread sources as reparsed")
		})
	}
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
	_, err = raw.ExecContext(t.Context(), `
		DROP TABLE session_project_identity_snapshots;
		DROP TABLE conversation_messages;
		DROP TABLE conversation_session_changes;
	`)
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
	assert.Contains(t, err.Error(), "project identification is pending")
	assert.Contains(t, err.Error(), "agentsview export sessions --wait")

	raw, err = sql.Open("sqlite3", dbPath)
	require.NoError(t, err)
	defer raw.Close()
	var exists int
	require.NoError(t, raw.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'session_project_identity_snapshots'
	`).Scan(&exists))
	assert.Equal(t, 1, exists)
	var state string
	require.NoError(t, raw.QueryRowContext(t.Context(), `
		SELECT state FROM background_migrations
		WHERE name = 'session_project_identity_snapshots_v1'
	`).Scan(&state))
	assert.Equal(t, "pending", state)
}

func pendingExportArchive(t *testing.T, archiveOnly bool) string {
	t.Helper()
	database := seedExportSessionsArchive(t)
	if archiveOnly {
		require.NoError(t, database.EnableArchiveOnly(t.Context()))
	}
	require.NoError(t, database.Close())
	path := filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "sessions.db")
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `DELETE FROM session_project_identity_snapshots; DELETE FROM background_migrations WHERE name = 'session_project_identity_snapshots_v1'; DELETE FROM stats WHERE key = 'session_project_identity_snapshots_v1_verified'`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	return path
}

func TestExportSessionsArchiveOnlyPending(t *testing.T) {
	for _, state := range []string{"pending", "failed"} {
		t.Run(state, func(t *testing.T) {
			if state == "pending" {
				pendingExportArchive(t, true)
			} else {
				database := seedExportSessionsArchive(t)
				require.NoError(t, database.EnableArchiveOnly(t.Context()))
				require.NoError(t, database.Close())
				path := filepath.Join(os.Getenv("AGENTSVIEW_DATA_DIR"), "sessions.db")
				raw, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state, total_items, completed_items, last_error) VALUES ('session_project_identity_snapshots_v1', 'failed', 2, 2, 'completion blocked')`)
				require.NoError(t, err)
				require.NoError(t, raw.Close())
			}
			forbidStartBackgroundServeForTransport(t, "archive-only export must not start a daemon")
			for _, args := range [][]string{{"export", "sessions"}, {"export", "sessions", "--wait"}} {
				stdout, _, err := executeExportSessionsCommand(newRootCommand(), args...)
				require.ErrorContains(t, err, "archive-only")
				assert.Contains(t, err.Error(), "writable source")
				assert.Empty(t, stdout)
			}
		})
	}
}

type exportProgressSignal chan struct{}

func (s exportProgressSignal) Write(p []byte) (int, error) {
	select {
	case s <- struct{}{}:
	default:
	}
	return len(p), nil
}

func exportWaitCommand(t *testing.T, ctx context.Context, progress io.Writer) (*bytes.Buffer, <-chan error) {
	t.Helper()
	cmd, _, err := newRootCommand().Find([]string{"export", "sessions"})
	require.NoError(t, err)
	cmd.SetContext(ctx)
	out := new(bytes.Buffer)
	cmd.SetOut(out)
	cmd.SetErr(progress)
	done := make(chan error, 1)
	go func() { done <- runExportSessions(cmd, exportSessionsConfig{Limit: 500, Wait: true}) }()
	return out, done
}

func waitExportProgress(t *testing.T, progress <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-progress:
	case err := <-done:
		require.FailNow(t, "export stopped before progress", "%v", err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "export did not report progress")
	}
}

func TestExportWaitDuringDaemonChanges(t *testing.T) {
	for _, mode := range []string{"daemon stops", "daemon stops during retry", "temporary ping failures", "single ping"} {
		t.Run(mode, func(t *testing.T) {
			path := pendingExportArchive(t, false)
			if mode == "daemon stops during retry" {
				raw, err := sql.Open("sqlite3", path)
				require.NoError(t, err)
				_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state, last_error, updated_at) VALUES (?, 'failed', 'disk error', strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 hour'))`, db.ProjectIdentityBackfillName)
				require.NoError(t, err)
				require.NoError(t, raw.Close())
			}
			dir := filepath.Dir(path)
			var fail, observed, calls atomic.Int32
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Service: daemonService, Version: "test"})
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if fail.Load() > 0 {
					fail.Add(-1)
					observed.Add(1)
					http.Error(w, "temporary ping error", http.StatusServiceUnavailable)
					return
				}
				ping.ServeHTTP(w, r)
			}))
			t.Cleanup(ts.Close)
			ep := serverEndpoint(t, ts)
			writeDaemonRuntimeForTest(t, dir, ep.Host, ep.Port, "test", false)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			progress := make(chan struct{}, 1)
			out, done := exportWaitCommand(t, ctx, exportProgressSignal(progress))
			waitExportProgress(t, progress, done)
			assert.Empty(t, out.String())
			switch mode {
			case "daemon stops", "daemon stops during retry":
				ts.Close()
				RemoveDaemonRuntime(dir)
				err := <-done
				require.ErrorContains(t, err, "agentsview daemon restart")
				require.ErrorContains(t, err, "agentsview export status")
				if mode == "daemon stops during retry" {
					assert.Contains(t, err.Error(), "disk error")
				}
				assert.Empty(t, out.String())
			case "single ping":
				before := calls.Load()
				waitExportProgress(t, progress, done)
				assert.EqualValues(t, 1, calls.Load()-before)
				cancel()
				require.ErrorIs(t, <-done, context.Canceled)
				assert.Empty(t, out.String())
			case "temporary ping failures":
				fail.Store(2)
				for observed.Load() < 2 {
					waitExportProgress(t, progress, done)
				}
				require.EqualValues(t, 2, observed.Load())
				require.NoError(t, backfillExportArchive(t.Context(), path))
				require.NoError(t, <-done)
				assert.Len(t, decodeExportSessionsDocument(t, out.String()).Sessions, 2)
			}
		})
	}
}

func backfillExportArchive(ctx context.Context, path string) error {
	database, err := db.Open(ctx, path)
	if err != nil {
		return err
	}
	defer database.Close()
	engine := agentsync.NewEngine(ctx, database, agentsync.EngineConfig{Machine: "local"})
	defer engine.Close()
	return engine.BackfillProjectIdentitySnapshots(ctx)
}

func TestExportStatusReportsLiveFailureWithOnePing(t *testing.T) {
	path := pendingExportArchive(t, false)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"agentsview","pid":%d,"project_identity_backfill_error":"blocked"}`, os.Getpid())
	}))
	t.Cleanup(ts.Close)
	ep := serverEndpoint(t, ts)
	writeDaemonRuntimeForTest(t, filepath.Dir(path), ep.Host, ep.Port, "test", false)
	stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "status")
	require.NoError(t, err)
	assert.Contains(t, stdout, "blocked")
	assert.Contains(t, stdout, "agentsview daemon restart")
	assert.EqualValues(t, 1, calls.Load())
}

func stubExportDaemon(t *testing.T, dir string) func() {
	t.Helper()
	home := t.TempDir()
	var child *exec.Cmd
	stop := func() {
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
			child = nil
		}
	}
	stubStartBackgroundServeForTransport(t, func(ctx context.Context, cfg *config.Config, _ time.Duration, replace bool) (*DaemonRuntime, error) {
		if replace {
			return nil, errors.New("unexpected daemon replacement")
		}
		child = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExportDaemonHelper$")
		child.Env = append(os.Environ(), "AGENTSVIEW_EXPORT_HELPER=1", "AGENTSVIEW_DATA_DIR="+dir, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"))
		if err := child.Start(); err != nil {
			return nil, err
		}
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			if rt := FindWritableDaemonRuntime(cfg.DataDir, cfg.AuthToken); rt != nil {
				return rt, nil
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
	})
	t.Cleanup(stop)
	return stop
}

func TestExportSessionsStartsRealWorker(t *testing.T) {
	path := pendingExportArchive(t, false)
	stubExportDaemon(t, filepath.Dir(path))
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	out, done := exportWaitCommand(t, ctx, io.Discard)
	require.NoError(t, <-done)
	doc := decodeExportSessionsDocument(t, out.String())
	require.Len(t, doc.Sessions, 2)
	assert.Equal(t, "alpha", doc.Sessions[0].ProjectReference.DisplayLabel)
	assert.NotEmpty(t, doc.Sessions[0].ProjectReference.ProjectKey)
	database, err := db.OpenReadOnly(t.Context(), path)
	require.NoError(t, err)
	defer database.Close()
	snapshots, err := database.ListSessionProjectIdentitySnapshots(t.Context())
	require.NoError(t, err)
	assert.Len(t, snapshots, 2)
}

func TestExportWaitAfterDaemonRestart(t *testing.T) {
	for _, mode := range []string{"missing evidence", "completed evidence"} {
		t.Run(mode, func(t *testing.T) { testExportFailureRecovery(t, mode, nil) })
	}
}

func TestExportFailureSurvivesInterruptedDaemonStartup(t *testing.T) {
	path := pendingExportArchive(t, false)
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state, last_error) VALUES (?, 'failed', 'disk error')`, db.ProjectIdentityBackfillName)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	database, err := openDBWithProgress(t.Context(), config.Config{DBPath: path, DataDir: filepath.Dir(path)}, nil)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	status, _, err := executeExportSessionsCommand(newRootCommand(), "export", "status")
	require.NoError(t, err)
	assert.Contains(t, status, "failed")
	assert.Contains(t, status, "disk error")
}

func TestExportWaitDuringRestartBeforeWorkerStarts(t *testing.T) {
	path := pendingExportArchive(t, false)
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state, last_error, updated_at) VALUES (?, 'failed', 'disk error', strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 hour'))`, db.ProjectIdentityBackfillName)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	dir := filepath.Dir(path)
	writeLiveRuntime(t, dir, false)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	progress := make(chan struct{}, 1)
	out, done := exportWaitCommand(t, ctx, exportProgressSignal(progress))
	waitExportProgress(t, progress, done)
	assert.Empty(t, out.String())
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestExportLiveFailureReporting(t *testing.T) {
	testExportFailureRecovery(t, "unrecordable failure", func(t *testing.T) {
		t.Helper()
		status, _, err := executeExportSessionsCommand(newRootCommand(), "export", "status")
		require.NoError(t, err)
		assert.Contains(t, status, "failed")
		assert.Contains(t, status, "backfill writes blocked")
		assert.Contains(t, status, "agentsview daemon restart")
		assert.NotContains(t, status, "agentsview export status")
		stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "sessions")
		require.ErrorContains(t, err, "backfill writes blocked")
		assert.Contains(t, err.Error(), "agentsview daemon restart")
		assert.Contains(t, err.Error(), "agentsview export status")
		assert.Empty(t, stdout)
	})
}

func testExportFailureRecovery(t *testing.T, mode string, verify func(*testing.T)) {
	t.Helper()
	path := pendingExportArchive(t, false)
	dir := filepath.Dir(path)
	if mode == "completed evidence" {
		database, err := db.Open(t.Context(), path)
		require.NoError(t, err)
		engine := agentsync.NewEngine(t.Context(), database, agentsync.EngineConfig{Machine: "local"})
		require.NoError(t, engine.BackfillProjectIdentitySnapshots(t.Context()))
		engine.Close()
		require.NoError(t, database.Close())
	}
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state) VALUES ('session_project_identity_snapshots_v1', 'pending') ON CONFLICT(name) DO UPDATE SET state = 'pending'`)
	require.NoError(t, err)
	trigger := `CREATE TRIGGER fail_backfill BEFORE INSERT ON session_project_identity_snapshots BEGIN SELECT RAISE(FAIL, 'backfill blocked'); END`
	switch mode {
	case "completed evidence":
		trigger = `CREATE TRIGGER fail_backfill BEFORE UPDATE ON background_migrations WHEN NEW.state = 'completed' BEGIN SELECT RAISE(FAIL, 'backfill blocked'); END`
	case "unrecordable failure":
		trigger = `CREATE TRIGGER fail_backfill BEFORE UPDATE ON background_migrations WHEN NEW.state IN ('running', 'failed') BEGIN SELECT RAISE(FAIL, 'backfill writes blocked'); END`
	}
	_, err = raw.ExecContext(t.Context(), trigger)
	require.NoError(t, err)
	stop := stubExportDaemon(t, dir)
	cfg := config.Config{DataDir: dir}
	_, err = startExportDaemon(t.Context(), &cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	out, done := exportWaitCommand(t, ctx, io.Discard)
	err = <-done
	require.ErrorContains(t, err, "agentsview daemon restart")
	assert.Contains(t, err.Error(), "agentsview export status")
	assert.Empty(t, out.String())
	var state string
	require.NoError(t, raw.QueryRowContext(t.Context(), `SELECT state FROM background_migrations WHERE name = 'session_project_identity_snapshots_v1'`).Scan(&state))
	if mode != "unrecordable failure" {
		require.Equal(t, "failed", state)
	} else {
		require.Equal(t, "pending", state)
		assert.Contains(t, err.Error(), "backfill writes blocked")
		verify(t)
	}
	stop()
	_, err = raw.ExecContext(t.Context(), `DROP TRIGGER fail_backfill`)
	require.NoError(t, err)
	_, err = startExportDaemon(t.Context(), &cfg)
	require.NoError(t, err)
	out, done = exportWaitCommand(t, ctx, io.Discard)
	require.NoError(t, <-done)
	assert.Len(t, decodeExportSessionsDocument(t, out.String()).Sessions, 2)
	status, _, err := executeExportSessionsCommand(newRootCommand(), "export", "status")
	require.NoError(t, err)
	assert.Contains(t, status, "completed")
}

func TestExportDaemonStartupCases(t *testing.T) {
	t.Run("startup error", func(t *testing.T) {
		dir := testDataDir(t)
		failure := errors.New("launch failed")
		stubStartBackgroundServeForTransport(t, func(context.Context, *config.Config, time.Duration, bool) (*DaemonRuntime, error) {
			return nil, failure
		})
		cfg := config.Config{DataDir: dir}
		_, err := startExportDaemon(t.Context(), &cfg)
		assert.ErrorIs(t, err, failure)
	})
	t.Run("read-only mirror", func(t *testing.T) {
		dir := testDataDir(t)
		host, port := testPingServer(t)
		writeDaemonRuntimeForTest(t, dir, host, port, "test", true)
		started := false
		stubStartBackgroundServeForTransport(t, func(_ context.Context, _ *config.Config, _ time.Duration, replace bool) (*DaemonRuntime, error) {
			started = true
			assert.False(t, replace)
			return &DaemonRuntime{}, nil
		})
		cfg := config.Config{DataDir: dir}
		rt, err := startExportDaemon(t.Context(), &cfg)
		require.NoError(t, err)
		require.NotNil(t, rt)
		assert.True(t, started)
	})
	t.Run("autostart disabled", func(t *testing.T) {
		dir := testDataDir(t)
		host, port := testPingServer(t)
		writeDaemonRuntimeForTest(t, dir, host, port, "test", true)
		t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
		forbidStartBackgroundServeForTransport(t, "autostart disabled")
		cfg := config.Config{DataDir: dir}
		_, err := startExportDaemon(t.Context(), &cfg)
		assert.ErrorContains(t, err, "agentsview daemon start")
	})
}

func TestExportWaitRejectsIncompatibleDaemon(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  int
	}{
		{"older", daemonAPIVersion - 1},
		{"newer", daemonAPIVersion + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := pendingExportArchive(t, false)
			writeLiveRuntime(t, filepath.Dir(path), false, withRuntimeVersion(tc.name), withRuntimeAPIVersion(tc.api))
			forbidStartBackgroundServeForTransport(t, "incompatible daemon")
			stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "sessions", "--wait")
			require.Error(t, err)
			if tc.name == "older" {
				assert.Contains(t, err.Error(), fmt.Sprintf("daemon API version %d", tc.api))
				assert.Contains(t, err.Error(), "agentsview daemon restart")
				assert.Contains(t, err.Error(), "agentsview export status")
				assert.Contains(t, err.Error(), "agentsview export sessions --wait")
				assert.NotContains(t, err.Error(), "pg push --watch")
			} else {
				assert.Contains(t, err.Error(), "Upgrade this agentsview install")
				assert.NotContains(t, err.Error(), "daemon restart")
			}
			assert.Empty(t, stdout)
		})
	}
}

func TestExportWaitRejectsNewerBackfillState(t *testing.T) {
	path := pendingExportArchive(t, false)
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	defer raw.Close()
	_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state) VALUES ('session_project_identity_snapshots_v1', 'future_state')`)
	require.NoError(t, err)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, filepath.Dir(path), host, port, "test", false)
	stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "sessions", "--wait")
	require.ErrorContains(t, err, "future_state")
	require.ErrorContains(t, err, "agentsview export status")
	assert.Empty(t, stdout)
}

func TestExportWaitCursorResetSuppressesStubStartupProgress(t *testing.T) {
	path := pendingExportArchive(t, false)
	dir := filepath.Dir(path)
	host, port := testPingServer(t)
	started := false
	polled := make(chan struct{})
	stubStartBackgroundServeForTransport(t, func(ctx context.Context, cfg *config.Config, _ time.Duration, replace bool) (*DaemonRuntime, error) {
		started = true
		if replace {
			return nil, errors.New("unexpected replacement")
		}
		fmt.Fprintln(daemonProgressWriter(ctx), "starting test daemon")
		writeDaemonRuntimeForTest(t, dir, host, port, "test", false)
		close(polled)
		return FindWritableDaemonRuntime(cfg.DataDir, cfg.AuthToken), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	workerDone := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			workerDone <- ctx.Err()
			return
		case <-polled:
		}
		workerDone <- backfillExportArchive(ctx, path)
	}()
	root := newRootCommand()
	root.SetContext(ctx)
	var stdout, stderr string
	var err error
	startupOutput := captureStderr(t, func() {
		stdout, stderr, err = executeExportSessionsCommand(root, "export", "sessions", "--wait", "--cursor", "invalid")
	})
	assert.True(t, started)
	assert.Empty(t, startupOutput)
	require.NoError(t, <-workerDone)
	require.Error(t, err)
	assert.Equal(t, sessionExportCursorResetExitCode, exitCodeFromError(err))
	assert.Empty(t, stdout)
	var doc exportSessionsDocument
	require.NoError(t, json.Unmarshal([]byte(stderr), &doc))
	assert.Equal(t, "cursor_reset", doc.Error)
}

func TestExportSessionsWaitReadyDoesNotStartDaemon(t *testing.T) {
	seedExportSessionsArchive(t)
	forbidStartBackgroundServeForTransport(t, "ready export must not start a daemon")
	stdout, stderr, err := executeExportSessionsCommand(newRootCommand(), "export", "sessions", "--wait")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	assert.Len(t, decodeExportSessionsDocument(t, stdout).Sessions, 2)
	cursor := firstExportSessionsCursor(t)
	stdout, stderr, err = executeExportSessionsCommand(newRootCommand(), "export", "sessions", "--cursor", cursor, "--wait")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	doc := decodeExportSessionsDocument(t, stdout)
	require.Len(t, doc.Sessions, 1)
	assert.Equal(t, "alpha-old", doc.Sessions[0].ID)
}

func TestExportDaemonHelper(t *testing.T) {
	if os.Getenv("AGENTSVIEW_EXPORT_HELPER") != "1" {
		return
	}
	dir := os.Getenv("AGENTSVIEW_DATA_DIR")
	cfg := config.Config{Host: "127.0.0.1", Port: 0, DataDir: dir, DBPath: filepath.Join(dir, "sessions.db"), NoSync: true}
	runServe(t.Context(), cfg, serveOptions{}, 0)
}

func TestExportSessionsFailedBackfillDoesNotStartDaemon(t *testing.T) {
	path := pendingExportArchive(t, false)
	raw, err := sql.Open("sqlite3", path)
	require.NoError(t, err)
	_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state, total_items, completed_items, last_error) VALUES ('session_project_identity_snapshots_v1', 'failed', 1, 0, 'metadata unavailable')`)
	require.NoError(t, err)
	require.NoError(t, raw.Close())
	forbidStartBackgroundServeForTransport(t, "failed backfill must not start a daemon")
	stdout, _, err := executeExportSessionsCommand(newRootCommand(), "export", "sessions", "--wait")
	require.ErrorContains(t, err, "project identification failed: metadata unavailable; run `agentsview daemon restart`, then check `agentsview export status`")
	assert.Empty(t, stdout)
}

func TestExportStatusReportsBackfillFailure(t *testing.T) {
	t.Run("recorded failure", func(t *testing.T) {
		dataDir := testDataDir(t)
		dbPath := filepath.Join(dataDir, "sessions.db")
		database := dbtest.OpenTestDBAt(t, dbPath)
		raw, err := sql.Open("sqlite3", dbPath)
		require.NoError(t, err)
		_, err = raw.ExecContext(t.Context(), `
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
		assert.Equal(t, "project identity evidence: failed (1/3)\n"+
			"last error: git metadata unavailable\nrun `agentsview daemon restart`\n",
			stdout)
	})
	t.Run("restart before worker starts", func(t *testing.T) {
		path := pendingExportArchive(t, false)
		raw, err := sql.Open("sqlite3", path)
		require.NoError(t, err)
		_, err = raw.ExecContext(t.Context(), `INSERT INTO background_migrations (name, state, last_error, updated_at) VALUES (?, 'failed', 'disk error', strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '-1 hour'))`, db.ProjectIdentityBackfillName)
		require.NoError(t, err)
		require.NoError(t, raw.Close())
		writeLiveRuntime(t, filepath.Dir(path), false)
		status, _, err := executeExportSessionsCommand(newRootCommand(), "export", "status")
		require.NoError(t, err)
		assert.Contains(t, status, "pending")
		assert.NotContains(t, status, "disk error")
	})
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
	_, err = raw.ExecContext(t.Context(), `DELETE FROM archive_metadata WHERE key = 'database_id'`)
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

	assertGoldenBytes(t, "session_export_v7.json", []byte(stdout))
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

	assertGoldenBytes(t, "session_export_v7.ndjson", []byte(stdout))
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
	require.NoError(t, database.InsertMessages(t.Context(), []db.Message{
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
	require.FailNow(t, "no exact fallback-priced model in embedded snapshot")
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
	require.NoError(t, database.UpsertSession(t.Context(), session),
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
