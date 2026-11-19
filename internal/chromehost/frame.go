package chromehost

import (
	"encoding/binary"
	"errors"
	"io"
)

const FrameLimit = 64 << 20

// Version covers message encoding and supported request shapes; bump both
// peers together. A new request field, or a new or changed path shape for a
// platform an older extension already knows, needs a bump. A new platform does
// not: an extension that does not know it replies unsupported.
const Version = 2

var ErrCompatibility = errors.New("Restart AgentsView if you upgraded it, run agentsview source chrome setup, reload the extension at chrome://extensions, then Sync again.") //nolint:staticcheck // Recovery instructions appear directly in the UI.

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
