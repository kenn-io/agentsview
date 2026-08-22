package capture

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cancelOnFirstRead struct {
	input  io.Reader
	cancel context.CancelFunc
	done   bool
}

func (r *cancelOnFirstRead) Read(p []byte) (int, error) {
	n, err := r.input.Read(p)
	if !r.done {
		r.done = true
		r.cancel()
	}
	return n, err
}

func TestCodexMetaMatchesLineLargerThanReaderBuffer(t *testing.T) {
	const sessionID = "11111111-1111-4111-8111-111111111111"
	line := fmt.Sprintf(
		`{"type":"session_meta","payload":{"id":%q,"padding":%q}}`+"\n",
		sessionID,
		strings.Repeat("x", 96<<10),
	)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(line), 0o600))

	matched, err := codexMetaMatches(t.Context(), path, sessionID, 128<<10)

	require.NoError(t, err)
	assert.True(t, matched)
}

func TestScanFirstLineStopsWhenContextIsCanceled(t *testing.T) {
	const inputSize = 8 << 20
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	input := strings.NewReader(strings.Repeat("x", inputSize))

	_, err := scanFirstLineReader(
		ctx,
		&cancelOnFirstRead{input: input, cancel: cancel},
		2*inputSize,
	)

	require.ErrorIs(t, err, context.Canceled)
	assert.Less(t, input.Len(), inputSize)
	assert.Positive(t, input.Len())
}
