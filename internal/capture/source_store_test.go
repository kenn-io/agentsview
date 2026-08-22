package capture

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyWithContextStopsBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	destination := &cancelOnFirstWrite{cancel: cancel}
	source := bytes.NewReader(bytes.Repeat([]byte("x"), 128<<10))

	written, err := copyWithContext(ctx, destination, source)

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, destination.calls)
	assert.Equal(t, int64(64<<10), written)
}

type cancelOnFirstWrite struct {
	cancel context.CancelFunc
	calls  int
}

func (w *cancelOnFirstWrite) Write(data []byte) (int, error) {
	w.calls++
	w.cancel()
	return len(data), nil
}
