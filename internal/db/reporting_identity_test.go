package db

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func TestReportingJointProjectIdentityRequiresEverySession(t *testing.T) {
	for _, tc := range []struct {
		name, secondRemote string
		resolution         export.ProjectResolution
		usageOnly          bool
	}{
		{"same repository", "https://example.com/team/api.git", export.ProjectResolutionResolved, false},
		{"same label different repository", "https://example.com/private/api.git", export.ProjectResolutionAmbiguous, false},
		{"missing identity", "", export.ProjectResolutionUnknown, false},
		{"usage only missing identity", "", export.ProjectResolutionUnknown, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			for i, remote := range []string{"https://example.com/team/api.git", tc.secondRemote} {
				id := []string{"session-a", "session-b"}[i]
				insertSession(t, d, id, "api", func(s *Session) {
					s.Agent = "agent-a"
					s.StartedAt, s.EndedAt = new("2026-07-28T12:00:00Z"), new("2026-07-28T12:01:00Z")
				})
				if !tc.usageOnly || i == 0 {
					insertMessages(t, d, Message{SessionID: id, Ordinal: 0, Role: "user", Timestamp: "2026-07-28T12:00:00Z"})
				}
				insertMessages(t, d, Message{SessionID: id, Ordinal: 1, Role: "assistant", Timestamp: "2026-07-28T12:01:00Z", Model: "model-a", TokenUsage: jsontext.Value(`{"output_tokens":10}`)})
				if remote != "" {
					require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(), export.ProjectIdentityObservation{
						SessionID: id, Project: "api", Machine: "synthetic", GitRemote: remote,
						ObservedAt: time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC),
					}))
				}
			}
			opts := ReportingExportOptions{Date: time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC),
				Now: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC), SchemaVersion: 4}
			day, err := d.ExportReportingDay(t.Context(), opts)
			require.NoError(t, err)
			hour := day.Hours[12]
			assert.Equal(t, int64(20), hour.Usage.Totals.OutputTokens)
			require.NotEmpty(t, hour.Joint.Cells)
			require.Contains(t, hour.Joint.Projects, hour.Joint.Cells[0].ProjectKey)
			identity := hour.Joint.Projects[hour.Joint.Cells[0].ProjectKey]
			assert.Equal(t, tc.resolution, identity.Resolution)
			if tc.resolution == export.ProjectResolutionResolved {
				require.NotNil(t, identity.Identity)
				assert.Equal(t, "example.com/team/api", identity.Identity.NormalizedRemote)
				opts.afterSnapshot = func() {
					_, err := d.getWriter().Exec(`DELETE FROM session_project_identity_snapshots WHERE session_id = 'session-b'`)
					require.NoError(t, err)
				}
				during, err := d.ExportReportingDay(t.Context(), opts)
				require.NoError(t, err)
				assert.Equal(t, day.Digest, during.Digest, "identity and usage share the read snapshot")
				opts.afterSnapshot = nil
				after, err := d.ExportReportingDay(t.Context(), opts)
				require.NoError(t, err)
				assert.NotEqual(t, hour.Digest, after.Hours[12].Digest, "identity-only correction changes the hour digest")
				assert.NotEqual(t, day.Digest, after.Digest)
				assert.Equal(t, hour.Joint.Cells, after.Hours[12].Joint.Cells)
				assert.Equal(t, export.ProjectResolutionUnknown, after.Hours[12].Joint.Projects[hour.Joint.Cells[0].ProjectKey].Resolution)
			} else {
				assert.Nil(t, identity.Identity)
			}
			if tc.usageOnly {
				assert.Equal(t, 1.0, hour.Activity.Totals.AgentMinutes)
			} else {
				assert.Equal(t, 2.0, hour.Activity.Totals.AgentMinutes)
			}
		})
	}
}
