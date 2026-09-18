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
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(err)
	result, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
	require.NoError(err)
	require.Len(result.Rows, 1)
	wire, err := json.Marshal(result.Rows[0])
	require.NoError(err)
	var row map[string]any
	require.NoError(json.Unmarshal(wire, &row))
	assert.Equal("9007199254740993", row["transcript_revision"])
	require.Contains(row, "local_modified_at")
	assert.Nil(row["local_modified_at"])
	_, err = d.getWriter().Exec(`UPDATE sessions
		SET local_modified_at = '2026-05-02T11:00:00.123Z'
		WHERE id = 'session-a'`)
	require.NoError(err)
	result, err = d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
	require.NoError(err)
	require.Len(result.Rows, 1)
	wire, err = json.Marshal(result.Rows[0])
	require.NoError(err)
	require.NoError(json.Unmarshal(wire, &row))
	assert.Equal("2026-05-02T11:00:00.123Z", row["local_modified_at"])
}

func TestSessionSummaryExportIndependentChangeSignals(t *testing.T) {
	for _, kind := range []string{"transcript", "usage-event", "project", "pricing"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(d.UpsertModelPricing([]ModelPricing{{
				ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("1"),
			}, {
				ModelPattern: "model-b", InputPerMTok: money.MustParseDollars("1"),
			}}))
			_, err := d.getWriter().Exec(`UPDATE model_pricing
				SET updated_at = '2100-01-01T00:00:00Z' WHERE model_pattern = 'model-b'`)
			require.NoError(err)
			// An old local timestamp makes a wall-clock mutation observable
			// without sleeping or relying on sub-millisecond test timing.
			_, err = d.getWriter().Exec(`UPDATE sessions
				SET local_modified_at = '2026-05-01T10:01:00Z'`)
			require.NoError(err)
			before, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
			require.NoError(err)
			require.Len(before.Rows, 1)
			switch kind {
			case "transcript":
				message.TokenUsage = []byte(`{"input_tokens":50,"output_tokens":20}`)
				require.NoError(d.ReplaceSessionMessages("session-a", []Message{message}))
			case "usage-event":
				require.NoError(d.ReplaceSessionUsageEvents("session-a", []UsageEvent{{
					Source: "provider", Model: "model-a", InputTokens: 200,
					OccurredAt: "2026-05-01T09:59:00Z",
				}}))
			case "project":
				require.NoError(d.UpsertProjectIdentityObservation(t.Context(),
					export.ProjectIdentityObservation{
						SessionID: "session-a", Project: "project-a", Machine: defaultMachine,
						GitRemote:  "https://example.com/team/project-a.git",
						ObservedAt: time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC),
					}))
			case "pricing":
				require.NoError(d.UpsertModelPricing([]ModelPricing{{
					ModelPattern: "model-a", InputPerMTok: money.MustParseDollars("2"),
				}}))
			}
			after, err := d.ExportSessionSummaries(t.Context(), SessionExportOptions{})
			require.NoError(err)
			require.Len(after.Rows, 1)
			a, b := after.Rows[0], before.Rows[0]
			assert.Equal(b.LastActivityAt, a.LastActivityAt)
			if kind == "transcript" {
				assert.NotEqual(b.TranscriptRevision, a.TranscriptRevision)
			} else {
				assert.Equal(b.TranscriptRevision, a.TranscriptRevision)
			}
			switch kind {
			case "transcript", "usage-event":
				require.NotNil(a.LocalModifiedAt)
				assert.NotEqual(b.LocalModifiedAt, a.LocalModifiedAt)
				assert.NotEqual(b.ModelUsage.InputTokens, a.ModelUsage.InputTokens)
			case "project":
				assert.Equal(export.ProjectResolutionResolved, a.ProjectReference.Resolution)
				assert.NotEqual(b.ProjectReference, a.ProjectReference)
			case "pricing":
				require.NotNil(after.Pricing)
				require.NotNil(before.Pricing)
				assert.NotEqual(before.Pricing.Digest, after.Pricing.Digest)
				assert.Equal(before.Pricing.LatestRowUpdatedAt, after.Pricing.LatestRowUpdatedAt,
					"changing another rate need not advance the latest pricing timestamp")
				assert.NotEqual(b.ModelUsage.Cost, a.ModelUsage.Cost)
			}
		})
	}
}

func TestSessionSummaryExportTranscriptRevisionSurvivesResync(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "identical", true: "corrected-usage"}[changed], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

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
			require.NoError(err)
			require.NoError(source.CloseConnections())
			rebuilt := testDB(t)
			require.NoError(rebuilt.CopyArchiveIdentityFrom(source.Path()))
			insertExportSession(t, rebuilt, session)
			want := "7"
			if changed {
				message.TokenUsage = []byte(`{"input_tokens":50,"output_tokens":20}`)
				want = "8"
			}
			insertMessages(t, rebuilt, message)
			_, err = rebuilt.CopyOrphanedDataFrom(source.Path())
			require.NoError(err)
			require.NoError(rebuilt.CopySessionMetadataFrom(source.Path()))
			result, err := rebuilt.ExportSessionSummaries(t.Context(), SessionExportOptions{})
			require.NoError(err)
			require.Len(result.Rows, 1)
			wire, err := json.Marshal(result.Rows[0])
			require.NoError(err)
			var row map[string]any
			require.NoError(json.Unmarshal(wire, &row))
			assert.Equal(want, row["transcript_revision"])
			assert.Equal("2026-05-01T10:00:00Z", row["last_activity_at"])
			assert.NotContains(string(wire), message.Content)
		})
	}
}
