package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexSessionIdentityIgnoresForeignMetadata(t *testing.T) {
	for _, laterID := range []string{"other-thread", ""} {
		t.Run("later_id="+laterID, func(t *testing.T) {
			content := testjsonl.JoinJSONL(
				`{"type":"session_meta","payload":{"id":"own-thread","cwd":"/work/project-a","git":{"branch":"feature-a"}}}`,
				testjsonl.CodexMsgJSON("user", "own question", tsEarly),
				testjsonl.CodexSessionMetaWithFieldsJSON(laterID, "/work/project-b", "codex_exec", tsEarlyS1, map[string]any{
					"git":              map[string]any{"branch": "feature-b"},
					"parent_thread_id": "other-parent", "thread_source": "subagent",
				}),
				testjsonl.CodexMsgJSON("assistant", "own answer", tsEarlyS5),
			)
			sess, msgs := runCodexParserTest(t, "restored.jsonl", content, false)
			require.NotNil(t, sess)
			assert.Equal(t, "codex:own-thread", sess.ID)
			assert.Equal(t, "/work/project-a", sess.Cwd)
			assert.Equal(t, "feature-a", sess.GitBranch)
			assert.Empty(t, sess.ParentSessionID)
			assert.Empty(t, sess.RelationshipType)
			assert.Empty(t, sess.SessionKind)
			require.Len(t, msgs, 2)
			assert.Equal(t, "own question", msgs[0].Content)
			assert.Equal(t, "own answer", msgs[1].Content)
		})
	}
}

func TestCodexSessionIdentityUsesFirstNonemptyID(t *testing.T) {
	content := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON("", "/work/project-a", "user", tsEarly),
		testjsonl.CodexSessionMetaJSON("own-thread", "/work/project-b", "user", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "question", tsEarlyS5),
	)
	sess, _ := runCodexParserTest(t, "restored.jsonl", content, false)
	require.NotNil(t, sess)
	assert.Equal(t, "codex:own-thread", sess.ID)
	assert.Equal(t, "/work/project-b", sess.Cwd)
}
