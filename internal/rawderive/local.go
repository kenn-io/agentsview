package rawderive

import (
	"context"
	"errors"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
)

// LocalSource configures the normal SQLite sync engine for one verified source.
// The caller owns the materialization and must keep it open until sync finishes.
type LocalSource struct {
	Path   string
	Config syncer.EngineConfig
}

// PrepareLocalSource shares hosted source validation without collecting parsed
// messages. The local engine can therefore retain its streaming Codex path.
func PrepareLocalSource(
	ctx context.Context, manifest rawsync.CanonicalManifest,
	materialized *Materialization, machine, storedPath string,
) (LocalSource, error) {
	if err := rawsync.ValidateCanonicalManifest(manifest); err != nil {
		return LocalSource{}, err
	}
	if materialized == nil || materialized.Root() == "" || storedPath == "" ||
		manifest.Manifest.Kind != rawsync.ManifestSnapshot {
		return LocalSource{}, errors.New("local reparse requires a materialized snapshot and stored source path")
	}
	dispatch, err := NewProviderParser(parser.ProviderFactories(), machine)
	if err != nil {
		return LocalSource{}, err
	}
	_, source, paths, err := dispatch.prepareSource(
		parser.WithoutFilesystemProjectDiscovery(ctx), manifest, materialized,
	)
	if err != nil {
		return LocalSource{}, err
	}
	paths.bindSource(source, storedPath)
	roots := materializedProviderRoots(manifest, materialized)
	agent := manifest.Manifest.Provider
	return LocalSource{Path: source.DisplayPath, Config: syncer.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: roots},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			agent: materializedProviderMetadataDirs(manifest, materialized, roots),
		},
		Machine: machine, Ephemeral: true, DiscardPendingWritesOnCancel: true,
		DisableFilesystemProjectDiscovery: true, StableSourceSnapshots: true,
		PathRewriter: paths.rewrite, StoredPathResolver: paths.resolve,
	}}, nil
}
