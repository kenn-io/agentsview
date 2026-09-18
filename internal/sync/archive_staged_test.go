package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

func TestCodexStagedArchiveProjection(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	for _, policy := range []config.ArchiveContent{config.ArchiveContentTranscripts, config.ArchiveContentUsage} {
		t.Run(string(policy), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			root := writeCodexParityRoot(t, uuid)
			database := openTestDB(t)
			engine := NewEngine(database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "local", ArchiveContent: policy, StagedCodexParseMinBytes: 1,
			})
			t.Cleanup(engine.Close)
			stats := engine.SyncAll(t.Context(), nil)
			require.Zero(stats.Failed)
			require.Equal(1, stats.Synced)
			id := "codex:" + uuid
			msgs, err := database.GetAllMessages(t.Context(), id)
			require.NoError(err)
			require.NotEmpty(msgs)
			for _, msg := range msgs {
				if policy.UsageOnly() {
					assert.Empty(msg.Content)
					assert.Empty(msg.ToolCalls)
				}
				for _, call := range msg.ToolCalls {
					assert.Empty(call.InputJSON)
					assert.Empty(call.ResultContent)
					for _, event := range call.ResultEvents {
						assert.Empty(event.Content)
					}
				}
			}
			if !policy.UsageOnly() {
				assert.Equal("run the suite", msgs[0].Content)
			}
			sess, err := database.GetSessionFull(t.Context(), id)
			require.NoError(err)
			require.NotNil(sess)
			assert.Zero(sess.SecretLeakCount)
			if policy.UsageOnly() {
				assert.Zero(sess.ToolFailureSignalCount)
			} else {
				// Keep the explicit error status, discard the content-only failure.
				assert.Equal(1, sess.ToolFailureSignalCount)
			}
			findings, err := database.SessionSecretFindings(t.Context(), id)
			require.NoError(err)
			assert.Empty(findings)
			// Resumable SHA state may include raw trailing transcript bytes.
			_, found, err := database.GetParserCheckpointBlobs(id)
			require.NoError(err)
			assert.False(found)
		})
	}
}
