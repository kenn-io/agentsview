package main

import (
	"bytes"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawparity"
	"hegel.dev/go/hegel"
)

// Counts and pass survive both output modes; JSON stdout stays machine-readable.
func TestRawParityReportOutputProperty(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "amd64" && os.Getenv("HEGEL_LIBHEGEL_PATH") == "" {
		t.Skip("Hegel requires HEGEL_LIBHEGEL_PATH on darwin/amd64")
	}
	hegel.Test(t, func(ht *hegel.T) {
		n := hegel.Draw(ht, hegel.Integers(0, 100000))
		r := rawparity.Report{Counts: rawparity.Counts{"equal": n}, Pass: n > 0}
		var stdout, stderr bytes.Buffer
		require.NoError(ht, writeRawParityReport(&stdout, &stderr, r, true))
		var decoded rawparity.Report
		require.NoError(ht, json.Unmarshal(stdout.Bytes(), &decoded))
		assert.Equal(ht, r.Counts, decoded.Counts)
		assert.Equal(ht, r.Pass, decoded.Pass)
		assert.Contains(ht, stderr.String(), fmt.Sprintf("equal=%d", n))
		assert.Contains(ht, stderr.String(), fmt.Sprintf("pass=%t", r.Pass))
		stdout.Reset()
		stderr.Reset()
		require.NoError(ht, writeRawParityReport(&stdout, &stderr, r, false))
		assert.Contains(ht, stdout.String(), fmt.Sprintf("equal=%d", n))
		assert.Empty(ht, stderr.String())
	}, hegel.WithTestCases(1000))
}

type parityFailWriter struct{ writes, failAt int }

func (w *parityFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, errors.New("output failed")
	}
	return len(p), nil
}

func TestRawParityWriterFailures(t *testing.T) {
	for _, tc := range []struct {
		json                 bool
		outFail, summaryFail int
	}{{true, 1, 0}, {true, 2, 0}, {true, 0, 1}, {false, 1, 0}} {
		err := writeRawParityReport(&parityFailWriter{failAt: tc.outFail}, &parityFailWriter{failAt: tc.summaryFail}, rawparity.Report{}, tc.json)
		assert.ErrorContains(t, err, "output failed")
	}
}
