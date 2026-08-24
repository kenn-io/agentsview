package parser

import (
	"encoding/binary"
	"hash"
	"strconv"
)

// Shared primitives for providers that store a per-session content digest in
// file_hash and compare it in the sync skip (Shelley, t3). Each provider owns
// which fields it folds and in what order; these helpers only fix the framing
// and rendering so the digests are comparable across the provider's own
// discovery and parse paths.

// digestLengthFramedFields folds fields into a running hash, length-framing
// each one so equal total byte counts cannot collide by moving bytes between
// fields.
func digestLengthFramedFields(h hash.Hash64, fields ...string) {
	var n [8]byte
	for _, s := range fields {
		binary.LittleEndian.PutUint64(n[:], uint64(len(s)))
		_, _ = h.Write(n[:])
		_, _ = h.Write([]byte(s))
	}
}

// digestFingerprintHex renders a content-digest hash as the stable string
// stored in file_hash and compared by the sync skip.
func digestFingerprintHex(h hash.Hash64) string {
	return strconv.FormatUint(h.Sum64(), 16)
}
