package capture

import (
	"errors"
)

type reasonError struct {
	reason ReasonCode
	err    error
}

func (e *reasonError) Error() string { return e.err.Error() }
func (e *reasonError) Unwrap() error { return e.err }

func errorWithReason(reason ReasonCode, message string) error {
	return &reasonError{reason: reason, err: errors.New(message)}
}

func reasonForError(err error, fallback ReasonCode) ReasonCode {
	var classified *reasonError
	if errors.As(err, &classified) {
		return classified.reason
	}
	return fallback
}
