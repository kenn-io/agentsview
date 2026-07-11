package parser

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLineReader(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   []string
	}{
		{
			"normal lines",
			"aaa\nbbb\nccc\n",
			100,
			[]string{"aaa", "bbb", "ccc"},
		},
		{
			"skips oversized line",
			"short\n" + strings.Repeat("x", 50) + "\nafter\n",
			30,
			[]string{"short", "after"},
		},
		{
			"all lines oversized",
			strings.Repeat("a", 50) + "\n" +
				strings.Repeat("b", 50) + "\n",
			30,
			nil,
		},
		{
			"empty input",
			"",
			100,
			nil,
		},
		{
			"blank lines skipped",
			"aaa\n\n\nbbb\n",
			100,
			[]string{"aaa", "bbb"},
		},
		{
			"line without trailing newline",
			"aaa\nbbb",
			100,
			[]string{"aaa", "bbb"},
		},
		{
			"exact limit kept",
			strings.Repeat("x", 30) + "\n",
			30,
			[]string{strings.Repeat("x", 30)},
		},
		{
			"one over limit skipped",
			strings.Repeat("x", 31) + "\n",
			30,
			nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := newLineReader(
				strings.NewReader(tt.input), tt.maxLen,
			)
			defer releaseLineReader(lr)
			var got []string
			for {
				line, ok := lr.next()
				if !ok {
					break
				}
				got = append(got, line)
			}
			assert.NoError(t, lr.Err())
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestLineReaderBytesRead(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{
			"complete lines",
			"aaa\nbbb\n",
			8, // 3+1+3+1
		},
		{
			"no trailing newline",
			"aaa\nbbb",
			7, // 3+1+3 (no newline after bbb)
		},
		{
			"empty",
			"",
			0,
		},
		{
			"single line with newline",
			"hello\n",
			6,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lr := newLineReader(
				strings.NewReader(tt.input), 100,
			)
			defer releaseLineReader(lr)
			for {
				_, ok := lr.next()
				if !ok {
					break
				}
			}
			assert.Equal(t, tt.want, lr.bytesRead)
		})
	}
}

func TestLineReaderIOError(t *testing.T) {
	ioErr := errors.New("disk read failed")
	r := io.MultiReader(
		strings.NewReader("aaa\nbbb\n"),
		iotest.ErrReader(ioErr),
	)

	lr := newLineReader(r, 100)
	defer releaseLineReader(lr)
	var got []string
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		got = append(got, line)
	}

	require.Len(t, got, 2)
	require.Error(t, lr.Err(), "expected non-nil Err() after I/O failure")
	require.ErrorIs(t, lr.Err(), ioErr)
}

func TestReadCodexJSONLReaderReusesWorkspace(t *testing.T) {
	var readErr error
	allocs := testing.AllocsPerRun(100, func() {
		r := bytes.NewReader(nil)
		_, readErr = readCodexJSONLReader(r, r, func(string) {})
	})

	require.NoError(t, readErr)
	assert.LessOrEqual(t, allocs, 1.0)
}
