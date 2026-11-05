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

func TestCodexForkRetainsSummaryWithoutOutput(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, keepCall := range []bool{false, true} {
			for _, blocked := range []bool{false, true} {
				t.Run(strconv.FormatInt(stage, 10)+"/call="+strconv.FormatBool(keepCall)+"/blocked="+strconv.FormatBool(blocked), func(t *testing.T) {
					e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
					prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
					require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
					require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
					id := "codex:" + forkReviewChild
					messages, err := d.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					require.Len(t, messages, 1)
					require.Len(t, messages[0].ToolCalls, 1)
					const output = "saved summary-only result"
					messages[0].ToolCalls[0].ResultContent = output
					messages[0].ToolCalls[0].ResultContentLength = len(output)
					require.NoError(t, d.ReplaceSessionMessages(t.Context(), id, messages))
					if blocked {
						e.blockedResultCategories = map[string]bool{"Bash": true}
					}
					if !keepCall {
						require.NoError(t, os.WriteFile(cp, []byte(meta), 0o600))
					}
					for retry := range 3 {
						if retry == 1 {
							require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
						}
						require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
						stored, err := d.GetAllMessages(t.Context(), id)
						require.NoError(t, err)
						require.Len(t, stored, 1)
						require.Len(t, stored[0].ToolCalls, 1)
						call := stored[0].ToolCalls[0]
						if blocked {
							assert.Empty(t, call.ResultContent)
						} else {
							assert.Equal(t, output, call.ResultContent)
						}
						assert.Equal(t, len(output), call.ResultContentLength, "retry %d", retry)
						assert.Empty(t, call.ResultEvents)
					}
				})
			}
		}
	}
}
