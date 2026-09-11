package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type crushProviderFactory struct {
	def     AgentDef
	tracker *crushChangeTracker
}

func newCrushProviderFactory(def AgentDef) ProviderFactory {
	return &crushProviderFactory{
		def:     cloneAgentDef(def),
		tracker: newCrushChangeTracker(),
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
	cfg.Roots = normalizeCrushRoots(cfg.Roots)
	spec := crushProviderSpec()
	base := &dbBackedProvider{
		Def:     cloneAgentDef(f.def),
		Caps:    withDBBackedRawCapture(spec.caps),
		Config:  cfg,
		spec:    spec,
		sources: newDBBackedSourceSet(spec, cfg.Roots),
	}
	return &crushProvider{dbBackedProvider: base, tracker: f.tracker}
}

type crushProvider struct {
	*dbBackedProvider
	tracker *crushChangeTracker
}

func (p *crushProvider) Discover(ctx context.Context) ([]SourceRef, error) {
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

func (p *crushProvider) DiscoverEach(
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
func (p *crushProvider) captureDiscoveryWatermarks(
	ctx context.Context,
) ([]crushDiscoveryWatermark, error) {
	watermarks := make([]crushDiscoveryWatermark, 0, len(p.sources.roots))
	for _, root := range p.sources.roots {
		dbPath := p.spec.findDB(root)
		if dbPath == "" {
			continue
		}
		state, err := readCrushTrackedDatabase(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		watermarks = append(watermarks, crushDiscoveryWatermark{
			dbPath: dbPath,
			state:  state,
		})
	}
	return watermarks, nil
}

// SourcesForChangedPath returns only Crush sessions with newly inserted
// session or message rows. Metadata-only updates and row deletes are
// intentionally handled by the provider's scheduled reconciliation pass.
func (p *crushProvider) SourcesForChangedPath(
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
			// cannot prove that any archived Crush member was deleted.
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
			meta, found, err := crushSessionMeta(ctx, dbPath, id)
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
	hash, found, err := crushSessionFingerprint(ctx, src.DBPath, src.SessionID)
	if err != nil {
		return SourceFingerprint{}, err
	}
	if found {
		fingerprint.Hash = hash
	}
	return fingerprint, nil
}

func crushProviderCapabilities() Capabilities {
	source := dbBackedSourceCapabilities(CapabilityNotApplicable)
	// Crush does not consume stored source hints; scheduling them would
	// enumerate every session for each WAL event.
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
			StopReason:           CapabilitySupported,
		},
		Sync: ProviderSyncSemantics{
			FingerprintHashInCacheKey:           true,
			FingerprintHashRequiredForFreshness: true,
		},
	}
}

func crushProviderSpec() dbBackedProviderSpec {
	return dbBackedProviderSpec{
		agent:  AgentCrush,
		dbName: CrushDBName,
		findDB: crushDBPath,
		streamMeta: func(
			ctx context.Context, dbPath string, yield func(dbBackedSessionMeta) error,
		) error {
			return forEachCrushSessionMeta(ctx, dbPath, yield)
		},
		metaForID: func(
			ctx context.Context, dbPath, sessionID string,
		) (dbBackedSessionMeta, bool, error) {
			return crushSessionMeta(ctx, dbPath, sessionID)
		},
		parse: func(dbPath, sessionID, machine string) ([]ParseResult, error) {
			sess, msgs, err := parseCrushSession(dbPath, sessionID, machine)
			if err != nil || sess == nil {
				return nil, err
			}
			return []ParseResult{{Session: *sess, Messages: msgs}}, nil
		},
		caps: crushProviderCapabilities(),
	}
}

// normalizeCrushRoots expands configured roots into per-project data
// directories. A root is one of:
//   - a directory directly holding crush.db (a <project>/.crush data dir)
//   - the path to a crush.db file itself
//   - a Crush data directory holding projects.json, whose listed data
//     dirs are each expanded (deduplicated); an unreadable or empty
//     registry leaves the root in place rather than failing discovery
func normalizeCrushRoots(roots []string) []string {
	cleaned := cleanJSONLRoots(roots)
	out := make([]string, 0, len(cleaned))
	seen := make(map[string]struct{}, len(cleaned))
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
		for _, dir := range expanded {
			add(dir)
		}
	}
	return out
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
	ctx context.Context, dbPath, sessionID string,
) (string, bool, error) {
	db, err := openCrushDB(dbPath)
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
	messageRows, err := db.QueryContext(ctx, `
		SELECT id, session_id, COALESCE(role, ''), COALESCE(parts, ''),
		       COALESCE(model, ''), COALESCE(provider, ''),
		       CAST(COALESCE(created_at, 0) AS TEXT),
		       CAST(COALESCE(updated_at, 0) AS TEXT),
		       COALESCE(CAST(finished_at AS TEXT), ''),
		       CAST(COALESCE(is_summary_message, 0) AS TEXT)
		  FROM messages WHERE session_id = ? ORDER BY rowid
	`, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("fingerprinting crush messages: %w", err)
	}
	defer messageRows.Close()
	for messageRows.Next() {
		var values [10]string
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
	return hex.EncodeToString(hasher.Sum(nil)), true, nil
}

func crushWriteFingerprintField(hasher hash.Hash, value string) {
	_, _ = hasher.Write([]byte(strconv.Itoa(len(value))))
	_, _ = hasher.Write([]byte{':'})
	_, _ = hasher.Write([]byte(value))
}

type crushRowCursor struct {
	id       int64
	identity string
}

type crushTrackedDatabase struct {
	schemaVersion int
	inode         uint64
	device        uint64
	sessions      crushRowCursor
	messages      crushRowCursor
}

type crushDiscoveryWatermark struct {
	dbPath string
	state  crushTrackedDatabase
}

type crushChangeTracker struct {
	mu      sync.Mutex
	entries map[string]*crushTrackedDatabaseEntry
}

type crushTrackedDatabaseEntry struct {
	mu    sync.Mutex
	known bool
	state crushTrackedDatabase
}

func newCrushChangeTracker() *crushChangeTracker {
	return &crushChangeTracker{
		entries: make(map[string]*crushTrackedDatabaseEntry),
	}
}

// entry returns the per-database tracker entry, creating it on first use.
// Each database has its own lock so a slow or busy crush.db cannot stall
// watcher classification for other Crush roots.
func (t *crushChangeTracker) entry(dbPath string) *crushTrackedDatabaseEntry {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := filepath.Clean(dbPath)
	entry, ok := t.entries[key]
	if !ok {
		entry = &crushTrackedDatabaseEntry{}
		t.entries[key] = entry
	}
	return entry
}

func (t *crushChangeTracker) storeDiscoveryWatermarks(
	watermarks []crushDiscoveryWatermark,
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
func (t *crushChangeTracker) commit(dbPath string, state crushTrackedDatabase) {
	entry := t.entry(dbPath)
	entry.mu.Lock()
	entry.mergeLocked(state)
	entry.mu.Unlock()
}

// mergeLocked adopts state without retreating row cursors that a concurrent
// watcher event already advanced past this snapshot. Callers hold entry.mu.
func (e *crushTrackedDatabaseEntry) mergeLocked(state crushTrackedDatabase) {
	if !e.known || crushTrackedDatabaseReplaced(e.state, state) {
		e.state = state
		e.known = true
		return
	}
	merged := state
	merged.sessions = furthestCrushRowCursor(e.state.sessions, state.sessions)
	merged.messages = furthestCrushRowCursor(e.state.messages, state.messages)
	e.state = merged
}

func furthestCrushRowCursor(a, b crushRowCursor) crushRowCursor {
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
func (t *crushChangeTracker) changedSessionIDs(
	ctx context.Context, dbPath string,
) (ids []string, cold bool, snapshot crushTrackedDatabase, err error) {
	entry := t.entry(dbPath)
	entry.mu.Lock()
	defer entry.mu.Unlock()

	info, err := os.Stat(dbPath)
	if err != nil {
		return nil, false, crushTrackedDatabase{},
			fmt.Errorf("stat crush sessions database: %w", err)
	}
	db, err := openCrushDB(dbPath)
	if err != nil {
		return nil, false, crushTrackedDatabase{}, err
	}
	defer db.Close()
	current, err := readCrushTrackedDatabaseFrom(ctx, db, info)
	if err != nil {
		return nil, false, crushTrackedDatabase{}, err
	}
	if !entry.known || crushTrackedDatabaseReplaced(entry.state, current) {
		return nil, true, current, nil
	}

	seen := make(map[string]struct{})
	for _, check := range []crushCursorCheck{
		{table: "sessions", previous: entry.state.sessions, current: current.sessions},
		{table: "messages", previous: entry.state.messages, current: current.messages},
	} {
		valid, err := crushCursorStillValid(ctx, db, check)
		if err != nil {
			return nil, false, crushTrackedDatabase{}, err
		}
		if !valid {
			continue
		}
		if err := listChangedCrushSessionIDsForTable(
			ctx, db, check.table, check.previous.id, seen,
		); err != nil {
			return nil, false, crushTrackedDatabase{}, err
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

func crushTrackedDatabaseReplaced(
	previous, current crushTrackedDatabase,
) bool {
	identityChanged := (previous.inode != 0 || previous.device != 0) &&
		(previous.inode != current.inode || previous.device != current.device)
	return identityChanged || previous.schemaVersion != current.schemaVersion
}

func readCrushTrackedDatabase(
	ctx context.Context, dbPath string,
) (crushTrackedDatabase, error) {
	info, err := os.Stat(dbPath)
	if err != nil {
		return crushTrackedDatabase{}, fmt.Errorf("stat crush sessions database: %w", err)
	}
	db, err := openCrushDB(dbPath)
	if err != nil {
		return crushTrackedDatabase{}, err
	}
	defer db.Close()
	return readCrushTrackedDatabaseFrom(ctx, db, info)
}

func readCrushTrackedDatabaseFrom(
	ctx context.Context, db *sql.DB, info os.FileInfo,
) (crushTrackedDatabase, error) {
	inode, device := sourceFileIdentity(info)
	schemaVersion, err := crushSchemaVersion(ctx, db)
	if err != nil {
		return crushTrackedDatabase{}, err
	}
	sessions, err := latestCrushRowCursor(ctx, db, "sessions")
	if err != nil {
		return crushTrackedDatabase{}, err
	}
	messages, err := latestCrushRowCursor(ctx, db, "messages")
	if err != nil {
		return crushTrackedDatabase{}, err
	}
	return crushTrackedDatabase{
		schemaVersion: schemaVersion,
		inode:         inode,
		device:        device,
		sessions:      sessions,
		messages:      messages,
	}, nil
}

// latestCrushRowCursor reads the newest rowid and a replacement-detecting
// identity per table. Both Crush tables use TEXT primary keys, so the
// rowid orders insertion.
func latestCrushRowCursor(
	ctx context.Context, db *sql.DB, table string,
) (crushRowCursor, error) {
	identityExpr, ok := crushRowIdentityExpression(table)
	if !ok {
		return crushRowCursor{}, fmt.Errorf("unsupported crush cursor table %q", table)
	}
	query := "SELECT rowid, " + identityExpr + " FROM " + table +
		" ORDER BY rowid DESC LIMIT 1"
	var cursor crushRowCursor
	err := db.QueryRowContext(ctx, query).Scan(&cursor.id, &cursor.identity)
	if errors.Is(err, sql.ErrNoRows) {
		return crushRowCursor{}, nil
	}
	if err != nil {
		return crushRowCursor{}, fmt.Errorf("reading latest crush %s row: %w", table, err)
	}
	return cursor, nil
}

func crushRowIdentityExpression(table string) (string, bool) {
	switch table {
	case "sessions":
		return "CAST(id AS TEXT)", true
	case "messages":
		return "session_id || char(31) || COALESCE(role, '') || char(31) || " +
			"CAST(COALESCE(created_at, 0) AS TEXT) || char(31) || " +
			"COALESCE(CAST(finished_at AS TEXT), '')", true
	default:
		return "", false
	}
}

type crushCursorCheck struct {
	table    string
	previous crushRowCursor
	current  crushRowCursor
}

func crushCursorStillValid(
	ctx context.Context, db *sql.DB, check crushCursorCheck,
) (bool, error) {
	if check.current.id < check.previous.id {
		return false, nil
	}
	if check.previous.id == 0 {
		return true, nil
	}
	identityExpr, ok := crushRowIdentityExpression(check.table)
	if !ok {
		return false, fmt.Errorf("unsupported crush cursor table %q", check.table)
	}
	var identity string
	err := db.QueryRowContext(
		ctx, "SELECT "+identityExpr+" FROM "+check.table+" WHERE rowid = ?",
		check.previous.id,
	).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading crush %s cursor identity: %w", check.table, err)
	}
	return identity == check.previous.identity, nil
}

func listChangedCrushSessionIDsForTable(
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
		query = "SELECT session_id FROM messages WHERE rowid > ? ORDER BY rowid"
	default:
		return fmt.Errorf("unsupported crush cursor table %q", table)
	}
	rows, err := db.QueryContext(ctx, query, after)
	if err != nil {
		return fmt.Errorf("listing changed crush sessions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("scanning changed crush session ID: %w", err)
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
