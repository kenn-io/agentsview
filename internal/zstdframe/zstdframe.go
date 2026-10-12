// Package zstdframe inspects and writes append-only files made of
// independent zstd frames, each carrying a content checksum.
//
// Writers append one complete frame at a time. A crash can leave a torn
// frame at the end of the file; Inspect reports where the last complete
// frame ends so readers can ignore the tail and writers can truncate it.
package zstdframe

import (
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	// MaxWindow bounds the decoder window a frame may require.
	MaxWindow = 8 << 20
	// DecoderMemory bounds the memory one decoder may allocate.
	DecoderMemory = 64 << 20
)

const frameMagic = 0xfd2fb528

// Layout describes the frames found in a file from a starting offset.
type Layout struct {
	CompleteFrames int
	// FrameEnds holds the end offset of each complete frame, in order.
	FrameEnds []int64
	Size      int64
	// Torn reports an incomplete frame after the last complete one.
	Torn bool
}

// End returns the end of the last complete frame, or from when there is
// none.
func (l Layout) End(from int64) int64 {
	if len(l.FrameEnds) == 0 {
		return from
	}
	return l.FrameEnds[len(l.FrameEnds)-1]
}

// FormatError reports bytes that cannot be a supported zstd frame.
type FormatError struct {
	Offset int64
	Reason string
	// Unsupported marks a valid frame this package refuses, such as one
	// without a content checksum.
	Unsupported bool
}

func (e *FormatError) Error() string {
	return fmt.Sprintf("%s at byte %d", e.Reason, e.Offset)
}

// Inspect walks frame headers and block headers from offset from without
// decompressing. It stops at the first incomplete frame and marks the
// layout torn.
func Inspect(r io.ReaderAt, size, from int64) (Layout, error) {
	layout := Layout{Size: size}
	for offset := from; offset < size; {
		start := offset
		magic, complete, err := readUintAt(r, size, offset, 4)
		if err != nil {
			return Layout{}, err
		}
		if !complete {
			layout.Torn = true
			break
		}
		if magic != frameMagic {
			return Layout{}, &FormatError{Offset: offset, Reason: "invalid frame magic"}
		}
		offset += 4
		descriptor, complete, err := readUintAt(r, size, offset, 1)
		if err != nil {
			return Layout{}, err
		}
		if !complete {
			layout.Torn = true
			break
		}
		offset++
		if descriptor&0x18 != 0 {
			return Layout{}, &FormatError{Offset: offset - 1, Reason: "reserved frame-header bit"}
		}
		if descriptor&0x04 == 0 {
			return Layout{}, &FormatError{
				Offset: start, Reason: "frame without checksum", Unsupported: true,
			}
		}
		offset += frameHeaderRest(descriptor)
		if offset > size {
			layout.Torn = true
			break
		}
		end, complete, err := blocksEnd(r, size, offset)
		if err != nil {
			return Layout{}, err
		}
		if !complete || 4 > size-end {
			layout.Torn = true
			break
		}
		offset = end + 4
		layout.CompleteFrames++
		layout.FrameEnds = append(layout.FrameEnds, offset)
	}
	return layout, nil
}

// frameHeaderRest returns the frame header bytes after the descriptor:
// window descriptor, dictionary ID, and frame content size.
func frameHeaderRest(descriptor uint64) int64 {
	contentSizeFlag := descriptor >> 6
	singleSegment := descriptor&0x20 != 0
	dictionaryBytes := descriptor & 0x03
	if dictionaryBytes == 3 {
		dictionaryBytes = 4
	}
	contentSizeBytes := uint64(0)
	if contentSizeFlag == 0 {
		if singleSegment {
			contentSizeBytes = 1
		}
	} else {
		contentSizeBytes = 1 << contentSizeFlag
	}
	rest := int64(dictionaryBytes + contentSizeBytes)
	if !singleSegment {
		rest++
	}
	return rest
}

// blocksEnd walks block headers from offset and returns the end of the
// frame's last block.
func blocksEnd(r io.ReaderAt, size, offset int64) (int64, bool, error) {
	for {
		header, complete, err := readUintAt(r, size, offset, 3)
		if err != nil || !complete {
			return 0, false, err
		}
		offset += 3
		blockType := (header >> 1) & 0x03
		if blockType == 0x03 {
			return 0, false, &FormatError{Offset: offset - 3, Reason: "reserved block type"}
		}
		payload := int64(header >> 3)
		if blockType == 0x01 {
			payload = 1
		}
		if payload > size-offset {
			return 0, false, nil
		}
		offset += payload
		if header&1 != 0 {
			return offset, true, nil
		}
	}
}

func readUintAt(r io.ReaderAt, size, offset int64, width int) (uint64, bool, error) {
	if offset < 0 || int64(width) > size-offset {
		return 0, false, nil
	}
	var data [8]byte
	if _, err := r.ReadAt(data[:width], offset); err != nil {
		return 0, false, err
	}
	var value uint64
	for index := range width {
		value |= uint64(data[index]) << (8 * index)
	}
	return value, true, nil
}

// NewDecoder returns a single-threaded decoder with bounded memory and
// window.
func NewDecoder(r io.Reader) (*zstd.Decoder, error) {
	return zstd.NewReader(
		r,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(DecoderMemory),
		zstd.WithDecoderMaxWindow(MaxWindow),
	)
}

var encoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(
		nil,
		zstd.WithEncoderCRC(true),
		zstd.WithEncoderConcurrency(1),
		zstd.WithWindowSize(MaxWindow),
	)
})

// EncodeFrame compresses src as one complete frame with a content
// checksum.
func EncodeFrame(src []byte) ([]byte, error) {
	enc, err := encoder()
	if err != nil {
		return nil, fmt.Errorf("create zstd encoder: %w", err)
	}
	return enc.EncodeAll(src, nil), nil
}
