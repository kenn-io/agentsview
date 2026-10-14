package remotesync

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// Usage-only archives store no session titles. A titled Codex thread, with or
// without a reverted rollout, must not look renamed on every import of the
// same unchanged archive.
func TestUsageOnlyImportSkipsUnchangedTitledCodexThread(t *testing.T) {
	for _, withRevert := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverted rollout %t", withRevert), func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "home")
			root := filepath.Join(home, "sessions")
			require.NoError(t, os.MkdirAll(root, 0o755))
			const id = "019f0000-0000-7000-8000-000000000009"
			write := func(name, text string) {
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(
					`{"timestamp":"2026-09-03T10:00:00Z","type":"session_meta","payload":{"id":"`+id+`","cwd":"/work"}}`+"\n"+
						`{"timestamp":"2026-09-03T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"`+text+`"}]}}`+"\n",
				), 0o600))
			}
			rollouts := 1
			write("rollout-2026-09-03T10-00-00-"+id+".jsonl", "Original prompt")
			if withRevert {
				rollouts = 2
				write("rollout-2026-09-03T11-00-00-"+id+"_019f0000-0000-7000-8002-000000000001.jsonl", "After revert")
			}
			require.NoError(t, os.WriteFile(filepath.Join(home, parser.CodexSessionIndexFilename),
				[]byte(`{"id":"`+id+`","thread_name":"Thread title"}`+"\n"), 0o600))
			targets, err := ResolveTargets(config.Config{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
			})
			require.NoError(t, err)
			dirScoped, _ := targets.SplitFileScoped()
			selected, ok := SelectAllowedTargets(targets, dirScoped)
			require.True(t, ok)
			var archive bytes.Buffer
			require.NoError(t, WriteArchive(t.Context(), &archive, selected))
			extracted := t.TempDir()
			_, err = ExtractTarStream(t.Context(), &archive, extracted)
			require.NoError(t, err)
			database, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			database.SetArchiveContent(config.ArchiveContentUsage)
			importer := Importer{Host: "remote", DB: database}

			stats, err := importer.ImportExtracted(t.Context(), selected, extracted)
			require.NoError(t, err)
			assert.Equal(t, rollouts, stats.SessionsSynced)
			stats, err = importer.ImportExtracted(t.Context(), selected, extracted)
			require.NoError(t, err)
			assert.Zero(t, stats.SessionsSynced, "an unchanged import must not reparse")
		})
	}
}
