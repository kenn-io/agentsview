package rawderive

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParityJSONRejectsDepthBeyondLimit(t *testing.T) {
	raw := []byte(strings.Repeat("[", maxParityJSONDepth+1) + "0" + strings.Repeat("]", maxParityJSONDepth+1))
	_, _, err := canonicalParityJSON(t.Context(), raw)
	assert.ErrorIs(t, err, errParityLimit)
}

func TestParityJSONRejectsNodeCountBeyondLimit(t *testing.T) {
	raw := []byte("[" + strings.Repeat("0,", maxParityJSONNodes-1) + "0]")
	_, _, err := canonicalParityJSON(t.Context(), raw)
	assert.ErrorIs(t, err, errParityLimit)
}

func TestParityJSONRejectsExpandedEncodingAtConfiguredLimit(t *testing.T) {
	limits := parityJSONLimits{maxDepth: 128, maxNodes: 100, maxExpandedBytes: 24}
	_, _, err := canonicalParityJSONWithLimits(t.Context(), []byte(`["abcdefghij","klmnopqrst"]`), limits)
	assert.ErrorIs(t, err, errParityLimit)
}

func TestParityJSONAcceptsConfiguredDepthAndNodeBoundaries(t *testing.T) {
	limits := parityJSONLimits{maxDepth: 2, maxNodes: 3, maxExpandedBytes: 128}
	canonical, valid, err := canonicalParityJSONWithLimits(t.Context(), []byte(`[[0]]`), limits)
	require.NoError(t, err)
	assert.True(t, valid)
	assert.NotEmpty(t, canonical)
}

func TestParityJSONPropagatesLimitInsteadOfTreatingItAsMalformed(t *testing.T) {
	limits := parityJSONLimits{maxDepth: 1, maxNodes: 100, maxExpandedBytes: maxParityGraphBytes}
	canonical, valid, err := canonicalParityJSONWithLimits(t.Context(), []byte(`[[0]]`), limits)
	assert.ErrorIs(t, err, errParityLimit)
	assert.False(t, valid)
	assert.Nil(t, canonical)
}

func TestParityJSONChecksCancellationDuringEncodingTraversal(t *testing.T) {
	node := &parityJSONNode{kind: 'a', array: []*parityJSONNode{{kind: 'd', value: "1e0"}}}
	ctx := &parityCancelAfterChecks{Context: context.Background(), cancelAt: 2}
	err := encodeParityJSONNode(ctx, newParityRecord(), node)
	assert.ErrorIs(t, err, context.Canceled)
}
