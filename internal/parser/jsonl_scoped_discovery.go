package parser

import (
	"cmp"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// DiscoverScopesEach traverses requested proof paths while retaining configured
// roots in every source identity. Unrequested siblings are not inspected.
func (s JSONLSourceSet) DiscoverScopesEach(
	ctx context.Context, scopes []ReconciliationScope, yield func(SourceRef) error,
) error {
	return s.discoverEach(ctx, func(root string) []string {
		return s.scopedDiscoveryPaths(root, scopes)
	}, yield)
}

func (s JSONLSourceSet) discoverEach(
	ctx context.Context, pathsForRoot func(string) []string, yield func(SourceRef) error,
) error {
	var incomplete error
	for _, root := range s.roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		paths := pathsForRoot(root)
		if len(paths) == 0 {
			continue
		}
		if s.options.PrepareDiscovery != nil {
			preparation := JSONLDiscoveryPreparation{Root: root, FullRoot: slices.ContainsFunc(paths, func(path string) bool {
				return samePath(root, path)
			})}
			if err := s.options.PrepareDiscovery(ctx, preparation); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				incomplete = errors.Join(incomplete, err)
				continue
			}
		}
		var covered []string
		for _, path := range paths {
			if slices.ContainsFunc(covered, func(previous string) bool {
				return samePath(previous, path) || pathUnderRoot(previous, path)
			}) {
				continue
			}
			covered = append(covered, path)
			err := s.discoverPathEach(ctx, root, path, yield)
			if cause, ok := discoveryYieldCause(err); ok {
				return cause
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			incomplete = errors.Join(incomplete, err)
		}
	}
	return incomplete
}

func (s JSONLSourceSet) scopedDiscoveryPaths(root string, scopes []ReconciliationScope) []string {
	var paths []string
	for _, scope := range scopes {
		if !slices.ContainsFunc(scope.TraversalRoots, func(candidate string) bool {
			return samePath(candidate, root)
		}) {
			continue
		}
		for _, proof := range scope.PhysicalProofScopes {
			path := filepath.Clean(proof.Path)
			if samePath(root, path) || s.pathAllowedByRoot(root, path) {
				paths = append(paths, path)
			}
		}
	}
	slices.SortFunc(paths, func(left, right string) int { return cmp.Compare(len(left), len(right)) })
	return paths
}

func (s JSONLSourceSet) discoverPathEach(
	ctx context.Context, root, path string, yield func(SourceRef) error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.sourcePathAllowedByDescendPath(root, path) {
		return nil
	}
	info, err := s.scopedPathInfo(ctx, root, path)
	if os.IsNotExist(err) {
		scope, unavailable := jsonlUnavailableAncestor(root, path)
		if !unavailable {
			return nil
		}
		return incompleteDiscoveryScope(s.provider, scope, "stat JSONL path "+path, err)
	}
	if err != nil {
		return incompleteDiscoveryScope(s.provider,
			DiscoveryScope{Root: root, Path: path, Subtree: true}, "stat JSONL path "+path, err)
	}
	if info == nil {
		return nil
	}
	if info.IsDir() {
		if !samePath(root, path) && (!s.options.Recursive || !s.descendPathIncluded(root, path)) {
			return nil
		}
		return s.discoverDirEach(ctx, root, path, yield)
	}
	if samePath(root, path) {
		return incompleteDiscoveryScope(s.provider,
			DiscoveryScope{Root: root, Path: path, Subtree: true},
			"stat JSONL root "+root, errors.New("not a directory"))
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	source, ok := s.sourceRef(root, path, info)
	if !ok {
		return nil
	}
	if err := yield(source); err != nil {
		return discoveryYieldError{cause: err}
	}
	return nil
}

func (s JSONLSourceSet) scopedPathInfo(ctx context.Context, root, path string) (os.FileInfo, error) {
	if samePath(root, path) {
		return os.Stat(path)
	}
	if !s.options.FollowSymlinkDirs {
		for parent := filepath.Dir(path); !samePath(root, parent) && pathUnderRoot(root, parent); parent = filepath.Dir(parent) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			info, err := os.Lstat(parent)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return nil, nil
			}
		}
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return info, err
	}
	if !s.options.FollowSymlinkDirs && (!s.options.FollowSymlinkFiles || !s.matchesExtension(path)) {
		return nil, nil
	}
	info, err = os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() && !s.options.FollowSymlinkDirs || !info.IsDir() && !s.options.FollowSymlinkFiles {
		return nil, nil
	}
	return info, nil
}

func jsonlUnavailableAncestor(root, path string) (DiscoveryScope, bool) {
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				if _, err := os.Stat(current); err != nil {
					return DiscoveryScope{Root: root, Path: current, Subtree: true}, true
				}
			}
			return DiscoveryScope{}, false
		}
		if !os.IsNotExist(err) {
			return DiscoveryScope{Root: root, Path: current, Subtree: true}, true
		}
		if samePath(current, root) || filepath.Dir(current) == current {
			return DiscoveryScope{}, false
		}
	}
}
