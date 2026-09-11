package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedEmbeddingOccurrenceSpill(t *testing.T) {
	c := newOccurrenceCounter(1, 128)
	t.Cleanup(func() { assert.NoError(t, c.close()) })
	for _, v := range []struct {
		key  string
		want int
	}{{"duplicate", 1}, {"other", 1}, {"duplicate", 2}, {"third", 1}, {"duplicate", 3}} {
		got, e := c.next(t.Context(), v.key)
		require.NoError(t, e)
		assert.Equal(t, v.want, got)
	}
	for i := 4; i <= 1000; i++ {
		got, e := c.next(t.Context(), "duplicate")
		require.NoError(t, e)
		require.Equal(t, i, got)
	}
	assert.Equal(t, int64(55), c.bytes)
	dir := c.dir
	require.NoError(t, c.close())
	_, e := os.Stat(dir)
	assert.True(t, os.IsNotExist(e))
}
func TestHostedEmbeddingOccurrenceExhaustionAndCancellation(t *testing.T) {
	c := newOccurrenceCounter(1, 16)
	t.Cleanup(func() { assert.NoError(t, c.close()) })
	n, e := c.next(t.Context(), "abc")
	require.NoError(t, e)
	assert.Equal(t, 1, n)
	_, e = c.next(t.Context(), "other")
	assert.ErrorIs(t, e, ErrHostedEmbeddingWorkLimit)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, e = c.next(ctx, "abc")
	assert.ErrorIs(t, e, context.Canceled)
	dir := c.dir
	require.NoError(t, c.close())
	_, e = os.Stat(dir)
	assert.True(t, os.IsNotExist(e))
}
