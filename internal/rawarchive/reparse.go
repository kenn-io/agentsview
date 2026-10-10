package rawarchive

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
)

// ReparseOptions selects accepted sources. Empty selection is an
// error; All must be explicit. ScratchBytes bounds one source materialization.
type ReparseOptions struct {
	ManifestIDs  []string
	All          bool
	ScratchBytes int64
	// BlockedResultCategories is the receiving archive's tool-result retention
	// filter. Reparse applies it exactly as ordinary sync does.
	BlockedResultCategories []string
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
	ownerData, err := os.ReadFile(filepath.Join(a.dataDir, "telemetry-install-id"))
	if err != nil {
		return report, err
	}
	if err := validateRecoveryIdentity(ownerData); err != nil {
		return report, err
	}
	owner := strings.TrimSpace(string(ownerData))
	roots, err := a.roots(ctx)
	if err != nil {
		return report, err
	}
	var selected []db.RawArchiveSource
	if opts.All {
		err = a.sourcePages(ctx, func(source db.RawArchiveSource) error {
			selected = append(selected, source)
			return nil
		})
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
			selected = append(selected, source)
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
	scratch, err := db.OpenIsolatedWithArchiveContent(ctx, scratchPath, a.database.ArchiveContent())
	if err != nil {
		return report, err
	}
	defer func() { retErr = errors.Join(retErr, scratch.Close()) }()
	scratch.SetToolResultImages(a.database.ToolResultImages())
	scratch.SetAssetsDir(a.database.AssetsDir())
	suppressed := make(map[string]bool)
	policies := make(map[string]map[string]db.RawArchiveSuppression)
	for i, source := range selected {
		device := roots[source.RootID].DeviceID
		policy, ok := policies[device]
		if !ok {
			deletions, err := scratch.RawArchiveSuppressions(ctx, device)
			if err != nil {
				return report, err
			}
			policy = make(map[string]db.RawArchiveSuppression, len(deletions))
			for _, d := range deletions {
				policy[d.ParserID] = d
			}
			policies[device] = policy
		}
		a.report(fmt.Sprintf("Reparsing source %d of %d", i+1, len(selected)))
		err = a.reparseSource(ctx, scratch, scratchDir, source, roots, owner, policy, suppressed, opts)
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
		report.Suppressed = len(suppressed)
	}
	return report, err
}

func (a *Archive) reparseSource(ctx context.Context, scratch *db.DB, scratchDir string, source db.RawArchiveSource, roots map[string]db.RawArchiveRoot, owner string, policy map[string]db.RawArchiveSuppression, suppressed map[string]bool, opts ReparseOptions) (retErr error) {
	manifest, err := a.canonical(ctx, source, roots)
	if err != nil {
		return err
	}
	if manifest.Manifest.Kind != rawsync.ManifestSnapshot {
		return errors.New("only retained snapshots can be reparsed")
	}
	materialized, err := (rawderive.Materializer{Store: a.objects, BaseDir: scratchDir, MaxTotalBytes: opts.ScratchBytes}).Materialize(ctx, manifest)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, materialized.Cleanup()) }()
	root := roots[source.RootID]
	storedPath := source.OriginalPath
	if root.DeviceID != owner {
		storedPath = "archive://" + root.ID + "/" + url.PathEscape(source.SourceKey)
	}
	prepared, err := rawderive.PrepareLocalSource(ctx, manifest, materialized, root.DeviceID, storedPath)
	if err != nil {
		return err
	}
	if root.DeviceID != owner {
		prepared.Config.IDPrefix = root.DeviceID + "~"
	}
	prepared.Config.BlockedResultCategories = opts.BlockedResultCategories
	aliases, err := scratch.GetMachineAliases(ctx)
	if err != nil {
		return err
	}
	// A seeded session may store a verified equivalent spelling of its source,
	// such as a path through a symlinked ancestor.
	sourcePaths := prepared.SourcePaths
	if root.DeviceID == owner {
		rootAliases, err := scratch.RawArchiveRootAliases(ctx, root.ID)
		if err != nil {
			return err
		}
		sourcePaths = equivalentSourcePaths(prepared.SourcePaths, root.OriginalPath, rootAliases)
	}
	// The owner may have stored a session under an older machine key that
	// now resolves to this installation.
	ownedBy := func(machine string) bool {
		return machine == root.DeviceID ||
			(root.DeviceID == owner && (machine == "" || machine == "local" || aliases[machine] == owner))
	}
	active := make(map[string]bool)
	for _, path := range sourcePaths {
		ids, err := scratch.ListSessionIDsByFilePath(ctx, path, root.Provider)
		if err != nil {
			return err
		}
		for _, id := range ids {
			session, err := scratch.GetSessionFull(ctx, id)
			if err != nil {
				return err
			}
			if session != nil && ownedBy(session.Machine) {
				active[id] = true
			}
		}
	}
	seen := make(map[string]bool)
	var policyError error
	prepared.Config.ArchiveSessionPolicy = func(ctx context.Context, s *db.Session, native string) (keep bool, retErr error) {
		defer func() { policyError = errors.Join(policyError, retErr) }()
		if strings.Contains(native, "~") {
			return false, errors.New("parser returned a transport-qualified session identity")
		}
		known, err := scratch.BindRawArchiveSession(ctx, root, source.SourceKey, native, s.ID)
		if err != nil {
			return false, err
		}
		existing, err := scratch.GetSessionFull(ctx, s.ID)
		if err != nil {
			return false, err
		}
		if existing != nil {
			// A joined continuation may have been stored under either verified
			// transcript on the source machine. Keep that spelling and identity.
			if root.DeviceID == owner && existing.FilePath != nil && slices.Contains(sourcePaths, *existing.FilePath) {
				s.FilePath = existing.FilePath
			}
			if (!known && root.DeviceID != owner) || existing.Agent != root.Provider || !ownedBy(existing.Machine) || existing.FilePath == nil || s.FilePath == nil || *existing.FilePath != *s.FilePath {
				return false, fmt.Errorf("session identity conflicts with a different source: %s", s.ID)
			}
		}
		s.Machine = root.DeviceID
		seen[s.ID] = true
		if d, ok := policy[native]; ok && (d.Provider == "" || d.Provider == root.Provider) {
			suppressed[s.ID] = true
			return false, nil
		}
		if root.DeviceID == owner && root.Provider == "claude" {
			recorded, err := scratch.GetClaudeSubagentSources(ctx, s.ID)
			if err != nil {
				return false, err
			}
			for _, path := range recorded {
				if !slices.Contains(sourcePaths, path) {
					return false, fmt.Errorf("missing recorded Claude continuation for %s", s.ID)
				}
			}
		}
		return true, nil
	}
	// The parser can intentionally drop a source's only session, such as a
	// content-free usage probe. Sync accepts that, so reparse does too.
	excluded := make(map[string]bool)
	prepared.Config.ParserExclusionPolicy = func(ctx context.Context, ids []string) (retErr error) {
		defer func() { policyError = errors.Join(policyError, retErr) }()
		for _, id := range ids {
			existing, err := scratch.GetSessionFull(ctx, id)
			if err != nil {
				return err
			}
			if existing != nil && (existing.Agent != root.Provider || !ownedBy(existing.Machine) || existing.FilePath == nil || !slices.Contains(sourcePaths, *existing.FilePath)) {
				return fmt.Errorf("parser exclusion conflicts with a different source: %s", id)
			}
			if active[id] {
				return fmt.Errorf("provider excluded an existing active session: %s", id)
			}
			native := strings.TrimPrefix(id, prepared.Config.IDPrefix)
			if strings.Contains(native, "~") {
				return errors.New("parser excluded a transport-qualified session identity")
			}
			if _, err := scratch.BindRawArchiveSession(ctx, root, source.SourceKey, native, id); err != nil {
				return err
			}
			excluded[id] = true
		}
		return nil
	}
	engine := syncer.NewEngine(ctx, scratch, prepared.Config)
	defer engine.Close()
	if err := engine.ReparsePathsContext(ctx, []string{prepared.Path}); err != nil || policyError != nil {
		return errors.Join(err, policyError)
	}
	// Sync keeps a trashed row in trash without parsing it again, so the
	// policy above never sees it. Count trash that this source owns as an
	// intentional suppression instead of a missing result.
	trashed, err := scratch.TrashedSessionsByFilePath(ctx, root.Provider, sourcePaths)
	if err != nil {
		return err
	}
	for id, machine := range trashed {
		if ownedBy(machine) {
			suppressed[id] = true
			seen[id] = true
		}
	}
	for id := range excluded {
		if !seen[id] {
			suppressed[id] = true
			seen[id] = true
		}
	}
	for id := range active {
		if !seen[id] || suppressed[id] {
			return fmt.Errorf("provider did not retain existing active session %s", id)
		}
	}
	if len(seen) == 0 {
		return errors.New("provider produced no archived sessions")
	}
	for id := range seen {
		if suppressed[id] {
			continue
		}
		session, err := scratch.GetSessionFull(ctx, id)
		if err != nil {
			return err
		}
		if session == nil {
			var excluded bool
			if err := scratch.Reader().QueryRow(ctx,
				"SELECT EXISTS(SELECT 1 FROM excluded_sessions WHERE id = ?)", id,
			).Scan(&excluded); err != nil {
				return fmt.Errorf("checking receiving archive deletion for %s: %w", id, err)
			}
			if excluded {
				suppressed[id] = true
				continue
			}
			return fmt.Errorf("provider did not publish reparsed session %s", id)
		}
	}
	return nil
}

// equivalentSourcePaths adds each recorded alias spelling of paths beneath the
// root's original path. Paths keep the source machine's separator.
func equivalentSourcePaths(paths []string, original string, aliases []string) []string {
	out := slices.Clone(paths)
	for _, path := range paths {
		rest, ok := strings.CutPrefix(path, original)
		if !ok || rest == "" || (rest[0] != '/' && rest[0] != '\\') {
			continue
		}
		for _, alias := range aliases {
			if spelled := alias + rest; !slices.Contains(out, spelled) {
				out = append(out, spelled)
			}
		}
	}
	return out
}
