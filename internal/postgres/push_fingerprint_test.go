package postgres

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPushDigestRepresentations(t *testing.T) {
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	binary, err := hex.DecodeString(digest)
	require.NoError(t, err)
	for _, value := range []any{binary, digest} {
		var got pushDigest
		require.NoError(t, got.Scan(value))
		assert.Equal(t, digest, string(got))
	}
}
