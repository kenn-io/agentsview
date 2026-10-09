package chromehost

import (
	"encoding/binary"
	"errors"
	"io"
)

const FrameLimit = 64 << 20
const Version = 1
const VersionError = "Chrome host protocol version mismatch; re-run agentsview chrome setup and reload the extension"

func ReadFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.NativeEndian.Uint32(header[:])
	if size > FrameLimit {
		return nil, errors.New("Chrome frame exceeds 64 MiB")
	}
	body := make([]byte, size)
	_, err := io.ReadFull(reader, body)
	return body, err
}

func WriteFrame(writer io.Writer, body []byte) error {
	if len(body) > FrameLimit {
		return errors.New("Chrome frame exceeds 64 MiB")
	}
	var header [4]byte
	binary.NativeEndian.PutUint32(header[:], uint32(len(body)))
	frame := append(header[:], body...)
	for len(frame) > 0 {
		n, err := writer.Write(frame)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		frame = frame[n:]
	}
	return nil
}
