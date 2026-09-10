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
