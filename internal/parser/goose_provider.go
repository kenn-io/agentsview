package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// GooseDBName is the SQLite filename inside Goose's sessions directory.
const GooseDBName = "sessions.db"

type gooseProviderFactory struct {
	def     AgentDef
	tracker *gooseChangeTracker
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
	return gooseProviderCapabilities()
}

func (f *gooseProviderFactory) NewProvider(cfg ProviderConfig) Provider {
	cfg = cfg.Clone()
	cfg.Roots = normalizeGooseRoots(cfg.Roots)
	spec := gooseProviderSpec()
	base := &dbBackedProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    spec.caps,
		Config:  cfg,
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
	return &gooseProvider{dbBackedProvider: base, tracker: f.tracker}
}

type gooseProvider struct {
	*dbBackedProvider
	tracker *gooseChangeTracker
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
) ([]gooseDiscoveryWatermark, error) {
	watermarks := make([]gooseDiscoveryWatermark, 0, len(p.sources.roots))
	for _, root := range p.sources.roots {
		dbPath := p.spec.findDB(root)
		if dbPath == "" {
			continue
		}
		state, err := readGooseTrackedDatabase(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		watermarks = append(watermarks, gooseDiscoveryWatermark{
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
		ids, cold, snapshot, err := p.tracker.changedSessionIDs(ctx, dbPath)
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
			meta, found, err := gooseSessionMeta(ctx, dbPath, id)
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
	hash, found, err := gooseSessionFingerprint(ctx, src.DBPath, src.SessionID)
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

func gooseProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentGoose,
		dbName: GooseDBName,
		findDB: gooseDBPath,
		streamMeta: func(
			ctx context.Context,
			dbPath string,
			yield func(dbBackedSessionMeta) error,
		) error {
			return forEachGooseSessionMeta(ctx, dbPath, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return gooseSessionMeta(ctx, dbPath, sessionID)
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			result, err := parseGooseSession(dbPath, sessionID, machine)
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

func openGooseDB(dbPath string) (*sql.DB, error) {
	dsn := "file:" + sqliteURIPath(dbPath) + "?mode=ro&immutable=0&_busy_timeout=3000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening goose sessions database %s: %w", dbPath, err)
	}
	return db, nil
}

type gooseRowCursor struct {
	id       int64
	identity string
}

type gooseTrackedDatabase struct {
	schemaVersion int
	inode         uint64
	device        uint64
	sessions      gooseRowCursor
	messages      gooseRowCursor
	usage         gooseRowCursor
	hasUsage      bool
}

type gooseDiscoveryWatermark struct {
	dbPath string
	state  gooseTrackedDatabase
}

type gooseChangeTracker struct {
	mu      sync.Mutex
	entries map[string]*gooseTrackedDatabaseEntry
}

type gooseTrackedDatabaseEntry struct {
	mu    sync.Mutex
	known bool
	state gooseTrackedDatabase
}

func newGooseChangeTracker() *gooseChangeTracker {
	return &gooseChangeTracker{
		entries: make(map[string]*gooseTrackedDatabaseEntry),
	}
}

// entry returns the per-database tracker entry, creating it on first use.
// Each database has its own lock so a slow or busy sessions.db cannot stall
// watcher classification for other Goose roots.
func (t *gooseChangeTracker) entry(dbPath string) *gooseTrackedDatabaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := filepath.Clean(dbPath)
	entry, ok := t.entries[key]
	if !ok {
		entry = &gooseTrackedDatabaseEntry{}
		t.entries[key] = entry
	}
	return entry
}

func (t *gooseChangeTracker) storeDiscoveryWatermarks(
	watermarks []gooseDiscoveryWatermark,
) {
	for _, watermark := range watermarks {
		entry := t.entry(watermark.dbPath)
		entry.mu.Lock()
		entry.mergeLocked(watermark.state)
		entry.mu.Unlock()
	}
}

// commit publishes a snapshot that was captured before a full enumeration.
// Callers invoke it only after that enumeration succeeds, so a failed pass
// cannot advance the cursors past rows it never delivered.
func (t *gooseChangeTracker) commit(dbPath string, state gooseTrackedDatabase) {
	entry := t.entry(dbPath)
	entry.mu.Lock()
	entry.mergeLocked(state)
	entry.mu.Unlock()
}

// mergeLocked adopts state without retreating row cursors that a concurrent
// watcher event already advanced past this snapshot. Callers hold entry.mu.
func (e *gooseTrackedDatabaseEntry) mergeLocked(state gooseTrackedDatabase) {
	if !e.known || gooseTrackedDatabaseReplaced(e.state, state) {
		e.state = state
		e.known = true
		return
	}
	merged := state
	merged.sessions = furthestGooseRowCursor(e.state.sessions, state.sessions)
	merged.messages = furthestGooseRowCursor(e.state.messages, state.messages)
	merged.usage = furthestGooseRowCursor(e.state.usage, state.usage)
	e.state = merged
}

func furthestGooseRowCursor(a, b gooseRowCursor) gooseRowCursor {
	if a.id > b.id {
		return a
	}
	return b
}

// changedSessionIDs lists sessions with rows inserted past the stored
// cursors. cold reports that the caller must fall back to full enumeration;
// the returned snapshot must then be published via commit only after that
// enumeration succeeds. A table whose cursor retreated or was rewritten is
// skipped — deletions are reconciliation's job — while inserts from the
// tables whose cursors are intact are still listed.
func (t *gooseChangeTracker) changedSessionIDs(
	ctx context.Context, dbPath string,
) (ids []string, cold bool, snapshot gooseTrackedDatabase, err error) {
	entry := t.entry(dbPath)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, false, gooseTrackedDatabase{},
			fmt.Errorf("stat goose sessions database: %w", err)
	}
	db, err := openGooseDB(dbPath)
	if err != nil {
		return nil, false, gooseTrackedDatabase{}, err
	}
	defer db.Close()
	current, err := readGooseTrackedDatabaseFrom(ctx, db, info)
	if err != nil {
		return nil, false, gooseTrackedDatabase{}, err
	}
	if !entry.known || gooseTrackedDatabaseReplaced(entry.state, current) {
		return nil, true, current, nil
	}

	seen := make(map[string]struct{})
	for _, check := range gooseCursorChecks(entry.state, current) {
		valid, err := gooseCursorStillValid(ctx, db, check)
		if err != nil {
			return nil, false, gooseTrackedDatabase{}, err
		}
		if !valid {
			continue
		}
		if err := listChangedGooseSessionIDsForTable(
			ctx, db, check.table, check.previous.id, seen,
		); err != nil {
			return nil, false, gooseTrackedDatabase{}, err
		}
	}
	entry.state = current
	entry.known = true
	ids = make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, false, current, nil
}

func gooseTrackedDatabaseReplaced(
	previous, current gooseTrackedDatabase,
) bool {
	identityChanged := (previous.inode != 0 || previous.device != 0) &&
		(previous.inode != current.inode || previous.device != current.device)
	return identityChanged || previous.schemaVersion != current.schemaVersion ||
		previous.hasUsage != current.hasUsage
}

func readGooseTrackedDatabase(
	ctx context.Context, dbPath string,
) (gooseTrackedDatabase, error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return gooseTrackedDatabase{}, fmt.Errorf("stat goose sessions database: %w", err)
	}
	db, err := openGooseDB(dbPath)
	if err != nil {
		return gooseTrackedDatabase{}, err
	}
	defer db.Close()
	return readGooseTrackedDatabaseFrom(ctx, db, info)
}

func readGooseTrackedDatabaseFrom(
	ctx context.Context, db *sql.DB, info os.FileInfo,
) (gooseTrackedDatabase, error) {
	inode, device := sourceFileIdentity(info)
	schemaVersion, err := gooseSchemaVersion(ctx, db)
	if err != nil {
		return gooseTrackedDatabase{}, err
	}
	hasUsage, err := gooseTableExists(ctx, db, "usage_ledger")
	if err != nil {
		return gooseTrackedDatabase{}, err
	}
	sessions, err := latestGooseRowCursor(ctx, db, "sessions")
	if err != nil {
		return gooseTrackedDatabase{}, err
	}
	messages, err := latestGooseRowCursor(ctx, db, "messages")
	if err != nil {
		return gooseTrackedDatabase{}, err
	}
	var usage gooseRowCursor
	if hasUsage {
		usage, err = latestGooseRowCursor(ctx, db, "usage_ledger")
		if err != nil {
			return gooseTrackedDatabase{}, err
		}
	}
	return gooseTrackedDatabase{
		schemaVersion: schemaVersion,
		inode:         inode,
		device:        device,
		sessions:      sessions,
		messages:      messages,
		usage:         usage,
		hasUsage:      hasUsage,
	}, nil
}

func latestGooseRowCursor(
	ctx context.Context, db *sql.DB, table string,
) (gooseRowCursor, error) {
	identityExpr, ok := gooseRowIdentityExpression(table)
	if !ok {
		return gooseRowCursor{}, fmt.Errorf("unsupported goose cursor table %q", table)
	}
	rowID := "rowid"
	if table != "sessions" {
		rowID = "id"
	}
	query := "SELECT " + rowID + ", " + identityExpr + " FROM " + table +
		" ORDER BY " + rowID + " DESC LIMIT 1"
	var cursor gooseRowCursor
	err := db.QueryRowContext(ctx, query).Scan(&cursor.id, &cursor.identity)
	if errors.Is(err, sql.ErrNoRows) {
		return gooseRowCursor{}, nil
	}
	if err != nil {
		return gooseRowCursor{}, fmt.Errorf("reading latest goose %s row: %w", table, err)
	}
	return cursor, nil
}

func gooseRowIdentityExpression(table string) (string, bool) {
	switch table {
	case "sessions":
		return "CAST(id AS TEXT)", true
	case "messages":
		return "session_id || char(31) || COALESCE(message_id, '') || char(31) || CAST(created_timestamp AS TEXT)", true
	case "usage_ledger":
		return "session_id || char(31) || COALESCE(model, '') || char(31) || CAST(created_timestamp AS TEXT)", true
	default:
		return "", false
	}
}

type gooseCursorCheck struct {
	table    string
	previous gooseRowCursor
	current  gooseRowCursor
}

func gooseCursorChecks(
	previous, current gooseTrackedDatabase,
) []gooseCursorCheck {
	checks := []gooseCursorCheck{
		{table: "sessions", previous: previous.sessions, current: current.sessions},
		{table: "messages", previous: previous.messages, current: current.messages},
	}
	if current.hasUsage {
		checks = append(checks, gooseCursorCheck{
			table: "usage_ledger", previous: previous.usage, current: current.usage,
		})
	}
	return checks
}

func gooseCursorStillValid(
	ctx context.Context, db *sql.DB, check gooseCursorCheck,
) (bool, error) {
	if check.current.id < check.previous.id {
		return false, nil
	}
	if check.previous.id == 0 {
		return true, nil
	}
	identity, found, err := gooseRowIdentityAt(
		ctx, db, check.table, check.previous.id,
	)
	if err != nil {
		return false, err
	}
	return found && identity == check.previous.identity, nil
}

func gooseRowIdentityAt(
	ctx context.Context, db *sql.DB, table string, rowID int64,
) (string, bool, error) {
	identityExpr, ok := gooseRowIdentityExpression(table)
	if !ok {
		return "", false, fmt.Errorf("unsupported goose cursor table %q", table)
	}
	idColumn := "rowid"
	if table != "sessions" {
		idColumn = "id"
	}
	query := "SELECT " + identityExpr + " FROM " + table + " WHERE " + idColumn + " = ?"
	var identity string
	err := db.QueryRowContext(ctx, query, rowID).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading goose %s cursor identity: %w", table, err)
	}
	return identity, true, nil
}

func listChangedGooseSessionIDsForTable(
	ctx context.Context,
	db *sql.DB,
	table string,
	after int64,
	seen map[string]struct{},
) error {
	var query string
	switch table {
	case "sessions":
		query = "SELECT id FROM sessions WHERE rowid > ? ORDER BY rowid"
	case "messages":
		query = "SELECT session_id FROM messages WHERE id > ? ORDER BY id"
	case "usage_ledger":
		query = "SELECT session_id FROM usage_ledger WHERE id > ? ORDER BY id"
	default:
		return fmt.Errorf("unsupported goose cursor table %q", table)
	}
	rows, err := db.QueryContext(ctx, query, after)
	if err != nil {
		return fmt.Errorf("listing changed goose sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning changed goose session ID: %w", err)
		}
		id = strings.TrimSpace(id)
		if id != "" {
			seen[id] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return rows.Close()
}
