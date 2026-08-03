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
	Key            string
	Agent          parser.AgentType
	DBPath         string
	PhysicalDBPath string
	Scope          string
	Generation     uint64
}

// BoundedCoverageFileIdentity is the physical file fence carried by one
// lease. It is value data so the worker process can enforce the same fence.
type BoundedCoverageFileIdentity struct {
	Path            string `json:"path"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
	Mode            uint32 `json:"mode"`
}

// BoundedCoverageLease is the immutable authority for one bounded lifecycle.
// Coordinator status stays outside this value; consumers receive this lease
// unchanged from admission through source application and repair.
type BoundedCoverageLease struct {
	Binding             BoundedCoverageBinding            `json:"binding"`
	Provider            parser.AgentType                  `json:"provider"`
	PhysicalDBPath      string                            `json:"physical_db_path"`
	ExactProviderScope  string                            `json:"exact_provider_scope"`
	Generation          uint64                            `json:"generation"`
	FileIdentity        BoundedCoverageFileIdentity       `json:"file_identity"`
	AdmissionCheckpoint parser.OpenCodeCoverageCheckpoint `json:"admission_checkpoint"`
	PendingWork         []string                          `json:"pending_work,omitempty"`
	AdmissionRowZero    bool                              `json:"admission_row_zero"`
	validate            func() error                      `json:"-"`
	fileInfo            os.FileInfo                       `json:"-"`
}

func (l *BoundedCoverageLease) Validate() error {
	if l == nil {
		return errors.New("nil bounded coverage lease")
	}
	if l.validate != nil {
		return l.validate()
	}
	return nil
}

func (l *BoundedCoverageLease) SetValidator(validate func() error) {
	if l != nil {
		l.validate = validate
	}
}

type boundedCoverageIdentityProvider interface {
	BoundedCoverageIdentity(context.Context, string, string) (string, string, error)
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

type BoundedCoverageLeaseResolver interface {
	AdmitBoundedCoverageLease(context.Context, BoundedCoverageBinding) (*BoundedCoverageLease, error)
	DrainBoundedCoverageLease(context.Context, *BoundedCoverageLease, parser.OpenCodeCoverageCheckpoint) (parser.OpenCodeFeedResult, []parser.SourceRef, error)
	ApplyBoundedCoverageSourcesLease(context.Context, *BoundedCoverageLease, []parser.SourceRef) (SyncStats, error)
	ReconcileBoundedCoverageLease(context.Context, *BoundedCoverageLease, string) error
}

// BoundedCoverageAdmitter owns the row-zero admission transition. It is kept
// separate from the read/apply resolver so test and generic providers cannot
// accidentally acquire lifecycle authority.
type BoundedCoverageAdmitter interface {
	InitializeBoundedCoverage(context.Context, BoundedCoverageBinding) (parser.OpenCodeCoverageCheckpoint, error)
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
				physicalDBPath, scope, err := boundedCoverageIdentity(ctx, provider, dbPath, watchRoot.Path)
				if err != nil {
					return nil, err
				}
				key := boundedCoverageBindingKey(requested.Agent, physicalDBPath, scope)
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				bindings = append(bindings, BoundedCoverageBinding{
					Key: key, Agent: requested.Agent, DBPath: physicalDBPath,
					PhysicalDBPath: physicalDBPath, Scope: scope,
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
					physicalDBPath, scope, err := boundedCoverageIdentity(ctx, provider, dbPath, watchRoot.Path)
					if err != nil {
						return nil, nil, err
					}
					key := boundedCoverageBindingKey(agent, physicalDBPath, scope)
					if _, ok := seen[key]; !ok {
						seen[key] = struct{}{}
						bindings = append(bindings, BoundedCoverageBinding{
							Key: key, Agent: agent, DBPath: physicalDBPath,
							PhysicalDBPath: physicalDBPath, Scope: scope,
						})
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
	dbPath := binding.PhysicalDBPath
	if dbPath == "" {
		dbPath = binding.DBPath
	}
	scope := binding.Scope
	if scope == "" {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil,
			errors.New("bounded coverage binding has no provider-resolved scope")
	}
	provider := factory.NewProvider(parser.ProviderConfig{
		Roots: []string{scope}, Machine: e.machine,
	})
	if _, err := os.Stat(dbPath); err != nil {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, fmt.Errorf("%w: %s", parser.ErrOpenCodeCoverageDatabaseMissing, dbPath)
	}
	_, compatible, err := parser.ProbeOpenCodeJournalCapability(ctx, dbPath)
	if err != nil {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, err
	}
	if !compatible {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, nil
	}
	result, err := parser.DrainOpenCodeJournal(ctx, dbPath, checkpoint)
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

func boundedCoverageIdentity(
	ctx context.Context, provider parser.Provider, dbPath, scope string,
) (string, string, error) {
	if resolver, ok := provider.(boundedCoverageIdentityProvider); ok {
		return resolver.BoundedCoverageIdentity(ctx, dbPath, scope)
	}
	return "", "", errors.New("provider does not resolve bounded coverage identity")
}

func (e *Engine) InitializeBoundedCoverage(
	ctx context.Context, binding BoundedCoverageBinding,
) (parser.OpenCodeCoverageCheckpoint, error) {
	path := binding.PhysicalDBPath
	if path == "" {
		path = binding.DBPath
	}
	return parser.InitializeOpenCodeCoverageCheckpoint(ctx, path)
}

func (e *Engine) ApplyBoundedCoverageSources(
	ctx context.Context, sources []parser.SourceRef,
) (SyncStats, error) {
	return e.SyncSourceRefsContext(ctx, sources)
}

func boundedCoverageBindingKey(agent parser.AgentType, physicalDBPath, scope string) string {
	return string(agent) + "\x00" + filepath.Clean(physicalDBPath) + "\x00" + filepath.Clean(scope)
}

func boundedCoverageFileIdentity(path string, info os.FileInfo) BoundedCoverageFileIdentity {
	return BoundedCoverageFileIdentity{Path: filepath.Clean(path), Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano(), Mode: uint32(info.Mode())}
}

func (e *Engine) AdmitBoundedCoverageLease(
	ctx context.Context, binding BoundedCoverageBinding,
) (*BoundedCoverageLease, error) {
	path := binding.PhysicalDBPath
	if path == "" {
		path = binding.DBPath
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	checkpoint, err := e.InitializeBoundedCoverage(ctx, binding)
	if err != nil {
		return nil, err
	}
	return &BoundedCoverageLease{
		Binding: binding, Provider: binding.Agent, PhysicalDBPath: path,
		ExactProviderScope: binding.Scope, Generation: binding.Generation,
		FileIdentity:        boundedCoverageFileIdentity(path, info),
		AdmissionCheckpoint: checkpoint, AdmissionRowZero: true,
		fileInfo: info,
	}, nil
}

func (e *Engine) validateBoundedCoverageLease(lease *BoundedCoverageLease) error {
	if err := lease.Validate(); err != nil {
		return err
	}
	info, err := os.Stat(lease.PhysicalDBPath)
	if err != nil {
		return err
	}
	current := boundedCoverageFileIdentity(lease.PhysicalDBPath, info)
	if lease.fileInfo != nil && !os.SameFile(lease.fileInfo, info) {
		return fmt.Errorf("bounded coverage lease physical identity changed: %s", lease.PhysicalDBPath)
	}
	if current != lease.FileIdentity {
		return fmt.Errorf("bounded coverage lease file identity changed: %s", lease.PhysicalDBPath)
	}
	return nil
}

func (e *Engine) DrainBoundedCoverageLease(
	ctx context.Context, lease *BoundedCoverageLease, checkpoint parser.OpenCodeCoverageCheckpoint,
) (parser.OpenCodeFeedResult, []parser.SourceRef, error) {
	if err := e.validateBoundedCoverageLease(lease); err != nil {
		return parser.OpenCodeFeedResult{Next: checkpoint}, nil, err
	}
	result, sources, err := e.DrainBoundedCoverage(ctx, lease.Binding, checkpoint)
	if err != nil {
		return result, nil, err
	}
	if err := e.validateBoundedCoverageLease(lease); err != nil {
		return result, nil, err
	}
	return result, sources, nil
}

func (e *Engine) ApplyBoundedCoverageSourcesLease(
	ctx context.Context, lease *BoundedCoverageLease, sources []parser.SourceRef,
) (SyncStats, error) {
	if err := e.validateBoundedCoverageLease(lease); err != nil {
		return SyncStats{}, err
	}
	stats, err := e.ApplyBoundedCoverageSources(ctx, sources)
	if err != nil {
		return stats, err
	}
	if err := e.validateBoundedCoverageLease(lease); err != nil {
		return stats, err
	}
	return stats, nil
}

func (e *Engine) ReconcileBoundedCoverageLease(
	ctx context.Context, lease *BoundedCoverageLease, reason string,
) error {
	if reason == "" {
		return errors.New("bounded coverage audit reason is empty")
	}
	if lease == nil || lease.Provider != lease.Binding.Agent ||
		filepath.Clean(lease.PhysicalDBPath) != filepath.Clean(lease.Binding.PhysicalDBPath) ||
		filepath.Clean(lease.ExactProviderScope) != filepath.Clean(lease.Binding.Scope) ||
		lease.Generation == 0 || lease.Generation != lease.Binding.Generation {
		return errors.New("bounded coverage audit lease identity mismatch")
	}
	if err := e.validateBoundedCoverageLease(lease); err != nil {
		return err
	}
	return e.ReconcileProviderRoots(ctx, lease.Provider, []string{lease.ExactProviderScope})
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
