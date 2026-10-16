package parser

import (
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// zstd limits shared by every session source. They sit far above anything an
// agent writes and only stop a corrupt frame header from reserving memory:
// klauspost's defaults allow a 512 MiB window and 64 GiB of output, and
// DecodeAll allocates a frame's declared content size before decoding. A
// frame over either limit fails its own source instead of the process.
const (
	zstdDecoderMaxWindow = 64 << 20
	zstdDecoderMaxMemory = 128 << 20
)

// newZstdStreamDecoder returns a sequential, low-memory decoder for one
// compressed file read once from start to end.
func newZstdStreamDecoder(src io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(
		src,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(zstdDecoderMaxMemory),
		zstd.WithDecoderMaxWindow(zstdDecoderMaxWindow),
	)
}

// zstdBlobDecoder is shared by in-memory DecodeAll callers. Its default
// concurrency lets one decode per CPU run at once, so parallel sync workers
// do not queue behind each other.
var zstdBlobDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(
		nil,
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(zstdDecoderMaxMemory),
		zstd.WithDecoderMaxWindow(zstdDecoderMaxWindow),
	)
})
