package sshremote

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/remotesync"
)

func TestDecodeTargetsCapsTheDecompressedReply(t *testing.T) {
	const head = `{"dirs":{"claude":["/home/u/.claude/projects"]}`
	const tail = `}`
	fixed := len(head) + len(tail)
	tests := []struct {
		name    string
		padding int
		after   string
		wantErr error
	}{
		{"exactly at the cap", maxTargetsBytes - fixed, "", nil},
		{"one byte over the cap", maxTargetsBytes - fixed + 1, "", errTargetsTooLarge},
		{"complete value at the cap with data after it", maxTargetsBytes - fixed, "x", errTargetsTooLarge},
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

			var targets remotesync.TargetSet
			err = decodeZstd(&reply, func(r io.Reader) error {
				return decodeTargets(r, &targets)
			})

			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				assert.Equal(t, "SSH remote sync failed: the remote host lists "+
					"more agent directories than fit in the 64 MiB request "+
					"limit; check that agentsview on the remote host is up "+
					"to date", remotesync.FailureSummary(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, map[parser.AgentType][]string{
				parser.AgentClaude: {"/home/u/.claude/projects"},
			}, targets.Dirs)
		})
	}
}
