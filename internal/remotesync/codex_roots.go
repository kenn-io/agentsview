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
	Version int      `json:"version"`
	Dirs    []string `json:"dirs"`
}

func mirrorCodexRootsPath(root string) string { return filepath.Clean(root) + ".codex-roots.json" }

// LoadMirrorCodexRoots reads the last successfully imported advertised scopes.
func LoadMirrorCodexRoots(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(mirrorCodexRootsPath(root))
	if err != nil {
		return nil, err
	}
	var saved mirrorCodexRoots
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, fmt.Errorf("invalid Codex mirror roots: %w", err)
	}
	if saved.Version != 1 {
		return nil, errors.New("unsupported Codex mirror roots version")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return saved.Dirs, nil
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
	if err == nil && json.Unmarshal(previous, &saved) == nil && saved.Version > 1 {
		return nil
	}
	dirs := p.targets.Dirs[parser.AgentCodex]
	if len(dirs) == 0 && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	data, err := json.Marshal(mirrorCodexRoots{Version: 1, Dirs: dirs})
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
