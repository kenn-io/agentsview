package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// GCOptions configures conservative artifact garbage collection.
type GCOptions struct {
	Root   string
	Grace  time.Duration
	DryRun bool
	Now    time.Time
	Logf   func(string, ...any)
}

// GCResult summarizes one artifact garbage collection scan.
type GCResult struct {
	DryRun         bool
	Origins        int
	SkippedOrigins int
	Scanned        int
	Candidates     int
	Eligible       int
	KeptByGrace    int
	Deleted        int
	BytesEligible  int64
	BytesDeleted   int64
}

type gcRef struct {
	kind string
	name string
}

type gcCandidate struct {
	path    string
	origin  string
	kind    string
	name    string
	size    int64
	modTime time.Time
}

type gcOriginResult struct {
	skipped    bool
	scanned    int
	candidates []gcCandidate
}

// GarbageCollect deletes or reports superseded artifacts that are no longer
// reachable from an origin's latest checkpoint and its live manifests.
func GarbageCollect(ctx context.Context, opts GCOptions) (GCResult, error) {
	if opts.Root == "" {
		return GCResult{}, fmt.Errorf("artifact gc root is required")
	}
	if opts.Grace < 0 {
		return GCResult{}, fmt.Errorf("artifact gc grace must be >= 0")
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cutoff := now.Add(-opts.Grace)

	origins, err := ListOrigins(opts.Root)
	if err != nil {
		return GCResult{}, fmt.Errorf("listing artifact origins: %w", err)
	}
	res := GCResult{DryRun: opts.DryRun}
	for _, origin := range origins {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		originRes, err := collectGCCandidates(ctx, opts.Root, origin)
		if err != nil {
			return res, fmt.Errorf("scanning %s: %w", origin, err)
		}
		res.Origins++
		if originRes.skipped {
			res.SkippedOrigins++
			logGC(opts, "artifact gc: skipping %s with no checkpoints", origin)
			continue
		}
		res.Scanned += originRes.scanned
		for _, cand := range originRes.candidates {
			res.Candidates++
			if cand.modTime.After(cutoff) {
				res.KeptByGrace++
				continue
			}
			res.Eligible++
			res.BytesEligible += cand.size
			if opts.DryRun {
				logGC(opts, "artifact gc: would delete %s (%d bytes)", cand.path, cand.size)
				continue
			}
			if err := os.Remove(cand.path); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return res, fmt.Errorf("deleting %s: %w", cand.path, err)
			}
			res.Deleted++
			res.BytesDeleted += cand.size
			logGC(opts, "artifact gc: deleted %s (%d bytes)", cand.path, cand.size)
		}
	}
	return res, nil
}

func collectGCCandidates(ctx context.Context, root, origin string) (gcOriginResult, error) {
	originRoot := filepath.Join(root, origin)
	checkpoints, err := validCheckpointPaths(originRoot)
	if err != nil {
		return gcOriginResult{}, err
	}
	if len(checkpoints) == 0 {
		return gcOriginResult{skipped: true}, nil
	}

	latestPath := checkpoints[len(checkpoints)-1]
	data, err := os.ReadFile(latestPath)
	if err != nil {
		return gcOriginResult{}, fmt.Errorf("reading live checkpoint: %w", err)
	}
	var cp checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return gcOriginResult{}, fmt.Errorf("decoding live checkpoint: %w", err)
	}
	if err := validateCheckpoint(&cp, origin); err != nil {
		return gcOriginResult{}, err
	}
	live := map[gcRef]struct{}{
		{kind: KindCheckpoints, name: filepath.Base(latestPath)}: {},
	}

	gids := make([]string, 0, len(cp.Sessions))
	for gid := range cp.Sessions {
		gids = append(gids, gid)
	}
	sort.Strings(gids)
	for _, gid := range gids {
		if err := ctx.Err(); err != nil {
			return gcOriginResult{}, err
		}
		manifestHash := cp.Sessions[gid]
		if err := validateHashHex(manifestHash); err != nil {
			return gcOriginResult{}, fmt.Errorf("live manifest %s: %w", gid, err)
		}
		manifestName := manifestHash + manifestExtension
		live[gcRef{kind: KindManifests, name: manifestName}] = struct{}{}

		m, err := readManifest(originRoot, manifestHash)
		if err != nil {
			return gcOriginResult{}, fmt.Errorf("reading live manifest %s: %w", manifestHash, err)
		}
		if err := validateManifest(m, origin, gid); err != nil {
			return gcOriginResult{}, err
		}
		for _, segmentHash := range m.Segments {
			if err := validateHashHex(segmentHash); err != nil {
				return gcOriginResult{}, fmt.Errorf("live segment %s: %w", gid, err)
			}
			live[gcRef{kind: KindSegments, name: segmentHash + segmentExtension}] = struct{}{}
		}
		if m.RawSource != nil && m.RawSource.Hash != "" {
			if err := validateHashHex(m.RawSource.Hash); err != nil {
				return gcOriginResult{}, fmt.Errorf("live raw source %s: %w", gid, err)
			}
			live[gcRef{kind: KindRaw, name: m.RawSource.Hash}] = struct{}{}
		}
	}

	candidates, scanned, err := scanGCCandidates(ctx, originRoot, origin, live)
	if err != nil {
		return gcOriginResult{}, err
	}
	return gcOriginResult{scanned: scanned, candidates: candidates}, nil
}

