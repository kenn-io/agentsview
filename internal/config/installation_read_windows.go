package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func readInstallationIDFile(path string) ([]byte, error) {
	openPath, err := installationIDWindowsPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	path16, err := windows.UTF16PtrFromString(openPath)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	// Readers must share delete access while the publishing rename still holds its handle.
	handle, err := windows.CreateFile(path16, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	return io.ReadAll(file)
}

func installationIDWindowsPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) || strings.HasPrefix(path, `\\.\`) || strings.HasPrefix(path, `\??\`) {
		return path, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Older Windows versions require the extended prefix even in Go processes.
	if len(absolute) < 248 {
		return path, nil
	}
	if strings.HasPrefix(absolute, `\\`) {
		return `\\?\UNC\` + absolute[2:], nil
	}
	return `\\?\` + absolute, nil
}
