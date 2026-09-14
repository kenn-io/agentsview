package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"hash"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

const crushRawProjectsDir = "projects"

type crushChildRelationshipsCacheEntry struct {
	mu       sync.Mutex
	known    bool
	state    SQLiteContainerState
	children map[string][]string
}

type crushChildRelationshipsCache struct {
	mu      sync.Mutex
	entries map[string]*crushChildRelationshipsCacheEntry
}

type crushProviderFactory struct {
	def     AgentDef
	tracker *sqliteChangeTracker
}

func newCrushProviderFactory(def AgentDef) ProviderFactory {
	return &crushProviderFactory{
		def: cloneAgentDef(def),
		tracker: &sqliteChangeTracker{
			agent: AgentCrush, open: openCrushDB, schema: crushCursorSchema,
		},
	}
}

func (f *crushProviderFactory) Definition() AgentDef {
	return cloneAgentDef(f.def)
}

func (f *crushProviderFactory) Capabilities() Capabilities {
	return withDBBackedRawCapture(crushProviderCapabilities())
}

func (f *crushProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	originalRoots := make([]string, len(cfg.Roots))
	copy(originalRoots, cfg.Roots)
	expandedRoots, registryMapping, projectMapping := normalizeCrushRoots(cfg.Roots)
	if cfg.StableSourceSnapshots {
		// crush.db does not store the project path, so hosted snapshots recover
		// it from the provider-owned logical manifest path.
		for _, root := range expandedRoots {
			if projectDir, ok := crushRawProjectDir(root); ok {
				projectMapping[filepath.Clean(root)] = projectDir
			}
		}
	}
	cfg.Roots = expandedRoots
	spec := crushProviderSpec(cfg.StableSourceSnapshots)
	base := &dbBackedProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    withDBBackedRawCapture(spec.caps),
		Config:  cfg,
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
	base.sources.tracker = f.tracker
	base.sources.stableSnapshot = cfg.StableSourceSnapshots
	p := &crushProvider{
		dbBackedProvider: base,
		originalRoots:    originalRoots,
		registryMapping:  registryMapping,
		projectMapping:   projectMapping,
		configuredRoot:   crushConfiguredRootByExpanded(originalRoots, registryMapping),
	}
	// Replace the parse closure to capture the project mapping so
	// parseCrushSession can attribute sessions to the correct project
	// when the data directory does not follow the default layout.
	p.spec.parse = func(
		ctx context.Context, dbPath, sessionID, machine string,
	) ([]ParseResult, error) {
		sess, msgs, err := parseCrushSession(
			ctx, dbPath, sessionID, machine,
			cfg.StableSourceSnapshots, p.projectMapping,
		)
		if err != nil || sess == nil {
			return nil, err
		}
		return []ParseResult{{
			Session:     *sess,
			Messages:    msgs,
			UsageEvents: sess.UsageEvents,
		}}, nil
	}
	return p
}

type crushProvider struct {
	*dbBackedProvider
	originalRoots   []string
	registryMapping map[string][]string
	projectMapping  map[string]string
	childCache      crushChildRelationshipsCache
	// configuredRoot maps each expanded data directory back onto the
	// configured registry, crush.db, or data-directory root that produced
	// it, so source-machine mapping stays bound to the user's spelling.
	configuredRoot map[string]string
}

func crushRawCaptureEntryPath(projectDir string) string {
	encoded := base64.RawURLEncoding.EncodeToString([]byte(projectDir))
	return crushRawProjectsDir + "/" + encoded + "/" + CrushDBName
}

