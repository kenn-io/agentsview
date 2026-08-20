package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

const (
	reportingGoldenProject      = "project \"雪\" \\\\ fixture"
	reportingGoldenAgent        = "agent \"α\" \\\\ interactive"
	reportingGoldenPrimaryModel = "model \"雪\" \\\\ survivor"
	reportingGoldenLatestModel  = "model duplicate-later"
	reportingGoldenStandalone   = "model standalone usage"
	reportingGoldenUsageOnly    = "model usage-only α"
	reportingGoldenUsageProject = "project usage-only 雪"
	reportingGoldenUsageAgent   = "agent usage-only"
)

func TestExportHourMatchesExactCanonicalDayElement(t *testing.T) {
	seedExportReportingArchive(t)
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
	root := newExportReportingTestRoot(now)

	dayOut, stderr, err := executeExportSessionsCommand(
		root, "export", "day", "2026-07-28",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var day export.ReportingDay
	require.NoError(t, json.Unmarshal([]byte(dayOut), &day))

	hourOut, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(now),
		"export", "hour", "2026-07-28-10",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var hour export.ReportingHour
	require.NoError(t, json.Unmarshal([]byte(hourOut), &hour))

	require.Equal(t, day.Hours[10], hour)
	canonical, err := export.MarshalCanonical(day.Hours[10])
	require.NoError(t, err)
	assert.Equal(t, string(canonical)+"\n", hourOut)
}

func TestExportDayCurrentDateContainsOnlyClosedHours(t *testing.T) {
	seedExportReportingArchive(t)
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(now),
		"export", "day", "2026-07-29",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var day export.ReportingDay
	require.NoError(t, json.Unmarshal([]byte(stdout), &day))
	assert.False(t, day.Complete)
	assert.Empty(t, day.Digest)
	require.Len(t, day.Hours, 14)
	assert.Equal(t, "2026-07-29-13", day.Hours[13].Period)
}

func TestExportHourRejectsOpenAndMalformedPeriods(t *testing.T) {
	seedExportReportingArchive(t)
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
	for _, period := range []string{
		"2026-07-29-14",
		"2026-07-29-15",
		"2026-7-29-13",
		"2026-07-29T13",
		"2026-02-30-13",
	} {
		t.Run(period, func(t *testing.T) {
			stdout, stderr, err := executeExportSessionsCommand(
				newExportReportingTestRoot(now),
				"export", "hour", period,
			)
			require.Error(t, err)
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
		})
	}
}

func TestExportDigestReturnsOrderedMultiDateRange(t *testing.T) {
	seedExportReportingArchive(t)
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(now),
		"export", "digest",
		"--from", "2026-07-27",
		"--to", "2026-07-29",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var digest export.ReportingDigest
	require.NoError(t, json.Unmarshal([]byte(stdout), &digest))
	assert.Equal(t, export.ReportingSchemaVersion, digest.SchemaVersion)
	assert.Equal(t, "2026-07-27", digest.From)
	assert.Equal(t, "2026-07-29", digest.To)
	require.Len(t, digest.Days, 3)
	assert.Equal(t, "2026-07-27", digest.Days[0].Date)
	assert.True(t, digest.Days[0].Complete)
	assert.NotEmpty(t, digest.Days[0].DayDigest)
	assert.Len(t, digest.Days[0].HourDigests, 24)
	assert.Equal(t, "2026-07-29", digest.Days[2].Date)
	assert.False(t, digest.Days[2].Complete)
	assert.Empty(t, digest.Days[2].DayDigest)
	assert.Len(t, digest.Days[2].HourDigests, 14)
}

func TestExportDigestRejectsReversedRange(t *testing.T) {
	seedExportReportingArchive(t)

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(
			time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC),
		),
		"export", "digest",
		"--from", "2026-07-29",
		"--to", "2026-07-28",
	)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	assert.Contains(t, err.Error(), "--from must not be after --to")
}

