package sync

import (
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A named child turn opens the boundary before this child's idless turn.
// Restoring an idless parent turn must not delete that archived child turn.
func TestCodexForkArchivedIdlessAfterChildBoundary(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, tc := range []struct {
			name               string
			parentIdless       bool
			keepIdlessInSource bool
		}{
			{"named_parent_control", false, false},
			{"idless_current_source_control", true, true},
			{"idless_archived_only_child", true, false},
		} {
			t.Run(fmt.Sprintf("stage=%d/%s", stage, tc.name), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				prefix := meta + testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "replayed parent question", forkReviewTime),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "child boundary question", forkReviewTime),
				)
				idless := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "retained idless child question", forkReviewTime),
				)
				require.NoError(t, os.WriteFile(cp, []byte(prefix+idless), 0o600))
				require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
				require.Equal(t, []string{"replayed parent question", "child boundary question", "retained idless child question"}, messageContents(t, d, "codex:"+forkReviewChild))
				parentBody := testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "replayed parent question", forkReviewTime),
				)
				if tc.parentIdless {
					parentBody += testjsonl.JoinJSONL(testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime), testjsonl.CodexMsgJSON("user", "old idless parent question", forkReviewTime))
				}
				current := prefix
				if tc.keepIdlessInSource {
					current += idless
				}
				require.NoError(t, os.WriteFile(cp, []byte(current), 0o600))
				require.NoError(t, os.WriteFile(pp, []byte(parentBody), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				got := messageContents(t, d, "codex:"+forkReviewChild)
				t.Logf("RECOVERED_TRANSCRIPT: %q", got)
				assert.Equal(t, []string{"child boundary question", "retained idless child question"}, got)
			})
		}
	}
}

// A replayed user record before any turn_context must be removed when the
// parent is recovered, including from the persisted session preview.
func TestCodexForkArchivedPreTurnReplayRecovery(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, parentIdless := range []bool{false, true} {
			t.Run(fmt.Sprintf("stage=%d/parentIdless=%v", stage, parentIdless), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				body := meta + testjsonl.JoinJSONL(
					testjsonl.CodexMsgJSON("user", "pre-turn replay question", forkReviewTime),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "named parent replay question", forkReviewTime),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "genuine child question", forkReviewTime),
				)
				require.NoError(t, os.WriteFile(cp, []byte(body), 0o600))
				require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
				require.Equal(t, []string{"pre-turn replay question", "named parent replay question", "genuine child question"}, messageContents(t, d, "codex:"+forkReviewChild))
				parentBody := testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "pre-turn replay question", forkReviewTime),
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime),
					testjsonl.CodexMsgJSON("user", "named parent replay question", forkReviewTime),
				)
				if parentIdless {
					parentBody += testjsonl.JoinJSONL(testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime), testjsonl.CodexMsgJSON("user", "old idless parent question", forkReviewTime))
				}
				require.NoError(t, os.WriteFile(pp, []byte(parentBody), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				got := messageContents(t, d, "codex:"+forkReviewChild)
				sess, err := d.GetSession(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.NotNil(t, sess)
				t.Logf("RECOVERED_TRANSCRIPT: %q; PREVIEW: %v", got, *sess.FirstMessage)
				assert.Equal(t, []string{"genuine child question"}, got)
				assert.Equal(t, "genuine child question", *sess.FirstMessage)
			})
		}
	}
}

func TestCodexForkReplayCannotClaimIdlessChild(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			replay := testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("assistant", "same answer", forkReviewTime))
			child := testjsonl.JoinJSONL(testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "same answer", forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(meta+replay+child), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			require.Equal(t, []string{"same answer", "same answer"}, messageContents(t, d, "codex:"+forkReviewChild))
			require.NoError(t, os.WriteFile(cp, []byte(meta+child), 0o600))
			require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "same answer", forkReviewTime))), 0o600))
			require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
			assert.Equal(t, []string{"same answer"}, messageContents(t, d, "codex:"+forkReviewChild))
		})
	}
}
