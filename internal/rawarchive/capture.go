package rawarchive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/kit/atomicfile"
)

type CaptureOptions struct {
	Destination    string
	DataDir        string
	Roots          []RootSpec
	IdentityFrom   string
	WritersStopped bool
	Settings       RecoverySettings
	ReaderBuild    string
	Progress       func(string)
}

// Capture writes a new portable source package. It never opens the source
// application database through AgentsView's migrating database constructor.
func Capture(ctx context.Context, opts CaptureOptions) (d CaptureDescriptor, retErr error) {
	d = CaptureDescriptor{Version: captureVersion, StartedAt: time.Now().UTC(), WritersStopped: opts.WritersStopped, ReaderBuild: opts.ReaderBuild, OrdinaryVault: "absent"}
	if len(opts.Roots) == 0 {
		return d, errors.New("at least one --root PROVIDER=PATH is required")
	}
	if err := opts.Settings.validate(); err != nil {
		return d, err
	}
	source, err := canonicalRecoverySource(opts.DataDir)
	if err != nil {
		return d, err
	}
	target, err := recoveryDestination(opts.Destination)
	if err != nil {
		return d, err
	}
	if pathsOverlap(source, target) {
		return d, errors.New("capture destination must be outside source data")
	}
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return d, errors.New("capture requires a new destination")
	}
	if _, err := os.Lstat(filepath.Join(source, "artifacts")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return d, err
		}
		return d, errors.New("ordinary artifact vault capture requires stopped-vault ownership support; no capture created")
	}
	if _, err := os.Lstat(filepath.Join(source, Directory)); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return d, err
		}
		return d, errors.New("source already contains a raw archive; use archive backup to preserve its vault")
	}
	var previous CaptureDescriptor
	if opts.IdentityFrom != "" {
		root, err := os.OpenRoot(filepath.Dir(opts.IdentityFrom))
		if err != nil {
			return d, fmt.Errorf("previous capture: %w", err)
		}
		previous, _, err = readCaptureDescriptor(ctx, root, filepath.Base(opts.IdentityFrom))
		err = errors.Join(err, root.Close())
		if err != nil {
			return d, fmt.Errorf("previous capture: %w", err)
		}
	}
	identity, err := os.ReadFile(filepath.Join(source, "telemetry-install-id"))
	if err == nil {
		if err := validateRecoveryIdentity(identity); err != nil {
			return d, err
		}
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return d, err
	}
	d.Source.DeviceID = strings.TrimSpace(string(identity))
	d.Source.Machine = opts.Settings.LocalMachineName
	if d.Source.DeviceID == "" {
		d.Source.DeviceID = previous.Source.DeviceID
		if d.Source.DeviceID == "" {
			d.Source.DeviceID = newCaptureIdentity()
			d.NewIdentities = append(d.NewIdentities, "installation")
		}
	}
	if err := validateRecoveryIdentity([]byte(d.Source.DeviceID)); err != nil {
		return d, err
	}
	if previous.Source.DeviceID != "" && previous.Source.DeviceID != d.Source.DeviceID {
		return d, errors.New("previous capture belongs to a different installation")
	}
	checkpoint := filepath.Join(source, "raw-sync", "checkpoint.db")
	if _, err := os.Lstat(checkpoint); err == nil {
		store, err := rawcheckpoint.OpenReadOnly(ctx, checkpoint)
		if err != nil {
			return d, err
		}
		evidence, readErr := store.ArchiveEvidence(ctx)
		if err = errors.Join(readErr, store.Close()); err != nil {
			return d, err
		}
		d.RawSync = &evidence
	} else if !errors.Is(err, os.ErrNotExist) {
		return d, err
	}
	seen := map[string]bool{"application": true}
	canonicalBases := make(map[string]string)
	for _, input := range opts.Roots {
		if input.Provider != "claude" && input.Provider != "codex" && input.Provider != "files" {
			return d, errors.New("capture provider must be claude, codex or files")
		}
		info, err := os.Lstat(input.Path)
		if err != nil {
			return d, err
		}
		if !info.IsDir() {
			return d, errors.New("selected capture root must be a directory, not a symlink")
		}
		original, err := filepath.Abs(input.Path)
		if err != nil {
			return d, err
		}
		configured, err := canonicalRecoverySource(input.Path)
		if err != nil {
			return d, err
		}
		base, dirs := captureRootLayout(input.Provider, original)
		canonicalBase, err := canonicalRecoverySource(base)
		if err != nil {
			return d, err
		}
		if pathsOverlap(canonicalBase, target) || pathsOverlap(canonicalBase, source) {
			return d, errors.New("capture roots must not overlap application data or destination")
		}
		input.OriginalPath = base
		input.ConfiguredPath = configured
		input.SessionDirs = dirs
		input.ID = ""
		if d.RawSync != nil {
			for _, r := range d.RawSync.Roots {
				if r.Provider == input.Provider && (r.LocalPath == configured || slices.ContainsFunc(dirs, func(dir string) bool { return r.LocalPath == filepath.Join(canonicalBase, dir) })) {
					if input.ID != "" && input.ID != r.ID {
						return d, errors.New("multiple raw-sync roots match; capture each configured transcript directory explicitly")
					}
					input.ID = r.ID
				}
			}
		}
		for _, r := range previous.Source.Roots {
			if r.Provider == input.Provider && r.ConfiguredPath == configured {
				if input.ID != "" && input.ID != r.ID {
					return d, errors.New("raw-sync root identity differs from previous capture")
				}
				input.ID = r.ID
			}
		}
		if input.ID == "" {
			input.ID = newCaptureIdentity()
			d.NewIdentities = append(d.NewIdentities, input.ID)
		}
		if seen[input.ID] {
			return d, errors.New("duplicate selected capture root")
		}
		seen[input.ID] = true
		canonicalBases[input.ID] = canonicalBase
		input.Path = "roots/" + input.ID
		d.Source.Roots = append(d.Source.Roots, input)
	}
	applicationOrigin := source
	for _, root := range previous.Source.Roots {
		if root.ID == "application" {
			applicationOrigin = root.OriginalPath
			break
		}
	}
	d.Source.Roots = append(d.Source.Roots, RootSpec{ID: "application", Provider: "files", Path: "roots/application", OriginalPath: applicationOrigin, ConfiguredPath: source})
	stage, err := os.MkdirTemp(filepath.Dir(target), ".capture-")
	if err != nil {
		return d, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(stage)) }()
	inventory := captureInventory{Version: captureVersion, Source: d.Source}
	for _, root := range d.Source.Roots {
		if root.ID == "application" {
			continue
		}
		if opts.Progress != nil {
			opts.Progress("Capturing " + root.Provider + " root " + root.ID)
		}
		files, omissions, err := captureTree(ctx, root.OriginalPath, filepath.Join(stage, root.Path), root)
		if err != nil {
			return d, err
		}
		inventory.Files = append(inventory.Files, files...)
		d.Omissions = append(d.Omissions, omissions...)
	}
	app := filepath.Join(stage, "roots", "application")
	if err := os.MkdirAll(app, 0o700); err != nil {
		return d, err
	}
	// The application root is an allowlist; runtime settings and connections are
	// never copied. Provider trees are handled separately above.
	for _, name := range []string{"sessions.db", "assets", "telemetry-install-created"} {
		path := filepath.Join(source, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			d.Omissions = append(d.Omissions, CaptureOmission{"application", name, "absent"})
			continue
		}
		if err != nil {
			return d, err
		}
		if info.IsDir() {
			files, omissions, err := captureTree(ctx, path, filepath.Join(app, name), RootSpec{ID: "application", Provider: "files"})
			if err != nil {
				return d, err
			}
			for i := range files {
				files[i].Path = name + "/" + files[i].Path
			}
			for i := range omissions {
				omissions[i].Path = name + "/" + omissions[i].Path
			}
			inventory.Files = append(inventory.Files, files...)
			d.Omissions = append(d.Omissions, omissions...)
		} else if info.Mode().IsRegular() {
			file, err := captureFile(ctx, path, filepath.Join(app, name), "application", name, info)
			if err != nil {
				return d, err
			}
			inventory.Files = append(inventory.Files, file)
		} else {
			return d, fmt.Errorf("required application component is not regular: %s", name)
		}
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return d, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if slices.Contains([]string{"sessions.db", "assets", "telemetry-install-id", "telemetry-install-created"}, name) {
			continue
		}
		reason := "runtime component excluded; only recovery settings retained"
		if slices.Contains([]string{"sessions.db-wal", "sessions.db-shm", "sessions.db-journal"}, name) {
			reason = "included through SQLite online backup"
		}
		if name == "raw-sync" {
			reason = "only identity and acknowledged receipts retained"
		}
		d.Omissions = append(d.Omissions, CaptureOmission{"application", name, reason})
	}
	add := func(name string, value any) error {
		data, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err = os.WriteFile(filepath.Join(app, name), data, 0o600); err != nil {
			return err
		}
		file, err := inventoryFile(ctx, filepath.Join(app, name), "application", name, "generated")
		if err == nil {
			inventory.Files = append(inventory.Files, file)
		}
		return err
	}
	if err := os.WriteFile(filepath.Join(app, "telemetry-install-id"), []byte(d.Source.DeviceID+"\n"), 0o600); err != nil {
		return d, err
	}
	file, err := inventoryFile(ctx, filepath.Join(app, "telemetry-install-id"), "application", "telemetry-install-id", "identity")
	if err != nil {
		return d, err
	}
	inventory.Files = append(inventory.Files, file)
	if err := add("recovery-settings.json", opts.Settings); err != nil {
		return d, err
	}
	d.Preflight, err = capturePreflight(ctx, filepath.Join(app, "sessions.db"), d.Source.DeviceID)
	if err != nil {
		return d, err
	}
	if d.Preflight.DatabaseSHA256 != "" {
		if err := recordRootAliases(ctx, filepath.Join(app, "sessions.db"), d.Source.Roots, canonicalBases); err != nil {
			return d, fmt.Errorf("recording equivalent root spellings: %w", err)
		}
		inventory.Source = d.Source
		database, err := db.OpenReadOnly(ctx, filepath.Join(app, "sessions.db"))
		if err != nil {
			return d, fmt.Errorf("cannot verify captured asset closure without a compatible schema: %w", err)
		}
		err = errors.Join(database.VerifyAssets(ctx, filepath.Join(app, "assets")), database.Close())
		if err != nil {
			return d, err
		}
	}
	d.CompletedAt = time.Now().UTC()
	if err := add("capture-report.json", d); err != nil {
		return d, err
	}
	b, err := json.Marshal(inventory)
	if err != nil {
		return d, err
	}
	if err := os.WriteFile(filepath.Join(stage, "inventory.json"), b, 0o600); err != nil {
		return d, err
	}
	digest := sha256.Sum256(b)
	d.CaptureID = hex.EncodeToString(digest[:])
	b, err = json.Marshal(d)
	if err != nil {
		return d, err
	}
	if err := os.WriteFile(filepath.Join(stage, "capture.json"), b, 0o600); err != nil {
		return d, err
	}
	if _, err := LoadCapture(ctx, filepath.Join(stage, "capture.json")); err != nil {
		return d, err
	}
	if err := ctx.Err(); err != nil {
		return d, err
	}
	if err := atomicfile.RenameNoReplace(stage, target); err != nil {
		return d, err
	}
	return d, nil
}

func newCaptureIdentity() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func captureRootLayout(provider, path string) (string, []string) {
	base := filepath.Base(path)
	if provider == "claude" && base == "projects" || provider == "codex" && (base == "sessions" || base == "archived_sessions") {
		return filepath.Dir(path), []string{base}
	}
	var dirs []string
	for _, name := range map[string][]string{"claude": {"projects"}, "codex": {"sessions", "archived_sessions"}}[provider] {
		if info, err := os.Lstat(filepath.Join(path, name)); err == nil && info.IsDir() {
			dirs = append(dirs, name)
		}
	}
	return path, dirs
}