func TestExportReportingSchemaVersions(t *testing.T) {
	seedExportReportingArchive(t)
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
	tests := []struct {
		name   string
		common []string
	}{
		{
			name:   "hour",
			common: []string{"export", "hour", "2026-07-28-10"},
		},
		{
			name:   "day",
			common: []string{"export", "day", "2026-07-28"},
		},
		{
			name: "digest",
			common: []string{
				"export", "digest",
				"--from", "2026-07-28",
				"--to", "2026-07-28",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defaultOut, defaultErrOut, err := executeExportSessionsCommand(
				newExportReportingTestRoot(now), tt.common...,
			)
			require.NoError(t, err)
			assert.Empty(t, defaultErrOut)

			currentArgs := append([]string(nil), tt.common...)
			currentArgs = append(currentArgs, "--schema-version", "2")
			currentOut, currentErrOut, err := executeExportSessionsCommand(
				newExportReportingTestRoot(now), currentArgs...,
			)
			require.NoError(t, err)
			assert.Empty(t, currentErrOut)
			assert.Equal(t, defaultOut, currentOut)
			assert.Contains(t, currentOut, `"schema_version":2`)

			legacyArgs := append([]string(nil), tt.common...)
			legacyArgs = append(legacyArgs, "--schema-version", "1")
			legacyOut, legacyErrOut, err := executeExportSessionsCommand(
				newExportReportingTestRoot(now), legacyArgs...,
			)
			require.NoError(t, err)
			assert.Empty(t, legacyErrOut)
			assert.Contains(t, legacyOut, `"schema_version":1`)
		})
	}
}

func TestExportReportingSchemaVersionRejectsBeforeOpen(t *testing.T) {
	now := time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "hour",
			args: []string{
				"export", "hour", "--schema-version", "3",
				"2026-07-28-10",
			},
		},
		{
			name: "day",
			args: []string{
				"export", "day", "--schema-version", "3", "2026-07-28",
			},
		},
		{
			name: "digest",
			args: []string{
				"export", "digest", "--schema-version", "3",
				"--from", "2026-07-28",
				"--to", "2026-07-28",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opened := false
			deps := exportReportingDeps{
				now: func() time.Time { return now },
				openDatabase: func(
					*cobra.Command,
				) (*db.DB, func(), error) {
					opened = true
					return nil, func() {}, errors.New(
						"unexpected database open",
					)
				},
			}
			stdout, stderr, err := executeExportSessionsCommand(
				newExportReportingTestRootWithDeps(deps), tt.args...,
			)
			require.EqualError(
				t, err, "unsupported reporting schema version 3",
			)
			assert.False(t, opened)
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
		})
	}
}

func TestExportReportingRequiresExistingArchiveWithoutCreatingOne(t *testing.T) {
	dataDir := testDataDir(t)
	dbPath := filepath.Join(dataDir, "sessions.db")

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(
			time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC),
		),
		"export", "day", "2026-07-28",
	)
	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.Empty(t, stderr)
	assert.NoFileExists(t, dbPath)
}

func TestExportReportingRunsWhileWriteOwnerLockHeld(t *testing.T) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.Close())
	holdWriteOwnerLockForTest(t, dataDir)

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(
			time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC),
		),
		"export", "day", "2026-07-28",
	)
	require.NoError(t, err)
	assert.NotEmpty(t, stdout)
	assert.Empty(t, stderr)
}

