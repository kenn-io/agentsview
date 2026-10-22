package parser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeSourceImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"user","uuid":"image-only","message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}
{"type":"assistant","uuid":"call","message":{"content":[]}}
{"type":"user","uuid":"result","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"read-1","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"YWJj"}}]}]}}
`), 0600))
	images, err := ClaudeSourceImages(t.Context(), path, "session", map[string]int{"call": 0})
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
	_, err = ClaudeSourceImages(ctx, path, "session", nil)
	assert.ErrorIs(t, err, context.Canceled)
	require.NoError(t, os.Remove(path))
	_, err = ClaudeSourceImages(t.Context(), path, "session", map[string]int{"call": 0})
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestClaudeSourceImagesSkipsOversizedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", maxLineSize+1)+"\n"+`{"type":"user","uuid":"valid","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}`), 0600))
	images, err := ClaudeSourceImages(t.Context(), path, "session", map[string]int{"call": 0})
	require.Error(t, err)
	require.Len(t, images, 1)
	assert.Equal(t, "valid", images[0].SourceUUID)
}

func TestClaudeSourceImagesSelectedBranch(t *testing.T) {
	for _, turns := range []int{1, 4} {
		t.Run(fmt.Sprint(turns), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			var source strings.Builder
			source.WriteString(`{"type":"user","uuid":"root","message":{"content":"start"}}` + "\n")
			parent := "root"
			for i := 0; i < turns; i++ {
				id := fmt.Sprintf("old-%d", i)
				fmt.Fprintf(&source, `{"type":"user","uuid":%q,"parentUuid":%q,"message":{"content":[{"type":"text","text":"old prompt"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}`+"\n", id, parent)
				parent = id
			}
			source.WriteString(`{"type":"user","uuid":"replacement","parentUuid":"root","message":{"content":"replacement"}}` + "\n")
			source.WriteString(`{"type":"user","uuid":"image-only","parentUuid":"replacement","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"ZGVm"}}]}}` + "\n")
			source.WriteString(`{"type":"assistant","uuid":"answer","parentUuid":"image-only","message":{"content":[{"type":"text","text":"answer"}]}}` + "\n")
			require.NoError(t, os.WriteFile(path, []byte(source.String()), 0600))
			sessionID := "session"
			if turns == 4 {
				sessionID = "session-replacement"
			}
			images, err := ClaudeSourceImages(t.Context(), path, sessionID, map[string]int{"replacement": 0, "answer": 1})
			require.NoError(t, err)
			require.Len(t, images, 1)
			assert.Equal(t, "image-only", images[0].SourceUUID)
			assert.Equal(t, "answer", images[0].NextSourceUUID)
			if turns == 4 {
				images, err = ClaudeSourceImages(t.Context(), path, "session", map[string]int{"old-0": 1})
				require.NoError(t, err)
				require.Len(t, images, 4)
				for _, image := range images {
					assert.True(t, strings.HasPrefix(image.SourceUUID, "old-"))
				}
			}
		})
	}
}

func TestClaudeSourceImagesIncomplete(t *testing.T) {
	for _, tail := range []string{`{"type":"user","message":{"content":[`, "invalid\n", `{"type":"user","uuid":"bad","message":{"content":[{"type":"image","source":{"type":"base64","data":"!!"}}]}}`} {
		t.Run(tail, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			source := `{"type":"user","uuid":"valid","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"YWJj"}}]}}` + "\n" + tail
			require.NoError(t, os.WriteFile(path, []byte(source), 0600))
			images, err := ClaudeSourceImages(t.Context(), path, "session", map[string]int{"valid": 0})
			require.Error(t, err)
			require.Len(t, images, 1)
			assert.Equal(t, "valid", images[0].SourceUUID)
		})
	}
}

func TestClaudeSourceImagesLargeSourceDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	// A sparse source exceeds the line limit without allocating its bytes in the fixture.
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(2*maxLineSize))
	require.NoError(t, f.Close())
	//nolint:kennlint // This assertion exercises source-scan deadline expiration on an oversized record.
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, err = ClaudeSourceImages(ctx, path, "session", nil)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClaudeSourceImagesUnavailablePaths(t *testing.T) {
	for _, path := range []string{t.TempDir(), "https://example.com/session.jsonl", filepath.Join(t.TempDir(), "missing.jsonl")} {
		_, err := ClaudeSourceImages(t.Context(), path, "session", nil)
		assert.Error(t, err)
	}
}
