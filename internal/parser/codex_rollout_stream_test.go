package parser

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexCompressedParentScanRejectsCorruptTail(t *testing.T) {
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	content := "{\"type\":\"turn_context\",\"payload\":{\"turn_id\":\"parent\"}}\n" + strings.Repeat("x", 300001)
	compressed := encoder.EncodeAll([]byte(content), nil)
	encoder.Close()
	compressed[len(compressed)-1] ^= 1
	path := writeFileIn(t, t.TempDir(), "rollout.jsonl.zst", compressed)
	_, err = readCodexJSONLFromContext(t.Context(), path, 0, func(string) {})
	require.ErrorIs(t, err, zstd.ErrCRCMismatch)
}

func TestCodexCompressedRolloutWithoutTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	missingTemp := filepath.Join(t.TempDir(), "absent")
	compressed := writeFileIn(t, codexDayDir(root), compressedDayPrefix+compressedThreadA+".jsonl.zst",
		zstdCompress(t, []byte(codexRolloutContent(compressedThreadA, "stream without scratch files"))))
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, compressedThreadA)
	t.Setenv("TMPDIR", missingTemp)
	t.Setenv("TMP", missingTemp)
	t.Setenv("TEMP", missingTemp)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	result := outcome.Results[0].Result
	require.Len(t, result.Messages, 2)
	assert.Equal(t, "stream without scratch files", result.Messages[0].Content)
	assert.Equal(t, compressed, result.Session.File.Path)
}

func TestCodexCompressedRolloutMalformedLineBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		tail string
		want int
	}{
		{name: "terminated", tail: "invalid\n", want: 1},
		{name: "CRLF", tail: "invalid\r\n", want: 1},
		{name: "unfinished", tail: "invalid", want: 0},
		{name: "buffered terminated", tail: strings.Repeat("x", initialScanBufSize+1) + "\n", want: 1},
		{name: "buffered unfinished", tail: strings.Repeat("x", initialScanBufSize+1), want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFileIn(t, codexDayDir(root), compressedDayPrefix+compressedThreadA+".jsonl.zst",
				zstdCompress(t, []byte(codexRolloutContent(compressedThreadA, "message")+tc.tail)))
			provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
			require.True(t, ok)
			outcome, err := provider.Parse(t.Context(), ParseRequest{
				Source: requireCodexProviderSource(t, provider, compressedThreadA),
			})
			require.NoError(t, err)
			require.Len(t, outcome.Results, 1)
			assert.Equal(t, tc.want, outcome.Results[0].Result.Session.MalformedLines)
		})
	}
}