func TestExportReportingFallbackPricingOnUnseededArchive(t *testing.T) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	model := exactFallbackPricedModel(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:               "fixture-fallback-priced",
		Machine:          "fixture-machine",
		Agent:            "agent fallback",
		StartedAt:        dbtest.Ptr("2026-07-28T10:00:00Z"),
		EndedAt:          dbtest.Ptr("2026-07-28T10:06:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{
		{
			SessionID:     "fixture-fallback-priced",
			Ordinal:       0,
			Role:          "user",
			Content:       "synthetic question",
			ContentLength: len("synthetic question"),
			Timestamp:     "2026-07-28T10:00:00Z",
		},
		{
			SessionID:     "fixture-fallback-priced",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "synthetic answer",
			ContentLength: len("synthetic answer"),
			Timestamp:     "2026-07-28T10:05:00Z",
			Model:         model,
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`,
			),
		},
	}))
	seeded, err := database.HasModelPricingRows(context.Background())
	require.NoError(t, err)
	assert.False(t, seeded)
	require.NoError(t, database.Close())

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRoot(
			time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC),
		),
		"export", "hour", "2026-07-28-10",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)

	var hour export.ReportingHour
	require.NoError(t, json.Unmarshal([]byte(stdout), &hour))
	assert.Positive(t, hour.Usage.Totals.Cost.Microdollars)
	assert.Positive(t, hour.Activity.Totals.Cost.Microdollars)
}

func TestExportReportingSnapshotStoredPricingOverridesInstalledFallback(
	t *testing.T,
) {
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	t.Cleanup(func() {
		require.NoError(t, database.Close())
	})
	model := exactFallbackPricedModel(t)
	require.NoError(t, database.UpsertSession(db.Session{
		ID:               "fixture-snapshot-pricing",
		Machine:          "fixture-machine",
		Agent:            "agent snapshot",
		StartedAt:        dbtest.Ptr("2026-07-28T10:00:00Z"),
		EndedAt:          dbtest.Ptr("2026-07-28T10:06:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{
		{
			SessionID:     "fixture-snapshot-pricing",
			Ordinal:       0,
			Role:          "user",
			Content:       "synthetic question",
			ContentLength: len("synthetic question"),
			Timestamp:     "2026-07-28T10:00:00Z",
		},
		{
			SessionID:     "fixture-snapshot-pricing",
			Ordinal:       1,
			Role:          "assistant",
			Content:       "synthetic answer",
			ContentLength: len("synthetic answer"),
			Timestamp:     "2026-07-28T10:05:00Z",
			Model:         model,
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":500}`,
			),
		},
	}))

	deps := defaultExportReportingDeps()
	deps.now = func() time.Time {
		return time.Date(2026, 7, 29, 14, 37, 0, 0, time.UTC)
	}
	openDatabase := deps.openDatabase
	deps.openDatabase = func(
		cmd *cobra.Command,
	) (*db.DB, func(), error) {
		reader, cleanup, err := openDatabase(cmd)
		if err != nil {
			return nil, func() {}, err
		}
		if err := database.UpsertModelPricing([]db.ModelPricing{{
			ModelPattern:  model,
			InputPerMTok:  money.MustParseDollars("123"),
			OutputPerMTok: money.MustParseDollars("456"),
		}}); err != nil {
			cleanup()
			return nil, func() {}, err
		}
		return reader, cleanup, nil
	}

	stdout, stderr, err := executeExportSessionsCommand(
		newExportReportingTestRootWithDeps(deps),
		"export", "hour", "2026-07-28-10",
	)
	require.NoError(t, err)
	assert.Empty(t, stderr)

	var hour export.ReportingHour
	require.NoError(t, json.Unmarshal([]byte(stdout), &hour))
	assert.Equal(
		t,
		money.Money{Microdollars: 351_000},
		hour.Usage.Totals.Cost,
	)
	assert.Equal(t, hour.Usage.Totals.Cost, hour.Activity.Totals.Cost)
}

func newExportReportingTestRoot(now time.Time) *cobra.Command {
	deps := defaultExportReportingDeps()
	deps.now = func() time.Time { return now }
	return newExportReportingTestRootWithDeps(deps)
}

func newExportReportingTestRootWithDeps(
	deps exportReportingDeps,
) *cobra.Command {
	root := &cobra.Command{
		Use:           "agentsview",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddGroup(&cobra.Group{ID: groupData, Title: "Data Commands:"})
	root.AddCommand(newExportCommandWithDeps(deps))
	return root
}

func seedExportReportingArchive(t *testing.T) {
	t.Helper()
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.Close())
}

