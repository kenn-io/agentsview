package parser

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zstdCorruptHeader builds a frame whose header declares the given window log
// and, when contentSize is non-zero, an 8-byte content size, followed by one
// empty last block. The declared sizes pass klauspost's default limits.
func zstdCorruptHeader(windowLog byte, contentSize uint64) []byte {
	frame := []byte{0x28, 0xb5, 0x2f, 0xfd}
	if contentSize == 0 {
		frame = append(frame, 0x00, (windowLog-10)<<3)
	} else {
		frame = append(frame, 0xc0, (windowLog-10)<<3)
		frame = binary.LittleEndian.AppendUint64(frame, contentSize)
	}
	return append(frame, 0x01, 0x00, 0x00)
}

func TestZstdDecodersRejectCorruptHeaders(t *testing.T) {
	tests := []struct {
		name    string
		frame   []byte
		stream  bool
		wantErr error
	}{
		{
			name:    "blob declaring 32 GiB of content",
			frame:   zstdCorruptHeader(20, 32<<30),
			wantErr: zstd.ErrDecoderSizeExceeded,
		},
		{
			name:    "blob asking for a 256 MiB window",
			frame:   zstdCorruptHeader(28, 0),
			wantErr: zstd.ErrWindowSizeExceeded,
		},
		{
			name:    "stream asking for a 256 MiB window",
			frame:   zstdCorruptHeader(28, 0),
			stream:  true,
			wantErr: zstd.ErrWindowSizeExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var err error
			if tt.stream {
				decoder, newErr := newZstdStreamDecoder(bytes.NewReader(tt.frame))
				require.NoError(t, newErr)
				defer decoder.Close()
				_, err = io.Copy(io.Discard, decoder)
			} else {
				decoder, newErr := zstdBlobDecoder()
				require.NoError(t, newErr)
				_, err = decoder.DecodeAll(tt.frame, nil)
			}
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}
