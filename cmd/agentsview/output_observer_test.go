package main

import "io"

// observedOutput lets concurrent fixtures wait for output produced by the
// command, rather than estimating when it reaches that stage.
type observedOutput struct {
	io.Writer
	observe func([]byte)
}

func (w observedOutput) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	w.observe(p[:n])
	return n, err
}
