package sync

import (
	"context"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"time"

	gitlog "go.kenn.io/agentsview/internal/db/git"
)

// ReconcileCommitTrailers discovers editor commits independently of source writes.
func (e *Engine) ReconcileCommitTrailers(ctx context.Context) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.reconcileCommitTrailersLocked(ctx)
}

func (e *Engine) reconcileCommitTrailersLocked(ctx context.Context) {
	e.reconcileCommitTrailersWithBudget(ctx, 60*time.Second)
}

// A nonpositive budget permits one repository attempt before yielding.
func (e *Engine) reconcileCommitTrailersWithBudget(ctx context.Context, budget time.Duration) {
	if e.ephemeral || e.forceParse {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	start := time.Now()
	repositories, err := e.db.TrailerRepositories(ctx)
	if err != nil {
		log.Printf("trailer repositories: %v", err)
		return
	}
	paths := map[string]string{}
	for _, r := range repositories {
		identity := e.cachedProjectIdentity(r.Machine, e.survivingTrailerCwd(r.Machine, r.Cwd))
		if identity.gitDir == "" {
			continue
		}
		key := identity.repositoryPath
		if key == "" {
			key = identity.rootPath
		}
		paths[key] = identity.rootPath
	}
	if e.trailerScanAttempts == nil {
		e.trailerScanAttempts = make(map[string]time.Time)
	}
	order := slices.Sorted(maps.Keys(paths))
	slices.SortStableFunc(order, func(a, b string) int {
		return e.trailerScanAttempts[a].Compare(e.trailerScanAttempts[b])
	})
	for _, repo := range order {
		if ctx.Err() != nil {
			return
		}
		// Failed scans also yield their place to repositories still waiting.
		e.trailerScanAttempts[repo] = time.Now()
		if err := e.reconcileRepositoryTrailers(ctx, repo, paths[repo]); err != nil {
			log.Printf("commit trailers in %s: %v", repo, err)
		}
		if time.Since(start) >= budget {
			return
		}
	}
}

// survivingTrailerCwd returns the nearest existing ancestor of a session cwd.
// Archived sessions can name temporary subdirectories that were removed, and
// the checkout that contained them still holds the session's commits.
func (e *Engine) survivingTrailerCwd(machine, cwd string) string {
	if !safeLocalAbsolutePath(cwd) {
		return cwd
	}
	for dir := filepath.Clean(cwd); e.mayDiscoverLocalProject(machine, dir); {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return cwd
}

func (e *Engine) reconcileRepositoryTrailers(ctx context.Context, repo, path string) error {
	_, commonDir, _ := gitDirectoryContext(path, e.mayProbeLocalPath)
	if commonDir != "" && !e.mayProbeLocalPath(filepath.Join(commonDir, "config")) {
		return nil
	}
	commits, err := gitlog.ScanCommitTrailers(ctx, path)
	if err != nil {
		return err
	}
	return e.db.StoreTrailerCommitLinks(ctx, repo, commits)
}
