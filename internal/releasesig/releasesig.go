// Package releasesig checks the signature on an AgentsView release's
// SHA256SUMS file.
//
// Every release since v0.43.0 publishes SHA256SUMS.sig: the base64 encoding
// of an ASN.1 DER ECDSA P-256 signature over the SHA-256 digest of
// SHA256SUMS. The release pipeline signs it with a key held in a cloud key
// service. The public half of each release key lives in keys/ and is
// compiled into the binary, so a check does not trust anything served next
// to the release.
//
// Rotating the key requires shipping the new public key in a release
// before any release is signed with it. Binaries that do not know the new
// key refuse every release signed only by it.
package releasesig

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
)

//go:embed keys/*.pem
var releaseKeyFiles embed.FS

// KeySet holds the public keys a signature may come from.
type KeySet struct {
	keys []*ecdsa.PublicKey
}

// Release returns the keys that sign official AgentsView releases.
func Release() KeySet {
	return releaseKeys
}

var releaseKeys = mustLoadReleaseKeys()

func mustLoadReleaseKeys() KeySet {
	paths, err := fs.Glob(releaseKeyFiles, "keys/*.pem")
	if err != nil {
		panic(fmt.Sprintf("releasesig: list release keys: %v", err))
	}
	pems := make([][]byte, 0, len(paths))
	for _, path := range paths {
		raw, err := releaseKeyFiles.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("releasesig: read %s: %v", path, err))
		}
		pems = append(pems, raw)
	}
	keys, err := ParseKeys(pems...)
	if err != nil {
		panic(fmt.Sprintf("releasesig: %v", err))
	}
	return keys
}

// ParseKeys builds a KeySet from PEM-encoded ECDSA public keys.
func ParseKeys(pems ...[]byte) (KeySet, error) {
	if len(pems) == 0 {
		return KeySet{}, errors.New("no public keys")
	}
	keys := make([]*ecdsa.PublicKey, 0, len(pems))
	for i, raw := range pems {
		block, _ := pem.Decode(raw)
		if block == nil || block.Type != "PUBLIC KEY" {
			return KeySet{}, fmt.Errorf("key %d: not a PEM public key", i)
		}
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return KeySet{}, fmt.Errorf("key %d: %w", i, err)
		}
		key, ok := parsed.(*ecdsa.PublicKey)
		if !ok {
			return KeySet{}, fmt.Errorf("key %d: not an ECDSA key", i)
		}
		keys = append(keys, key)
	}
	return KeySet{keys: keys}, nil
}

// ErrBadSignature reports a signature that no key in the set produced.
var ErrBadSignature = errors.New("SHA256SUMS signature does not match any release key")

// Verify checks that sig, the contents of SHA256SUMS.sig, is a signature
// over sums by one of the keys in the set.
func (k KeySet) Verify(sums, sig []byte) error {
	der, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(sig)))
	if err != nil {
		return fmt.Errorf("decode SHA256SUMS signature: %w", err)
	}
	digest := sha256.Sum256(sums)
	for _, key := range k.keys {
		if ecdsa.VerifyASN1(key, digest[:], der) {
			return nil
		}
	}
	return ErrBadSignature
}
