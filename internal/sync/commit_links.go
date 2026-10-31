package sync

import (
	"context"
	"encoding/json/v2"
	"errors"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	dbgit "go.kenn.io/agentsview/internal/db/git"
)

type commitScanState struct {
	Tips         []string `json:"tips"`
	Seen         []string `json:"seen"`
	Offset       int      `json:"offset"`
	Complete     bool     `json:"complete"`
	QueuedTips   []string `json:"queued_tips"`
	QueuedOffset int      `json:"queued_offset"`
}

// BackfillStoredCommitLinks derives a bounded batch without consulting source
// files or Git. Callers serialize it against archive replacement.
func (e *Engine) BackfillStoredCommitLinks(ctx context.Context) (int, error) {
	if e.db == nil || e.refuseWriteInForceParse("commit provenance") {
		return 0, nil
	}
	return e.db.BackfillCommitLinks(ctx, 100)
}

// BackfillCommitProvenance performs database-only extraction followed by bounded,
// offline discovery in eligible local repositories. Reads never invoke this path.
func (e *Engine) BackfillCommitProvenance(ctx context.Context) (int, error) {
	if e.db == nil {
		return 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	n, err := e.BackfillStoredCommitLinks(ctx)
	if err != nil {
		return n, err
	}
	if e.ephemeral || e.db.ArchiveContent() == config.ArchiveContentUsage || e.disableProjectDiscovery || e.idPrefix != "" || e.pathRewriter != nil {
		return n, nil
	}
	e.resolveCommitObservations(ctx)
	after, _ := e.db.GetSyncState(ctx, "commit_links_repo_after")
	paths, err := e.db.CommitRepositoryCandidates(ctx, e.machine, after, 20)
	if err != nil {
		return n, err
	}
	if len(paths) == 0 && after != "" {
		paths, err = e.db.CommitRepositoryCandidates(ctx, e.machine, "", 20)
		if err != nil {
			return n, err
		}
	}
	seen := map[string]bool{}
	for _, path := range paths {
		if ctx.Err() != nil {
			break
		}
		repo := e.localCommitRepository(e.machine, path)
		if repo != "" && !seen[repo] {
			seen[repo] = true
			if err := e.scanCommitRepository(ctx, repo); err != nil && ctx.Err() == nil {
				log.Printf("commit provenance: repository scan unavailable")
			}
		}
		if err := e.db.SetSyncState(ctx, "commit_links_repo_after", path); err != nil {
			return n, err
		}
	}
	return n, ctx.Err()
}

func (e *Engine) localCommitRepository(machine, path string) string {
	if e.disableProjectDiscovery || e.idPrefix != "" || e.pathRewriter != nil || !e.isLocalMachineAttribution(machine) || !safeLocalAbsolutePath(path) || !e.mayProbeLocalPath(path) {
		return ""
	}
	identity := discoverLocalGitIdentity(path, e.mayProbeLocalPath)
	if identity.rootPath == "" || identity.gitDir == "" ||
		identity.commonDir == "" ||
		!e.mayProbeLocalPath(filepath.Join(identity.gitDir, "commondir")) ||
		!e.mayProbeLocalPath(identity.commonDir) {
		return ""
	}
	return identity.rootPath
}

func (e *Engine) resolveCommitObservations(ctx context.Context) {
	after, _ := e.db.GetSyncState(ctx, "commit_links_resolve_after")
	rows, err := e.db.UnresolvedCommitLinksAfter(ctx, after, 100)
	if err != nil {
		return
	}
	if len(rows) == 0 {
		_ = e.db.SetSyncState(ctx, "commit_links_resolve_after", "")
		return
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return
		}
		if err := e.db.SetSyncState(ctx, "commit_links_resolve_after", row.ID); err != nil {
			return
		}
		// An explicit unknown/dynamic target must never fall back to the cwd.
		if row.Link.RepositoryPath == "" || strings.Contains(row.Link.SessionID, "~") {
			continue
		}
		machine, err := e.db.CommitLinkSessionMachine(ctx, row.Link.SessionID)
		if err != nil {
			continue
		}
		repo := e.localCommitRepository(machine, row.Link.RepositoryPath)
		if repo == "" {
			continue
		}
		object, err := dbgit.ReadCommitObject(ctx, repo, row.Link.CommitSHA)
		if err != nil {
			continue
		}
		if err := e.db.ResolveCommitLink(ctx, row.ID, object.SHA, object.At); err != nil && ctx.Err() == nil {
			log.Printf("commit provenance: resolution publication deferred")
		}
	}
}

