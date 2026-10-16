package parser

import (
	"io"

	"github.com/klauspost/compress/zstd"
)

// newBoundedZstdDecoder returns a sequential, low-memory zstd decoder for
// session sources. It rejects frames that ask for more than maxWindow bytes
// of history, so a corrupt header cannot reserve memory. maxMemory caps the
// decoder's buffers and, for DecodeAll, the size of each decoded result;
// klauspost also clamps the window to maxMemory. Pass a nil src to decode
// in-memory blobs with DecodeAll.
func newBoundedZstdDecoder(src io.Reader, maxWindow, maxMemory uint64) (*zstd.Decoder, error) {
	return zstd.NewReader(
		src,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(maxMemory),
		zstd.WithDecoderMaxWindow(maxWindow),
	)
}
