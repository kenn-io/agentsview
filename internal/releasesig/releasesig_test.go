package releasesig

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/releasesig/releasesigtest"
)

// The v0.47.0 fixtures are the files published on that GitHub release, so
// these tests fail if the compiled-in key stops matching what the release
// pipeline signs with.
func readRelease(t *testing.T) (sums, sig []byte) {
	t.Helper()
	dir := filepath.Join("testdata", "v0.47.0")
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	require.NoError(t, err)
	sig, err = os.ReadFile(filepath.Join(dir, "SHA256SUMS.sig"))
	require.NoError(t, err)
	return sums, sig
}

func TestReleaseVerifiesPublishedRelease(t *testing.T) {
	sums, sig := readRelease(t)
	assert.NoError(t, Release().Verify(sums, sig))
	assert.NoError(t, Release().Verify(sums, append(sig, '\n')),
		"a trailing newline in the .sig file is accepted")
}

func TestReleaseRejectsAlteredRelease(t *testing.T) {
	sums, sig := readRelease(t)

	altered := append([]byte(nil), sums...)
	altered[0] ^= 0x01
	require.ErrorIs(t, Release().Verify(altered, sig), ErrBadSignature)

	require.ErrorIs(t, Release().Verify(append(sums, "extra\n"...), sig), ErrBadSignature)

	require.Error(t, Release().Verify(sums, []byte("not base64!")))
	assert.ErrorIs(t, Release().Verify(sums, []byte("MEUCIQ==")), ErrBadSignature)
}

func TestReleaseRejectsOtherKeys(t *testing.T) {
	sums, _ := readRelease(t)
	other := releasesigtest.NewKey(t)
	assert.ErrorIs(t, Release().Verify(sums, other.Sign(t, sums)), ErrBadSignature)
}

func TestKeySetAcceptsAnyListedKey(t *testing.T) {
	first, second := releasesigtest.NewKey(t), releasesigtest.NewKey(t)
	keys, err := ParseKeys(first.PEM, second.PEM)
	require.NoError(t, err)

	sums := []byte("abc  agentsview_1.0.0_linux_amd64.tar.gz\n")
	assert.NoError(t, keys.Verify(sums, first.Sign(t, sums)))
	assert.NoError(t, keys.Verify(sums, second.Sign(t, sums)))
}

func TestParseKeysRejectsUnusableInput(t *testing.T) {
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rsaDER, err := x509.MarshalPKIXPublicKey(&rsaKey.PublicKey)
	require.NoError(t, err)

	tests := []struct {
		name string
		pems [][]byte
	}{
		{name: "no keys"},
		{name: "not PEM", pems: [][]byte{[]byte("hello")}},
		{name: "wrong block type", pems: [][]byte{pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rsaDER})}},
		{name: "RSA key", pems: [][]byte{pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: rsaDER})}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseKeys(tt.pems...)
			assert.Error(t, err)
		})
	}
}
