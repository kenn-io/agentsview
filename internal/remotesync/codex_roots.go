package remotesync

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/kit/atomicfile"
)

type mirrorCodexRoots struct {
	Version int                       `json:"version"`
	Dirs    []string                  `json:"dirs"`
	Homes   map[string]CodexHomeRoots `json:"homes,omitempty"`
}

func mirrorCodexRootsPath(root string) string { return filepath.Clean(root) + ".codex-roots.json" }

// LoadMirrorCodexRoots reads the last successfully imported advertised scopes.
func LoadMirrorCodexRoots(ctx context.Context, root string) ([]string, error) {
	saved, err := LoadMirrorCodexMetadata(ctx, root)
	return saved.Dirs, err
}

// MirrorCodexMetadata is the last successfully imported host scope.
type MirrorCodexMetadata struct {
	Dirs  []string
	Homes map[string]CodexHomeRoots
}

// LoadMirrorCodexMetadata reads the versioned mirror scope under its lock.
func LoadMirrorCodexMetadata(ctx context.Context, root string) (MirrorCodexMetadata, error) {
	if err := ctx.Err(); err != nil {
		return MirrorCodexMetadata{}, err
	}
	data, err := os.ReadFile(mirrorCodexRootsPath(root))
	if err != nil {
		return MirrorCodexMetadata{}, err
	}
	var saved mirrorCodexRoots
	if err := json.Unmarshal(data, &saved); err != nil {
		return MirrorCodexMetadata{}, fmt.Errorf("invalid Codex mirror roots: %w", err)
	}
	if saved.Version != 1 && saved.Version != 2 {
		return MirrorCodexMetadata{}, errors.New("unsupported Codex mirror roots version")
	}
	if saved.Version == 1 {
		saved.Homes = nil
	}
	if err := ctx.Err(); err != nil {
		return MirrorCodexMetadata{}, err
	}
	return MirrorCodexMetadata{Dirs: saved.Dirs, Homes: saved.Homes}, nil
}

func (p *PreparedRemote) persistCodexRoots(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := mirrorCodexRootsPath(p.root)
	previous, err := os.ReadFile(name)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var saved mirrorCodexRoots
	if err == nil && json.Unmarshal(previous, &saved) == nil && saved.Version > 2 {
		return nil
	}
	dirs := p.targets.Dirs[parser.AgentCodex]
	if len(dirs) == 0 && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	data, err := json.Marshal(mirrorCodexRoots{Version: 2, Dirs: dirs, Homes: p.targets.CodexHomeRoots})
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if bytes.Equal(data, previous) {
		return nil
	}
	return atomicfile.WriteFile(name, data, atomicfile.WithPrivate())
}
