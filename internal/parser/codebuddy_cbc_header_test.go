package parser

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A conversation whose first item exceeds 1 MiB, such as a large pasted
// prompt, still records the session id that keys its IDE catalog row, so the
// IDE title applies even though the id differs from the filename.
func TestCodeBuddyCBCHeaderReadsLargeFirstLine(t *testing.T) {
	home, project := cbcHome(t)
	pasted := strings.Repeat("x", 2<<20)
	writeCBC(t, filepath.Join(project, "archived-1.jsonl"),
		`{"id":"u1","type":"message","role":"user","timestamp":1000,"sessionId":"run-big","content":[{"type":"input_text","text":"`+pasted+`"}]}`,
		`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1001,"content":[{"type":"output_text","text":"done"}]}`,
	)
	writeCBCCatalog(t, home,
		`INSERT INTO conversations (id, title, runtime_session_id) VALUES ('conv-big', 'IDE title', 'run-big')`,
	)
	header := readCodeBuddyCBCHeader(t.Context(), filepath.Join(project, "archived-1.jsonl"))
	assert.Equal(t, "run-big", header.sessionID, "the header reads the whole first record")

	results := parseCBCHome(t, home)
	s := cbcSession(t, results, "codebuddy:archived-1")
	assert.Equal(t, "IDE title", s.Session.SessionName)
	assert.Equal(t, "run-big", s.Session.SourceSessionID)
	require.Len(t, s.Messages, 2)
}
