package rawarchive

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/artifact"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	syncer "go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/docbank"
	"go.kenn.io/kit/atomicfile"
)

const recoveryInventoryName = "inventory.json"

// RecoverySettings is the complete allowlist of settings retained in a backup.
// Original credentials, connections, provider roots and runtime config are omitted.
type RecoverySettings struct {
	ResultContentBlockedCategories *[]string               `json:"result_content_blocked_categories,omitzero"`
	ArchiveContent                 config.ArchiveContent   `json:"archive_content"`
	ToolResultImages               config.ToolResultImages `json:"tool_result_images"`
	LocalMachineName               string                  `json:"local_machine_name"`
}

type recoveryInventory struct {
	Version     int      `json:"version"`
	ReaderBuild string   `json:"reader_build"`
	Files       []string `json:"files"`
}

func (s RecoverySettings) validate() error {
	_, contentErr := config.ParseArchiveContent(string(s.ArchiveContent))
	_, imagesErr := config.ParseToolResultImages(string(s.ToolResultImages))
	if strings.TrimSpace(s.LocalMachineName) == "" {
		return errors.New("original machine label is required")
	}
	return errors.Join(contentErr, imagesErr)
}

// Backup retains the caller's writer lock and this archive's exclusive raw
// vault owner. The SQLite snapshot is taken inside Docbank's metadata freeze.
func (a *Archive) Backup(ctx context.Context, target string, settings RecoverySettings, readerBuild string) (report Report, retErr error) {
	if err := settings.validate(); err != nil {
		return report, err
	}
	source, err := canonicalRecoverySource(a.dataDir)
	if err != nil {
		return report, err
	}
	destination, err := recoveryDestination(target)
	if err != nil {
		return report, err
	}
	if err := rejectPathsOverlap(source, destination, "backup repository must be outside the archive data directory"); err != nil {
		return report, err
	}
	// There is no public stopped-owner API for copying a closed ordinary
	// Docbank vault. Refuse this input until custody can be held without opening
	// or migrating it; never silently drop that component from a recovery point.
	if _, err := os.Lstat(filepath.Join(source, "artifacts")); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return report, err
		}
		return report, errors.New("backup of an ordinary artifact vault requires stopped-vault ownership support; no snapshot created")
	}
	if report, err = a.Verify(ctx); err != nil {
		return report, err
	}
	if err := a.database.VerifyAssets(ctx, filepath.Join(source, "assets")); err != nil {
		return report, err
	}
	scratch, err := os.MkdirTemp(source, ".archive-backup-")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	inventory := recoveryInventory{Version: 1, ReaderBuild: readerBuild}
	var extras []docbank.BackupExtraFile
	add := func(path, name string) {
		inventory.Files = append(inventory.Files, name)
		extras = append(extras, docbank.BackupExtraFile{Path: path, RecordAs: "application/" + name})
	}
	add(filepath.Join(scratch, "sessions.db"), "sessions.db")
	add(filepath.Join(scratch, "recovery-settings.json"), "recovery-settings.json")
	identity, err := os.ReadFile(filepath.Join(source, "telemetry-install-id"))
	if err != nil {
		return report, fmt.Errorf("reading installation identity: %w", err)
	}
	if err := validateRecoveryIdentity(identity); err != nil {
		return report, err
	}
	for _, component := range []string{"telemetry-install-id", "telemetry-install-created", "assets"} {
		path := filepath.Join(source, component)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return report, err
		}
		if err := filepath.WalkDir(path, func(path string, entry fs.DirEntry, walkErr error) error {
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
				return errors.New("backup component contains a non-regular file")
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			add(path, filepath.ToSlash(rel))
			return nil
		}); err != nil {
			return report, err
		}
	}
	extras = append(extras, docbank.BackupExtraFile{Path: filepath.Join(scratch, recoveryInventoryName), RecordAs: "application/" + recoveryInventoryName})
	var repository *docbank.BackupRepository
	if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
		repository, err = docbank.InitBackupRepository(destination)
		if err != nil {
			return report, err
		}
	} else if err != nil {
		return report, err
	} else {
		repository, err = docbank.OpenBackupRepository(destination)
		if err != nil {
			return report, err
		}
	}
	if err := rejectPathsOverlap(source, repository.Root(), "backup repository must be outside the archive data directory"); err != nil {
		return report, err
	}
	snapshot, err := a.repository.CreateBackup(ctx, repository, docbank.BackupOptions{
		ExtraFiles: extras,
		Prepare: func(ctx context.Context) error {
			if err := a.database.SnapshotTo(ctx, filepath.Join(scratch, "sessions.db")); err != nil {
				return err
			}
			for name, value := range map[string]any{"recovery-settings.json": settings, recoveryInventoryName: inventory} {
				data, err := json.Marshal(value)
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(scratch, name), data, 0o600); err != nil {
					return err
				}
			}
			return nil
		},
		Progress: func(p docbank.BackupProgress) {
			if p.Final {
				a.report("Backup: " + p.Stage)
			}
		},
	})
	if err != nil {
		return report, err
	}
	report.RepositoryID, report.SnapshotID = repository.ID(), snapshot.ID
	report.MinReaderVersion, report.ReaderBuild = snapshot.MinReaderVersion, readerBuild
	report.Excluded = []string{"original runtime configuration except recovery settings", "live provider directories and credentials outside the retained raw vault"}
	return report, nil
}

