package parser

import (
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// codexCheckpointAnchorSize is the trailing window whose digest the
// checkpoint stores instead of the raw anchor bytes.
const codexCheckpointAnchorSize = 128 << 10

// codexHashAnchorTee wraps a snapshot reader so one pass produces both the
// resumable SHA-256 state covering every byte read and the digest of the
// trailing anchor window. The Codex full parse threads it under the line
// reader, which eliminates the second full-file read the checkpoint
// persistence used to perform.
type codexHashAnchorTee struct {
	r     io.Reader
	h     hash.Hash
	ring  []byte
	pos   int
	total int64
}

func newCodexHashAnchorTee(r io.Reader) *codexHashAnchorTee {
	return &codexHashAnchorTee{
		r:    r,
		h:    sha256.New(),
		ring: make([]byte, codexCheckpointAnchorSize),
	}
}

func (t *codexHashAnchorTee) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		_, _ = t.h.Write(p[:n])
		t.total += int64(n)
		// Keep the trailing anchor window in chronological order with
		// bulk copies: this runs on the full-parse hot path for every
		// byte of the snapshot, so per-byte loops are not acceptable.
		chunk := p[:n]
		for len(chunk) > 0 {
			space := len(t.ring) - t.pos
			if len(chunk) <= space {
				copy(t.ring[t.pos:], chunk)
				t.pos += len(chunk)
				break
			}
			copy(t.ring[t.pos:], chunk[:space])
			chunk = chunk[space:]
			t.pos = 0
		}
	}
	return n, err
}

// HashState returns the resumable SHA-256 state covering all bytes read so
// far.
func (t *codexHashAnchorTee) HashState() ([]byte, error) {
	m, ok := t.h.(encoding.BinaryMarshaler)
	if !ok {
		return nil, fmt.Errorf("sha256 does not support state capture")
	}
	state, err := m.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("marshaling codex hash state: %w", err)
	}
	return state, nil
}

// HashDigest finalizes the current state into the full digest.
func (t *codexHashAnchorTee) HashDigest() (string, error) {
	return hex.EncodeToString(t.h.Sum(nil)), nil
}

// AnchorDigest returns the SHA-256 digest of the last
// min(codexCheckpointAnchorSize, total) bytes read, in order.
func (t *codexHashAnchorTee) AnchorDigest() string {
	var anchor []byte
	switch {
	case t.total <= int64(len(t.ring)):
		anchor = t.ring[:t.total]
	case t.pos == 0:
		anchor = t.ring
	default:
		anchor = make([]byte, len(t.ring))
		copy(anchor, t.ring[t.pos:])
		copy(anchor[len(t.ring)-t.pos:], t.ring[:t.pos])
	}
	sum := sha256.Sum256(anchor)
	return hex.EncodeToString(sum[:])
}
