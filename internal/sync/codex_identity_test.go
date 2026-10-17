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

const (
	identityThreadA = "11111111-1111-7111-8111-111111111111"
	identityThreadB = "22222222-2222-7222-8222-222222222222"
)

func writeCodexIdentityRollouts(t *testing.T) (root, pathA, pathB string) {
	t.Helper()
	root = t.TempDir()
	const ts = "2025-10-20T19:00:00Z"
	metaA := testjsonl.CodexSessionMetaJSON(identityThreadA, "/work/project-a", "codex_cli_rs", ts)
	pathA = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-00-00-"+identityThreadA+".jsonl", testjsonl.JoinJSONL(
		metaA,
		testjsonl.CodexMsgJSON("user", "first question", ts),
		testjsonl.CodexMsgJSON("assistant", "first answer", ts),
		testjsonl.CodexMsgJSON("user", "second question", ts),
		testjsonl.CodexMsgJSON("assistant", "second answer", ts),
	))
	pathB = writeCodexRollout(t, root, "01", "rollout-2025-10-20T12-09-52-"+identityThreadB+".jsonl", testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(identityThreadB, "/work/project-b", "codex_cli_rs", ts),
		metaA,
		testjsonl.CodexMsgJSON("user", "other question", ts),
	))
	return root, pathA, pathB
}

func TestCodexForeignMetadataKeepsSessionsSeparate(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		for _, staged := range []bool{false, true} {
			t.Run(fmt.Sprintf("reverse=%t/staged=%t", reverse, staged), func(t *testing.T) {
				root, pathA, pathB := writeCodexIdentityRollouts(t)
				database := openTestDB(t)
				cfg := EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local"}
				if staged {
					cfg.StagedCodexParseMinBytes = 1
				}
				engine := NewEngine(t.Context(), database, cfg)
				t.Cleanup(engine.Close)
				paths := []string{pathA, pathB}
				if reverse {
					paths = []string{pathB, pathA}
				}
				for _, path := range paths {
					require.NoError(t, engine.SyncPathsContext(t.Context(), []string{path}))
				}
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, database, "codex:"+identityThreadA))
				assert.Equal(t, []string{"other question"}, messageContents(t, database, "codex:"+identityThreadB))

				// A later foreign header forces a full parse even after a checkpoint.
				f, err := os.OpenFile(pathB, os.O_APPEND|os.O_WRONLY, 0o600)
				require.NoError(t, err)
				_, err = f.WriteString(testjsonl.JoinJSONL(
					testjsonl.CodexSessionMetaJSON(identityThreadA, "/work/project-a", "codex_cli_rs", "2025-10-20T19:10:00Z"),
					testjsonl.CodexMsgJSON("assistant", "other answer", "2025-10-20T19:10:01Z"),
				))
				require.NoError(t, err)
				require.NoError(t, f.Close())
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{pathB}))
				assert.Equal(t, []string{"first question", "first answer", "second question", "second answer"}, messageContents(t, database, "codex:"+identityThreadA))
				assert.Equal(t, []string{"other question", "other answer"}, messageContents(t, database, "codex:"+identityThreadB))
			})
		}
	}
}
