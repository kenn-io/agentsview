package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

// BoundedCoverageRoot is provider-owned scope metadata supplied by the poll
// coordinator. The provider capability, not the agent label, admits feed mode.
type BoundedCoverageRoot struct {
	Agent parser.AgentType
	Root  string
}

// BoundedCoverageBinding is the immutable coordinator key for one provider
// coverage obligation.
type BoundedCoverageBinding struct {
	Key    string
	Agent  parser.AgentType
	DBPath string
}

// BoundedCoverageScope is the provider-resolved physical scope retained by a
// binding. It prevents retries from reconstructing ownership from all roots.
type BoundedCoverageScope struct {
	Agent parser.AgentType
	Root  string
}

type BoundedCoverageResolver interface {
	BoundedCoverageBindings(context.Context, []BoundedCoverageRoot) ([]BoundedCoverageBinding, error)
	BoundedCoverageBindingsForPaths(context.Context, []string) ([]BoundedCoverageBinding, []string, error)
	DrainBoundedCoverage(context.Context, BoundedCoverageBinding, parser.OpenCodeCoverageCheckpoint) (parser.OpenCodeFeedResult, []parser.SourceRef, error)
	ApplyBoundedCoverageSources(context.Context, []parser.SourceRef) (SyncStats, error)
}

// BoundedCoveragePrimer establishes the journal cursor before a watcher can
// deliver the event that caused admission. It is separate from the resolver
// interface so existing test and auxiliary resolvers remain source compatible.
type BoundedCoveragePrimer interface {
	PrimeBoundedCoverage(context.Context, BoundedCoverageBinding) (parser.OpenCodeCoverageCheckpoint, error)
}

var (
	ErrBoundedCoverageAuditRequired = errors.New("bounded coverage requires authoritative audit")
	ErrBoundedCoverageUnresolved    = errors.New("bounded coverage source unresolved")
)

func (e *Engine) BoundedCoverageBindings(
	ctx context.Context, roots []BoundedCoverageRoot,
) ([]BoundedCoverageBinding, error) {
	bindings := make([]BoundedCoverageBinding, 0)
	seen := make(map[string]struct{})
	for _, requested := range roots {
		factory := e.providerFactories[requested.Agent]
		if factory == nil || factory.Capabilities().Source.BoundedCoverage != parser.CapabilitySupported {
			continue
		}
		providerRoot := filepath.Clean(requested.Root)
		for _, candidate := range e.agentDirs[requested.Agent] {
			if sameCoveragePath(candidate, providerRoot) || withinOrEqual(providerRoot, candidate) {
				providerRoot = candidate
				break
			}
		}
		provider := factory.NewProvider(parser.ProviderConfig{Roots: []string{providerRoot}, Machine: e.machine})
		plan, err := provider.WatchPlan(ctx)
		if err != nil {
			return nil, err
		}
		for _, watchRoot := range plan.Roots {
			for _, include := range watchRoot.IncludeGlobs {
				dbPath := coverageDBPath(watchRoot.Path, include)
				if watchRoot.Recursive || (!sameCoveragePath(dbPath, requested.Root) && !withinOrEqual(dbPath, requested.Root)) {
					continue
				}
				if _, compatible, err := parser.ProbeOpenCodeJournalCapability(ctx, dbPath); err != nil {
					if errors.Is(err, parser.ErrOpenCodeCoverageDatabaseMissing) {
						continue
					}
					return nil, err
				} else if !compatible {
					continue
				}
				key := string(requested.Agent) + "\x00" + dbPath
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				bindings = append(bindings, BoundedCoverageBinding{
					Key: key, Agent: requested.Agent, DBPath: dbPath,
				})
			}
		}
	}
	return bindings, nil
}

