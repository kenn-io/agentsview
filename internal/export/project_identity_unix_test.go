//go:build !windows

package export

import "os"

func mkdirAll(path string) error {
	return os.MkdirAll(path, 0o755)
}

func symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}
