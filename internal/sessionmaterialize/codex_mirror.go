package sessionmaterialize

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"go.kenn.io/agentsview/internal/remotesync"
)

func codexMirrorGroup(ctx context.Context, mirror, anchor, host string) (string, []string, map[string]bool, error) {
	saved, err := remotesync.LoadMirrorCodexMetadata(ctx, mirror)
	if err != nil {
		return "", nil, nil, fmt.Errorf("cannot read Codex source locations for host %s; use a compatible or newer AgentsView and sync the host if locations are missing or outdated: %w", host, err)
	}
	mapped, err := mapCodexMirrorRoots(ctx, mirror, saved.Dirs)
	if err != nil {
		return "", nil, nil, err
	}
	var root string
	for _, candidate := range mapped {
		rel, err := filepath.Rel(candidate, anchor)
		if err == nil && filepath.IsLocal(rel) && (root == "" || len(candidate) < len(root)) {
			root = candidate
		}
	}
	if root == "" {
		return "", nil, nil, fmt.Errorf("recorded source is outside Codex roots advertised at the last sync; sync host %s again", host)
	}
	roots, roles, err := codexMirrorHomeGroup(root, saved, mapped)
	if err != nil {
		return "", nil, nil, err
	}
	return root, roots, roles, nil
}

func codexMirrorHomeGroup(root string, saved remotesync.MirrorCodexMetadata, mapped map[string]string) ([]string, map[string]bool, error) {
	roots, roles := []string{root}, make(map[string]bool)
	var association *remotesync.CodexHomeRoots
	for _, home := range saved.Homes {
		live, archive := mapped[home.Sessions], mapped[home.ArchivedSessions]
		if live != root && archive != root {
			continue
		}
		if live == archive || (association != nil && *association != home) {
			return nil, nil, errors.New("contradictory Codex mirror home roots")
		}
		selectedHome := home
		association = &selectedHome
		for _, pair := range []struct {
			dir      string
			archived bool
		}{{live, false}, {archive, true}} {
			if pair.dir != "" {
				roles[pair.dir] = pair.archived
				if !slices.Contains(roots, pair.dir) {
					roots = append(roots, pair.dir)
				}
			}
		}
	}
	if association == nil && (filepath.Base(root) == "sessions" || filepath.Base(root) == "archived_sessions") {
		other := map[string]string{"sessions": "archived_sessions", "archived_sessions": "sessions"}[filepath.Base(root)]
		sibling := filepath.Join(filepath.Dir(root), other)
		if slices.ContainsFunc(saved.Dirs, func(dir string) bool { return mapped[dir] == sibling }) {
			roots = append(roots, sibling)
		}
	}
	return roots, roles, nil
}

func mapCodexMirrorRoots(ctx context.Context, mirror string, advertised []string) (map[string]string, error) {
	mapped := make(map[string]string, len(advertised))
	for _, remoteRoot := range advertised {
		candidate, err := remotesync.SafeRemappedRemotePath(mirror, remoteRoot)
		if err != nil {
			return nil, fmt.Errorf("invalid advertised Codex root: %w", err)
		}
		if err := validateCodexMirrorRoot(ctx, mirror, candidate); err != nil {
			return nil, err
		}
		mapped[remoteRoot] = candidate
	}
	return mapped, nil
}

// validateCodexMirrorRoot permits absent advertised roots only inside the mirror.
func validateCodexMirrorRoot(ctx context.Context, mirror, candidate string) error {
	for current := candidate; ; current = filepath.Dir(current) {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(mirror, current)
		if err != nil || (rel != "." && !filepath.IsLocal(rel)) {
			return errors.New("advertised Codex root is outside the mirror")
		}
		if _, err := os.Lstat(current); err == nil {
			if !within(mirror, current) {
				return errors.New("advertised Codex root is outside the mirror")
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if rel == "." {
			return errors.New("native Codex mirror root is unavailable or outside the mirror")
		}
	}
}