func crushRawProjectDir(dataDir string) (string, bool) {
	if filepath.Base(filepath.Dir(dataDir)) != crushRawProjectsDir {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(filepath.Base(dataDir))
	if err != nil || len(decoded) == 0 || !utf8.Valid(decoded) {
		return "", false
	}
	return string(decoded), true
}

func (p *crushProvider) currentRawCaptureProvider() (*dbBackedProvider, map[string]string) {
	// Periodic raw-sync audits reuse one provider. Re-read projects.json so a
	// project registered after startup enters the next bounded audit.
	currentRoots, _, currentProjects := normalizeCrushRoots(p.originalRoots)
	roots, _, _ := normalizeCrushRoots(
		append(append([]string(nil), p.Config.Roots...), currentRoots...),
	)
	projects := maps.Clone(p.projectMapping)
	maps.Copy(projects, currentProjects)
	base := *p.dbBackedProvider
	base.sources = newDBBackedSourceSet(p.spec, roots)
	return &base, projects
}

func (p *crushProvider) DiscoverRawCaptureSourcesEach(
	ctx context.Context, yield func(SourceRef) error,
) (bool, error) {
	provider, _ := p.currentRawCaptureProvider()
	return provider.DiscoverRawCaptureSourcesEach(ctx, yield)
}

func (p *crushProvider) PlanRawCapture(
	ctx context.Context, source SourceRef,
) (RawCapturePlan, error) {
	plan, err := p.dbBackedProvider.PlanRawCapture(ctx, source)
	if err != nil {
		return RawCapturePlan{}, err
	}
	raw, ok := source.Opaque.(dbBackedRawSource)
	if !ok {
		return RawCapturePlan{}, invalidRawCapturePlan("Crush project path unavailable")
	}
	_, projects := p.currentRawCaptureProvider()
	plan.Entries[0].Path = crushRawCaptureEntryPath(
		crushProjectDir(raw.DBPath, projects),
	)
	return plan, nil
}

// crushConfiguredRootByExpanded maps every expanded data directory onto the
// configured root that produced it. Later originals do not overwrite earlier
// ones: the first configured spelling wins when roots collapse together.
func crushConfiguredRootByExpanded(
	originalRoots []string, registryMapping map[string][]string,
) map[string]string {
	mapping := make(map[string]string, len(originalRoots))
	add := func(expanded, original string) {
		expanded = filepath.Clean(expanded)
		original = filepath.Clean(original)
		if expanded == "" || original == "" || expanded == "." || original == "." {
			return
		}
		if _, ok := mapping[expanded]; !ok {
			mapping[expanded] = original
		}
	}
	for _, root := range originalRoots {
		cleaned := filepath.Clean(root)
		if cleaned == "" || cleaned == "." {
			continue
		}
		physical := cleaned
		if container, _, ok := ParseVirtualSourcePath(physical); ok {
			physical = container
		}
		if filepath.Base(physical) == CrushDBName {
			add(filepath.Dir(physical), cleaned)
			continue
		}
		if dataDirs, ok := registryMapping[cleaned]; ok {
			for _, dir := range dataDirs {
				add(dir, cleaned)
			}
			continue
		}
		add(cleaned, cleaned)
	}
	return mapping
}

// withConfiguredRoot stamps the original configured root onto a source whose
// expanded data directory came from a registry or crush.db spelling.
func (p *crushProvider) withConfiguredRoot(source SourceRef) SourceRef {
	if source.ConfiguredRoot != "" {
		return source
	}
	src, ok := source.Opaque.(dbBackedSource)
	if !ok {
		return source
	}
	if configured, ok := p.configuredRoot[src.Root]; ok {
		source.ConfiguredRoot = configured
	}
	return source
}

func (p *crushProvider) withConfiguredRoots(sources []SourceRef) []SourceRef {
	for i := range sources {
		sources[i] = p.withConfiguredRoot(sources[i])
	}
	return sources
}

// ResolveReconciliationScopes expands registry roots to their per-project
// data directories before scope resolution. The configured roots contain
// only expanded data directories, so a request root that is a registry
// directory would otherwise match no scope. A crush.db database-file root
// or virtual member widens through the container topology onto the owning
// data directory so virtual session members stay in the proof scope.
// TraversalRoots are rewritten back onto the original configured spelling
// so a scoped NewProvider reconstruction re-applies registry expansion,
// project mapping, and configured-root machine attribution.
func (p *crushProvider) ResolveReconciliationScopes(
	_ context.Context, req ReconciliationScopeRequest,
) (ReconciliationScopePlan, error) {
	expanded := make([]string, 0, len(req.Roots))
	for _, root := range req.Roots {
		if dataDirs, ok := p.registryMapping[filepath.Clean(root)]; ok {
			expanded = append(expanded, dataDirs...)
			continue
		}
		expanded = append(expanded, root)
	}
	if err := ValidateReconciliationScopeRoots(
		p.Def.Type, p.Config.Roots, expanded,
	); err != nil {
		return ReconciliationScopePlan{}, err
	}
	plan := containerAwareReconciliationScopePlan(
		p.Config.Roots, expanded, p.reconciliationContainer,
	)
	for i := range plan.Scopes {
		plan.Scopes[i].TraversalRoots = p.withOriginalTraversalRoots(
			plan.Scopes[i].TraversalRoots,
		)
	}
	return plan, nil
}

// withOriginalTraversalRoots maps each expanded data directory back onto the
// configured registry, crush.db, or data-directory root that produced it. A
// registry traversal also includes all of its expanded data directories so
// sibling projects discovered through the registry stay inside the declared
// traversal boundary.
func (p *crushProvider) withOriginalTraversalRoots(roots []string) []string {
	out := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	appendRoot := func(root string) {
		root = filepath.Clean(root)
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	for _, root := range roots {
		if original, ok := p.configuredRoot[filepath.Clean(root)]; ok {
			appendRoot(original)
			for _, dataDir := range p.registryMapping[filepath.Clean(original)] {
				appendRoot(dataDir)
			}
			continue
		}
		appendRoot(root)
	}
	return out
}

// reconciliationContainer maps a crush.db path or virtual member onto the
// owning data directory, which is the spelling configured roots carry after
// normalizeCrushRoots. Classification must not stat: a deleted database must
// still resolve so its members remain reclaimable.
func (p *crushProvider) reconciliationContainer(requested string) (string, bool) {
	physical := requested
	if container, _, ok := ParseVirtualSourcePath(physical); ok {
		physical = container
	}
	if filepath.Base(physical) != CrushDBName {
		return "", false
	}
	return filepath.Dir(physical), true
}

func (p *crushProvider) Discover(ctx context.Context) ([]SourceRef, error) {
	sources, err := p.dbBackedProvider.Discover(ctx)
	if err != nil {
		return nil, err
	}
	return p.withConfiguredRoots(sources), nil
}

func (p *crushProvider) DiscoverEach(
	ctx context.Context, yield func(SourceRef) error,
) error {
	return p.dbBackedProvider.DiscoverEach(ctx, func(source SourceRef) error {
		return yield(p.withConfiguredRoot(source))
	})
}

func (p *crushProvider) SourcesForChangedPath(
	ctx context.Context, req ChangedPathRequest,
) ([]SourceRef, error) {
	sources, err := p.dbBackedProvider.SourcesForChangedPath(ctx, req)
	if err != nil {
		return nil, err
	}
	return p.withConfiguredRoots(sources), nil
}

func (p *crushProvider) FindSource(
	ctx context.Context, req FindSourceRequest,
) (SourceRef, bool, error) {
	source, found, err := p.dbBackedProvider.FindSource(ctx, req)
	if err != nil || !found {
		return source, found, err
	}
	return p.withConfiguredRoot(source), true, nil
}

func (p *crushProvider) Fingerprint(
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
	hash, found, err := crushSessionFingerprint(
		ctx, src.DBPath, src.SessionID, p.Config.StableSourceSnapshots,
		&p.childCache,
	)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if found {
		hasher := sha256.New()
		crushWriteFingerprintField(hasher, hash)
		crushWriteFingerprintField(
			hasher, crushProjectDir(src.DBPath, p.projectMapping),
		)
		fingerprint.Hash = hex.EncodeToString(hasher.Sum(nil))
	}
	return fingerprint, nil
}

func crushProviderCapabilities() Capabilities {
	source := dbBackedSourceCapabilities(CapabilityNotApplicable)
	// Crush does not consume stored source hints; scheduling them would
	// enumerate every session for each WAL event.
	source.StoredSourceHints = CapabilityUnsupported
	source.ExplicitDeletionOnly = CapabilitySupported
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
			StopReason:           CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}

func crushProviderSpec(stableSnapshot bool) dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentCrush,
		dbName: CrushDBName,
		findDB: crushDBPath,
		streamMeta: func(
			ctx context.Context, dbPath string, yield func(dbBackedSessionMeta) error,
		) error {
			return forEachCrushSessionMeta(ctx, dbPath, stableSnapshot, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return crushSessionMeta(ctx, dbPath, sessionID, stableSnapshot)
		},
		parse: func(
			ctx context.Context, dbPath, sessionID, machine string,
		) ([]ParseResult, error) {
			sess, msgs, err := parseCrushSession(ctx, dbPath, sessionID, machine, stableSnapshot, nil)
			if err != nil || sess == nil {
				return nil, err
			}
			// The engine writes usage rows only from ParseResult
			// .UsageEvents; the ParsedSession field feeds ID validation.
			return []ParseResult{{
				Session:     *sess,
				Messages:    msgs,
				UsageEvents: sess.UsageEvents,
			}}, nil
		},
		caps: crushProviderCapabilities(),
	}
}

// normalizeCrushRoots expands configured roots into per-project data
// directories and returns the expanded roots alongside a mapping from
// each original registry root to its expanded data directories. A root
// is one of:
//   - a directory directly holding crush.db (a <project>/.crush data dir)
//   - the path to a crush.db file itself
//   - a Crush data directory holding projects.json, whose listed data
//     dirs are each expanded (deduplicated); an unreadable or empty
//     registry leaves the root in place rather than failing discovery
func normalizeCrushRoots(roots []string) ([]string, map[string][]string, map[string]string) {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
	registryMapping := make(map[string][]string)
	projectMapping := make(map[string]string)
	add := func(root string) {
		if _, ok := seen[root]; ok {
			return
		}
		seen[root] = struct{}{}
		out = append(out, root)
	}
	for _, root := range cleaned {
		root = filepath.Clean(root)
		if root == "" || root == "." {
			continue
		}
		if filepath.Base(root) == CrushDBName {
			add(filepath.Dir(root))
			continue
		}
		if IsRegularFile(filepath.Join(root, CrushDBName)) {
			add(root)
			continue
		}
		expanded := crushProjectsDataDirs(filepath.Join(root, CrushProjectsFileName))
		if len(expanded) == 0 {
			add(root)
			continue
		}
		registryMapping[root] = expanded
		mapping := crushProjectDirsMapping(filepath.Join(root, CrushProjectsFileName))
		maps.Copy(projectMapping, mapping)
		for _, dir := range expanded {
			add(dir)
		}
	}
	return out, registryMapping, projectMapping
}

func crushDBPath(dir string) string {
	if dir == "" {
		return ""
	}
	path := filepath.Join(dir, CrushDBName)
	if !IsRegularFile(path) {
		return ""
	}
	return path
}

// crushSessionFingerprint hashes the session row and every message row so a
// same-second metadata or parts edit produces a fresh fingerprint even
// though the store's second-resolution timestamps did not move.
func crushSessionFingerprint(
	ctx context.Context, dbPath, sessionID string, stableSnapshot bool,
	childCache *crushChildRelationshipsCache,
) (string, bool, error) {
	db, err := openCrushDB(dbPath, stableSnapshot)
	if err != nil {
		return "", false, err
	}
	defer db.Close()
	row, err := scanCrushSessionRow(db.QueryRowContext(
		ctx, crushSessionSelect+" WHERE sessions.id = ?", sessionID,
	))
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("fingerprinting crush session %s: %w", sessionID, err)
	}
	hasher := sha256.New()
	for _, value := range []string{
		row.id, row.title, row.parentSessionID,
		strconv.FormatInt(row.messageCount, 10),
		strconv.FormatInt(row.promptTokens, 10),
		strconv.FormatInt(row.completionTokens, 10),
		strconv.FormatFloat(row.cost, 'g', -1, 64),
		strconv.FormatInt(row.createdAt, 10),
		strconv.FormatInt(row.updatedAt, 10),
		strconv.FormatInt(row.maxMessageAt.Int64, 10),
	} {
		crushWriteFingerprintField(hasher, value)
	}
	messageColumns, err := crushTableColumns(ctx, db, "messages")
	if err != nil {
		return "", false, fmt.Errorf("fingerprinting crush messages: %w", err)
	}
	columns := []struct {
		name    string
		exists  bool
		express string
	}{
		{"id", true, "id"},
		{"session_id", true, "session_id"},
		{"role", true, "COALESCE(role, '')"},
		{"parts", true, "COALESCE(parts, '')"},
		{"model", true, "COALESCE(model, '')"},
		{"provider", messageColumns["provider"], "COALESCE(provider, '')"},
		{"created_at", true, "CAST(COALESCE(created_at, 0) AS TEXT)"},
		{"updated_at", messageColumns["updated_at"], "CAST(COALESCE(updated_at, 0) AS TEXT)"},
		{"finished_at", messageColumns["finished_at"], "COALESCE(CAST(finished_at AS TEXT), '')"},
		{"is_summary_message", messageColumns["is_summary_message"], "CAST(COALESCE(is_summary_message, 0) AS TEXT)"},
	}
	selectExprs := make([]string, 0, len(columns))
	for _, col := range columns {
		if col.exists {
			selectExprs = append(selectExprs, col.express)
		}
	}
	selectStmt := "SELECT " + strings.Join(selectExprs, ", ") +
		" FROM messages WHERE session_id = ? ORDER BY rowid"
	messageRows, err := db.QueryContext(ctx, selectStmt, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("fingerprinting crush messages: %w", err)
	}
	defer messageRows.Close()
	for messageRows.Next() {
		values := make([]string, len(selectExprs))
		destinations := make([]any, len(values))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := messageRows.Scan(destinations...); err != nil {
			return "", false, fmt.Errorf("scanning crush fingerprint message: %w", err)
		}
		for _, value := range values {
			crushWriteFingerprintField(hasher, value)
		}
	}
	if err := messageRows.Err(); err != nil {
		return "", false, err
	}
	children, err := crushChildSessionIDsCached(ctx, db, dbPath, childCache)
	if err != nil {
		return "", false, fmt.Errorf("fingerprinting crush child sessions: %w", err)
	}
	for _, childID := range children[sessionID] {
		crushWriteFingerprintField(hasher, childID)
	}
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

// crushChildSessionIDsCached scans the unindexed parent relationship once and
// reuses the grouped result until SQLiteContainerState proves the DB changed.
func crushChildSessionIDsCached(
	ctx context.Context, db *sql.DB, dbPath string,
	cache *crushChildRelationshipsCache,
) (map[string][]string, error) {
	entry := cache.entry(dbPath)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	state, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return loadCrushChildSessionIDs(ctx, db)
	}
	if entry.known && entry.state == state {
		return entry.children, nil
	}
	children, err := loadCrushChildSessionIDs(ctx, db)
	if err != nil {
		return nil, err
	}
	after, unchanged := StatSQLiteContainerState(dbPath)
	if unchanged && after == state {
		entry.known = true
		entry.state = state
		entry.children = children
	} else {
		entry.known = false
		entry.children = nil
	}
	return children, nil
}

