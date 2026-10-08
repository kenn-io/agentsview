package rawderive

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
	syncer "go.kenn.io/agentsview/internal/sync"
)

// LocalSource configures the normal SQLite sync engine for one verified source.
// The caller owns the materialization and must keep it open until sync finishes.
type LocalSource struct {
	Path   string
	Config syncer.EngineConfig
	// SourcePaths are the verified original continuation paths. A seeded session
	// may retain any of these spellings without changing its source ownership.
	SourcePaths []string
}

// PrepareLocalSource validates a local capture through provider-owned plans.
// Claude continuation membership is parsed; Codex keeps its streaming sync path.
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
	provider, source, paths, err := dispatch.prepareSource(
		parser.WithoutFilesystemProjectDiscovery(ctx), manifest, materialized, true,
	)
	if err != nil {
		return LocalSource{}, err
	}
	sourcePaths := []string{storedPath}
	// Reconstruct the same verified group used at import. Preserve original
	// member paths for a seed, including forks parsed from companion files.
	discovery, err := parser.DiscoverRawCaptureSources(ctx, provider)
	if err != nil {
		return LocalSource{}, err
	}
	_, members, err := PlanLocalCapture(ctx, provider, source, discovery.Sources)
	if err != nil {
		return LocalSource{}, err
	}
	if len(members) > 1 && storedPath == manifest.Manifest.SourceKey {
		sourcePaths = nil
		clientRoot, separator := clientSourceRoot(manifest.Manifest.SourceKey, manifestPrimaryEntry(manifest))
		if clientRoot == "" {
			return LocalSource{}, errors.New("continuation source has no original root")
		}
		for _, member := range members {
			rel, err := filepath.Rel(materialized.Root(), member.DisplayPath)
			if err != nil || !filepath.IsLocal(rel) {
				return LocalSource{}, errors.New("continuation source is outside materialization")
			}
			original := clientRoot + separator + strings.ReplaceAll(filepath.ToSlash(rel), "/", separator)
			paths.bindSource(member, original)
			sourcePaths = append(sourcePaths, original)
		}
	}
	paths.bindSource(source, storedPath)
	roots := materializedProviderRoots(manifest, materialized)
	agent := manifest.Manifest.Provider
	return LocalSource{Path: source.DisplayPath, SourcePaths: sourcePaths, Config: syncer.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{agent: roots},
		ProviderMetadata: map[parser.AgentType]map[string][]string{
			agent: materializedProviderMetadataDirs(manifest, materialized, roots),
		},
		ArchiveReparse: true,
		Machine:        machine, Ephemeral: true, DiscardPendingWritesOnCancel: true,
		DisableFilesystemProjectDiscovery: true, StableSourceSnapshots: true,
		PathRewriter: paths.rewrite, StoredPathResolver: paths.resolve,
	}}, nil
}
