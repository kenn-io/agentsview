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

func TestClaudeSourceImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"image-only","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}
{"type":"assistant","uuid":"call","message":{"content":[]}}
{"type":"user","uuid":"result","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"YWJj"}}]}]}}
`), 0600))
	images, err := ClaudeSourceImages(t.Context(), path, map[string]int{"call": 0})
	require.NoError(t, err)
	require.Len(t, images, 2)
	assert.Equal(t, "image-only", images[0].SourceUUID)
	assert.Equal(t, "call", images[0].NextSourceUUID)
	assert.Equal(t, "read-1", images[1].ToolUseID)
	assert.Equal(t, "application/pdf", images[1].MediaType)
	for _, image := range images {
		assert.Equal(t, "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", image.SHA256)
		assert.Equal(t, int64(3), image.Size)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = ClaudeSourceImages(ctx, path, nil)
	assert.ErrorIs(t, err, context.Canceled)
	require.NoError(t, os.Remove(path))
	_, err = ClaudeSourceImages(t.Context(), path, map[string]int{"call": 0})
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestClaudeSourceImagesSkipsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxLineSize+1)+"\n"+`{"type":"user","uuid":"valid","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}`), 0600))
	images, err := ClaudeSourceImages(t.Context(), path, map[string]int{"call": 0})
	require.Error(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, "valid", images[0].SourceUUID)
}
