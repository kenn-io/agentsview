package rawarchive

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const recoveryManifestName = "recovery.json"

type recoveryFile struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	ModTimeNS int64  `json:"mod_time_ns"`
}
type recoveryManifest struct {
	Version   int            `json:"version"`
	CreatedAt time.Time      `json:"created_at"`
	Files     []recoveryFile `json:"files"`
}

// Backup copies a completely stopped archive. The caller must close SQLite and
// Docbank and retain its writer lock for the whole operation.
func Backup(ctx context.Context, databasePath, dataDir, target string) (report Report, retErr error) {
	sourceAbs, err := canonicalRecoverySource(dataDir)
	if err != nil {
		return report, err
	}
	targetAbs, err := recoveryDestination(target)
	if err != nil {
		return report, err
	}
	if _, err := containedPath(sourceAbs, targetAbs); err == nil {
		return report, errors.New("recovery target must be outside the source data directory")
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return report, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(target))
		}
	}()
	manifest := recoveryManifest{Version: 1, CreatedAt: time.Now().UTC()}
	add := func(source, dest string) error {
		file, err := copyRecoveryFile(ctx, source, filepath.Join(target, filepath.FromSlash(dest)))
		if err != nil {
			return err
		}
		file.Path = dest
		manifest.Files = append(manifest.Files, file)
		report.Files++
		report.Bytes += file.Size
		return nil
	}
	if err := add(databasePath, "sessions.db"); err != nil {
		return report, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		source := databasePath + suffix
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return report, err
		}
		if err := add(source, "sessions.db"+suffix); err != nil {
			return report, err
		}
	}
	for _, component := range []string{Directory, "artifacts", "assets", "config.toml", "config.json", "telemetry-install-id"} {
		source := filepath.Join(dataDir, component)
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			if component == Directory {
				return report, errors.New("raw archive vault is missing")
			}
			continue
		} else if err != nil {
			return report, err
		}
		err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return errors.New("recovery component contains a non-regular file")
			}
			rel, err := filepath.Rel(dataDir, path)
			if err != nil {
				return err
			}
			return add(path, filepath.ToSlash(rel))
		})
		if err != nil {
			return report, err
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return report, err
	}
	if err := os.WriteFile(filepath.Join(target, recoveryManifestName), encoded, 0o600); err != nil {
		return report, err
	}
	_, err = VerifyRecovery(ctx, target)
	return report, err
}

func copyRecoveryFile(ctx context.Context, source, dest string) (recoveryFile, error) {
	var out recoveryFile
	info, err := os.Lstat(source)
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() {
		return out, errors.New("recovery input must be a regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return out, err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return out, err
	}
	output, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return out, err
	}
	ref, copyErr := hashReader(ctx, io.TeeReader(input, output))
	syncErr := output.Sync()
	closeErr := output.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return out, err
	}
	final, err := input.Stat()
	if err != nil {
		return out, err
	}
	if ref.Length != info.Size() || !final.ModTime().Equal(info.ModTime()) || final.Size() != info.Size() {
		return out, errors.New("recovery input changed during copy")
	}
	if err := os.Chtimes(dest, info.ModTime(), info.ModTime()); err != nil {
		return out, err
	}
	return recoveryFile{SHA256: ref.SHA256, Size: ref.Length, ModTimeNS: info.ModTime().UnixNano()}, nil
}

func loadRecovery(root *os.Root) (recoveryManifest, error) {
	f, err := root.Open(recoveryManifestName)
	if err != nil {
		return recoveryManifest{}, err
	}
	defer f.Close()
	var manifest recoveryManifest
	if err := json.UnmarshalRead(io.LimitReader(f, 32<<20), &manifest, json.RejectUnknownMembers(true)); err != nil {
		return manifest, err
	}
	if manifest.Version != 1 || len(manifest.Files) == 0 {
		return manifest, errors.New("unsupported or empty recovery manifest")
	}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		if !fs.ValidPath(file.Path) || file.Path == "." || file.Path == recoveryManifestName || seen[file.Path] || file.Size < 0 {
			return manifest, errors.New("invalid recovery file path or size")
		}
		seen[file.Path] = true
	}
	if !seen["sessions.db"] {
		return manifest, errors.New("recovery has no session database")
	}
	return manifest, nil
}

// VerifyRecovery checks every listed byte and rejects unlisted files. This is
// filesystem verification; Restore also verifies the accepted source closure.
func VerifyRecovery(ctx context.Context, path string) (Report, error) {
	var report Report
	root, err := os.OpenRoot(path)
	if err != nil {
		return report, err
	}
	defer root.Close()
	manifest, err := loadRecovery(root)
	if err != nil {
		return report, err
	}
	listed := map[string]bool{recoveryManifestName: true}
	for _, entry := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		file, err := root.Open(filepath.FromSlash(entry.Path))
		if err != nil {
			return report, err
		}
		ref, readErr := hashReader(ctx, file)
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return report, err
		}
		if ref.SHA256 != entry.SHA256 || ref.Length != entry.Size {
			return report, fmt.Errorf("recovery file failed verification: %s", entry.Path)
		}
		listed[entry.Path] = true
		report.Files++
		report.Bytes += entry.Size
	}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() || !listed[path] {
			return errors.New("recovery contains an unlisted or non-regular file")
		}
		return nil
	})
	return report, err
}

// Restore copies a verified recovery point into a new empty data directory.
// The caller then opens that new archive and calls Verify before reporting it
// usable; this detects stale or incomplete vault/database combinations.
func Restore(ctx context.Context, source, target string) (report Report, retErr error) {
	sourceAbs, err := canonicalRecoverySource(source)
	if err != nil {
		return report, err
	}
	targetAbs, err := recoveryDestination(target)
	if err != nil {
		return report, err
	}
	if _, err := containedPath(sourceAbs, targetAbs); err == nil {
		return report, errors.New("restore destination must be outside the recovery point")
	}
	report, err = VerifyRecovery(ctx, source)
	if err != nil {
		return report, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return report, err
	}
	defer root.Close()
	manifest, err := loadRecovery(root)
	if err != nil {
		return report, err
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return report, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(target))
		}
	}()
	for _, entry := range manifest.Files {
		input, err := root.Open(filepath.FromSlash(entry.Path))
		if err != nil {
			return report, err
		}
		dest := filepath.Join(target, filepath.FromSlash(entry.Path))
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			_ = input.Close()
			return report, err
		}
		output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = input.Close()
			return report, err
		}
		ref, copyErr := hashReader(ctx, io.TeeReader(input, output))
		err = errors.Join(copyErr, output.Sync(), output.Close(), input.Close())
		if err != nil {
			return report, err
		}
		if ref.SHA256 != entry.SHA256 || ref.Length != entry.Size {
			return report, errors.New("recovery changed while restoring")
		}
		mtime := time.Unix(0, entry.ModTimeNS)
		if err := os.Chtimes(dest, mtime, mtime); err != nil {
			return report, err
		}
	}
	return report, nil
}

// Resolve the existing parent before creating a new destination. A symlinked
// parent must not turn an apparently separate backup into a recursive copy.
func recoveryDestination(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func canonicalRecoverySource(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}
