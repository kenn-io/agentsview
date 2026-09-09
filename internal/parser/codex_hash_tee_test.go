package parser

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestCodexHashAnchorTeeWrapAndDigest feeds a payload larger than the
// anchor window through the tee and verifies the state digest equals the
// whole payload's hash while the anchor digest equals the trailing
// window's hash — including the ring wrap boundary.
func TestCodexHashAnchorTeeWrapAndDigest(t *testing.T) {
	payload := make([]byte, 300<<10)
	_, err := rand.Read(payload)
	require.NoError(t, err)

	tee := newCodexHashAnchorTee(bytes.NewReader(payload))
	buf := make([]byte, 64<<10)
	for {
		_, err := tee.Read(buf)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
	}

	state, err := tee.HashState()
	require.NoError(t, err)
	h := sha256.New()
	require.NoError(t, h.(encoding.BinaryUnmarshaler).UnmarshalBinary(state))
	require.Equal(t, sha256.Sum256(payload), *(*[32]byte)(h.Sum(nil)))

	wantAnchor := sha256.Sum256(payload[len(payload)-codexCheckpointAnchorSize:])
	require.Equal(t, hex.EncodeToString(wantAnchor[:]), tee.AnchorDigest())
}

// TestCodexHashAnchorTeeSmallPayload pins the sub-window behavior: a
// payload shorter than the anchor window digests the whole prefix.
func TestCodexHashAnchorTeeSmallPayload(t *testing.T) {
	payload := []byte("short codex snapshot")
	tee := newCodexHashAnchorTee(bytes.NewReader(payload))
	_, err := io.Copy(io.Discard, tee)
	require.NoError(t, err)
	wantAnchor := sha256.Sum256(payload)
	require.Equal(t, hex.EncodeToString(wantAnchor[:]), tee.AnchorDigest())
}
