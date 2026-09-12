package parser

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// GooseDBName is the SQLite filename inside Goose's sessions directory.
const GooseDBName = "sessions.db"

type gooseProviderFactory struct {
	def     AgentDef
	tracker *sqliteChangeTracker
}

func newGooseProviderFactory(def AgentDef) ProviderFactory {
	return &gooseProviderFactory{
		def:     cloneAgentDef(def),
		tracker: newGooseChangeTracker(),
	}
}

func (f *gooseProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f *gooseProviderFactory) Capabilities() Capabilities {
	return withDBBackedRawCapture(gooseProviderCapabilities())
}

func (f *gooseProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	cfg.Roots = normalizeGooseRoots(cfg.Roots)
	spec := gooseProviderSpec(cfg.StableSourceSnapshots)
	base := &dbBackedProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    withDBBackedRawCapture(spec.caps),
		Config:  cfg,
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
	return &gooseProvider{dbBackedProvider: base, tracker: f.tracker}
}

type gooseProvider struct {
	*dbBackedProvider
	tracker *sqliteChangeTracker
}

func (p *gooseProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	watermarks, err := p.captureDiscoveryWatermarks(ctx)
	if err != nil {
		return nil, err
	}
	sources, err := p.dbBackedProvider.Discover(ctx)
	if err != nil {
		return nil, err
	}
	p.tracker.storeDiscoveryWatermarks(watermarks)
	return sources, nil
}

func (p *gooseProvider) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	watermarks, err := p.captureDiscoveryWatermarks(ctx)
	if err != nil {
		return err
	}
	if err := p.dbBackedProvider.DiscoverEach(ctx, yield); err != nil {
		return err
	}
	p.tracker.storeDiscoveryWatermarks(watermarks)
	return nil
}

// captureDiscoveryWatermarks reads the change cursors before enumeration.
// Publishing them only after a successful pass leaves rows committed during
// discovery available to the next watcher event.
func (p *gooseProvider) captureDiscoveryWatermarks(
	ctx context.Context,
) ([]sqliteDiscoveryWatermark, error) {
	watermarks := make([]sqliteDiscoveryWatermark, 0, len(p.sources.roots))
	for _, root := range p.sources.roots {
		dbPath := p.spec.findDB(root)
		if dbPath == "" {
			continue
		}
		state, err := p.tracker.read(ctx, dbPath, p.Config.StableSourceSnapshots)
		if err != nil {
			return nil, err
		}
		watermarks = append(watermarks, sqliteDiscoveryWatermark{
			dbPath: dbPath,
			state:  state,
		})
	}
	return watermarks, nil
}

// SourcesForChangedPath returns only Goose sessions with newly inserted
// session, message, or usage rows. Metadata-only updates and row deletes are
// intentionally handled by the provider's scheduled reconciliation pass.
func (p *gooseProvider) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, root := range p.sources.roots {
		if req.WatchRoot != "" && !samePath(req.WatchRoot, root) {
			continue
		}
		if ref, ok := p.sources.sourceRef(root, req.Path, true); ok {
			return []SourceRef{ref}, nil
		}
		dbPath, ok := p.sources.dbPathForEvent(root, req.Path)
		if !ok {
			continue
		}
		if !IsRegularFile(dbPath) {
			// The SQLite archive is persistent. A vanished physical database
			// cannot prove that any archived Goose member was deleted.
			return nil, nil
		}
		ids, cold, snapshot, err := p.tracker.changedSessionIDs(ctx, dbPath, p.Config.StableSourceSnapshots)
		if err != nil {
			return nil, err
		}
		if cold {
			sources, err := p.dbBackedProvider.SourcesForChangedPath(ctx, ChangedPathRequest{
				Path:      req.Path,
				EventKind: req.EventKind,
				WatchRoot: req.WatchRoot,
			})
			if err != nil {
				return nil, err
			}
			p.tracker.commit(dbPath, snapshot)
			return sources, nil
		}

		sources := make([]SourceRef, 0, len(ids))
		for _, id := range ids {
			meta, found, err := gooseSessionMeta(ctx, dbPath, id, p.Config.StableSourceSnapshots)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			sources = append(sources, p.sources.newSourceRef(
				root, dbPath, meta.SessionID, meta.VirtualPath,
			))
		}
		sort.Slice(sources, func(i, j int) bool {
			return sources[i].DisplayPath < sources[j].DisplayPath
		})
		return sources, nil
	}
	return nil, nil
}

func (p *gooseProvider) Fingerprint(
	ctx context.Context, source SourceRef,
) (SourceFingerprint, error) {
	fingerprint, err := p.dbBackedProvider.Fingerprint(ctx, source)
	if err != nil {
		return SourceFingerprint{}, err
	}
	src, ok := p.sources.sourceFromRef(source)
	if !ok || !IsRegularFile(src.DBPath) {
		return fingerprint, nil
	}
	hash, found, err := gooseSessionFingerprint(ctx, src.DBPath, src.SessionID, p.Config.StableSourceSnapshots)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if found {
		fingerprint.Hash = hash
	}
	return fingerprint, nil
}

