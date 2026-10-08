package rawarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

// ImportSpec binds immutable capture directories to their original identities.
// Paths may change between imports; IDs and OriginalPath must not.
type ImportSpec struct {
	DeviceID      string     `json:"device_id"`
	Machine       string     `json:"machine"`
	Roots         []RootSpec `json:"roots"`
	capture       *CaptureDescriptor
	inventory     []CaptureFile
	inventoryData []byte
}
type RootSpec struct {
	ID             string   `json:"id"`
	Provider       string   `json:"provider"`
	Path           string   `json:"path"`
	OriginalPath   string   `json:"original_path"`
	ConfiguredPath string   `json:"configured_path,omitempty"`
	SessionDirs    []string `json:"session_dirs,omitempty"`
}

func LoadImportSpec(ctx context.Context, path string) (ImportSpec, error) {
	spec, err := loadCaptureSpec(ctx, path)
	if err != nil {
		return ImportSpec{}, err
	}
	if err := spec.capture.Preflight.importError(false); err != nil {
		return ImportSpec{}, err
	}
	return spec, nil
}

func loadCaptureSpec(ctx context.Context, path string) (ImportSpec, error) {
	descriptor, inventory, err := readCapture(ctx, path)
	if err != nil {
		return ImportSpec{}, err
	}
	spec := descriptor.Source
	spec.capture, spec.inventory, spec.inventoryData = &descriptor, inventory.Files, inventory.data
	spec.Roots = slices.Clone(spec.Roots)
	for i := range spec.Roots {
		if !filepath.IsAbs(spec.Roots[i].Path) {
			spec.Roots[i].Path = filepath.Join(filepath.Dir(path), spec.Roots[i].Path)
		}
	}
	return spec, nil
}