func TestExportReportingGolden(t *testing.T) {
	got, emptyDays := buildExportReportingGoldenDocuments(t)
	repeated, repeatedEmptyDays := buildExportReportingGoldenDocuments(t)
	require.Equal(t, got, repeated, "independent fixture seeds must be byte-identical")
	require.Equal(t, emptyDays, repeatedEmptyDays)

	base := filepath.Join("testdata", "reporting")
	manifest := reportingGoldenManifest(got)
	if *updateGolden {
		require.NoError(t, os.MkdirAll(base, 0o755))
		for name, contents := range got {
			require.NoError(t, os.WriteFile(
				filepath.Join(base, name), contents, 0o644,
			))
		}
		require.NoError(t, os.WriteFile(
			filepath.Join(base, "manifest.sha256"), manifest, 0o644,
		))
		t.Logf("rewrote reporting goldens under %s", base)
	} else {
		for name, contents := range got {
			want, err := os.ReadFile(filepath.Join(base, name))
			require.NoError(
				t, err, "read %s (run with -update to generate)", name,
			)
			assert.Equal(t, string(want), string(contents), name)
		}
		wantManifest, err := os.ReadFile(filepath.Join(base, "manifest.sha256"))
		require.NoError(t, err, "read reporting manifest")
		assert.Equal(t, string(wantManifest), string(manifest))
	}

	var legacyHour export.ReportingHour
	require.NoError(t, json.Unmarshal(got["hour-v1.json"], &legacyHour))
	finalLegacyHour, canonicalLegacyHour, err := export.FinalizeReportingHour(legacyHour)
	require.NoError(t, err)
	assert.Equal(t, legacyHour, finalLegacyHour)
	assert.Equal(t, string(canonicalLegacyHour)+"\n", string(got["hour-v1.json"]))
	require.Len(t, legacyHour.Usage.ByModel, 3)
	assert.Equal(t, reportingGoldenPrimaryModel, legacyHour.Usage.ByModel[0].Key)
	assert.Equal(t, int64(15_000), legacyHour.Usage.Totals.Cost.Microdollars)

	var hour export.ReportingHour
	require.NoError(t, json.Unmarshal(got["hour-v2.json"], &hour))
	finalHour, canonicalHour, err := export.FinalizeReportingHour(hour)
	require.NoError(t, err)
	assert.Equal(t, hour, finalHour)
	assert.Equal(t, string(canonicalHour)+"\n", string(got["hour-v2.json"]))

	var day export.ReportingDay
	require.NoError(t, json.Unmarshal(got["day-v2.json"], &day))
	finalDay, canonicalDay, err := export.FinalizeReportingDay(day)
	require.NoError(t, err)
	assert.Equal(t, day, finalDay)
	assert.Equal(t, string(canonicalDay)+"\n", string(got["day-v2.json"]))
	require.Equal(t, day.Hours[11], hour)
	assert.InDelta(t, 2, day.Hours[10].Activity.Totals.AgentMinutes, 0.0001)
	assert.InDelta(t, 3, hour.Activity.Totals.AgentMinutes, 0.0001)
	require.Len(t, hour.Usage.ByModel, 3)
	assert.Equal(t, reportingGoldenLatestModel, hour.Usage.ByModel[0].Key)
	assert.Equal(t, int64(71_000), hour.Usage.Totals.Cost.Microdollars)
	assert.Equal(t, int64(62_000), hour.Activity.Totals.Cost.Microdollars)
	assert.Equal(t, reportingGoldenStandalone, hour.Usage.ByModel[1].Key)
	assert.Equal(t, reportingGoldenUsageOnly, hour.Usage.ByModel[2].Key)
	require.Len(t, hour.Usage.ByAgent, 3)
	assert.Equal(t, reportingGoldenUsageAgent, hour.Usage.ByAgent[1].Key)
	assert.Equal(t, "cursor", hour.Usage.ByAgent[2].Key)
	require.Len(t, hour.Usage.ByProject, 2)
	assert.ElementsMatch(
		t,
		[]string{reportingGoldenProject, reportingGoldenUsageProject},
		[]string{
			hour.Usage.ByProject[0].Project,
			hour.Usage.ByProject[1].Project,
		},
	)
	assert.Greater(
		t,
		hour.Usage.Totals.Cost.Microdollars,
		hour.Activity.Totals.Cost.Microdollars,
	)
	require.Len(t, hour.Activity.ByAgent, 1)
	assert.Equal(t, reportingGoldenAgent, hour.Activity.ByAgent[0].Key)
	require.Len(t, hour.Activity.ByProject, 1)
	assert.Equal(t, reportingGoldenProject, hour.Activity.ByProject[0].Project)
	assert.Equal(t, 2, hour.Activity.Totals.NewModels)
	quiet := day.Hours[12]
	assert.False(t, quiet.HasData)
	assert.Zero(t, quiet.Activity.Totals.IdleMinutes)
	assert.Len(t, quiet.Activity.Buckets, 12)
	assert.Empty(t, quiet.Activity.ByModel)
	assert.Empty(t, quiet.Activity.ByAgent)
	assert.Empty(t, quiet.Activity.ByProject)
	assert.Empty(t, quiet.Usage.ByModel)

	var digest export.ReportingDigest
	require.NoError(t, json.Unmarshal(got["digest-v2.json"], &digest))
	require.Len(t, digest.Days, 2)
	assertDigestDayMatchesReportingDay(t, digest.Days[1], day)
	var empty export.ReportingDay
	require.NoError(t, json.Unmarshal(emptyDays[2], &empty))
	assert.False(t, empty.HasData)
	assertDigestDayMatchesReportingDay(t, digest.Days[0], empty)
}

