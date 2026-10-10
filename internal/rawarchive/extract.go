package rawarchive

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/rawsync"
)

// Extract recovers one complete portable capture when captureID is set. Without
// a selector, it writes all retained files below their archive root IDs and
// refuses conflicting contents for the same path.
func (a *Archive) Extract(ctx context.Context, target, captureID string) (report Report, retErr error) {
	if captureID == "" {
		conflict, err := a.database.HasConflictingRawArchiveFiles(ctx)
		if err != nil {
			return report, fmt.Errorf("checking retained file versions: %w", err)
		}
		if conflict {
			return report, errors.New("multiple versions of a retained path exist; select a capture with --capture CAPTURE_ID")
		}
	}
	if err := os.Mkdir(target, 0o700); err != nil {
		return report, err
	}
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, os.RemoveAll(target))
		}
	}()
	root, err := os.OpenRoot(target)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, root.Close()) }()
	if captureID != "" {
		return a.extractCapture(ctx, root, target, captureID)
	}
	if err := a.checkExtractCaseCollisions(ctx, target); err != nil {
		return report, err
	}
	var after int64
	for {
		files, err := a.database.ListRawArchiveFiles(ctx, after, pageSize)
		if err != nil {
			return report, err
		}
		for _, file := range files {
			if !filepath.IsLocal(file.RootID) || filepath.Base(file.RootID) != file.RootID || !filepath.IsLocal(file.Path) {
				return report, errors.New("invalid native file path")
			}
			path := filepath.Join(file.RootID, filepath.FromSlash(file.Path))
			if err := a.extractFile(ctx, root, path, rawsync.ObjectRef{SHA256: file.SHA256, Length: file.Size}, file.ModTimeNS); err != nil {
				return report, err
			}
			report.Files++
			report.Bytes += file.Size
			after = file.ID
		}
		if len(files) < pageSize {
			return report, nil
		}
	}
}

func (a *Archive) extractFile(ctx context.Context, root *os.Root, path string, ref rawsync.ObjectRef, modTimeNS int64) error {
	if err := root.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	output, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("extracting %s (conflicting versions cannot be extracted together): %w", path, err)
	}
	_, copyErr := a.objects.CopyObject(ctx, a.tenant, ref, output)
	if err := errors.Join(copyErr, output.Sync(), output.Close()); err != nil {
		return err
	}
	mtime := time.Unix(0, modTimeNS)
	return root.Chtimes(path, mtime, mtime)
}

func (a *Archive) extractCapture(ctx context.Context, root *os.Root, target, captureID string) (Report, error) {
	report := Report{CaptureID: captureID}
	evidence, err := a.database.RawArchiveCaptureEvidence(ctx, captureID)
	if err != nil {
		return report, err
	}
	if len(evidence) != 2 || evidence[0].RootID != evidence[1].RootID {
		return report, errors.New("capture evidence is missing or ambiguous")
	}
	for _, file := range evidence {
		name := filepath.Base(file.Path)
		limit := int64(4 << 20)
		if name == "inventory.json" {
			limit = 256 << 20
			if file.SHA256 != captureID {
				return report, errors.New("capture inventory does not match selected ID")
			}
		}
		if file.Size < 0 || file.Size > limit {
			return report, errors.New("invalid capture metadata size")
		}
		if err := a.extractFile(ctx, root, name, rawsync.ObjectRef{SHA256: file.SHA256, Length: file.Size}, file.ModTimeNS); err != nil {
			return report, err
		}
		report.Files++
		report.Bytes += file.Size
	}
	b, err := root.ReadFile("inventory.json")
	if err != nil {
		return report, err
	}
	var inventory captureInventory
	if err := json.Unmarshal(b, &inventory, json.RejectUnknownMembers(true)); err != nil {
		return report, err
	}
	// Preserve empty roots and session directories as well as files. Provider
	// discovery requires every recorded session directory to exist. LoadCapture
	// validates the complete package below; check paths before creating any
	// inventory-directed output.
	for _, binding := range inventory.Source.Roots {
		if !filepath.IsLocal(binding.ID) || filepath.Base(binding.ID) != binding.ID || binding.Path != "roots/"+binding.ID {
			return report, errors.New("invalid capture root binding")
		}
		if err := root.MkdirAll(filepath.FromSlash(binding.Path), 0o700); err != nil {
			return report, err
		}
		for _, dir := range binding.SessionDirs {
			if !filepath.IsLocal(dir) {
				return report, errors.New("invalid capture session directory")
			}
			if err := root.MkdirAll(filepath.Join(filepath.FromSlash(binding.Path), dir), 0o700); err != nil {
				return report, err
			}
		}
		report.Roots++
	}
	for _, file := range inventory.Files {
		if !filepath.IsLocal(file.RootID) || filepath.Base(file.RootID) != file.RootID || !filepath.IsLocal(file.Path) || file.Path == "." {
			return report, errors.New("invalid capture inventory path")
		}
		path := filepath.Join("roots", file.RootID, filepath.FromSlash(file.Path))
		if err := a.extractFile(ctx, root, path, rawsync.ObjectRef{SHA256: file.SHA256, Length: file.Size}, file.ModTimeNS); err != nil {
			return report, err
		}
		report.Files++
		report.Bytes += file.Size
	}
	_, err = LoadCapture(ctx, filepath.Join(target, "capture.json"))
	return report, err
}

func (a *Archive) checkExtractCaseCollisions(ctx context.Context, target string) (retErr error) {
	probe, err := os.CreateTemp(target, ".case-probe-*")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.Remove(probe.Name())) }()
	if err := probe.Close(); err != nil {
		return err
	}
	info, err := os.Stat(probe.Name())
	if err != nil {
		return err
	}
	alias, err := os.Stat(filepath.Join(target, strings.ToUpper(filepath.Base(probe.Name()))))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !os.SameFile(info, alias) {
		return nil
	}
	seen := map[string]string{}
	var after int64
	for {
		files, err := a.database.ListRawArchiveFiles(ctx, after, pageSize)
		if err != nil {
			return err
		}
		for _, file := range files {
			path := filepath.Join(file.RootID, filepath.FromSlash(file.Path))
			folded := strings.ToLower(path)
			if previous, ok := seen[folded]; ok && previous != path {
				return fmt.Errorf("case-folding collision between %q and %q on the extraction destination", previous, path)
			}
			seen[folded] = path
			after = file.ID
		}
		if len(files) < pageSize {
			return nil
		}
	}
}