func (e *Engine) BoundedCoverageBindingsForPaths(
	ctx context.Context, paths []string,
) ([]BoundedCoverageBinding, []string, error) {
	bindings := make([]BoundedCoverageBinding, 0)
	remaining := make([]string, 0, len(paths))
	seen := make(map[string]struct{})
	for _, path := range paths {
		matched := false
		for agent, factory := range e.providerFactories {
			if factory == nil || factory.Capabilities().Source.BoundedCoverage != parser.CapabilitySupported {
				continue
			}
			provider := factory.NewProvider(parser.ProviderConfig{Roots: e.agentDirs[agent], Machine: e.machine})
			plan, err := provider.WatchPlan(ctx)
			if err != nil {
				return nil, nil, err
			}
			for _, watchRoot := range plan.Roots {
				for _, include := range watchRoot.IncludeGlobs {
					dbPath := coverageDBPath(watchRoot.Path, include)
					if watchRoot.Recursive || !sameCoveragePath(path, dbPath) &&
						!sameCoveragePath(path, dbPath+"-wal") &&
						!sameCoveragePath(path, dbPath+"-shm") {
						continue
					}
					relevance, err := parser.ResolveChangedPathRelevance(
						ctx, provider, parser.ChangedPathRequest{
							Path: path, WatchRoot: watchRoot.Path,
						},
					)
					if err != nil {
						return nil, nil, err
					}
					if relevance == parser.ChangedPathNonData {
						continue
					}
					if _, compatible, err := parser.ProbeOpenCodeJournalCapability(ctx, dbPath); err != nil {
						if errors.Is(err, parser.ErrOpenCodeCoverageDatabaseMissing) {
							continue
						}
						return nil, nil, err
					} else if !compatible {
						continue
					}
					matched = true
					key := string(agent) + "\x00" + dbPath
					if _, ok := seen[key]; !ok {
						seen[key] = struct{}{}
						bindings = append(bindings, BoundedCoverageBinding{Key: key, Agent: agent, DBPath: dbPath})
					}
				}
			}
		}
		if !matched {
			remaining = append(remaining, path)
		}
	}
	return bindings, remaining, nil
}

// DrainBoundedCoverage performs provider admission, bounded journal reading,
// and complete ready-set resolution. It has no checkpoint side effect.
func (e *Engine) DrainBoundedCoverage(
	ctx context.Context, binding BoundedCoverageBinding,
	checkpoint parser.OpenCodeCoverageCheckpoint,
) (parser.OpenCodeFeedResult, []parser.SourceRef, error) {
	factory := e.providerFactories[binding.Agent]
	if factory == nil || factory.Capabilities().Source.BoundedCoverage != parser.CapabilitySupported {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, nil
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots: []string{filepath.Dir(binding.DBPath)}, Machine: e.machine,
	})
	if _, err := os.Stat(binding.DBPath); err != nil {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, fmt.Errorf("%w: %s", parser.ErrOpenCodeCoverageDatabaseMissing, binding.DBPath)
	}
	_, compatible, err := parser.ProbeOpenCodeJournalCapability(ctx, binding.DBPath)
	if err != nil {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, err
	}
	if !compatible {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, nil
	}
	result, err := parser.DrainOpenCodeJournal(ctx, binding.DBPath, checkpoint)
	if err != nil {
		return result, nil, err
	}
	if result.AuditRequired || len(result.ReadyIDs) == 0 {
		return result, nil, nil
	}
	sources := make([]parser.SourceRef, 0, len(result.ReadyIDs))
	for _, id := range result.ReadyIDs {
		source, found, err := provider.FindSource(ctx, parser.FindSourceRequest{RawSessionID: id})
		if err != nil {
			return result, nil, err
		}
		if !found {
			return result, nil, fmt.Errorf("%w: %s", ErrBoundedCoverageUnresolved, id)
		}
		sources = append(sources, source)
	}
	return result, sources, nil
}

func (e *Engine) PrimeBoundedCoverage(
	ctx context.Context, binding BoundedCoverageBinding,
) (parser.OpenCodeCoverageCheckpoint, error) {
	result, _, err := e.DrainBoundedCoverage(
		ctx, binding, parser.OpenCodeCoverageCheckpoint{},
	)
	if err != nil {
		return parser.OpenCodeCoverageCheckpoint{}, err
	}
	return result.Next, nil
}

func (e *Engine) ApplyBoundedCoverageSources(
	ctx context.Context, sources []parser.SourceRef,
) (SyncStats, error) {
	return e.SyncSourceRefsContext(ctx, sources)
}

func withinOrEqual(path, root string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && (rel == "." || rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func sameCoveragePath(a, b string) bool { return filepath.Clean(a) == filepath.Clean(b) }

func coverageDBPath(root, include string) string {
	path := filepath.Clean(filepath.Join(root, include))
	for _, suffix := range []string{"-wal", "-shm"} {
		if strings.HasSuffix(path, suffix) {
			return strings.TrimSuffix(path, suffix)
		}
	}
	return path
}
