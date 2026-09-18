package parser

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconciliationCacheAddIntIncrementsEachKeyIndependently(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ctx, cleanup, err := WithReconciliationCache(t.Context())
	require.NoError(err)
	t.Cleanup(func() { require.NoError(cleanup()) })

	first, err := reconciliationCacheAddInt(ctx, "first")
	require.NoError(err)
	second, err := reconciliationCacheAddInt(ctx, "first")
	require.NoError(err)
	other, err := reconciliationCacheAddInt(ctx, "other")
	require.NoError(err)

	assert.Equal(0, first)
	assert.Equal(1, second)
	assert.Equal(0, other)
}
