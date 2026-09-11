package main

import (
	"encoding/json/v2"
	"errors"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

func TestExportJointProjectScopeAgreesAcrossHourDayAndDigest(t *testing.T) {
	seedExportReportingGoldenArchive(t)
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	out, stderr, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
		"export", "day", "--schema-version", "4", "2026-07-28")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var all export.ReportingDay
	require.NoError(t, json.Unmarshal([]byte(out), &all))
	var key string
	foundStandalone := false
	for _, cell := range all.Hours[11].Joint.Cells {
		if cell.Project == reportingGoldenProject {
			key = cell.ProjectKey
		}
		if cell.Agent == "cursor" {
			foundStandalone = true
			assert.Empty(t, cell.ProjectKey)
			assert.Equal(t, "unknown", cell.Automation)
			assert.Equal(t, int64(4_000), cell.Pricing.ReportedCost.Microdollars)
			assert.Zero(t, cell.AgentMinutes)
		}
	}
	require.True(t, foundStandalone, "standalone cost must remain visible without invented attribution")
	require.NotEmpty(t, key)
	out, stderr, err = executeExportSessionsCommand(newExportReportingTestRoot(now),
		"export", "day", "--schema-version", "4", "--project-key", key, "2026-07-28")
	require.NoError(t, err)
	assert.Empty(t, stderr)
	var day export.ReportingDay
	require.NoError(t, json.Unmarshal([]byte(out), &day))
	assert.Equal(t, int64(200), day.Hours[11].Usage.Totals.OutputTokens)
	assert.Equal(t, 3.0, day.Hours[11].Activity.Totals.AgentMinutes)
	assert.NotContains(t, out, "fixture-cross")
	assert.NotContains(t, out, `"content"`)
	for _, cell := range day.Hours[11].Joint.Cells {
		assert.Equal(t, key, cell.ProjectKey)
	}

	hourOut, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
		"export", "hour", "--schema-version", "4", "--project-key", key, "2026-07-28-11")
	require.NoError(t, err)
	var hour export.ReportingHour
	require.NoError(t, json.Unmarshal([]byte(hourOut), &hour))
	assert.Equal(t, day.Hours[11], hour)
	digestOut, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
		"export", "digest", "--schema-version", "4", "--project-key", key,
		"--from", "2026-07-28", "--to", "2026-07-28")
	require.NoError(t, err)
	var digest export.ReportingDigest
	require.NoError(t, json.Unmarshal([]byte(digestOut), &digest))
	require.Len(t, digest.Days, 1)
	assert.Equal(t, day.Digest, digest.Days[0].DayDigest)
	assert.Equal(t, hour.Digest, digest.Days[0].HourDigests[11])
}

func TestExportJointRejectsScopeOnOldVersionBeforeOpening(t *testing.T) {
	for _, args := range [][]string{
		{"export", "hour", "2026-07-28-12"},
		{"export", "day", "2026-07-28"},
		{"export", "digest", "--from", "2026-07-28", "--to", "2026-07-28"},
	} {
		t.Run(args[1], func(t *testing.T) {
			opened := false
			deps := exportReportingDeps{now: time.Now,
				openDatabase: func(*cobra.Command) (*db.DB, func(), error) {
					opened = true
					return nil, nil, errors.New("archive must not be opened")
				}}
			out, _, err := executeExportSessionsCommand(newExportReportingTestRootWithDeps(deps),
				append(args, "--schema-version", "3", "--project-key", "synthetic-key")...)
			assert.ErrorContains(t, err, "project scope requires reporting schema 4")
			assert.False(t, opened)
			assert.Empty(t, out)
		})
	}
}

