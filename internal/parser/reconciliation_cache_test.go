package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconciliationCacheAddIntIncrementsEachKeyIndependently(t *testing.T) {
	ctx, cleanup, err := WithReconciliationCache(t.Context())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, cleanup()) })

	first, err := reconciliationCacheAddInt(ctx, "first")
	require.NoError(t, err)
	second, err := reconciliationCacheAddInt(ctx, "first")
	require.NoError(t, err)
	other, err := reconciliationCacheAddInt(ctx, "other")
	require.NoError(t, err)

	assert.Equal(t, 0, first)
	assert.Equal(t, 1, second)
	assert.Equal(t, 0, other)
}
