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

// TestChangedPathPassLogsSummary checks the per-pass log line that tells a
// user whether watcher events name session sources and how much source
// content each pass re-read because stats could not prove a source unchanged.
func TestChangedPathPassLogsSummary(t *testing.T) {
	claudeDir := filepath.Join(t.TempDir(), "claude")
	sessions := writeClaudeCorpus(t, claudeDir, 2)
	sessionPath := filepath.Join(claudeDir, "project", sessions[0]+".jsonl")
	info, err := os.Stat(sessionPath)
	require.NoError(t, err)
	otherPath := filepath.Join(claudeDir, "project", "notes.txt")
	require.NoError(t, os.WriteFile(otherPath, []byte("not a session"), 0o644))

	engine := NewEngine(t.Context(), openTestDB(t), EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		Machine: "local",
	})
	t.Cleanup(engine.Close)
	require.Equal(t, 2, engine.SyncAll(t.Context(), nil).Synced)

	var logs summaryLogBuffer
	previous := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previous) })

	steps := []struct {
		name   string
		before func(t *testing.T)
		paths  []string
		want   string
	}{
		{
			name:  "path outside any session source",
			paths: []string{otherPath},
			want: "sync: changed paths: 1 path(s) matched 0 source(s): " +
				"0 updated, 0 unchanged, 0 with errors; " +
				"0 source(s) re-read to check their content (0 B)",
		},
		{
			// Changing only the file mode moves the change time, which
			// breaks the stored stat digest, so the pass re-reads the
			// transcript to prove its content did not change. Read-only is
			// the mode change Windows records.
			name: "metadata-only change re-reads the source",
			before: func(t *testing.T) {
				require.NoError(t, os.Chmod(sessionPath, 0o444))
				t.Cleanup(func() { _ = os.Chmod(sessionPath, 0o644) })
			},
			paths: []string{sessionPath},
			want: fmt.Sprintf(
				"sync: changed paths: 1 path(s) matched 1 source(s): "+
					"0 updated, 1 unchanged, 0 with errors; "+
					"1 source(s) re-read to check their content (%d B)",
				info.Size(),
			),
		},
		{
			name:  "repeated event for an unchanged source skips on stats",
			paths: []string{sessionPath},
			want: "sync: changed paths: 1 path(s) matched 1 source(s): " +
				"0 updated, 1 unchanged, 0 with errors; " +
				"0 source(s) re-read to check their content (0 B)",
		},
	}
	for _, step := range steps {
		if step.before != nil {
			step.before(t)
		}
		logs.take()
		require.NoError(t, engine.SyncPathsContext(t.Context(), step.paths), step.name)
		assert.Contains(t, logs.take(), step.want, step.name)
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
