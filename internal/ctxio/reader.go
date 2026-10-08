package ctxio

import (
	"context"
	"io"
)

// Reader checks cancellation before each underlying read.
type Reader struct {
	Context context.Context
	io.Reader
}

func (r Reader) Read(p []byte) (int, error) {
	if err := r.Context.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