// VerifyRecovery selects one immutable recovery point, never the latest entry.
func VerifyRecovery(ctx context.Context, source, snapshot string) (docbank.BackupVerifyReport, error) {
	if snapshot == "" {
		return docbank.BackupVerifyReport{}, errors.New("--snapshot is required")
	}
	repository, err := docbank.OpenBackupRepository(source)
	if err != nil {
		return docbank.BackupVerifyReport{}, err
	}
	report, err := repository.Verify(ctx, docbank.BackupVerifyOptions{SnapshotID: snapshot})
	if err == nil && len(report.Problems) > 0 {
		err = fmt.Errorf("backup verification: %s", report.Problems[0].Detail)
	}
	return report, err
}

// Restore proves the selected backup and its application state in staging, then
// publishes a new data directory. Existing destinations are never replaced.
func Restore(ctx context.Context, source, snapshot, target string, progress func(string)) (report Report, retErr error) {
	if snapshot == "" {
		return report, errors.New("--snapshot is required")
	}
	repository, err := docbank.OpenBackupRepository(source)
	if err != nil {
		return report, err
	}
	destination, err := recoveryDestination(target)
	if err != nil {
		return report, err
	}
	if err := rejectPathsOverlap(repository.Root(), destination, "restore destination must be outside the backup repository"); err != nil {
		return report, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return report, err
		}
		return report, errors.New("restore requires a new destination")
	}
	scratch, err := os.MkdirTemp(filepath.Dir(destination), ".archive-restore-")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratch)) }()
	vaultPath := filepath.Join(scratch, "vault")
	if _, err := repository.Restore(ctx, docbank.BackupRestoreOptions{SnapshotID: snapshot, Target: vaultPath, ProtectedRoots: []string{destination}}); err != nil {
		return report, err
	}
	application := filepath.Join(vaultPath, "application")
	inventory, settings, err := loadRecovery(application)
	if err != nil {
		return report, err
	}
	assembled := filepath.Join(scratch, "archive")
	if err := os.Mkdir(assembled, 0o700); err != nil {
		return report, err
	}
	for _, name := range inventory.Files {
		if name == "recovery-settings.json" {
			continue
		}
		path := filepath.Join(assembled, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return report, err
		}
		if err := atomicfile.RenameNoReplace(filepath.Join(application, filepath.FromSlash(name)), path); err != nil {
			return report, err
		}
	}
	if err := os.RemoveAll(application); err != nil {
		return report, err
	}
	if err := os.Mkdir(filepath.Join(assembled, Directory), 0o700); err != nil {
		return report, err
	}
	if err := atomicfile.RenameNoReplace(vaultPath, filepath.Join(assembled, Directory, "artifacts")); err != nil {
		return report, err
	}
	if err := writeRecoveryConfig(assembled, settings); err != nil {
		return report, err
	}
	if report, err = verifyRestoredArchive(ctx, assembled, settings, progress); err != nil {
		return report, err
	}
	// Resolve every report field while the restore is still staged, so a
	// lookup failure cannot leave a published destination behind.
	readerVersion, err := snapshotReaderVersion(repository, snapshot)
	if err != nil {
		return report, err
	}
	// Refuse a destination created during the restore, including an empty one.
	if err := atomicfile.RenameNoReplace(assembled, destination); err != nil {
		return report, err
	}
	report.RepositoryID, report.SnapshotID, report.ReaderBuild = repository.ID(), snapshot, inventory.ReaderBuild
	report.MinReaderVersion = readerVersion
	return report, nil
}

// snapshotReaderVersion reports the selected recovery point's reader
// requirement so a restore report matches the backup report.
func snapshotReaderVersion(repository *docbank.BackupRepository, id string) (int, error) {
	snapshots, err := repository.Snapshots()
	if err != nil {
		return 0, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			return snapshot.MinReaderVersion, nil
		}
	}
	return 0, fmt.Errorf("restored snapshot %s is not listed in the repository", id)
}

