package parser

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCopilotTranscriptMetersBytesRead checks that the Copilot transcript
// fingerprint reports the bytes it hashed, not the size from an earlier stat.
func TestCopilotTranscriptMetersBytesRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session-state", "a", "events.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	first := `{"type":"session.start","timestamp":"2026-09-04T17:00:00Z","data":{"sessionId":"a"}}` + "\n"
	require.NoError(t, os.WriteFile(path, []byte(first), 0o644))
	staleInfo, err := os.Stat(path)
	require.NoError(t, err)

	appended := `{"type":"user.message","timestamp":"2026-09-04T17:00:01Z","data":{"content":"Question"}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(appended)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	tests := []struct {
		name      string
		cancel    bool
		wantBytes int64
	}{
		{name: "bytes appended after the stat", wantBytes: int64(len(first) + len(appended))},
		{name: "canceled before reading", cancel: true, wantBytes: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var meter ContentReadMeter
			_, err := newCopilotSourceCache().transcript(
				WithContentReadMeter(ctx, &meter), path, staleInfo, nil,
			)
			if tt.cancel {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantBytes, meter.Bytes())
		})
	}
}
