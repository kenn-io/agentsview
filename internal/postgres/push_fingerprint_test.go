package postgres

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadPushSessionMessageComparisonsNoSessions(t *testing.T) {
	comparisons, err := readPushSessionMessageComparisons(
		t.Context(), nil, nil, nil, true,
	)
	require.NoError(t, err)
	require.NotNil(t, comparisons)
	assert.Empty(t, comparisons.MessageAggregates)
	assert.Empty(t, comparisons.MessageContentHash)
	assert.Empty(t, comparisons.MessageRoleTime)
	assert.Empty(t, comparisons.MessageFlags)
	assert.Empty(t, comparisons.MessageTokenFingerprint)
	assert.Empty(t, comparisons.ToolCallFingerprint)
	assert.Empty(t, comparisons.ToolResultFingerprint)
	assert.Empty(t, comparisons.UsageEventFingerprint)
}

func TestPushDigestRepresentations(t *testing.T) {
	const digest = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	binary, err := hex.DecodeString(digest)
	require.NoError(t, err)
	for _, value := range []any{binary, digest} {
		var got pushDigest
		require.NoError(t, got.Scan(value))
		assert.Equal(t, digest, string(got))
	}
	for _, value := range []any{nil, []byte{1}, "invalid", "ab"} {
		var got pushDigest
		assert.Error(t, got.Scan(value))
	}
}
