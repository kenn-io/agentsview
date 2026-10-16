package remotesync

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

// remoteZstdMaxWindow bounds decoder memory for remote-controlled streams.
// The writer pins the same window so every stream we produce decodes.
const remoteZstdMaxWindow = 8 << 20

// NewZstdReader decodes a remote-sync zstd stream with a bounded window.
func NewZstdReader(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxWindow(remoteZstdMaxWindow),
	)
}

// NewZstdWriter encodes a remote-sync zstd stream that NewZstdReader accepts.
func NewZstdWriter(w io.Writer, level zstd.EncoderLevel) (*zstd.Encoder, error) {
	return zstd.NewWriter(w,
		zstd.WithEncoderLevel(level),
		zstd.WithWindowSize(remoteZstdMaxWindow),
	)
}
