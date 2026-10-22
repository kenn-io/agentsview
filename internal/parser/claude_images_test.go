package parser

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeSourceImagesArchivedEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "uploaded.jsonl")
	// Retries, replayed parents, and forks can coexist in one producer file.
	source := `{"type":"user","uuid":"parent","message":{"content":[{"type":"image","source":{"type":"base64","data":"b2xk"}}]}}
{"type":"user","uuid":"retry","parentUuid":"parent","message":{"content":[{"type":"image","source":{"type":"base64","data":"b2xk"}}]}}
{"type":"user","uuid":"own","parentUuid":"parent","message":{"content":[{"type":"text","text":"prompt"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}
{"type":"user","uuid":"fork","parentUuid":"parent","message":{"content":[{"type":"image","source":{"type":"base64","data":"b2xk"}}]}}
{"type":"user","uuid":"result","message":{"content":[{"type":"tool_result","tool_use_id":"read-1","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"YWJj"}}]},{"type":"tool_result","tool_use_id":"other-call","content":[{"type":"image","source":{"type":"base64","data":"b2xk"}}]}]}}
{"type":"api-request-blob","uuid":"blob","message":{"content":[{"type":"image","source":{"type":"base64","data":"b2xk"}}]}}
`
	source += lineageAssistantLine("call", "own", "2026-01-01T00:00:00Z", "continuation", "bg", "message-1", "answer", 1)
	require.NoError(t, os.WriteFile(path, []byte(source), 0600))
	images, err := ClaudeSourceImages(t.Context(), path, map[string]int{"own": 4, "call": 5}, map[string]int{"read-1": 5})
	require.NoError(t, err)
	require.Len(t, images, 2)
	assert.Equal(t, "own", images[0].SourceUUID)
	assert.Empty(t, images[0].ToolUseID)
	assert.Equal(t, "read-1", images[1].ToolUseID)
	assert.Equal(t, "application/pdf", images[1].MediaType)
	for _, image := range images {
		assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", image.SHA256)
		assert.Equal(t, int64(3), image.Size)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ClaudeSourceImages(ctx, path, nil, nil)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClaudeSourceImagesSourceDamage(t *testing.T) {
	for _, tc := range []struct {
		name, tail       string
		missing, invalid bool
	}{
		{name: "malformed nonimage", tail: "invalid\n"},
		{name: "truncated image", tail: `{"type":"user","uuid":"bad","message":{"content":[{"type":"image","source":{"type":"base64"`, invalid: true},
		{name: "corrupt base64", tail: `{"type":"user","uuid":"bad","message":{"content":[{"type":"image","source":{"type":"base64","data":"!!"}}]}}`, invalid: true},
		{name: "missing archived entry", missing: true, invalid: true},
		{name: "oversized", tail: strings.Repeat("x", maxLineSize+1), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"valid","message":{"content":"text only"}}`+"\n"+tc.tail), 0600))
			archived := map[string]int{"valid": 0}
			if tc.missing {
				archived["gone"] = 1
			}
			if tc.name == "corrupt base64" {
				archived["bad"] = 1
			}
			_, err := ClaudeSourceImages(t.Context(), path, archived, map[string]int{})
			if tc.invalid {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestClaudeSourceImagesUnavailablePaths(t *testing.T) {
	for _, path := range []string{t.TempDir(), "https://example.com/session.jsonl", filepath.Join(t.TempDir(), "missing.jsonl")} {
		_, err := ClaudeSourceImages(t.Context(), path, nil, nil)
		assert.Error(t, err)
	}
}
