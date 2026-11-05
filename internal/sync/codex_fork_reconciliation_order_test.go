package sync

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexForkRestoredOrderKeepsUsage(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, resolveFirst := range []bool{false, true} {
			t.Run(strconv.FormatInt(stage, 10)+"/resolved="+strconv.FormatBool(resolveFirst), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				a := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-a", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "answer A", forkReviewTime))
				b := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-b", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "answer B", forkReviewTime))
				id := "codex:" + forkReviewChild
				for _, snapshot := range []string{b + testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 7, 0)), a + testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))} {
					require.NoError(t, os.WriteFile(cp, []byte(meta+snapshot), 0o600))
					require.Zero(t, e.SyncAll(t.Context(), nil).Failed)
				}
				require.Equal(t, []string{"answer B", "answer A"}, messageContents(t, d, id))
				require.NoError(t, os.WriteFile(cp, []byte(meta+a+b), 0o600))
				if resolveFirst {
					require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
				}
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
				assert.Equal(t, []string{"answer A", "answer B"}, messageContents(t, d, id))
				if !resolveFirst {
					state, err := d.GetCodexForkState(t.Context(), id)
					require.NoError(t, err)
					require.NotNil(t, state)
					require.Len(t, state.Messages, 2)
					require.NotNil(t, state.Messages[0].PendingUsage)
					assert.Equal(t, 5, state.Messages[0].PendingUsage.OutputTokens)
					require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
				}
				for range 2 {
					require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
					usage, err := d.GetSessionUsage(t.Context(), id, true)
					require.NoError(t, err)
					require.NotNil(t, usage)
					assert.Equal(t, 12, usage.TotalOutputTokens)
				}
			})
		}
	}
}