func (e *Engine) scanCommitRepository(ctx context.Context, repo string) error {
	tips, err := dbgit.SnapshotCommitTips(ctx, repo)
	if err != nil {
		return err
	}
	key := "commit_links_scan:" + dbgit.CacheKey("scan-state-v1", repo, "", "", "")
	raw, err := e.db.GetSyncState(ctx, key)
	if err != nil {
		return err
	}
	state := commitScanState{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return err
		}
	}
	if len(state.Tips) == 0 {
		state.Tips = tips
		state.Offset = 0
		state.Complete = false
	}
	save := func() error {
		encoded, err := json.Marshal(state)
		if err != nil {
			return err
		}
		return e.db.SetSyncState(ctx, key, string(encoded))
	}
	if len(state.QueuedTips) == 0 {
		historicalTipsChanged := !state.Complete && !slices.Equal(state.Tips, tips)
		completedTipsChanged := state.Complete && !slices.Equal(state.Seen, tips)
		if historicalTipsChanged || completedTipsChanged {
			state.QueuedTips = tips
			state.QueuedOffset = 0
		}
	}
	// Historical work goes first. Each successfully published fact advances a
	// durable cursor, so a timeout cannot restart an expensive page forever.
	advance := func(scanTips, excludedTips []string, offset *int, finish func()) error {
		objects, err := e.db.ScanCommitTrailersCached(ctx, repo, scanTips, excludedTips, *offset)
		if err != nil {
			return err
		}
		if len(objects) == 0 {
			finish()
			return save()
		}
		for _, object := range objects[:min(len(objects), 100)] {
			if err := e.storeTrailerObjects(ctx, repo, []dbgit.CommitObject{object}); err != nil {
				return err
			}
			*offset++
			if err := save(); err != nil {
				return err
			}
		}
		return nil
	}
	if !state.Complete {
		if err := advance(state.Tips, nil, &state.Offset, func() {
			state.Complete = true
			state.Seen = slices.Clone(state.Tips)
		}); err != nil {
			if !errors.Is(err, dbgit.ErrCommitScanTipUnavailable) || slices.Equal(state.Tips, tips) {
				return err
			}
			// Retain published facts, but abandon a pruned historical frontier
			// so the current queued tips can scan. Current-tip errors propagate.
			state.Complete = true
			if err := save(); err != nil {
				return err
			}
		}
	}
	if len(state.QueuedTips) > 0 {
		queued := state.QueuedTips
		if err := advance(queued, state.Seen, &state.QueuedOffset, func() { state.Seen = queued; state.QueuedTips = nil; state.QueuedOffset = 0 }); err != nil {
			if !errors.Is(err, dbgit.ErrCommitScanTipUnavailable) || slices.Equal(queued, tips) {
				return err
			}
			// Replace the unavailable frontier with today's reachable tips so
			// later commits can make progress.
			state.QueuedTips = slices.Clone(tips)
			state.QueuedOffset = 0
			if err := save(); err != nil {
				return err
			}
			if len(state.QueuedTips) > 0 {
				queued = state.QueuedTips
				if err := advance(queued, state.Seen, &state.QueuedOffset, func() { state.Seen = queued; state.QueuedTips = nil; state.QueuedOffset = 0 }); err != nil {
					return err
				}
			}
		}
	}
	return save()
}

func (e *Engine) storeTrailerObjects(ctx context.Context, repo string, objects []dbgit.CommitObject) error {
	for _, object := range objects {
		if len(object.Sessions) == 0 {
			continue
		}
		links := make([]db.CommitLink, 0, len(object.Sessions))
		for _, id := range object.Sessions {
			links = append(links, db.CommitLink{SessionID: id, CommitSHA: object.SHA, Resolved: true, Source: "trailer", RepositoryPath: repo, Branch: object.Branch, Subject: object.Subject, CommittedAt: object.At})
		}
		paths, complete := object.Paths, object.PathsComplete
		if !complete {
			paths, complete, _ = dbgit.ReadCommitPaths(ctx, repo, object.SHA)
		}
		if err := e.db.StoreCommitTrailerFacts(ctx, links, paths, complete); err != nil {
			return err
		}
	}
	return nil
}