func gooseProviderCapabilities() Capabilities {
	source := dbBackedSourceCapabilities(CapabilityNotApplicable)
	// Watcher events use the bounded producer-row cursor below. Stored source
	// hints would enumerate every archived virtual member for each WAL event.
	source.StoredSourceHints = CapabilityUnsupported
	return Capabilities{
		Source: source,
		Content: ContentCapabilities{
			FirstMessage:         CapabilitySupported,
			SessionName:          CapabilitySupported,
			Cwd:                  CapabilitySupported,
			Relationships:        CapabilitySupported,
			Thinking:             CapabilitySupported,
			ToolCalls:            CapabilitySupported,
			ToolResults:          CapabilitySupported,
			AggregateUsageEvents: CapabilitySupported,
			Model:                CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}

func gooseProviderSpec(stableSnapshot bool) dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentGoose,
		dbName: GooseDBName,
		findDB: gooseDBPath,
		streamMeta: func(
			ctx context.Context,
			dbPath string,
			yield func(dbBackedSessionMeta) error,
		) error {
			return forEachGooseSessionMeta(ctx, dbPath, stableSnapshot, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return gooseSessionMeta(ctx, dbPath, sessionID, stableSnapshot)
		},
		parse: func(
			ctx context.Context, dbPath, sessionID, machine string,
		) ([]ParseResult, error) {
			result, err := parseGooseSession(ctx, dbPath, sessionID, machine, stableSnapshot)
			if err != nil || result == nil {
				return nil, err
			}
			return []ParseResult{*result}, nil
		},
		caps: gooseProviderCapabilities(),
	}
}

func normalizeGooseRoots(roots []string) []string {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
	for _, root := range cleaned {
		normalized := normalizeGooseRoot(root)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	return out
}

// ResolveGoosePathRoot expands GOOSE_PATH_ROOT using Goose's producer-defined
// data layout. Unlike goose_dirs, the environment variable is never a direct
// sessions directory, even when its basename is "data" or "sessions".
func ResolveGoosePathRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	return filepath.Join(root, "data", "sessions")
}

func normalizeGooseRoot(root string) string {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return ""
	}
	if filepath.Base(root) == GooseDBName {
		return filepath.Dir(root)
	}
	candidates := []string{
		root,
		filepath.Join(root, "sessions"),
		filepath.Join(root, "data", "sessions"),
	}
	for _, candidate := range candidates {
		if IsRegularFile(filepath.Join(candidate, GooseDBName)) {
			return candidate
		}
	}
	switch filepath.Base(root) {
	case "sessions":
		return root
	case "data":
		return filepath.Join(root, "sessions")
	default:
		// A goose_dirs entry may point at the path root when no more specific
		// existing path or conventional basename identifies its shape.
		return filepath.Join(root, "data", "sessions")
	}
}

func gooseDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, GooseDBName)
	if !IsRegularFile(path) {
		return ""
	}
	return path
}

// GooseSQLiteVirtualPath identifies one Goose session inside sessions.db.
func GooseSQLiteVirtualPath(dbPath, sessionID string) string {
	return VirtualSourcePath(dbPath, sessionID)
}

func openGooseDB(dbPath string, stableSnapshot bool) (*sql.DB, error) {
	immutable := "0"
	if stableSnapshot {
		immutable = "1"
	}
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=" + immutable + "&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening goose sessions database %s: %w", dbPath, err)
	}
	return db, nil
}

func newGooseChangeTracker() *sqliteChangeTracker {
	return &sqliteChangeTracker{
		agent:  AgentGoose,
		open:   openGooseDB,
		schema: gooseCursorSchema,
	}
}

func gooseCursorSchema(
	ctx context.Context, db *sql.DB,
) (int, []sqliteCursorTable, error) {
	version, err := gooseSchemaVersion(ctx, db)
	if err != nil {
		return 0, nil, err
	}
	hasUsage, err := gooseTableExists(ctx, db, "usage_ledger")
	if err != nil {
		return 0, nil, err
	}
	tables := []sqliteCursorTable{
		{
			name: "sessions", rowID: "rowid", sessionID: "id",
			identity: "CAST(id AS TEXT)",
		},
		{
			name: "messages", rowID: "id", sessionID: "session_id",
			identity: "session_id || char(31) || COALESCE(message_id, '') || char(31) || CAST(created_timestamp AS TEXT)",
		},
	}
	if hasUsage {
		tables = append(tables, sqliteCursorTable{
			name: "usage_ledger", rowID: "id", sessionID: "session_id",
			identity: "session_id || char(31) || COALESCE(model, '') || char(31) || CAST(created_timestamp AS TEXT)",
		})
	}
	return version, tables, nil
}
