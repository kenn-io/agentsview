package zstdframe

import (
	"bytes"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func frames(t *testing.T, payloads ...string) ([]byte, []int64) {
	t.Helper()
	var buf bytes.Buffer
	var ends []int64
	for _, p := range payloads {
		frame, err := EncodeFrame([]byte(p))
		require.NoError(t, err)
		buf.Write(frame)
		ends = append(ends, int64(buf.Len()))
	}
	return buf.Bytes(), ends
}

func TestInspectCompleteFrames(t *testing.T) {
	data, ends := frames(t, "first\n", "second\n")
	layout, err := Inspect(bytes.NewReader(data), int64(len(data)), 0)
	require.NoError(t, err)
	assert.Equal(t, Layout{CompleteFrames: 2, FrameEnds: ends, Size: int64(len(data))}, layout)
}

func TestInspectTornTail(t *testing.T) {
	data, ends := frames(t, "first\n", "second\n", "third line\n")
	torn := data[:ends[1]+(ends[2]-ends[1])/2]
	layout, err := Inspect(bytes.NewReader(torn), int64(len(torn)), 0)
	require.NoError(t, err)
	assert.Equal(t, 2, layout.CompleteFrames)
	assert.Equal(t, ends[:2], layout.FrameEnds)
	assert.True(t, layout.Torn)
}

func TestInspectFromOffset(t *testing.T) {
	data, ends := frames(t, "first\n", "second\n")
	layout, err := Inspect(bytes.NewReader(data), int64(len(data)), ends[0])
	require.NoError(t, err)
	assert.Equal(t, 1, layout.CompleteFrames)
	assert.Equal(t, ends[1:], layout.FrameEnds)
}

func TestInspectRejectsGarbage(t *testing.T) {
	data := []byte("not a zstd frame at all")
	_, err := Inspect(bytes.NewReader(data), int64(len(data)), 0)
	formatErr, ok := errors.AsType[*FormatError](err)
	require.True(t, ok)
	assert.Equal(t, "invalid frame magic", formatErr.Reason)
	assert.Equal(t, int64(0), formatErr.Offset)
	assert.False(t, formatErr.Unsupported)
}

func TestInspectRejectsFrameWithoutChecksum(t *testing.T) {
	data, _ := frames(t, "payload\n")
	// Clear the content-checksum flag and drop the 4-byte checksum.
	data[4] &^= 0x04
	data = data[:len(data)-4]
	_, err := Inspect(bytes.NewReader(data), int64(len(data)), 0)
	formatErr, ok := errors.AsType[*FormatError](err)
	require.True(t, ok)
	assert.True(t, formatErr.Unsupported)
	assert.Equal(t, "frame without checksum", formatErr.Reason)
}
