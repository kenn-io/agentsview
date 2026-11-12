// Package releasesigtest signs SHA256SUMS fixtures with throwaway keys.
package releasesigtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"

	"github.com/stretchr/testify/require"
)

// Key is a throwaway release key for tests.
type Key struct {
	private *ecdsa.PrivateKey
	// PEM is the public key, in the same form as the files in
	// internal/releasesig/keys.
	PEM []byte
}

// NewKey generates a P-256 key.
func NewKey(tb testing.TB) Key {
	tb.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(tb, err)
	der, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	require.NoError(tb, err)
	return Key{
		private: private,
		PEM:     pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
	}
}

// Sign returns a SHA256SUMS.sig body for sums, in the format the release
// pipeline publishes.
func (k Key) Sign(tb testing.TB, sums []byte) []byte {
	tb.Helper()
	digest := sha256.Sum256(sums)
	der, err := ecdsa.SignASN1(rand.Reader, k.private, digest[:])
	require.NoError(tb, err)
	return []byte(base64.StdEncoding.EncodeToString(der))
}
