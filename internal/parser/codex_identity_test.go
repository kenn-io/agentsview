package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexSessionIdentityIgnoresForeignMetadata(t *testing.T) {
	for _, tc := range []struct{ name, firstID, laterID, cwd, branch string }{
		{"foreign_id", "own-thread", "other-thread", "/work/project-a", "feature-a"},
		{"empty_later_id", "own-thread", "", "/work/project-a", "feature-a"},
		{"empty_first_id", "", "own-thread", "/work/project-b", "feature-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			content := testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaWithFieldsJSON(tc.firstID, "/work/project-a", "user", tsEarly, map[string]any{"git": map[string]any{"branch": "feature-a"}}),
				testjsonl.CodexMsgJSON("user", "own question", tsEarly),
				testjsonl.CodexSessionMetaWithFieldsJSON(tc.laterID, "/work/project-b", "codex_exec", tsEarlyS1, map[string]any{
					"git":              map[string]any{"branch": "feature-b"},
					"parent_thread_id": "other-parent", "thread_source": "subagent",
				}),
				testjsonl.CodexMsgJSON("assistant", "own answer", tsEarlyS5),
			)
			sess, msgs := runCodexParserTest(t, "restored.jsonl", content, false)
			require.NotNil(t, sess)
			assert.Equal(t, "codex:own-thread", sess.ID)
			assert.Equal(t, tc.cwd, sess.Cwd)
			assert.Equal(t, tc.branch, sess.GitBranch)
			if tc.firstID != "" {
				assert.Empty(t, sess.ParentSessionID)
				assert.Empty(t, sess.RelationshipType)
				assert.Empty(t, sess.SessionKind)
			}
			require.Len(t, msgs, 2)
			assert.Equal(t, "own question", msgs[0].Content)
			assert.Equal(t, "own answer", msgs[1].Content)
		})
	}
}
