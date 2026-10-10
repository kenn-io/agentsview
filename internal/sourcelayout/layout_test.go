package sourcelayout

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func pathPtr(p string) *string { return &p }

func TestForAgentSelection(t *testing.T) {
	assert.IsType(t, fileLayout{}, For("claude"))
	assert.IsType(t, fileLayout{}, For("codex"))
	assert.IsType(t, fileLayout{}, For("totally-unknown-agent"))
	assert.IsType(t, openCodeTree{}, For("opencode"))
	assert.IsType(t, reportOnly{}, For("traex"))
}

func TestFileLayoutDecisions(t *testing.T) {
	layout := fileLayout{}
	missingAgent := db.Session{ID: "s1"}
	decision := layout.Decide(missingAgent)
	assert.False(t, decision.Deletable)
	assert.Contains(t, decision.Reason, "no source file")

	gone := filepath.Join(t.TempDir(), "gone.jsonl")
	decision = layout.Decide(db.Session{ID: "s1", FilePath: pathPtr(gone)})
	assert.False(t, decision.Deletable)
	assert.Contains(t, decision.Reason, "missing")

	present := filepath.Join(t.TempDir(), "here.jsonl")
	require.NoError(t, os.WriteFile(present, []byte("x"), 0o644))
	decision = layout.Decide(db.Session{ID: "s1", FilePath: pathPtr(present)})
	assert.True(t, decision.Deletable)
	assert.Equal(t, []string{present}, decision.Paths)
}

func TestOpenCodeTreeCollectsSessionMessagesAndParts(t *testing.T) {
	root := t.TempDir()
	sessionPath := filepath.Join(root, "storage", "session", "proj-a", "sess-1.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(sessionPath), 0o755))
	require.NoError(t, os.WriteFile(sessionPath, []byte("{}"), 0o644))

	messageDir := filepath.Join(root, "storage", "message", "sess-1")
	require.NoError(t, os.MkdirAll(messageDir, 0o755))
	for _, id := range []string{"msg-1", "msg-2"} {
		require.NoError(t, os.WriteFile(
			filepath.Join(messageDir, id+".json"), []byte("{}"), 0o644))
	}
	partDir := filepath.Join(root, "storage", "part", "msg-1")
	require.NoError(t, os.MkdirAll(partDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(partDir, "p.json"), []byte("{}"), 0o644))

	decision := openCodeTree{}.Decide(
		db.Session{ID: "sess-1", Agent: "opencode", FilePath: pathPtr(sessionPath)})
	assert.True(t, decision.Deletable)
	assert.ElementsMatch(t, []string{
		sessionPath,
		messageDir,
		partDir,
	}, decision.Paths, "msg-2 has no part dir and must not appear")
}

func TestOpenCodeContainerIsReportOnly(t *testing.T) {
	layout := openCodeTree{}
	container := filepath.Join(t.TempDir(), "opencode.db")
	decision := layout.Decide(
		db.Session{ID: "s1", Agent: "opencode", FilePath: pathPtr(container)})
	assert.False(t, decision.Deletable)
	assert.Equal(t, ReportOnlyReason, decision.Reason)

	decision = openCodeTree{}.Decide(db.Session{ID: "s2", Agent: "opencode"})
	assert.False(t, decision.Deletable)
}

func TestTraeIsReportOnly(t *testing.T) {
	decision := For("traex").Decide(
		db.Session{ID: "t1", Agent: "traex", FilePath: pathPtr("/any/state.vscdb")})
	assert.False(t, decision.Deletable)
	assert.Equal(t, ReportOnlyReason, decision.Reason)
}
