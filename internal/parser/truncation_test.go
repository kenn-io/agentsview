package parser

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKiloLegacySessionNameUTF8(t *testing.T) {
	taskDir := writeKiloLegacyFixture(t)
	text := strings.Repeat("a", 76) + "\u65e5\u672c"
	mustWriteJSON(t, filepath.Join(taskDir, "ui_messages.json"), []map[string]any{
		{"ts": 1700000000000, "type": "say", "say": "text", "text": text},
	})
	sess, _, err := parseKiloLegacySession(taskDir, "project", "local")
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, strings.Repeat("a", 76)+"...", sess.SessionName)
	assert.Equal(t, text, sess.FirstMessage)
}

func TestClaudePersistedToolResultUTF8(t *testing.T) {
	dir := t.TempDir()
	resultPath := filepath.Join(dir, "session", "tool-results", "large.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(resultPath), 0o755))
	prefix := strings.Repeat("a", maxPersistedToolResultSize-1)
	require.NoError(t, os.WriteFile(resultPath, []byte(prefix+"\u65e5"), 0o644))
	got, ok := readClaudePersistedToolResult(filepath.Join(dir, "session.jsonl"), resultPath)
	require.True(t, ok)
	assert.Equal(t, prefix+"\n\n[agentsview: persisted tool result truncated at 16 MiB]", got)
}

func TestTimestampDiagnosticUTF8(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	logParseError(strings.Repeat("a", 99) + "\u65e5")
	assert.Contains(t, output.String(), `"`+strings.Repeat("a", 99)+`..."`)
}