func buildExportReportingGoldenDocuments(
	t *testing.T,
) (map[string][]byte, map[int][]byte) {
	t.Helper()
	seedExportReportingGoldenArchive(t)
	now := time.Date(2026, 7, 29, 12, 34, 0, 0, time.UTC)
	run := func(args ...string) []byte {
		t.Helper()
		stdout, stderr, err := executeExportSessionsCommand(
			newExportReportingTestRoot(now), args...,
		)
		require.NoError(t, err, "%v", args)
		require.Empty(t, stderr)
		return []byte(stdout)
	}
	documents := make(map[string][]byte, 6)
	emptyDays := make(map[int][]byte, 2)
	for _, version := range []int{1, 2} {
		versionValue := strconv.Itoa(version)
		documents[fmt.Sprintf("hour-v%d.json", version)] = run(
			"export", "hour", "--schema-version", versionValue, "2026-07-28-11",
		)
		documents[fmt.Sprintf("day-v%d.json", version)] = run(
			"export", "day", "--schema-version", versionValue, "2026-07-28",
		)
		documents[fmt.Sprintf("digest-v%d.json", version)] = run(
			"export", "digest", "--schema-version", versionValue,
			"--from", "2026-07-27",
			"--to", "2026-07-28",
		)
		emptyDays[version] = run(
			"export", "day", "--schema-version", versionValue, "2026-07-27",
		)
	}
	return documents, emptyDays
}

