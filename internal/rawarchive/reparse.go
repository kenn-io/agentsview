package rawarchive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
)

// ReparseOptions selects accepted current generations. Empty selection is an
// error; All must be explicit. ScratchBytes bounds one source materialization.
type ReparseOptions struct {
	ManifestIDs  []string
	All          bool
	ScratchBytes int64
}

// Reparse builds one full archive copy for the selected batch. A failed parse
// discards that copy; publication reuses the existing checked database swap.
func (a *Archive) Reparse(ctx context.Context, opts ReparseOptions) (report Report, retErr error) {
	if opts.All == (len(opts.ManifestIDs) > 0) {
		return report, errors.New("select manifest IDs or explicitly select all current sources")
	}
	if opts.ScratchBytes <= 0 {
		return report, errors.New("a positive scratch byte budget is required")
	}
	roots, err := a.roots(ctx)
	if err != nil {
		return report, err
	}
	var selected []db.RawArchiveSource
	choose := func(source db.RawArchiveSource) error {
		head, err := a.database.RawArchiveHead(ctx, source.RootID, source.SourceKey)
		if err != nil {
			return err
		}
		if head == nil || head.ManifestID != source.ManifestID {
			if opts.All {
				return nil
			}
			return errors.New("selected manifest is not the accepted source head")
		}
		selected = append(selected, source)
		return nil
	}
	if opts.All {
		err = a.sourcePages(ctx, choose)
	} else {
		seen := map[string]bool{}
		for _, id := range opts.ManifestIDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			source, getErr := a.database.GetRawArchiveSource(ctx, id)
			if getErr != nil {
				return report, getErr
			}
			if err = choose(source); err != nil {
				break
			}
		}
	}
	if err != nil {
		return report, err
	}
	if len(selected) == 0 {
		return report, errors.New("no accepted sources selected")
	}
	parent := filepath.Dir(a.database.Path())
	scratchDir, err := os.MkdirTemp(parent, ".archive-reparse-")
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(scratchDir)) }()
	scratchPath := filepath.Join(scratchDir, "sessions.db")
	a.report("Copying SQLite once for atomic reparse publication")
	if err := a.database.SnapshotTo(ctx, scratchPath); err != nil {
		return report, err
	}
	scratch, err := db.OpenIsolatedContext(ctx, scratchPath)
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, scratch.Close()) }()
	scratch.SetArchiveContent(a.database.ArchiveContent())
	scratch.SetToolResultImages(a.database.ToolResultImages())
	scratch.SetAssetsDir(a.database.AssetsDir())
	batchOwners := make(map[string]string)
	for i, source := range selected {
		a.report(fmt.Sprintf("Reparsing source %d of %d", i+1, len(selected)))
		err = a.reparseSource(ctx, scratch, scratchDir, source, roots, batchOwners, opts.ScratchBytes)
		if err != nil {
			recordErr := a.database.RecordRawArchiveParse(context.WithoutCancel(ctx), source.ManifestID, strconv.Itoa(db.CurrentDataVersion()), err.Error())
			return report, errors.Join(err, recordErr)
		}
		// The cloned database holds the parse record in the same replacement as
		// its normalized content. Acceptance in the original remains unchanged.
		if err := scratch.RecordRawArchiveParse(ctx, source.ManifestID, strconv.Itoa(db.CurrentDataVersion()), ""); err != nil {
			return report, err
		}
	}
	if err := scratch.CopySessionMetadataFrom(a.database.Path()); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	if err := scratch.CheckpointWALTruncate(ctx); err != nil {
		return report, err
	}
	if err := scratch.Close(); err != nil {
		return report, err
	}
	engine := syncer.NewEngine(ctx, a.database, syncer.EngineConfig{Ephemeral: true})
	defer engine.Close()
	a.report("Installing reparsed archive")
	installed, err := engine.SwapResyncDatabase(scratchPath)
	if installed {
		report.Parsed = len(selected)
	}
	return report, err
}

func (a *Archive) reparseSource(ctx context.Context, scratch *db.DB, scratchDir string, source db.RawArchiveSource, roots map[string]db.RawArchiveRoot, batchOwners map[string]string, budget int64) (retErr error) {
	manifest, err := a.canonical(ctx, source, roots)
	if err != nil {
		return err
	}
	if manifest.Manifest.Kind != rawsync.ManifestSnapshot {
		return errors.New("only retained snapshots can be reparsed")
	}
	materialized, err := (rawderive.Materializer{Store: a.objects, BaseDir: scratchDir, MaxTotalBytes: budget}).Materialize(ctx, manifest)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, materialized.Cleanup()) }()
	prepared, err := rawderive.PrepareLocalSource(ctx, manifest, materialized, roots[source.RootID].Machine, source.OriginalPath)
	if err != nil {
		return err
	}
	engine := syncer.NewEngine(ctx, scratch, prepared.Config)
	defer engine.Close()
	if err := engine.ReparsePathsContext(ctx, []string{prepared.Path}); err != nil {
		return err
	}
	if engine.LastSyncStats().Synced == 0 {
		return errors.New("provider did not publish any reparsed sessions")
	}
	rows, err := scratch.Reader().Query(ctx, "SELECT id FROM sessions WHERE file_path = ?", source.OriginalPath)
	if err != nil {
		return err
	}
	defer rows.Close()
	var count int
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		count++
		if owner, ok := batchOwners[id]; ok && owner != source.OriginalPath {
			return fmt.Errorf("session identity conflicts with another selected source: %s", id)
		}
		batchOwners[id] = source.OriginalPath
		existing, err := a.database.GetSessionFull(ctx, id)
		if err != nil {
			return err
		}
		if existing != nil && (existing.FilePath == nil || *existing.FilePath != source.OriginalPath) {
			return fmt.Errorf("session identity conflicts with a different source: %s", id)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		return errors.New("provider produced no archived sessions")
	}
	return nil
}