// Import retains every regular file before accepting complete provider sources.
// It never reads a live provider database specially: the input must be a closed,
// immutable capture. Unsupported files remain supplemental and are recoverable.
func (a *Archive) Import(ctx context.Context, spec ImportSpec) (Report, error) {
	var report Report
	if spec.capture == nil {
		return report, errors.New("validated capture is required; load it with LoadImportSpec")
	}
	report.CaptureID = spec.capture.CaptureID
	report.Preflight = &spec.capture.Preflight
	expected := make(map[string]CaptureFile, len(spec.inventory))
	for _, file := range spec.inventory {
		expected[file.RootID+"/"+file.Path] = file
	}
	identity, err := rawsync.NewAuthIdentity(a.tenant, spec.DeviceID)
	if err != nil {
		return report, err
	}
	if strings.TrimSpace(spec.Machine) == "" || len(spec.Roots) == 0 {
		return report, errors.New("original machine and roots are required")
	}
	inventory := make(map[string]db.RawArchiveFile)
	opened := make(map[string]*os.Root)
	defer func() {
		for _, root := range opened {
			_ = root.Close()
		}
	}()
	for _, input := range spec.Roots {
		if input.Provider != "files" && input.Provider != "claude" && input.Provider != "codex" {
			return report, fmt.Errorf("provider %q is not supported for archive reparse; use files for supplemental custody", input.Provider)
		}
		if _, ok := opened[input.ID]; ok {
			return report, errors.New("duplicate capture root ID")
		}
		if _, err := rawsync.NewAuthIdentity(input.ID, spec.DeviceID); err != nil {
			return report, err
		}
		rootPath, err := filepath.Abs(input.Path)
		if err != nil {
			return report, err
		}
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			return report, err
		}
		opened[input.ID] = root
		binding := db.RawArchiveRoot{ID: archiveRootID(spec.DeviceID, input.Provider, input.ID), ConfiguredRootID: input.ID, DeviceID: spec.DeviceID, Machine: spec.Machine, Provider: input.Provider, OriginalPath: input.OriginalPath}
		if err := a.database.RegisterRawArchiveRoot(ctx, binding); err != nil {
			return report, err
		}
		report.Roots++
		a.report("Retaining files in root " + input.ID)
		err = fs.WalkDir(root.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
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
				return fmt.Errorf("capture entry %q is not a regular file", rel)
			}
			f, err := root.Open(filepath.FromSlash(rel))
			if err != nil {
				return err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return err
			}
			key := input.ID + "/" + filepath.ToSlash(rel)
			want, ok := expected[key]
			if !ok || want.Size != info.Size() {
				return errors.New("capture changed after inventory verification")
			}
			delete(expected, key)
			ref := rawsync.ObjectRef{SHA256: want.SHA256, Length: want.Size}
			// Create verifies the declared digest even on a retry; custody is only
			// recorded after the complete stream has been consumed and checked.
			if _, err = a.objects.PutObject(ctx, a.tenant, ref, contextReader{ctx, f}); err != nil {
				return err
			}
			final, err := f.Stat()
			if err != nil {
				return err
			}
			if final.Size() != info.Size() || !final.ModTime().Equal(info.ModTime()) {
				return errors.New("capture changed while storing")
			}
			// The verified inventory owns source timestamps. A copied capture may
			// carry new filesystem times; those only detect concurrent changes.
			record := db.RawArchiveFile{RootID: binding.ID, Path: filepath.ToSlash(rel), SHA256: ref.SHA256, Size: ref.Length, ModTimeNS: want.ModTimeNS}
			if err := a.database.RecordRawArchiveFile(ctx, record); err != nil {
				return err
			}
			inventory[filepath.Join(rootPath, filepath.FromSlash(rel))] = record
			report.Files++
			report.Bytes += ref.Length
			if report.Files%100 == 0 {
				a.report(fmt.Sprintf("Retained %d files (%d bytes)", report.Files, report.Bytes))
			}
			return nil
		})
		if err != nil {
			return report, fmt.Errorf("retaining root %s: %w", input.ID, err)
		}
	}
	if len(expected) > 0 {
		return report, errors.New("capture files disappeared after verification")
	}
	if err := a.retainCaptureEvidence(ctx, spec); err != nil {
		return report, err
	}
	if err := a.database.RegisterRawArchiveDevice(ctx, spec.DeviceID, spec.Machine, spec.capture.Preflight.Deletions); err != nil {
		if !errors.Is(err, db.ErrRawArchiveDeletionConflict) {
			return report, err
		}
		report.Gaps = append(report.Gaps, err.Error())
		report.Supplemental = len(inventory)
		return report, nil
	}
	for _, input := range spec.Roots {
		if input.Provider == "files" {
			continue
		}
		rootPath, err := filepath.Abs(input.Path)
		if err != nil {
			return report, err
		}
		roots, err := captureSessionRoots(input)
		if err != nil {
			return report, err
		}
		if input.Provider == "codex" {
			// A fork may name a parent captured under another Codex root. Later
			// roots resolve companions only; this root still wins a duplicate.
			for _, other := range spec.Roots {
				if other.Provider != "codex" || other.ID == input.ID {
					continue
				}
				companions, err := captureSessionRoots(other)
				if err != nil {
					return report, err
				}
				roots = append(roots, companions...)
			}
		}
		provider, ok := parser.NewProvider(parser.AgentType(input.Provider), parser.ProviderConfig{Roots: roots, Machine: spec.Machine, StableSourceSnapshots: true})
		if !ok {
			return report, errors.New("provider is unavailable")
		}
		parseCtx := parser.WithoutFilesystemProjectDiscovery(ctx)
		discovery, err := parser.DiscoverRawCaptureSources(parseCtx, provider)
		if err != nil {
			return report, err
		}
		if !discovery.Complete {
			return report, errors.New("capture discovery is incomplete")
		}
		discovery.Sources = slices.DeleteFunc(discovery.Sources, func(source parser.SourceRef) bool {
			_, err := containedPath(rootPath, source.DisplayPath)
			return err != nil
		})
		grouped := make(map[string]bool)
		for _, source := range discovery.Sources {
			if grouped[source.Key] {
				continue
			}
			if err := ctx.Err(); err != nil {
				return report, err
			}
			plan, members, err := rawderive.PlanLocalCapture(parseCtx, provider, source, discovery.Sources)
			if err != nil {
				report.Gaps = append(report.Gaps, fmt.Sprintf("root %s: capture plan: %v", input.ID, err))
				continue
			}
			var entries []rawsync.Entry
			var covered []db.RawArchiveFile
			capturedAt := time.Unix(0, 0).UTC()
			for _, entry := range plan.Entries {
				record, ok := inventory[filepath.Clean(entry.LocalPath)]
				if !ok {
					return report, fmt.Errorf("provider plan references an uncaptured file in root %s", input.ID)
				}
				ref := rawsync.ObjectRef{SHA256: record.SHA256, Length: record.Size}
				entries = append(entries, rawsync.Entry{Path: entry.Path, Type: "file", Length: record.Size, ModTimeNS: record.ModTimeNS, Objects: []rawsync.ObjectRef{ref}})
				capturedAt = maxTime(capturedAt, time.Unix(0, record.ModTimeNS).UTC())
				covered = append(covered, record)
			}
			slices.SortFunc(entries, func(a, b rawsync.Entry) int { return strings.Compare(a.Path, b.Path) })
			sourceKey := plan.SourceKey
			if rel, err := containedPath(rootPath, sourceKey); err == nil {
				sourceKey = filepath.Join(input.OriginalPath, rel)
			}
			originalRel, err := containedPath(rootPath, source.DisplayPath)
			if err != nil {
				return report, err
			}
			originalPath := filepath.Join(input.OriginalPath, originalRel)
			manifest := rawsync.Manifest{SchemaVersion: rawsync.ManifestSchemaVersion, Provider: parser.AgentType(input.Provider), ConfiguredRootID: input.ID, SourceKey: sourceKey, Kind: rawsync.ManifestSnapshot, Entries: entries, CapturedAt: capturedAt}
			encoded, err := json.Marshal(manifest)
			if err != nil {
				return report, err
			}
			captureRef, err := hashReader(ctx, strings.NewReader(string(encoded)))
			if err != nil {
				return report, err
			}
			manifest.CaptureID = captureRef.SHA256
			canonical, err := rawsync.ValidateAndCanonicalize(identity, manifest, rawsync.DefaultManifestLimits())
			if err != nil {
				report.Gaps = append(report.Gaps, fmt.Sprintf("root %s: source retained as supplemental: %v", input.ID, err))
				continue
			}
			head, err := a.database.RawArchiveHead(ctx, archiveRootID(spec.DeviceID, input.Provider, input.ID), sourceKey)
			if err != nil {
				return report, err
			}
			if head != nil {
				old, err := rawsync.ParseCanonicalManifest(identity, head.ManifestID, head.CanonicalJSON, rawsync.DefaultManifestLimits())
				if err != nil {
					return report, err
				}
				if !reflect.DeepEqual(old.Manifest.Entries, canonical.Manifest.Entries) || head.OriginalPath != originalPath {
					// Preserve proposed evidence but never auto-reparent competing imports.
					if _, err := a.objects.PutManifest(ctx, canonical); err != nil {
						return report, err
					}
					report.Gaps = append(report.Gaps, fmt.Sprintf("source conflict: accepted %s, proposed %s", head.ManifestID, canonical.ManifestID))
					continue
				}
			} else {
				if _, err := a.objects.PutManifest(ctx, canonical); err != nil {
					return report, err
				}
				if _, err := a.database.AcceptRawArchiveSource(ctx, db.RawArchiveSource{ManifestID: canonical.ManifestID, RootID: archiveRootID(spec.DeviceID, input.Provider, input.ID), SourceKey: sourceKey, OriginalPath: originalPath, CanonicalJSON: canonical.CanonicalJSON}); err != nil {
					return report, err
				}
			}
			for _, record := range covered {
				record.Covered = true
				if err := a.database.RecordRawArchiveFile(ctx, record); err != nil {
					return report, err
				}
				inventory[findInventoryPath(spec, record)] = record
			}
			for _, member := range members {
				grouped[member.Key] = true
			}
			report.Sources++
			if report.Sources%100 == 0 {
				a.report(fmt.Sprintf("Accepted %d provider sources", report.Sources))
			}
		}
	}
	for _, record := range inventory {
		if !record.Covered {
			report.Supplemental++
		}
	}
	return report, nil
}

// captureSessionRoots returns the absolute provider roots recorded for one
// capture root.
func captureSessionRoots(input RootSpec) ([]string, error) {
	rootPath, err := filepath.Abs(input.Path)
	if err != nil {
		return nil, err
	}
	dirs := input.SessionDirs
	if len(dirs) == 0 {
		dirs = []string{"."}
	}
	roots := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		if !filepath.IsLocal(dir) {
			return nil, errors.New("session directory must be relative to capture root")
		}
		roots = append(roots, filepath.Join(rootPath, dir))
	}
	return roots, nil
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func findInventoryPath(spec ImportSpec, record db.RawArchiveFile) string {
	for _, root := range spec.Roots {
		if archiveRootID(spec.DeviceID, root.Provider, root.ID) == record.RootID {
			path, _ := filepath.Abs(root.Path)
			return filepath.Join(path, filepath.FromSlash(record.Path))
		}
	}
	return ""
}

// Internal namespaces never change the configured root IDs in wire manifests.
func archiveRootID(device, provider, root string) string {
	sum := sha256.Sum256([]byte(device + "\x00" + provider + "\x00" + root))
	return hex.EncodeToString(sum[:])
}