func seedExportReportingGoldenArchive(t *testing.T) {
	t.Helper()
	dataDir := testDataDir(t)
	database := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "sessions.db"))
	require.NoError(t, database.SetArchiveIdentityForTest(
		context.Background(),
		"reporting-fixture-archive",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	))
	require.NoError(t, database.UpsertModelPricing([]db.ModelPricing{
		{
			ModelPattern:  reportingGoldenPrimaryModel,
			InputPerMTok:  money.MustParseDollars("3"),
			OutputPerMTok: money.MustParseDollars("15"),
		},
		{
			ModelPattern:  reportingGoldenLatestModel,
			InputPerMTok:  money.MustParseDollars("30"),
			OutputPerMTok: money.MustParseDollars("60"),
		},
	}))
	require.NoError(t, database.UpsertProjectIdentityObservation(
		context.Background(),
		export.ProjectIdentityObservation{
			SessionID: "fixture-cross",
			Project:   reportingGoldenProject,
			Machine:   "fixture-machine",
			RootPath:  `/work/project "雪"`,
			ObservedAt: time.Date(
				2026, 7, 28, 10, 58, 0, 0, time.UTC,
			),
		},
	))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:               "fixture-cross",
		Project:          reportingGoldenProject,
		Machine:          "fixture-machine",
		Agent:            reportingGoldenAgent,
		StartedAt:        dbtest.Ptr("2026-07-28T10:58:00Z"),
		EndedAt:          dbtest.Ptr("2026-07-28T11:03:00Z"),
		MessageCount:     2,
		UserMessageCount: 1,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{
		{
			SessionID: "fixture-cross",
			Ordinal:   1,
			Role:      "user",
			Content:   "start",
			Timestamp: "2026-07-28T10:58:00Z",
		},
		{
			SessionID:       "fixture-cross",
			Ordinal:         2,
			Role:            "assistant",
			Content:         "finish",
			Timestamp:       "2026-07-28T11:03:00Z",
			Model:           reportingGoldenPrimaryModel,
			ClaudeMessageID: "fixture-duplicate-message",
			ClaudeRequestID: "fixture-duplicate-request",
			TokenUsage: jsontext.Value(
				`{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":40,"server_tool_use":{"web_search_requests":2}}`,
			),
		},
	}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:               "fixture-duplicate-later",
		Project:          reportingGoldenProject,
		Machine:          "fixture-machine",
		Agent:            "agent duplicate-later",
		StartedAt:        dbtest.Ptr("2026-07-28T11:10:00Z"),
		EndedAt:          dbtest.Ptr("2026-07-28T11:10:00Z"),
		MessageCount:     1,
		UserMessageCount: 0,
	}))
	require.NoError(t, database.InsertMessages([]db.Message{{
		SessionID:       "fixture-duplicate-later",
		Ordinal:         1,
		Role:            "assistant",
		Content:         "duplicate",
		Timestamp:       "2026-07-28T11:10:00Z",
		Model:           reportingGoldenLatestModel,
		ClaudeMessageID: "fixture-duplicate-message",
		ClaudeRequestID: "fixture-duplicate-request",
		TokenUsage: jsontext.Value(
			`{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":30,"cache_read_input_tokens":40}`,
		),
	}}))
	require.NoError(t, database.UpsertSession(db.Session{
		ID:           "fixture-usage-only",
		Project:      reportingGoldenUsageProject,
		Machine:      "fixture-machine",
		Agent:        reportingGoldenUsageAgent,
		StartedAt:    dbtest.Ptr("2026-07-26T08:00:00Z"),
		EndedAt:      dbtest.Ptr("2026-07-26T08:01:00Z"),
		MessageCount: 1,
	}))
	usageOnlyCost := money.MustParseDollars("0.005")
	require.NoError(t, database.ReplaceSessionUsageEvents(
		"fixture-usage-only",
		[]db.UsageEvent{{
			Source:       "fixture-source",
			Model:        reportingGoldenUsageOnly,
			InputTokens:  31,
			OutputTokens: 37,
			Cost:         &usageOnlyCost,
			CostStatus:   "exact",
			CostSource:   "reported",
			OccurredAt:   "2026-07-28T11:04:00Z",
			DedupKey:     "fixture-usage-only",
		}},
	))
	require.NoError(t, database.InsertCursorUsageEvents([]db.CursorUsageEvent{{
		OccurredAt:       "2026-07-28T11:05:00Z",
		Model:            reportingGoldenStandalone,
		Kind:             "usage",
		InputTokens:      17,
		OutputTokens:     19,
		CacheWriteTokens: 23,
		CacheReadTokens:  29,
		Charged:          money.MustParseDollars("0.004"),
		DedupKey:         "standalone-fixture",
	}}))
	require.NoError(t, database.Close())
}

func reportingGoldenManifest(documents map[string][]byte) []byte {
	names := make([]string, 0, len(documents))
	for name := range documents {
		names = append(names, name)
	}
	sort.Strings(names)
	var manifest bytes.Buffer
	for _, name := range names {
		sum := sha256.Sum256(documents[name])
		_, _ = fmt.Fprintf(&manifest, "%x  %s\n", sum, name)
	}
	return manifest.Bytes()
}

func assertDigestDayMatchesReportingDay(
	t *testing.T,
	digest export.ReportingDigestDay,
	day export.ReportingDay,
) {
	t.Helper()
	assert.Equal(t, day.Date, digest.Date)
	assert.Equal(t, day.Complete, digest.Complete)
	assert.Equal(t, day.HasData, digest.HasData)
	assert.Equal(t, day.Digest, digest.DayDigest)
	hourDigests := make([]string, len(day.Hours))
	for i := range day.Hours {
		hourDigests[i] = day.Hours[i].Digest
	}
	assert.Equal(t, hourDigests, digest.HourDigests)
}
