//go:build windows

package postgres

import (
	"errors"

	"golang.org/x/sys/windows"
)

func isRawUploadDirectorySyncUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_INVALID_HANDLE) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}
