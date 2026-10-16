package parser

import (
	"bytes"
	"io"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBoundedZstdDecoderEnforcesLimits(t *testing.T) {
	const limit = 64 << 10
	encode := func(t *testing.T, size int, opts ...zstd.EOption) []byte {
		t.Helper()
		encoder, err := zstd.NewWriter(nil, opts...)
		require.NoError(t, err)
		defer encoder.Close()
		return encoder.EncodeAll(bytes.Repeat([]byte("x"), size), nil)
	}

	windowed := []zstd.EOption{zstd.WithSingleSegment(false), zstd.WithWindowSize(limit)}
	tests := []struct {
		name    string
		size    int
		opts    []zstd.EOption
		stream  bool
		wantErr bool
	}{
		{name: "blob at the memory limit decodes", size: limit},
		{name: "blob above the memory limit is rejected", size: limit + 1, wantErr: true},
		{name: "stream within the window limit decodes", size: limit, opts: windowed, stream: true},
		{
			name:    "stream asking for a larger window is rejected",
			size:    4 * limit,
			opts:    []zstd.EOption{zstd.WithSingleSegment(false), zstd.WithWindowSize(4 * limit)},
			stream:  true,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frame := encode(t, tt.size, tt.opts...)
			var err error
			if tt.stream {
				var decoder *zstd.Decoder
				decoder, err = newBoundedZstdDecoder(bytes.NewReader(frame), limit, 2*limit)
				require.NoError(t, err)
				defer decoder.Close()
				_, err = io.Copy(io.Discard, decoder)
			} else {
				var decoder *zstd.Decoder
				decoder, err = newBoundedZstdDecoder(nil, limit, limit)
				require.NoError(t, err)
				defer decoder.Close()
				_, err = decoder.DecodeAll(frame, nil)
			}
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
