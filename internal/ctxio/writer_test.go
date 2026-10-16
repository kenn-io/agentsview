package ctxio

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriterStopsAfterCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var buf bytes.Buffer
	w := Writer{Context: ctx, Writer: &buf}

	n, err := w.Write([]byte("before"))
	require.NoError(t, err)
	assert.Equal(t, 6, n)

	cancel()
	n, err = w.Write([]byte("after"))
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, n)
	assert.Equal(t, "before", buf.String())
}
