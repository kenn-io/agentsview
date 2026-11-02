package sync

import (
	"context"
	"encoding/json/v2"
	"log"
	"maps"
	"slices"
	"time"

	gitlog "go.kenn.io/agentsview/internal/db/git"
)

type trailerScanState struct {
	Fingerprint string   `json:"fingerprint"`
	Tips        []string `json:"tips"`
}

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
		identity := e.cachedProjectIdentity(r.Machine, r.Cwd)
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

func (e *Engine) reconcileRepositoryTrailers(ctx context.Context, repo, path string) error {
	fingerprint, tips, err := gitlog.RefsFingerprint(ctx, path)
	if err != nil || fingerprint == "" {
		return err
	}
	key := "commit_trailers:" + repo
	value, err := e.db.GetSyncState(ctx, key)
	if err != nil {
		return err
	}
	var previous trailerScanState
	if value != "" {
		if err := json.Unmarshal([]byte(value), &previous); err != nil {
			return err
		}
	}
	if fingerprint == previous.Fingerprint {
		return nil
	}
	commits, err := gitlog.ScanCommitTrailers(ctx, path, previous.Tips)
	if err != nil {
		return err
	}
	if err := e.db.StoreTrailerCommitLinks(ctx, repo, commits); err != nil {
		return err
	}
	state, err := json.Marshal(trailerScanState{Fingerprint: fingerprint, Tips: tips})
	if err != nil {
		return err
	}
	return e.db.SetSyncState(ctx, key, string(state))
}
