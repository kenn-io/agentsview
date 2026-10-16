package ctxio

import (
	"context"
	"io"
)

// Writer checks cancellation before each underlying write.
type Writer struct {
	Context context.Context
	io.Writer
}

func (w Writer) Write(p []byte) (int, error) {
	if err := w.Context.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}
