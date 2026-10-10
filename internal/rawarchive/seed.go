package rawarchive

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/kit/atomicfile"
)

// Seed assembles a new archive from one verified capture. The complete captured
// database owns its existing sessions and curation; import only adds raw custody.
func Seed(ctx context.Context, capturePath, target string, progress func(string)) (report Report, retErr error) {
	spec, err := loadCaptureSpec(ctx, capturePath)
	if err != nil {
		return report, err
	}
	report.CaptureID, report.Preflight = spec.capture.CaptureID, &spec.capture.Preflight
	if err := spec.capture.Preflight.importError(true); err != nil {
		return report, err
	}
	if spec.capture.OrdinaryVault != "absent" {
		return report, errors.New("seeding an ordinary artifact vault requires stopped-vault ownership support")
	}
	source, err := canonicalRecoverySource(filepath.Dir(capturePath))
	if err != nil {
		return report, err
	}
	destination, err := recoveryDestination(target)
	if err != nil {
		return report, err
	}
	if err := rejectPathsOverlap(source, destination, "seed destination must be outside the capture"); err != nil {
		return report, err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return report, err
		}
		return report, errors.New("seed import requires a new data directory")
	}
	staging, err := os.MkdirTemp(filepath.Dir(destination), ".archive-seed-")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(staging)) }()
	if progress != nil {
		progress("Assembling captured database and assets")
	}
	application, err := os.OpenRoot(filepath.Join(source, "roots", "application"))
	if err != nil {
		return report, err
	}
	defer application.Close()
	for _, file := range spec.inventory {
		if file.RootID != "application" || !recoveryComponent(file.Path) {
			continue
		}
		if err := copySeedFile(ctx, application, staging, file); err != nil {
			return report, err
		}
	}
	settingsFile, err := os.Open(filepath.Join(staging, "recovery-settings.json"))
	if err != nil {
		return report, err
	}
	var settings RecoverySettings
	decodeErr := json.UnmarshalRead(io.LimitReader(settingsFile, 32<<20), &settings, json.RejectUnknownMembers(true))
	if err := errors.Join(decodeErr, settingsFile.Close(), settings.validate()); err != nil {
		return report, err
	}
	identity, err := os.ReadFile(filepath.Join(staging, "telemetry-install-id"))
	if err != nil {
		return report, err
	}
	if strings.TrimSpace(string(identity)) != spec.DeviceID || settings.LocalMachineName != spec.Machine {
		return report, errors.New("seed application identity differs from capture source")
	}
	if err := writeRecoveryConfig(staging, settings); err != nil {
		return report, err
	}
	if report, err = importSeed(ctx, staging, spec, settings, progress); err != nil {
		return report, err
	}
	if _, err := verifyRestoredArchive(ctx, staging, settings, progress); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := atomicfile.RenameNoReplace(staging, destination); err != nil {
		return report, err
	}
	return report, nil
}

// The package was checked before staging; verify each copied stream again before
// opening its database or publishing it as application state.
func copySeedFile(ctx context.Context, source *os.Root, target string, file CaptureFile) error {
	input, err := source.Open(filepath.FromSlash(file.Path))
	if err != nil {
		return err
	}
	defer input.Close()
	path := filepath.Join(target, filepath.FromSlash(file.Path))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ref, copyErr := hashReader(ctx, io.TeeReader(input, output))
	if err := errors.Join(copyErr, output.Close()); err != nil {
		return err
	}
	if ref.SHA256 != file.SHA256 || ref.Length != file.Size {
		return fmt.Errorf("seed file changed after inventory verification: %s", file.Path)
	}
	return nil
}

func importSeed(ctx context.Context, path string, spec ImportSpec, settings RecoverySettings, progress func(string)) (report Report, retErr error) {
	database, err := db.OpenIsolatedWithArchiveContent(ctx, filepath.Join(path, "sessions.db"), settings.ArchiveContent)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, database.Close()) }()
	if err := database.EnableArchiveOnly(ctx); err != nil {
		return report, err
	}
	database.SetToolResultImages(settings.ToolResultImages)
	owner, err := database.GetSyncState(ctx, "artifact_local_installation_id")
	if err != nil {
		return report, err
	}
	if owner != "" && owner != spec.DeviceID {
		return report, errors.New("seed database owner differs from captured installation")
	}
	roots, err := database.ListRawArchiveRoots(ctx)
	if err != nil {
		return report, err
	}
	if len(roots) > 0 {
		return report, errors.New("seed database already contains raw archive state; use archive backup")
	}
	archive, err := Open(ctx, database, path, progress)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, archive.Close()) }()
	if report, err = archive.Import(ctx, spec); err != nil {
		return report, err
	}
	if len(report.Gaps) > 0 {
		return report, errors.New("seed capture has provider coverage gaps; see report")
	}
	candidates, err := database.ListMachineIdentityCandidates(ctx)
	if err != nil {
		return report, err
	}
	aliases, err := database.GetMachineAliases(ctx)
	if err != nil {
		return report, err
	}
	for _, candidate := range candidates {
		if candidate.Machine != "" && candidate.Machine != "local" &&
			candidate.Machine != spec.DeviceID && aliases[candidate.Machine] != spec.DeviceID {
			report.UnownedMachines = append(report.UnownedMachines, candidate)
		}
	}
	return report, nil
}
