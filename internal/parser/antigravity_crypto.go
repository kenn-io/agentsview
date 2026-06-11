package parser

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"os"
	"sync"
)

// Antigravity CLI conversations are AES-encrypted on disk.
// The Python decryptor (github.com/arashz/antigravity_decryptor)
// tries CTR (primary) then CBC / GCM with various skip offsets.
// We mirror that strategy with Go stdlib primitives.
//
// LAST RESORT. This is the lowest-priority decode path for
// legacy .pb sessions; agy-reader trajectory.json sidecars are
// the documented, preferred path, and the history.jsonl + brain
// fallbacks rank above this too (priority: sidecar > history
// fallback > this). It is kept because it is the only decode
// that works with zero agy-reader setup on archived .pb files.
// Current Antigravity CLI versions no longer produce .pb files,
// so this path serves only pre-existing archives; every parallel
// reimplementation of the undocumented agy format is a breakage
// surface per agy release, so this code is deliberately not
// extended and is a removal candidate once archived .pb sessions
// without sidecars are no longer a concern.
//
// Key sources (v1, in order):
//  1. ANTIGRAVITY_KEY env var, base64-encoded (matches the
//     Python tool's env-var fallback).
//
// Follow-up: probe libsecret / KWallet on Linux and the macOS
// Keychain via `security find-generic-password`. Tracked.

var (
	agKeyOnce sync.Once
	agKeyVal  []byte
	agKeyErr  error
)

// loadAntigravityKey returns the AES key (16, 24, or 32 bytes)
// or an error explaining why no key was found. Cached for the
// life of the process — agents only re-resolve the key on
// restart.
func loadAntigravityKey() ([]byte, error) {
	agKeyOnce.Do(func() {
		raw := os.Getenv("ANTIGRAVITY_KEY")
		if raw == "" {
			agKeyErr = errors.New(
				"ANTIGRAVITY_KEY env var not set; set it " +
					"to the base64-encoded key to decrypt " +
					"Antigravity CLI conversations",
			)
			return
		}
		key, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			agKeyErr = errors.New(
				"ANTIGRAVITY_KEY is not valid base64",
			)
			return
		}
		switch len(key) {
		case 16, 24, 32:
			agKeyVal = key
		default:
			agKeyErr = errors.New(
				"ANTIGRAVITY_KEY decodes to an unsupported " +
					"AES key length (need 16/24/32 bytes)",
			)
		}
	})
	if agKeyErr != nil {
		return nil, agKeyErr
	}
	return agKeyVal, nil
}

// hasAntigravityKey reports whether decryption is currently
// possible. Callers use this to decide between full transcript
// mode and the brain/+history.jsonl fallback.
func hasAntigravityKey() bool {
	_, err := loadAntigravityKey()
	return err == nil
}

// decryptAntigravity tries each (mode, skip, post-skip)
// combination from the Python decryptor and returns the first
// plaintext that walks as protobuf with at least one field.
// Returns nil, nil when none worked (caller should treat as a
// non-decryptable session rather than an error).
func decryptAntigravity(data []byte) ([]byte, error) {
	key, err := loadAntigravityKey()
	if err != nil {
		return nil, err
	}
	type modeFn func([]byte, []byte, int) []byte
	modes := []modeFn{
		decryptAesCTR,
		decryptAesCBC,
		decryptAesGCM,
	}
	skips := []int{0, 1, 2, 4, 8}
	for _, fn := range modes {
		for _, skip := range skips {
			plain := fn(data, key, skip)
			if plain == nil {
				continue
			}
			for _, post := range skips {
				if post >= len(plain) {
					continue
				}
				cand := plain[post:]
				if isLikelyAntigravityProto(cand) {
					return cand, nil
				}
			}
		}
	}
	return nil, nil
}

func decryptAesCTR(data, key []byte, skip int) []byte {
	if len(data) < skip+16 {
		return nil
	}
	data = data[skip:]
	nonce := data[:16]
	ct := data[16:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	out := make([]byte, len(ct))
	cipher.NewCTR(block, nonce).XORKeyStream(out, ct)
	return out
}

func decryptAesCBC(data, key []byte, skip int) []byte {
	if len(data) < skip+16 {
		return nil
	}
	data = data[skip:]
	iv := data[:16]
	ct := data[16:]
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	out := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ct)
	return stripPKCS7(out)
}

func decryptAesGCM(data, key []byte, skip int) []byte {
	if len(data) < skip+12+16 {
		return nil
	}
	data = data[skip:]
	nonce := data[:12]
	ct := data[12:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil
	}
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil
	}
	return plain
}

func stripPKCS7(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(data) {
		return data
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return data
		}
	}
	return data[:len(data)-pad]
}

// isLikelyAntigravityProto runs the wire walker over the first
// chunk of a candidate plaintext and reports whether it looks
// like a well-formed message. Caps work at 4KB to keep the
// retry loop cheap. Uses the prefix-tolerant validator so that
// large valid transcripts whose 4KB sniff falls mid-field still
// pass.
func isLikelyAntigravityProto(data []byte) bool {
	head := data
	if len(head) > 4096 {
		head = head[:4096]
	}
	return agProtoLooksLikePrefix(head)
}
