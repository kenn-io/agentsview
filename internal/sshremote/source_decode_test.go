package sshremote

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/remotesync"
)

func TestDecodeZstdJSONCapsTheDecompressedReply(t *testing.T) {
	const head = `{"files":[{"path":"/home/u/.claude/projects/p/s.jsonl","size":7,"mtime_ns":9}`
	const tail = `]}`
	fixed := len(head) + len(tail)
	tests := []struct {
		name    string
		padding int
		after   string
		wantErr error
	}{
		{"exactly at the cap", maxResponseBytes - fixed, "", nil},
		{"one byte over the cap", maxResponseBytes - fixed + 1, "", errResponseTooLarge},
		{"complete value at the cap with data after it", maxResponseBytes - fixed, "x", errResponseTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Whitespace is valid JSON padding that compresses to almost
			// nothing, so the reply is small before decompression.
			var reply bytes.Buffer
			zw, err := remotesync.NewZstdWriter(&reply, zstd.SpeedFastest)
			require.NoError(t, err)
			_, err = zw.Write([]byte(head))
			require.NoError(t, err)
			_, err = zw.Write(bytes.Repeat([]byte(" "), tt.padding))
			require.NoError(t, err)
			_, err = zw.Write([]byte(tail + tt.after))
			require.NoError(t, err)
			require.NoError(t, zw.Close())

			var manifest remotesync.Manifest
			err = decodeZstdJSON(&reply, &manifest)

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []remotesync.ManifestEntry{
				{Path: "/home/u/.claude/projects/p/s.jsonl", Size: 7, MtimeNS: 9},
			}, manifest.Files)
		})
	}
}
