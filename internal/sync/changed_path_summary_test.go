package sync

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	gosync "sync"
	"testing"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type summaryLogBuffer struct {
	mu  gosync.Mutex
	buf bytes.Buffer
}

func (b *summaryLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *summaryLogBuffer) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.buf.String()
	b.buf.Reset()
	return out
}

// summaryStep is one changed-path event: an optional file change, then a sync
// of the session path, then the summary the pass must log.
type summaryStep struct {
	name string
	// chmod makes the change metadata-only: the change time moves, which
	// breaks the stored stat digest, while size, mtime, and bytes stay
	// the same. Read-only is the mode change Windows records.
	chmod bool
	// reads is the number of sources whose content the pass must read.
	reads int
}

// TestChangedPathPassLogsSummary checks the per-pass log line that tells a
// user whether watcher events name session sources and how much source
// content each pass read because stats could not prove a source unchanged.
func TestChangedPathPassLogsSummary(t *testing.T) {
	tests := []struct {
		name  string
		agent parser.AgentType
		// write creates one session under root and returns its path.
		write func(t *testing.T, root string) string
		steps []summaryStep
	}{
		{
			name:  "claude",
			agent: parser.AgentClaude,
			write: func(t *testing.T, root string) string {
				t.Helper()
				ids := writeClaudeCorpus(t, root, 1)
				return filepath.Join(root, "project", ids[0]+".jsonl")
			},
			steps: []summaryStep{
				{name: "metadata-only change", chmod: true, reads: 1},
				{name: "repeated event", reads: 0},
			},
		},
		{
			name:  "codex",
			agent: parser.AgentCodex,
			write: func(t *testing.T, root string) string {
				t.Helper()
				uuid := "11111111-2222-3333-4444-555555555555"
				path := filepath.Join(root, "2024", "01", "01",
					"rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(
					path, []byte(codexParityTranscript(uuid)), 0o644,
				))
				return path
			},
			steps: []summaryStep{
				{name: "metadata-only change", chmod: true, reads: 1},
				{name: "repeated event", reads: 0},
			},
		},
		{
			// Gemini has no stat-based freshness gate, so every event on
			// an unchanged session fingerprints its content again.
			name:  "gemini",
			agent: parser.AgentGemini,
			write: func(t *testing.T, root string) string {
				t.Helper()
				path := filepath.Join(root, "tmp", "project", "chats",
					"session-gemini-a.json")
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
				require.NoError(t, os.WriteFile(path, []byte(testjsonl.GeminiSessionJSON(
					"gemini-a", "project",
					"2026-08-09T10:00:00Z", "2026-08-09T10:01:00Z",
					[]map[string]any{testjsonl.GeminiUserMsg(
						"user", "2026-08-09T10:00:00Z", "hello",
					)},
				)), 0o644))
				return path
			},
			steps: []summaryStep{
				{name: "unchanged event", reads: 1},
				{name: "repeated event", reads: 1},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), string(tt.agent))
			sessionPath := tt.write(t, root)
			info, err := os.Stat(sessionPath)
			require.NoError(t, err)
			otherPath := filepath.Join(filepath.Dir(sessionPath), "notes.txt")
			require.NoError(t, os.WriteFile(otherPath, []byte("not a session"), 0o644))

			engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{
				AgentDirs: map[parser.AgentType][]string{tt.agent: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)

			var logs summaryLogBuffer
			previous := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previous) })

			require.NoError(t, engine.SyncPathsContext(t.Context(), []string{otherPath}))
			assert.Contains(t, logs.take(),
				"sync: changed paths: 1 path(s) matched 0 source(s): "+
					"0 updated, 0 unchanged, 0 with errors; "+
					"0 source(s) read to check their content (0 B)",
				"path outside any session source")

			for _, step := range tt.steps {
				if step.chmod {
					require.NoError(t, os.Chmod(sessionPath, 0o444))
					t.Cleanup(func() { _ = os.Chmod(sessionPath, 0o644) })
				}
				logs.take()
				require.NoError(t, engine.SyncPathsContext(
					t.Context(), []string{sessionPath},
				), step.name)
				assert.Contains(t, logs.take(), fmt.Sprintf(
					"sync: changed paths: 1 path(s) matched 1 source(s): "+
						"0 updated, 1 unchanged, 0 with errors; "+
						"%d source(s) read to check their content (%s)",
					step.reads, formatByteCount(int64(step.reads)*info.Size()),
				), step.name)
			}
		})
	}
}

func TestFormatByteCount(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{5 << 30, "5.0 GiB"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, formatByteCount(tt.in), tt.in)
	}
}
