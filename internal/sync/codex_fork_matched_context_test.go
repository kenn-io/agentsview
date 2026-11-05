package sync

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexForkMatchedMessageKeepsSourceBoundary(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, tc := range []struct {
			name                          string
			firstBoundary, secondBoundary string
			secondChild, parentIdless     bool
			parentBoundary                string
		}{
			{"shortened_matching_child", "message-free-child", "", true, true, ""},
			{"child_absent_control", "message-free-child", "", false, true, ""},
			{"boundary_present_control", "message-free-child", "message-free-child", true, true, ""},
			{"named_parent_control", "message-free-child", "", true, false, ""},
			{"returning_boundary_control", "", "message-free-child", true, true, ""},
			{"changed_boundary_control", "earlier-turn", "new-child-turn", true, true, "earlier-turn"},
		} {
			t.Run(fmt.Sprintf("stage=%d/%s", stage, tc.name), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				id := "codex:" + forkReviewChild
				replay := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("assistant", "parent replay", forkReviewTime),
				)
				child := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime),
					testjsonl.CodexMsgJSON("assistant", "retained idless child", forkReviewTime),
				)
				first := meta + replay
				if tc.firstBoundary != "" {
					first += testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", tc.firstBoundary, forkReviewTime))
				}
				first += child + testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))
				require.NoError(t, os.WriteFile(cp, []byte(first), 0o600))
				require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
				require.Equal(t, []string{"parent replay", "retained idless child"}, messageContents(t, d, id))
				second := meta + replay
				if tc.secondBoundary != "" {
					second += testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", tc.secondBoundary, forkReviewTime))
				}
				if tc.secondChild {
					second += child
				}
				require.NoError(t, os.WriteFile(cp, []byte(second), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
				require.Equal(t, []string{"parent replay", "retained idless child"}, messageContents(t, d, id))
				parent := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime)) + replay
				if tc.parentBoundary != "" {
					parent += testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", tc.parentBoundary, forkReviewTime))
				}
				if tc.parentIdless {
					parent += testjsonl.JoinJSONL(
						testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime),
						testjsonl.CodexMsgJSON("assistant", "different old idless parent answer", forkReviewTime),
					)
				}
				require.NoError(t, os.WriteFile(pp, []byte(parent), 0o600))
				for _, rebuild := range []bool{false, true} {
					if rebuild {
						stats := e.ResyncAll(t.Context(), nil)
						require.True(t, stats.ArchiveRebuilt)
						require.Zero(t, stats.Failed)
					} else {
						require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
					}
					got := messageContents(t, d, id)
					usage, err := d.GetSessionUsage(t.Context(), id, true)
					require.NoError(t, err)
					require.NotNil(t, usage)
					assert.Equal(t, []string{"retained idless child"}, got)
					assert.Equal(t, 5, usage.TotalOutputTokens)
				}
			})
		}
	}
}
