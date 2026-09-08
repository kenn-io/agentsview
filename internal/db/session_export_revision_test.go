package db

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

func TestSessionSummaryExportRevisionFields(t *testing.T) {
	d := testDB(t)
	insertExportSession(t, d, Session{
		ID: "session-a", Project: "project-a", UserMessageCount: 1,
		EndedAt: Ptr("2026-05-01T10:00:00Z"),
	})
	// Preserve the decimal counter exactly, including beyond JSON's common
	// floating-point integer range. Missing local timestamps stay unknown.
	_, err := d.getWriter().Exec(`UPDATE sessions
		SET transcript_revision = '9007199254740993', local_modified_at = NULL
		WHERE id = 'session-a'`)
	require.NoError(t, err)
	result, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	wire, err := json.Marshal(result.Rows[0])
	require.NoError(t, err)
	var row map[string]any
	require.NoError(t, json.Unmarshal(wire, &row))
	assert.Equal(t, "9007199254740993", row["transcript_revision"])
	require.Contains(t, row, "local_modified_at")
	assert.Nil(t, row["local_modified_at"])
	_, err = d.getWriter().Exec(`UPDATE sessions
		SET local_modified_at = '2026-05-02T11:00:00.123Z'
		WHERE id = 'session-a'`)
	require.NoError(t, err)
	result, err = d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
	require.NoError(t, err)
	require.Len(t, result.Rows, 1)
	wire, err = json.Marshal(result.Rows[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &row))
	assert.Equal(t, "2026-05-02T11:00:00.123Z", row["local_modified_at"])
}

func TestSessionSummaryExportIndependentChangeSignals(t *testing.T) {
	for _, kind := range []string{"transcript", "usage-event", "project", "pricing"} {
		t.Run(kind, func(t *testing.T) {
			d := testDB(t)
			insertExportSession(t, d, Session{
				ID: "session-a", Project: "project-a", UserMessageCount: 1,
				EndedAt: Ptr("2026-05-01T10:00:00Z"),
			})
			message := Message{
				SessionID: "session-a", Ordinal: 0, Role: "assistant", Model: "model-a",
				Timestamp:  "2026-05-01T09:59:00Z",
				TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`),
			}
			insertMessages(t, d, message)
			require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
				ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("1"),
			}, {
				ModelPattern: "model-b", InputPerMTok: money.MustParseDollars("1"),
			}}))
			_, err := d.getWriter().Exec(`UPDATE model_pricing
				SET updated_at = '2100-01-01T00:00:00Z' WHERE model_pattern = 'model-b'`)
			require.NoError(t, err)
			// An old local timestamp makes a wall-clock mutation observable
			// without sleeping or relying on sub-millisecond test timing.
			_, err = d.getWriter().Exec(`UPDATE sessions
				SET local_modified_at = '2026-05-01T10:01:00Z'`)
			require.NoError(t, err)
			before, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
			require.NoError(t, err)
			require.Len(t, before.Rows, 1)
			switch kind {
			case "transcript":
				message.TokenUsage = []byte(`{"input_tokens":50,"output_tokens":20}`)
				require.NoError(t, d.ReplaceSessionMessages("session-a", []Message{message}))
			case "usage-event":
				require.NoError(t, d.ReplaceSessionUsageEvents("session-a", []UsageEvent{{
					Source: "provider", Model: "model-a", InputTokens: 200,
					OccurredAt: "2026-05-01T09:59:00Z",
				}}))
			case "project":
				require.NoError(t, d.UpsertProjectIdentityObservation(t.Context(),
					export.ProjectIdentityObservation{
						SessionID: "session-a", Project: "project-a", Machine: defaultMachine,
						GitRemote:  "https://example.com/team/project-a.git",
						ObservedAt: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
					}))
			case "pricing":
				require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
					ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("2"),
				}}))
			}
			after, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
			require.NoError(t, err)
			require.Len(t, after.Rows, 1)
			a, b := after.Rows[0], before.Rows[0]
			assert.Equal(t, b.LastActivityAt, a.LastActivityAt)
			if kind == "transcript" {
				assert.NotEqual(t, b.TranscriptRevision, a.TranscriptRevision)
			} else {
				assert.Equal(t, b.TranscriptRevision, a.TranscriptRevision)
			}
			switch kind {
			case "transcript", "usage-event":
				require.NotNil(t, a.LocalModifiedAt)
				assert.NotEqual(t, b.LocalModifiedAt, a.LocalModifiedAt)
				assert.NotEqual(t, b.ModelUsage.InputTokens, a.ModelUsage.InputTokens)
			case "project":
				assert.Equal(t, export.ProjectResolutionResolved, a.ProjectReference.Resolution)
				assert.NotEqual(t, b.ProjectReference, a.ProjectReference)
			case "pricing":
				require.NotNil(t, after.Pricing)
				require.NotNil(t, before.Pricing)
				assert.NotEqual(t, before.Pricing.Digest, after.Pricing.Digest)
				assert.Equal(t, before.Pricing.LatestRowUpdatedAt, after.Pricing.LatestRowUpdatedAt,
					"changing another rate need not advance the latest pricing timestamp")
				assert.NotEqual(t, b.ModelUsage.Cost, a.ModelUsage.Cost)
			}
		})
	}
}

func TestSessionSummaryExportTranscriptRevisionSurvivesResync(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "identical", true: "corrected-usage"}[changed], func(t *testing.T) {
			source := testDB(t)
			session := Session{
				ID: "session-a", Project: "project-a", UserMessageCount: 1,
				EndedAt: Ptr("2026-05-01T10:00:00Z"),
			}
			insertExportSession(t, source, session)
			message := Message{
				SessionID: session.ID, Ordinal: 0, Role: "assistant", Model: "model-a",
				Content: "An example answer", Timestamp: "2026-05-01T09:59:00Z",
				TokenUsage: []byte(`{"input_tokens":100,"output_tokens":20}`),
			}
			insertMessages(t, source, message)
			_, err := source.getWriter().Exec(`UPDATE sessions SET transcript_revision = '7'`)
			require.NoError(t, err)
			require.NoError(t, source.CloseConnections())
			rebuilt := testDB(t)
			require.NoError(t, rebuilt.CopyArchiveIdentityFrom(source.Path()))
			insertExportSession(t, rebuilt, session)
			want := "7"
			if changed {
				message.TokenUsage = []byte(`{"input_tokens":50,"output_tokens":20}`)
				want = "8"
			}
			insertMessages(t, rebuilt, message)
			_, err = rebuilt.CopyOrphanedDataFrom(source.Path())
			require.NoError(t, err)
			require.NoError(t, rebuilt.CopySessionMetadataFrom(source.Path()))
			result, err := rebuilt.ExportSessionSummaries(t.Context(), SessionExportOptions{})
			require.NoError(t, err)
			require.Len(t, result.Rows, 1)
			wire, err := json.Marshal(result.Rows[0])
			require.NoError(t, err)
			var row map[string]any
			require.NoError(t, json.Unmarshal(wire, &row))
			assert.Equal(t, want, row["transcript_revision"])
			assert.Equal(t, "2026-05-01T10:00:00Z", row["last_activity_at"])
			assert.NotContains(t, string(wire), message.Content)
		})
	}
}