func (c *crushChildRelationshipsCache) entry(
	dbPath string,
) *crushChildRelationshipsCacheEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := filepath.Clean(dbPath)
	if c.entries == nil {
		c.entries = make(map[string]*crushChildRelationshipsCacheEntry)
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &crushChildRelationshipsCacheEntry{}
		c.entries[key] = entry
	}
	return entry
}

func loadCrushChildSessionIDs(
	ctx context.Context, db *sql.DB,
) (map[string][]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT parent_session_id, id
		FROM sessions
		WHERE parent_session_id IS NOT NULL
		ORDER BY parent_session_id, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := make(map[string][]string)
	for rows.Next() {
		var parentID, childID string
		if err := rows.Scan(&parentID, &childID); err != nil {
			return nil, err
		}
		if idx := strings.LastIndex(childID, "$$"); idx >= 0 && idx+2 < len(childID) {
			children[parentID] = append(children[parentID], childID)
		}
	}
	return children, rows.Err()
}

func crushWriteFingerprintField(hasher hash.Hash, value string) {
	_, _ = hasher.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(value))
}

func crushCursorSchema(
	ctx context.Context, db *sql.DB,
) (int, []sqliteCursorTable, error) {
	version, err := crushSchemaVersion(ctx, db)
	if err != nil {
		return 0, nil, err
	}
	columns, err := crushTableColumns(ctx, db, "messages")
	if err != nil {
		return 0, nil, fmt.Errorf("inspecting crush messages columns: %w", err)
	}
	messageIdentity := "session_id || char(31) || COALESCE(role, '') || char(31) || " +
		"CAST(COALESCE(created_at, 0) AS TEXT)"
	if columns["finished_at"] {
		messageIdentity += " || char(31) || COALESCE(CAST(finished_at AS TEXT), '')"
	}
	return version, []sqliteCursorTable{
		{
			name: "sessions", rowID: "rowid", sessionID: "id",
			identity: "CAST(id AS TEXT)",
		},
		{
			name: "messages", rowID: "rowid", sessionID: "session_id",
			identity: messageIdentity,
		},
	}, nil
}

// crushSchemaVersion reads the vendored goose migration version so a
// re-written or downgraded database invalidates stored cursors.
func crushSchemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	hasVersion, err := crushTableExists(ctx, db, "goose_db_version")
	if err != nil {
		return 0, err
	}
	if !hasVersion {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(
		ctx, "SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version",
	).Scan(&version); err != nil {
		return 0, fmt.Errorf("reading crush schema version: %w", err)
	}
	return version, nil
}
