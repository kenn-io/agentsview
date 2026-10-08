package rawarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

const captureVersion = 1

// CaptureDescriptor identifies one completed, immutable source capture.
// Original paths are provenance; only root-relative paths locate captured data.
type CaptureDescriptor struct {
	data           []byte
	Version        int                            `json:"version"`
	CaptureID      string                         `json:"capture_id"`
	Source         ImportSpec                     `json:"source"`
	StartedAt      time.Time                      `json:"started_at"`
	CompletedAt    time.Time                      `json:"completed_at"`
	WritersStopped bool                           `json:"writers_stopped"`
	ReaderBuild    string                         `json:"reader_build"`
	NewIdentities  []string                       `json:"new_identities,omitempty"`
	Omissions      []CaptureOmission              `json:"omissions"`
	Preflight      CapturePreflight               `json:"preflight"`
	RawSync        *rawcheckpoint.ArchiveEvidence `json:"raw_sync,omitempty"`
	OrdinaryVault  string                         `json:"ordinary_vault"`
}
type CaptureOmission struct {
	RootID string `json:"root_id"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}
type CaptureFile struct {
	RootID    string `json:"root_id"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	ModTimeNS int64  `json:"mod_time_ns"`
	Method    string `json:"method"`
}
type CapturePreflight struct {
	Deletions                []db.RawArchiveSuppression `json:"deletions,omitempty"`
	DeletionAttributionError string                     `json:"deletion_attribution_error,omitempty"`
	DatabaseSHA256           string                     `json:"database_sha256"`
	Counts                   map[string]*int64          `json:"counts"`
	Unknown                  map[string]string          `json:"unknown,omitempty"`
}
type captureInventory struct {
	data    []byte
	Version int           `json:"version"`
	Source  ImportSpec    `json:"source"`
	Files   []CaptureFile `json:"files"`
}

// LoadCapture validates the complete inventory and every captured file. It
// never resolves original paths, so a moved capture has the same identity.
func LoadCapture(ctx context.Context, path string) (CaptureDescriptor, error) {
	descriptor, _, err := readCapture(ctx, path)
	return descriptor, err
}

func readCaptureJSON(ctx context.Context, root *os.Root, name string, limit int64, out any) ([]byte, error) {
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid capture metadata file")
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(contextReader{ctx, f}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("capture metadata exceeds size limit")
	}
	return b, json.Unmarshal(b, out, json.RejectUnknownMembers(true))
}

// readCaptureDescriptor validates identity metadata without requiring the old
// payload. Import and recovery additionally verify the inventory in readCapture.
func readCaptureDescriptor(ctx context.Context, root *os.Root, name string) (CaptureDescriptor, map[string]RootSpec, error) {
	var d CaptureDescriptor
	var err error
	if d.data, err = readCaptureJSON(ctx, root, name, 4<<20, &d); err != nil {
		return d, nil, err
	}
	if d.Version != captureVersion || d.CompletedAt.IsZero() || d.StartedAt.IsZero() || d.CompletedAt.Before(d.StartedAt) {
		return d, nil, errors.New("unsupported or incomplete capture descriptor")
	}
	captureID, err := hex.DecodeString(d.CaptureID)
	if err != nil || len(captureID) != sha256.Size {
		return d, nil, errors.New("invalid capture ID")
	}
	if err := validateRecoveryIdentity([]byte(d.Source.DeviceID)); err != nil {
		return d, nil, err
	}
	if strings.TrimSpace(d.Source.Machine) == "" || len(d.Source.Roots) < 2 {
		return d, nil, errors.New("capture source bindings are incomplete")
	}
	roots := make(map[string]RootSpec)
	for _, r := range d.Source.Roots {
		if _, exists := roots[r.ID]; exists || r.ID == "" || r.Path != filepath.ToSlash(filepath.Join("roots", r.ID)) || !filepath.IsLocal(r.Path) {
			return d, nil, errors.New("invalid capture root binding")
		}
		if (r.Provider != "claude" && r.Provider != "codex" && r.Provider != "files") || r.OriginalPath == "" || r.ConfiguredPath == "" {
			return d, nil, errors.New("invalid capture source binding")
		}
		if _, err := rawsync.NewAuthIdentity(r.ID, d.Source.DeviceID); err != nil {
			return d, nil, err
		}
		for _, dir := range r.SessionDirs {
			if !filepath.IsLocal(dir) {
				return d, nil, errors.New("invalid capture session directory")
			}
		}
		roots[r.ID] = r
	}
	if root, ok := roots["application"]; !ok || root.Provider != "files" {
		return d, nil, errors.New("capture application binding is missing")
	}
	return d, roots, nil
}

func readCapture(ctx context.Context, path string) (CaptureDescriptor, captureInventory, error) {
	var d CaptureDescriptor
	var inventory captureInventory
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return d, inventory, err
	}
	defer root.Close()
	d, roots, err := readCaptureDescriptor(ctx, root, filepath.Base(path))
	if err != nil {
		return d, inventory, err
	}
	b, err := readCaptureJSON(ctx, root, "inventory.json", 256<<20, &inventory)
	if err != nil {
		return d, inventory, err
	}
	digest := sha256.Sum256(b)
	if d.CaptureID != hex.EncodeToString(digest[:]) || inventory.Version != captureVersion || !reflect.DeepEqual(inventory.Source, d.Source) {
		return d, inventory, errors.New("capture inventory does not match descriptor")
	}
	inventory.data = b
	expected := make(map[string]CaptureFile, len(inventory.Files))
	for _, file := range inventory.Files {
		binding, ok := roots[file.RootID]
		if !ok || !filepath.IsLocal(file.Path) || file.Path == "." || file.Size < 0 {
			return d, inventory, errors.New("invalid capture inventory entry")
		}
		name := filepath.Join(binding.Path, filepath.FromSlash(file.Path))
		if _, exists := expected[name]; exists {
			return d, inventory, errors.New("duplicate capture inventory entry")
		}
		expected[name] = file
	}
	if d.Preflight.DatabaseSHA256 != "" && expected[filepath.Join("roots", "application", "sessions.db")].SHA256 != d.Preflight.DatabaseSHA256 {
		return d, inventory, errors.New("preflight is not bound to the captured database")
	}
	for _, binding := range d.Source.Roots {
		err = fs.WalkDir(root.FS(), binding.Path, func(name string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			file, ok := expected[filepath.FromSlash(name)]
			if !ok || !entry.Type().IsRegular() {
				return fmt.Errorf("file outside capture inventory: %s", name)
			}
			f, err := root.Open(filepath.FromSlash(name))
			if err != nil {
				return err
			}
			ref, readErr := hashReader(ctx, f)
			err = errors.Join(readErr, f.Close())
			if err != nil {
				return err
			}
			if ref.SHA256 != file.SHA256 || ref.Length != file.Size {
				return fmt.Errorf("capture file differs from inventory: %s", name)
			}
			delete(expected, filepath.FromSlash(name))
			return nil
		})
		if err != nil {
			return d, inventory, err
		}
	}
	if len(expected) > 0 {
		return d, inventory, errors.New("capture inventory has missing files")
	}
	var report CaptureDescriptor
	if _, err := readCaptureJSON(ctx, root, "roots/application/capture-report.json", 4<<20, &report); err != nil {
		return d, inventory, err
	}
	want := d
	want.CaptureID = ""
	want.data = nil
	if !reflect.DeepEqual(report, want) {
		return d, inventory, errors.New("capture report differs from descriptor")
	}
	return d, inventory, nil
}
