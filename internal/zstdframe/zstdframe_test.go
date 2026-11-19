package zstdframe

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
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
}

// uncheckedFrame encodes payload as a frame without a content checksum,
// which zstd producers may emit.
func uncheckedFrame(t *testing.T, payload string) []byte {
	t.Helper()
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderCRC(false))
	require.NoError(t, err)
	defer encoder.Close()
	frame := encoder.EncodeAll([]byte(payload), nil)
	require.Zero(t, frame[4]&0x04, "frame must not set the checksum flag")
	return frame
}

func TestInspectAcceptsFramesWithoutChecksum(t *testing.T) {
	checked, _ := frames(t, "first\n")
	unchecked := uncheckedFrame(t, "second\n")
	data := append(append([]byte(nil), checked...), unchecked...)
	layout, err := Inspect(bytes.NewReader(data), int64(len(data)), 0)
	require.NoError(t, err)
	assert.Equal(t, Layout{
		CompleteFrames: 2,
		FrameEnds:      []int64{int64(len(checked)), int64(len(data))},
		Size:           int64(len(data)),
	}, layout)

	decoder, err := NewDecoder(bytes.NewReader(data))
	require.NoError(t, err)
	defer decoder.Close()
	plain, err := io.ReadAll(decoder)
	require.NoError(t, err)
	assert.Equal(t, "first\nsecond\n", string(plain))
}

func TestInspectTornFrameWithoutChecksum(t *testing.T) {
	checked, _ := frames(t, "first\n")
	unchecked := uncheckedFrame(t, "second\n")
	for removed := 1; removed < len(unchecked); removed++ {
		data := append(append([]byte(nil), checked...), unchecked[:len(unchecked)-removed]...)
		layout, err := Inspect(bytes.NewReader(data), int64(len(data)), 0)
		require.NoError(t, err, "removed %d bytes", removed)
		assert.Equal(t, []int64{int64(len(checked))}, layout.FrameEnds, "removed %d bytes", removed)
		assert.True(t, layout.Torn, "removed %d bytes", removed)
	}
}
