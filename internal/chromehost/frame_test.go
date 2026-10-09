package chromehost

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestShapesVersion(t *testing.T) {
	body, err := os.ReadFile("../importer/claude_ai_requests.txt")
	require.NoError(t, err)
	hash, ok := requestShapeHashes[Version]
	require.True(t, ok, "record the request shape hash for the new protocol Version")
	digest := sha256.Sum256([]byte(strings.ReplaceAll(string(body), "\r\n", "\n")))
	assert.Equal(t, hash, hex.EncodeToString(digest[:]), "request shapes changed; bump the protocol Version in both peers and record its hash")
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) {
	return w.Buffer.Write(p[:1])
}

func TestWriteFrameShortWrites(t *testing.T) {
	var writer shortWriter
	require.NoError(t, WriteFrame(&writer, []byte(`{"version":1}`)))
	frame := writer.Bytes()
	require.Len(t, frame, 17)
	assert.Equal(t, uint32(13), binary.NativeEndian.Uint32(frame[:4]))
	assert.Equal(t, `{"version":1}`, string(frame[4:]))
}

func TestReadFrame(t *testing.T) {
	for _, tt := range []struct {
		name      string
		size      uint32
		body      string
		wantError string
	}{
		{"reply", 2, "[]", ""},
		{"oversized", 64<<20 + 1, "", "exceeds 64 MiB"},
		{"truncated", 3, "[]", "unexpected EOF"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var header [4]byte
			binary.NativeEndian.PutUint32(header[:], tt.size)
			body, err := ReadFrame(io.MultiReader(bytes.NewReader(header[:]), bytes.NewBufferString(tt.body)))
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "[]", string(body))
		})
	}
}
