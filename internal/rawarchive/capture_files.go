package rawarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/ctxio"
	"go.kenn.io/agentsview/internal/rawcapture"
)

func captureTree(ctx context.Context, source, target string, binding RootSpec) ([]CaptureFile, []CaptureOmission, error) {
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	if err := os.MkdirAll(target, 0o700); err != nil {
		return nil, nil, err
	}
	var files []CaptureFile
	var omitted []CaptureOmission
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		reason := ""
		if entry.Type()&os.ModeSymlink != 0 {
			reason = "symlink not followed"
		} else if binding.Provider == "claude" && slices.Contains(binding.SessionDirs, "projects") && name == "ide" {
			reason = "Claude IDE runtime state"
		} else if credentialCaptureName(entry.Name()) {
			reason = "credential or runtime settings file"
		} else if !entry.IsDir() && !entry.Type().IsRegular() {
			reason = "special file"
		}
		if reason != "" {
			omitted = append(omitted, CaptureOmission{binding.ID, name, reason})
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		destination := filepath.Join(target, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o700)
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if before, ok := strings.CutSuffix(name, suffix); ok {
				f, err := root.Open(before)
				if err == nil {
					sqlite := isSQLiteFile(f)
					_ = f.Close()
					if sqlite {
						omitted = append(omitted, CaptureOmission{binding.ID, name, "included through SQLite online backup"})
						return nil
					}
				}
			}
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file, err := captureRootFile(ctx, root, filepath.FromSlash(name), destination, binding.ID, name, info)
		if err != nil {
			return fmt.Errorf("capture %s: %w", name, err)
		}
		files = append(files, file)
		return nil
	})
	return files, omitted, err
}

func credentialCaptureName(name string) bool {
	switch strings.ToLower(name) {
	case "auth.json", ".credentials.json", "credentials.json", "credentials", ".git-credentials", "settings.json", "settings.local.json", "mcp.json", "mcp_settings.json", "config.toml", "config.json", ".claude.json", ".env":
		return true
	}
	return strings.HasPrefix(strings.ToLower(name), ".env.")
}

func isSQLiteFile(f *os.File) bool {
	var header [16]byte
	n, _ := f.ReadAt(header[:], 0)
	return n == len(header) && string(header[:]) == "SQLite format 3\x00"
}

func captureFile(ctx context.Context, source, target, rootID, name string, info os.FileInfo) (CaptureFile, error) {
	root, err := os.OpenRoot(filepath.Dir(source))
	if err != nil {
		return CaptureFile{}, err
	}
	defer root.Close()
	return captureRootFile(ctx, root, filepath.Base(source), target, rootID, name, info)
}

func captureRootFile(ctx context.Context, root *os.Root, source, target, rootID, name string, info os.FileInfo) (CaptureFile, error) {
	f, err := root.Open(source)
	if err != nil {
		return CaptureFile{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return CaptureFile{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return CaptureFile{}, errors.New("source changed before capture")
	}
	method := "copy"
	wantHash := ""
	if isSQLiteFile(f) {
		method = "sqlite-online-backup"
		if err := rawcapture.SnapshotSQLite(ctx, filepath.Join(root.Name(), source), target, opened); err != nil {
			return CaptureFile{}, err
		}
	} else {
		if info.Size() != opened.Size() || !info.ModTime().Equal(opened.ModTime()) {
			return CaptureFile{}, errors.New("source changed before capture")
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return CaptureFile{}, err
		}
		hash := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(output, hash), ctxio.Reader{Context: ctx, Reader: f})
		wantHash = hex.EncodeToString(hash.Sum(nil))
		err = errors.Join(copyErr, output.Close())
		if err != nil {
			return CaptureFile{}, err
		}
		after, err := f.Stat()
		if err != nil {
			return CaptureFile{}, err
		}
		current, err := root.Lstat(source)
		if err != nil {
			return CaptureFile{}, err
		}
		if n != opened.Size() || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) || !os.SameFile(opened, current) {
			return CaptureFile{}, errors.New("source changed during capture")
		}
	}
	if err := os.Chtimes(target, info.ModTime(), info.ModTime()); err != nil {
		return CaptureFile{}, err
	}
	file, err := inventoryFile(ctx, target, rootID, name, method)
	if err == nil && wantHash != "" && wantHash != file.SHA256 {
		return CaptureFile{}, errors.New("copied bytes differ from source hash")
	}
	return file, err
}

func inventoryFile(ctx context.Context, path, rootID, name, method string) (CaptureFile, error) {
	f, err := os.Open(path)
	if err != nil {
		return CaptureFile{}, err
	}
	defer f.Close()
	ref, err := hashReader(ctx, f)
	if err != nil {
		return CaptureFile{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return CaptureFile{}, err
	}
	return CaptureFile{RootID: rootID, Path: filepath.ToSlash(name), SHA256: ref.SHA256, Size: ref.Length, ModTimeNS: info.ModTime().UnixNano(), Method: method}, nil
}