func loadRecovery(path string) (recoveryInventory, RecoverySettings, error) {
	var inventory recoveryInventory
	var settings RecoverySettings
	root, err := os.OpenRoot(path)
	if err != nil {
		return inventory, settings, err
	}
	defer root.Close()
	read := func(name string, value any) error {
		f, err := root.Open(name)
		if err != nil {
			return err
		}
		defer f.Close()
		return json.UnmarshalRead(io.LimitReader(f, 32<<20), value, json.RejectUnknownMembers(true))
	}
	if err := read(recoveryInventoryName, &inventory); err != nil {
		return inventory, settings, err
	}
	if inventory.Version != 1 {
		return inventory, settings, errors.New("unsupported application recovery inventory")
	}
	seen := map[string]bool{recoveryInventoryName: true}
	for _, name := range inventory.Files {
		if !recoveryComponent(name) || !fs.ValidPath(name) || strings.Contains(name, "\\") || seen[name] {
			return inventory, settings, errors.New("invalid or repeated recovery component")
		}
		seen[name] = true
		info, err := root.Lstat(filepath.FromSlash(name))
		if err != nil {
			return inventory, settings, err
		}
		if !info.Mode().IsRegular() {
			return inventory, settings, errors.New("recovery component is not a regular file")
		}
	}
	for _, required := range []string{"sessions.db", "telemetry-install-id", "recovery-settings.json"} {
		if !seen[required] {
			return inventory, settings, fmt.Errorf("recovery is missing %s", required)
		}
	}
	if err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && (!entry.Type().IsRegular() || !seen[name]) {
			return errors.New("recovery has an unlisted component")
		}
		return nil
	}); err != nil {
		return inventory, settings, err
	}
	if err := read("recovery-settings.json", &settings); err != nil {
		return inventory, settings, err
	}
	identity, err := root.ReadFile("telemetry-install-id")
	if err != nil {
		return inventory, settings, err
	}
	return inventory, settings, errors.Join(settings.validate(), validateRecoveryIdentity(identity))
}

func recoveryComponent(name string) bool {
	return name == "sessions.db" || name == "telemetry-install-id" || name == "telemetry-install-created" || name == "recovery-settings.json" || strings.HasPrefix(name, "assets/")
}

func validateRecoveryIdentity(data []byte) error {
	if err := config.ValidateInstallationID(strings.TrimSpace(string(data))); err != nil {
		return errors.New("recovery installation identity must contain 32 hexadecimal characters")
	}
	return nil
}

func writeRecoveryConfig(path string, settings RecoverySettings) error {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	cfg := config.Config{DataDir: path}
	values := map[string]any{
		"archive_content": settings.ArchiveContent, "tool_result_images": settings.ToolResultImages,
		"local_machine_name": settings.LocalMachineName, "host": "127.0.0.1", "require_auth": true,
		"cursor_secret": base64.StdEncoding.EncodeToString(secret),
	}
	if settings.ResultContentBlockedCategories != nil {
		values["result_content_blocked_categories"] = *settings.ResultContentBlockedCategories
	}
	if err := cfg.SaveSettings(values); err != nil {
		return err
	}
	return cfg.EnsureAuthToken()
}

func verifyRestoredArchive(ctx context.Context, path string, settings RecoverySettings, progress func(string)) (report Report, retErr error) {
	database, err := db.OpenIsolatedWithArchiveContent(ctx, filepath.Join(path, "sessions.db"), settings.ArchiveContent)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, database.Close()) }()
	if err := database.EnableArchiveOnly(ctx); err != nil {
		return report, fmt.Errorf("marking restored archive: %w", err)
	}
	database.SetToolResultImages(settings.ToolResultImages)
	var integrity string
	if err := database.Reader().QueryRow(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return report, err
	}
	if integrity != "ok" {
		return report, errors.New("restored SQLite archive failed integrity check")
	}
	archive, err := Open(ctx, database, path, progress)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, archive.Close()) }()
	if report, err = archive.Verify(ctx); err != nil {
		return report, err
	}
	if err := database.VerifyAssets(ctx, filepath.Join(path, "assets")); err != nil {
		return report, err
	}
	if database.NeedsResync() {
		engine := syncer.NewEngine(ctx, database, syncer.EngineConfig{Ephemeral: true, ArchiveContent: settings.ArchiveContent})
		stats, buildErr := engine.ResyncAllWithOptions(ctx, nil, syncer.RebuildOptions{})
		engine.Close()
		if buildErr != nil {
			return report, buildErr
		}
		if !stats.ArchiveRebuilt {
			return report, errors.New("restored archive rebuild was aborted")
		}
	}
	return report, nil
}

func rejectPathsOverlap(a, b, message string) error {
	overlap, err := artifact.PathsOverlap(a, b)
	if err != nil {
		return err
	}
	if overlap {
		return errors.New(message)
	}
	return nil
}

// Resolve the existing parent before creating a new destination.
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
