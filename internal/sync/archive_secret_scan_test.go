package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

func TestStoredSecretScansRespectArchiveContent(t *testing.T) {
	for _, operation := range []string{"recompute", "scan"} {
		for _, tc := range []struct {
			policy       config.ArchiveContent
			wantFindings int
		}{
			{config.ArchiveContentFull, 3},
			{config.ArchiveContentTranscripts, 1},
			{config.ArchiveContentUsage, 0},
		} {
			t.Run(operation+"/"+string(tc.policy), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)

				fx := newEngineFixture(t)
				ctx := t.Context()
				const id = "archive-policy"
				const accessKey = "AKIA" + "7QHWN2DKR4FYPLJM"
				require.NoError(fx.db.UpsertSession(db.Session{
					ID: id, Project: "proj", Machine: "local", Agent: "claude",
					MessageCount: 2, UserMessageCount: 1,
				}))
				require.NoError(fx.db.ReplaceSessionMessages(id, []db.Message{
					{SessionID: id, Ordinal: 0, Role: "user", Content: "aws " + accessKey},
					{SessionID: id, Ordinal: 1, Role: "assistant", Content: "Checking credentials.",
						ToolCalls: []db.ToolCall{{
							ToolName: "Bash", ToolUseID: "tool-1",
							InputJSON:     `{"command":"echo ` + accessKey + `"}`,
							ResultContent: accessKey,
						}}},
				}))
				// Model a full archive opened under a stricter policy before
				// its old message payloads have been resynchronized.
				require.NoError(fx.engine.RecomputeSignals(ctx, id))
				fx.db.SetArchiveContent(tc.policy)
				if operation == "recompute" {
					require.NoError(fx.engine.RecomputeSignals(ctx, id))
				} else {
					summary, err := fx.engine.ScanSecrets(ctx, SecretScanInput{}, nil)
					require.NoError(err)
					assert.Equal(1, summary.Scanned)
					assert.Equal(tc.wantFindings, summary.TotalFindings)
				}
				findings, err := fx.db.SessionSecretFindings(ctx, id)
				require.NoError(err)
				assert.Len(findings, tc.wantFindings)
				if tc.policy == config.ArchiveContentTranscripts {
					for _, finding := range findings {
						assert.Equal("message", finding.LocationKind)
						assert.Equal(0, finding.MessageOrdinal)
					}
				}
				session, err := fx.db.GetSession(ctx, id)
				require.NoError(err)
				require.NotNil(session)
				assert.Equal(tc.wantFindings, session.SecretLeakCount)
			})
		}
	}
}