func validCheckpointPaths(originRoot string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(originRoot, KindCheckpoints, "cp-*.json"))
	if err != nil {
		return nil, err
	}
	valid := paths[:0]
	for _, path := range paths {
		if _, err := checkpointSequence(filepath.Base(path)); err == nil {
			valid = append(valid, path)
		}
	}
	sort.Strings(valid)
	return valid, nil
}

func scanGCCandidates(
	ctx context.Context,
	originRoot string,
	origin string,
	live map[gcRef]struct{},
) ([]gcCandidate, int, error) {
	var candidates []gcCandidate
	scanned := 0
	for _, spec := range []struct {
		kind  string
		valid func(string) bool
	}{
		{kind: KindCheckpoints, valid: isGCCheckpointName},
		{kind: KindManifests, valid: isGCManifestName},
		{kind: KindSegments, valid: isGCSegmentName},
		{kind: KindRaw, valid: isGCRawName},
	} {
		dir := filepath.Join(originRoot, spec.kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, scanned, fmt.Errorf("reading %s: %w", dir, err)
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return nil, scanned, err
			}
			if ent.IsDir() || isTempArtifactEntry(ent.Name()) || !spec.valid(ent.Name()) {
				continue
			}
			info, err := ent.Info()
			if err != nil {
				return nil, scanned, fmt.Errorf("stat %s: %w", filepath.Join(dir, ent.Name()), err)
			}
			if !info.Mode().IsRegular() {
				continue
			}
			scanned++
			if _, ok := live[gcRef{kind: spec.kind, name: ent.Name()}]; ok {
				continue
			}
			candidates = append(candidates, gcCandidate{
				path:    filepath.Join(dir, ent.Name()),
				origin:  origin,
				kind:    spec.kind,
				name:    ent.Name(),
				size:    info.Size(),
				modTime: info.ModTime(),
			})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].origin != candidates[j].origin {
			return candidates[i].origin < candidates[j].origin
		}
		if candidates[i].kind != candidates[j].kind {
			return candidates[i].kind < candidates[j].kind
		}
		return candidates[i].name < candidates[j].name
	})
	return candidates, scanned, nil
}

func isGCCheckpointName(name string) bool {
	_, err := checkpointSequence(name)
	return err == nil
}

func isGCManifestName(name string) bool {
	_, _, err := normalizeHashName(name, manifestExtension)
	return err == nil
}

func isGCSegmentName(name string) bool {
	_, _, err := normalizeHashName(name, segmentExtension)
	return err == nil
}

func isGCRawName(name string) bool {
	return validateHashHex(name) == nil
}

func logGC(opts GCOptions, format string, args ...any) {
	if opts.Logf != nil {
		opts.Logf(format, args...)
	}
}