func TestExportJointBucketResolutionAcrossCommands(t *testing.T) {
	seedExportReportingGoldenArchive(t)
	now := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
	quietDigests := make(map[string]string)
	for _, tc := range []struct {
		bucket             string
		seconds, count     int
		usageBucket        string
		firstBucketMinutes float64
	}{
		{"1m", 60, 60, "2026-07-28T11:10:00Z", 1},
		{"2m", 120, 30, "2026-07-28T11:10:00Z", 2},
		{"5m", 300, 12, "2026-07-28T11:10:00Z", 3},
		{"15m", 900, 4, "2026-07-28T11:00:00Z", 3},
		{"1h", 3600, 1, "2026-07-28T11:00:00Z", 3},
	} {
		t.Run(tc.bucket, func(t *testing.T) {
			flags := []string{"--schema-version", "4", "--bucket", tc.bucket}
			out, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
				append([]string{"export", "day", "2026-07-28"}, flags...)...)
			require.NoError(t, err)
			var day export.ReportingDay
			require.NoError(t, json.Unmarshal([]byte(out), &day))
			require.Len(t, day.Hours, 24)
			for _, hour := range day.Hours {
				assert.Len(t, hour.Activity.Buckets, tc.count, "quiet hours use the same resolution")
			}
			assert.Equal(t, 3.0, day.Hours[11].Activity.Totals.AgentMinutes, "bucket size must not change the inactivity gap cap")
			assert.Equal(t, tc.firstBucketMinutes, day.Hours[11].Activity.Buckets[0].AgentMinutes)
			assert.Equal(t, int64(256), day.Hours[11].Usage.Totals.OutputTokens)
			assert.Equal(t, int64(71_000), day.Hours[11].Usage.Totals.Cost.Microdollars)
			found := false
			for _, cell := range day.Hours[11].Joint.Cells {
				if cell.Model == reportingGoldenLatestModel && cell.Usage.OutputTokens > 0 {
					found = true
					assert.Equal(t, tc.usageBucket, cell.BucketStart)
					assert.Equal(t, int64(200), cell.Usage.OutputTokens)
				}
			}
			require.True(t, found)
			quietDigests[tc.bucket] = day.Hours[0].Digest
			hourOut, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
				append([]string{"export", "hour", "2026-07-28-11"}, flags...)...)
			require.NoError(t, err)
			var hour export.ReportingHour
			require.NoError(t, json.Unmarshal([]byte(hourOut), &hour))
			assert.Equal(t, day.Hours[11], hour)
			digestOut, _, err := executeExportSessionsCommand(newExportReportingTestRoot(now),
				append([]string{"export", "digest", "--from", "2026-07-28", "--to", "2026-07-28"}, flags...)...)
			require.NoError(t, err)
			var digest export.ReportingDigest
			require.NoError(t, json.Unmarshal([]byte(digestOut), &digest))
			require.Len(t, digest.Days, 1)
			assert.Equal(t, day.Digest, digest.Days[0].DayDigest)
			assert.Equal(t, hour.Digest, digest.Days[0].HourDigests[11])
			for _, document := range []string{out, hourOut, digestOut} {
				var wire map[string]any
				require.NoError(t, json.Unmarshal([]byte(document), &wire))
				assert.Equal(t, float64(tc.seconds), wire["bucket_seconds"])
			}
		})
	}
	assert.NotEqual(t, quietDigests["1m"], quietDigests["5m"], "even empty replacements bind resolution")
}

func TestExportJointRejectsInvalidBucketBeforeOpening(t *testing.T) {
	for _, args := range [][]string{
		{"export", "hour", "2026-07-28-12"},
		{"export", "day", "2026-07-28"},
		{"export", "digest", "--from", "2026-07-28", "--to", "2026-07-28"},
	} {
		for _, tc := range []struct{ version, bucket, message string }{
			{"4", "0m", "whole-minute divisor"},
			{"4", "-1m", "whole-minute divisor"},
			{"4", "30s", "whole-minute divisor"},
			{"4", "7m", "whole-minute divisor"},
			{"4", "2h", "whole-minute divisor"},
			{"4", "invalid", "invalid reporting bucket"},
			{"3", "1m", "bucket selection requires reporting schema 4"},
		} {
			t.Run(args[1]+"/v"+tc.version+"/"+tc.bucket, func(t *testing.T) {
				opened := false
				deps := exportReportingDeps{now: time.Now,
					openDatabase: func(*cobra.Command) (*db.DB, func(), error) {
						opened = true
						return nil, nil, errors.New("archive must not be opened")
					}}
				out, _, err := executeExportSessionsCommand(newExportReportingTestRootWithDeps(deps),
					append(append([]string(nil), args...), "--schema-version", tc.version, "--bucket", tc.bucket)...)
				assert.ErrorContains(t, err, tc.message)
				assert.False(t, opened)
				assert.Empty(t, out)
			})
		}
	}
}
