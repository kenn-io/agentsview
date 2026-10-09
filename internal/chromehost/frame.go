package chromehost

import (
	"encoding/binary"
	"errors"
	"io"
)

const FrameLimit = 64 << 20

// Version covers message encoding and supported request shapes; bump both peers together.
const Version = 1

var ErrCompatibility = errors.New("run agentsview chrome setup, reload the extension at chrome://extensions, then Sync again")

type VersionError struct {
	Version int
}

func (e VersionError) Error() string {
	if e.Version > Version {
		return "upgrade AgentsView, then Sync again"
	}
	return ErrCompatibility.Error()
}

func (e VersionError) Unwrap() error { return ErrCompatibility }

func ReadFrame(reader io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return nil, err
	}
	size := binary.NativeEndian.Uint32(header[:])
	if size > FrameLimit {
		return nil, errors.New("chrome frame exceeds 64 MiB")
	}
	body := make([]byte, size)
	_, err := io.ReadFull(reader, body)
	return body, err
}

func WriteFrame(writer io.Writer, body []byte) error {
	if len(body) > FrameLimit {
		return errors.New("chrome frame exceeds 64 MiB")
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
