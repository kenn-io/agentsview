//go:build !windows

package capture

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func createSecureCaptureDirectory(path string) error {
	return os.Mkdir(path, 0o700)
}

func verifyCaptureParentSafety(path string) error {
	for parent := filepath.Dir(filepath.Clean(path)); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("capture parent chain contains a non-directory")
		}
		owner, ok := info.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != 0 && int(owner.Uid) != os.Geteuid() {
			return fmt.Errorf(
				"capture parent %q is not owned by the current user or root",
				parent,
			)
		}
		if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
			return fmt.Errorf(
				"capture parent %q is writable by another user without sticky protection",
				parent,
			)
		}
		if err := verifyCaptureParentACL(parent); err != nil {
			return err
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}

func verifyCaptureDirectoryOwner(path string) error {
	if err := verifyCapturePathOwner(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("capture path is not a private directory")
	}
	return nil
}

func verifyCapturePathOwner(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("capture state contains a symbolic link")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("capture state is not owned by the current user")
	}
	return nil
}

func secureCaptureDirectory(path string) error {
	if err := verifyCaptureDirectoryOwner(path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return secureCaptureDirectoryACL(path)
}
