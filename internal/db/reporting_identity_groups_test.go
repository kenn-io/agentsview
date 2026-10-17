package db

import (
	"encoding/json/jsontext"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestReportingIdentityGroupsDoNotInventProjects(t *testing.T) {
	for _, tc := range []struct {
		name, project string
		newProjects   int
	}{
		{name: "projectless"},
		{name: "redacted label", project: "/synthetic/api", newProjects: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
			insertSession(t, d, "session-a", tc.project, func(s *Session) {
				s.Agent = "agent-a"
				s.StartedAt, s.EndedAt = new("2026-07-28T12:00:00Z"), new("2026-07-28T12:02:00Z")
			})
			insertMessages(t, d,
				Message{SessionID: "session-a", Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z"},
				Message{SessionID: "session-a", Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:02:00Z", Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`)},
			)
			for _, version := range []int{export.ReportingSchemaVersion, export.ReportingJointSchemaVersion} {
				t.Run(strconv.Itoa(version), func(t *testing.T) {
					day, err := d.ExportReportingDay(t.Context(), ReportingExportOptions{
						Date: date, Now: date.Add(24 * time.Hour), SchemaVersion: version,
					})
					require.NoError(t, err)
					hour := day.Hours[12]
					assert.Equal(t, tc.newProjects, hour.Activity.Totals.NewProjects)
					assert.Len(t, hour.Activity.ByProject, tc.newProjects)
					assert.Len(t, hour.Usage.ByProject, tc.newProjects)
					assert.Equal(t, 1, hour.Activity.Totals.NewSessions)
					assert.InDelta(t, 2.0, hour.Activity.Totals.AgentMinutes, 0)
					assert.Equal(t, int64(10), hour.Usage.Totals.OutputTokens)
				})
			}
		})
	}
}

func TestReportingIdentityGroupsKeepVerifiedContributionsSelectable(t *testing.T) {
	for _, tc := range []struct {
		name, remote, snapshotLabel string
		resolution                  export.ProjectResolution
		usageOnly                   bool
	}{
		{name: "missing snapshot", resolution: export.ProjectResolutionUnknown},
		{name: "different repository", remote: "https://example.com/private/api.git", snapshotLabel: "api", resolution: export.ProjectResolutionResolved},
		{name: "ambiguous snapshot", snapshotLabel: "api", resolution: export.ProjectResolutionAmbiguous},
		{name: "stale label", remote: "https://example.com/team/api.git", snapshotLabel: "old-api", resolution: export.ProjectResolutionUnknown},
		{name: "usage without activity", resolution: export.ProjectResolutionUnknown, usageOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
			for _, id := range []string{"known", "other"} {
				insertSession(t, d, id, "api", func(s *Session) {
					s.Agent = "agent-a"
					s.StartedAt, s.EndedAt = new("2026-07-28T12:00:00Z"), new("2026-07-28T12:02:00Z")
				})
				if id == "known" || !tc.usageOnly {
					insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z"})
				}
				insertMessages(t, d, Message{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:02:00Z", Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`)})
			}
			require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
				SessionID: "known", Project: "api", Machine: "synthetic", GitRemote: "https://example.com/team/api.git", ObservedAt: date,
			}))
			if tc.snapshotLabel != "" {
				require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
					SessionID: "other", Project: tc.snapshotLabel, Machine: "synthetic", GitRemote: tc.remote,
					RemoteResolution: tc.resolution, ObservedAt: date,
				}))
			}
			opts := ReportingExportOptions{Date: date, Now: date.Add(24 * time.Hour), SchemaVersion: export.ReportingJointSchemaVersion}
			day, err := d.ExportReportingDay(t.Context(), opts)
			require.NoError(t, err)
			hour := day.Hours[12]
			require.Len(t, hour.Joint.Projects, 2)
			require.Len(t, hour.Joint.Cells, 2)
			assert.Equal(t, int64(20), hour.Usage.Totals.OutputTokens)
			assert.Equal(t, 1, hour.Activity.Totals.NewProjects)
			var knownKey string
			for key, entry := range hour.Joint.Projects {
				assert.Equal(t, "api", entry.DisplayLabel)
				if entry.Identity != nil && entry.Identity.NormalizedRemote == "example.com/team/api" {
					knownKey = key
				} else {
					assert.Equal(t, tc.resolution, entry.Resolution)
				}
			}
			require.NotEmpty(t, knownKey)
			for _, cell := range hour.Joint.Cells {
				assert.Equal(t, "api", cell.Project)
				assert.Equal(t, int64(10), cell.Usage.OutputTokens)
			}
			opts.ProjectKeys = []string{knownKey}
			scoped, err := d.ExportReportingDay(t.Context(), opts)
			require.NoError(t, err)
			got := scoped.Hours[12]
			assert.Equal(t, int64(10), got.Usage.Totals.OutputTokens)
			assert.InDelta(t, 2.0, got.Activity.Totals.AgentMinutes, 0)
			assert.Equal(t, 1, got.Activity.Buckets[0].MaxAgents)
			require.Len(t, got.Activity.ByProject, 1)
			assert.Equal(t, "api", got.Activity.ByProject[0].Project)
			assert.Equal(t, knownKey, got.Activity.ByProject[0].ProjectKey)
			require.Len(t, got.Usage.ByProject, 1)
			assert.Equal(t, "api", got.Usage.ByProject[0].Project)
			assert.Equal(t, knownKey, got.Usage.ByProject[0].ProjectKey)
			require.Len(t, got.Joint.Projects, 1)
			assert.Equal(t, export.ProjectResolutionResolved, got.Joint.Projects[knownKey].Resolution)
		})
	}
}
