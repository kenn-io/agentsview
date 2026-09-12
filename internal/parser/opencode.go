package parser

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tidwall/gjson"
)

const openCodeStorageFingerprintPrefix = "opencode-storage:v1:"

// OpenCodeSessionMeta is lightweight metadata for a session,
// used to detect changes without parsing messages or parts.
type OpenCodeSessionMeta struct {
	SessionID   string
	VirtualPath string
	FileMtime   int64
	// CompositeMtime reports that FileMtime is the per-session composite
	// (see openCodeCompositeMtimeExpr) rather than the session row's own
	// time_updated. When true the fingerprint omits the shared container's
	// size, because the composite already discriminates per session.
	CompositeMtime bool
	// ChildDigest is the per-session freshness identity carried into the
	// fingerprint's Hash. It folds the composite watermark together with the
	// child row counts, so a deletion that leaves the watermark untouched
	// still changes it. Empty when the container has no composite support.
	ChildDigest string
	// WatermarkOnly reports that FileMtime is only the session-row watermark
	// (session and project time_updated, no child tables) and ChildDigest is
	// deliberately unresolved. Changed-path listings use this bounded form so
	// a watcher event does not scan every child row in the container; the
	// engine skips such a source when its stored composite watermark already
	// covers the carried value, and resolves the full composite and digest
	// through the indexed per-session lookup otherwise.
	WatermarkOnly bool
}

// OpenCodeSQLiteSessionExists reports whether a session row with
// the given ID is present in the OpenCode SQLite database at
// dbPath. Returns false when the file is missing, the schema is
// unexpected, or no row matches. Used by the OpenCode-format
// provider's source lookup so callers can distinguish "this DB has
// the session" from
// "this DB exists but doesn't have it" — the latter must let
// resolution continue to other configured roots.
func OpenCodeSQLiteSessionExists(dbPath, sessionID string) bool {
	if dbPath == "" || sessionID == "" {
		return false
	}
	info, err := os.Stat(dbPath)
	if err != nil || info.IsDir() {
		return false
	}
	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return false
	}
	defer db.Close()
	table, err := openCodeSessionTableCached(db, dbPath, sessionID)
	if err != nil {
		return false
	}
	from, err := openCodeSessionFromCached(db, dbPath, table)
	if err != nil {
		return false
	}
	var found int
	err = db.QueryRow(
		"SELECT 1 FROM "+from+" WHERE id = ? LIMIT 1",
		sessionID,
	).Scan(&found)
	return err == nil
}

// ListOpenCodeSessionMeta returns lightweight metadata for
// all sessions without parsing messages or parts. Used by
// the sync engine to detect which sessions have changed.
func ListOpenCodeSessionMeta(
	dbPath string,
) ([]OpenCodeSessionMeta, error) {
	var metas []OpenCodeSessionMeta
	err := ForEachOpenCodeSessionMeta(
		context.Background(), dbPath,
		func(meta OpenCodeSessionMeta) error {
			metas = append(metas, meta)
			return nil
		},
	)
	return metas, err
}

// The two container freshness query shapes, counted so tests can pin that
// watcher-driven passes stay bounded by the changed batch: the grouped
// whole-container child scan must not run on a changed-path pass, and
// per-session child lookups must scale with the number of changed sessions,
// not the archive.
var (
	openCodeContainerChildScans atomic.Int64
	openCodeSessionChildLookups atomic.Int64
)

// OpenCodeContainerChildScans returns how many whole-container child-table
// identity scans (grouped message/part aggregation) have run.
func OpenCodeContainerChildScans() int64 {
	return openCodeContainerChildScans.Load()
}

// OpenCodeSessionChildLookups returns how many single-session indexed child
// digest lookups have run.
func OpenCodeSessionChildLookups() int64 {
	return openCodeSessionChildLookups.Load()
}

// ListOpenCodeSessionWatermarkMeta is the materialized form of
// ForEachOpenCodeSessionWatermarkMeta.
func ListOpenCodeSessionWatermarkMeta(
	dbPath string,
) ([]OpenCodeSessionMeta, error) {
	var metas []OpenCodeSessionMeta
	err := ForEachOpenCodeSessionWatermarkMeta(
		context.Background(), dbPath,
		func(meta OpenCodeSessionMeta) error {
			metas = append(metas, meta)
			return nil
		},
	)
	return metas, err
}

// ForEachOpenCodeSessionWatermarkMeta streams session rows carrying only the
// session-row watermark: MAX(session.time_updated, project.time_updated),
// touching no child tables. A watcher event on a shared container must not
// pay a whole-archive child scan to find the one session that changed, so
// changed-path classification lists sessions through this bounded form and
// leaves the full composite and child digest to the indexed per-session
// lookup, which the engine runs only for sessions it cannot skip against
// their stored watermark. What this signal cannot see — a child-only write
// that leaves the session and project rows untouched, wherever its
// timestamps land relative to the stored composite — is a known, deliberate
// deferral reconciled by the next full-discovery pass, which still carries
// the complete child digest (see the change-detection entry in
// docs/internal/session-format-sources.md). Legacy containers without
// composite support keep the full listing; their conservative
// container-size fingerprint must not be bypassed by a watermark-only skip.
//
// This listing scans the session table once and that scan is O(session
// count) by design: OpenCode's schema indexes neither time_updated column,
// and the schema is not ours to alter, so any sound candidate selection
// must read every session row. The rows are few and fixed-width (one per
// session, no transcript bytes), which is what makes this the bounded form
// — the quantities that previously scaled with the archive were the child
// tables (two orders of magnitude more rows) and the per-event
// materialization downstream, and both are now bounded by the changed
// batch.
func ForEachOpenCodeSessionWatermarkMeta(
	ctx context.Context,
	dbPath string,
	yield func(OpenCodeSessionMeta) error,
) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	composite, err := openCodeCompositeMtimeSupportedCached(db, dbPath)
	if err != nil {
		return err
	}
	// Ordered by id so the virtual paths ("<db>#<id>") stream in ascending
	// byte order: the changed-path merge walks a paged stored-freshness
	// cursor in step with this stream, and both sides must share one order.
	tables, idle, migration, err := openCodeSessionTablesCached(db, dbPath)
	if err != nil {
		return err
	}
	var queries []string
	for _, table := range tables {
		from := openCodeSessionFrom(table, idle, migration)
		query := "SELECT s.id, s.time_updated FROM " + from + " s"
		if composite {
			query = "SELECT s.id, " + openCodeSessionRowWatermarkExpr +
				" FROM " + from + " s" + openCodeSessionCompositeMtimeJoins
		}

		if table == "session" && len(tables) == 2 {
			query += " WHERE NOT EXISTS (SELECT 1 FROM session_v2 WHERE id = s.id)"
		}
		queries = append(queries, query)
	}
	query := strings.Join(queries, " UNION ALL ") + " ORDER BY 1"

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf(
			"listing opencode session watermarks: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var watermark int64
		if err := rows.Scan(&id, &watermark); err != nil {
			return fmt.Errorf(
				"scanning opencode session watermark: %w", err,
			)
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(OpenCodeSessionMeta{
			SessionID:      id,
			VirtualPath:    dbPath + "#" + id,
			FileMtime:      watermark * 1_000_000,
			CompositeMtime: composite,
			WatermarkOnly:  composite,
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ForEachOpenCodeSessionMeta streams lightweight session rows directly from
// SQLite. The callback runs while the read-only query is open and receives one
// row at a time; callers must not retain database-owned values.
func ForEachOpenCodeSessionMeta(
	ctx context.Context,
	dbPath string,
	yield func(OpenCodeSessionMeta) error,
) error {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	composite, err := openCodeCompositeMtimeSupportedCached(db, dbPath)
	if err != nil {
		return err
	}
	tables, idle, migration, err := openCodeSessionTablesCached(db, dbPath)
	if err != nil {
		return err
	}
	var queries []string
	for _, table := range tables {
		from := openCodeSessionFrom(table, idle, migration)
		query := "SELECT s.id, s.time_updated, s.time_updated, 0, 0, 0, '', '', 0, 0, '' " +
			"FROM " + from + " s"
		if composite {
			openCodeContainerChildScans.Add(1)
			v2, err := openCodeV2SupportedCached(db, dbPath)
			if err != nil {
				return err
			}
			columns, joins := openCodeV2AggregateSQL(v2, false)
			watermark, counts, baseJoins := openCodeCompositeMtimeExpr, openCodeCompositeCountsExpr, openCodeCompositeMtimeJoins
			if table == "session_v2" {
				watermark, counts, baseJoins = openCodeSessionRowWatermarkExpr, openCodeV2BaseCountsExpr, openCodeSessionCompositeMtimeJoins
			}
			query = "SELECT s.id, " + watermark + ", " + counts + columns +
				" FROM " + from + " s" + baseJoins + joins
		}

		if table == "session" && len(tables) == 2 {
			query += " WHERE NOT EXISTS (SELECT 1 FROM session_v2 WHERE id = s.id)"
		}
		queries = append(queries, query)
	}
	query := strings.Join(queries, " UNION ALL ") + " ORDER BY 1"

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf(
			"listing opencode sessions: %w", err,
		)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var agg openCodeChildAggregate
		if err := rows.Scan(
			&id, &agg.watermark, &agg.sessionTime, &agg.projectTime,
			&agg.messages, &agg.parts,
			&agg.messageIdent, &agg.partIdent,
			&agg.projectionTime, &agg.projections, &agg.projectionIdent,
		); err != nil {
			return fmt.Errorf(
				"scanning opencode session meta: %w", err,
			)
		}
		observeStreamingDiscoveryBuffer(ctx, 1)
		if err := yield(OpenCodeSessionMeta{
			SessionID:      id,
			VirtualPath:    dbPath + "#" + id,
			FileMtime:      max(agg.watermark, agg.projectionTime) * 1_000_000,
			CompositeMtime: composite,
			ChildDigest:    agg.digest(composite),
		}); err != nil {
			return err
		}
	}
	return rows.Err()
}

// openCodeSessionCompositeMtime returns one session's composite change signal
// in milliseconds, and whether the container schema supports it. Discovery,
// single-session source lookup, and the parse path all resolve mtime through
// this so a session's stored file_mtime always equals the value the freshness
// gate compares it against.
func openCodeSessionCompositeMtime(
	db *sql.DB, dbPath, sessionID string,
) (int64, string, bool, error) {
	composite, err := openCodeCompositeMtimeSupportedCached(db, dbPath)
	if err != nil {
		return 0, "", false, err
	}
	table, err := openCodeSessionTableCached(db, dbPath, sessionID)
	if err != nil {
		return 0, "", false, err
	}

	from, err := openCodeSessionFromCached(db, dbPath, table)
	if err != nil {
		return 0, "", false, err
	}

	query := "SELECT s.time_updated, s.time_updated, 0, 0, 0, '', '', 0, 0, '' " +
		"FROM " + from + " s WHERE s.id = ?"
	if composite {
		openCodeSessionChildLookups.Add(1)
		v2, err := openCodeV2SupportedCached(db, dbPath)
		if err != nil {
			return 0, "", false, err
		}
		columns, _ := openCodeV2AggregateSQL(v2, true)
		watermark, counts := openCodeSessionCompositeMtimeExpr, openCodeSessionCompositeCountsExpr
		if table == "session_v2" {
			watermark, counts = openCodeSessionRowWatermarkExpr, openCodeV2BaseCountsExpr
		}
		query = "SELECT " + watermark + ", " + counts + columns +
			" FROM " + from + " s" + openCodeSessionCompositeMtimeJoins +
			" WHERE s.id = ?"
	}
	var agg openCodeChildAggregate
	if err := db.QueryRow(query, sessionID).Scan(
		&agg.watermark, &agg.sessionTime, &agg.projectTime,
		&agg.messages, &agg.parts,
		&agg.messageIdent, &agg.partIdent,
		&agg.projectionTime, &agg.projections, &agg.projectionIdent,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, "", composite, nil
		}
		return 0, "", composite, fmt.Errorf(
			"loading opencode session mtime %s#%s: %w",
			dbPath, sessionID, err,
		)
	}
	return max(agg.watermark, agg.projectionTime), agg.digest(composite), composite, nil
}

// openCodeSessionWatermark resolves only the composite watermark, skipping the
// eight child COUNT/SUM/MIN/MAX subqueries that make up the digest. Callers
// that stamp or compare an mtime do not need the digest, and one of them
// (OpenCodeSourceMtime) backs the session watcher's 1.5s poll, so computing a
// digest there would burn eight child-range scans per tick per watched session
// for a value the caller discards.
func openCodeSessionWatermark(
	db *sql.DB, dbPath, sessionID string,
) (int64, bool, error) {
	composite, err := openCodeCompositeMtimeSupportedCached(db, dbPath)
	if err != nil {
		return 0, false, err
	}
	table, err := openCodeSessionTableCached(db, dbPath, sessionID)
	if err != nil {
		return 0, false, err
	}

	from, err := openCodeSessionFromCached(db, dbPath, table)
	if err != nil {
		return 0, false, err
	}

	query := "SELECT s.time_updated FROM " + from + " s WHERE s.id = ?"
	if composite {
		v2, err := openCodeV2SupportedCached(db, dbPath)
		if err != nil {
			return 0, false, err
		}
		watermark := openCodeSessionCompositeMtimeExpr
		if table == "session_v2" {
			watermark = openCodeSessionRowWatermarkExpr
		}
		if v2 {
			watermark = "MAX(" + watermark + ", COALESCE((SELECT MAX(time_updated) FROM session_message WHERE session_id = s.id), 0))"
		}
		query = "SELECT " + watermark +
			" FROM " + from + " s" + openCodeSessionCompositeMtimeJoins +
			" WHERE s.id = ?"
	}
	var watermark int64
	if err := db.QueryRow(query, sessionID).Scan(&watermark); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, composite, nil
		}
		return 0, composite, fmt.Errorf(
			"loading opencode session mtime %s#%s: %w",
			dbPath, sessionID, err,
		)
	}
	return watermark, composite, nil
}

// openCodeChildAggregate is the cheap per-session identity read alongside the
// watermark. Each component covers a change the others cannot:
//
//   - watermark: an edit or insert that advances a timestamp
//   - session/project times: a metadata update, including a worktree rename,
//     that lands below an already-higher child watermark
//   - counts: a deletion, which never moves a MAX
//   - child identity: every ordered (id, time_updated) pair, hashed. Reduced
//     aggregates are not collision-resistant — swapping a non-boundary row for
//     one carrying the same timestamp preserves counts, sums and min/max ids
//     alike, so only the complete set separates them.
//
// All of it lives in the child tables' main b-tree pages, so computing it does
// not read the transcript text held in overflow pages.
type openCodeChildAggregate struct {
	watermark       int64
	sessionTime     int64
	projectTime     int64
	messages        int64
	parts           int64
	messageIdent    string
	partIdent       string
	projectionTime  int64
	projections     int64
	projectionIdent string
}

// The field layout is load-bearing beyond equality comparison:
// OpenCodeChildDigestMetadataWatermarkNS recovers the session/project times
// from a stored digest by position. Any layout change must bump the prefix
// version so stale digests fail that parse (and the equality gate) instead
// of yielding wrong fields.
func (a openCodeChildAggregate) digest(composite bool) string {
	if !composite {
		return ""
	}
	identity := a.messageIdent + "\x00" + a.partIdent
	if a.projections != 0 {
		identity += "\x00v2:" + a.projectionIdent
	}
	sum := sha256.Sum256([]byte(identity))
	return fmt.Sprintf(
		"%s%d:%d:%d:%d:%d:%x",
		openCodeChildDigestPrefix,
		max(a.watermark, a.projectionTime), a.sessionTime, a.projectTime,
		a.messages+a.projections, a.parts,
		sum[:16],
	)
}

const openCodeChildDigestPrefix = "opencode-child:v1:"

// OpenCodeChildDigestMetadataWatermarkNS recovers the session/project
// metadata watermark (nanoseconds) embedded in a stored child digest. The
// watermark filter compares the live session-row watermark like-for-like
// against this value: the stored composite MTimeNS may be dominated by a
// newer child timestamp, and comparing the session-row watermark against
// that composite would hide a metadata update (title, directory, worktree)
// whose stamp lands below it. Returns false for any other hash shape —
// legacy fingerprints, storage fingerprints, future digest versions — which
// callers treat as "compare against the composite instead", the
// conservative pre-digest behavior.
func OpenCodeChildDigestMetadataWatermarkNS(hash string) (int64, bool) {
	rest, ok := strings.CutPrefix(hash, openCodeChildDigestPrefix)
	if !ok {
		return 0, false
	}
	fields := strings.Split(rest, ":")
	if len(fields) != 6 {
		return 0, false
	}
	// Validate every numeric field, not just the two consumed: a digest with
	// any malformed component is not a digest this version wrote, and the
	// caller's composite fallback is the safe answer for it.
	for _, field := range fields[:5] {
		if _, err := strconv.ParseInt(field, 10, 64); err != nil {
			return 0, false
		}
	}
	sessionTime, _ := strconv.ParseInt(fields[1], 10, 64)
	projectTime, _ := strconv.ParseInt(fields[2], 10, 64)
	return max(sessionTime, projectTime) * 1_000_000, true
}

// parseOpenCodeDBSession parses a single session by ID from the
// OpenCode SQLite database. The OpenCode-format provider owns this
// path; Kilo and MiMoCode reuse it and relabel the result.
func parseOpenCodeDBSession(
	dbPath, sessionID, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil, nil, fmt.Errorf(
			"opencode db not found: %s", dbPath,
		)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer db.Close()

	projects, err := loadOpenCodeProjectsCached(db, dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading opencode projects: %w", err,
		)
	}

	table, err := openCodeSessionTableCached(db, dbPath, sessionID)
	if err != nil {
		return nil, nil, err
	}
	hasDirectory, err := openCodeSessionHasDirectoryCached(db, dbPath, table)
	if err != nil {
		return nil, nil, fmt.Errorf("probing opencode session schema: %w", err)
	}
	from, err := openCodeSessionFromCached(db, dbPath, table)
	if err != nil {
		return nil, nil, err
	}
	s, err := loadOneOpenCodeSession(db, from, sessionID, hasDirectory)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"loading opencode session %s: %w",
			sessionID, err,
		)
	}

	projectWorktree := strings.TrimSpace(projects[s.projectID])
	cwd := resolveOpenCodeWorktree(s.directory, projectWorktree)
	if cwd == "" &&
		(strings.TrimSpace(s.directory) == "" ||
			strings.TrimSpace(s.directory) == string(filepath.Separator) ||
			strings.TrimSpace(s.directory) == "/") &&
		(strings.TrimSpace(projectWorktree) == string(filepath.Separator) ||
			strings.TrimSpace(projectWorktree) == "/") {
		// SQLite's global project uses / as its declared worktree. Preserve
		// that source value; file-backed metadata applies the stricter policy.
		cwd = "/"
	}
	if !openCodeUsableWorktree(projectWorktree) {
		projectWorktree = cwd
	}
	return buildOpenCodeSession(
		db, s, cwd, projectWorktree, dbPath, machine,
	)
}

// resolveOpenCodeWorktree picks the session working directory used for
// cwd/project. OpenCode's synthetic "global" project stores worktree="/",
// while session.directory still holds the real path the session ran in.
// Prefer a concrete session directory; fall back to the project worktree.
func resolveOpenCodeWorktree(
	sessionDirectory, projectWorktree string,
) string {
	if dir := strings.TrimSpace(sessionDirectory); openCodeUsableWorktree(dir) {
		return dir
	}
	if worktree := strings.TrimSpace(projectWorktree); openCodeUsableWorktree(worktree) {
		return worktree
	}
	return ""
}

func openCodeUsableWorktree(path string) bool {
	if path == "" {
		return false
	}
	// Root is OpenCode's global-project placeholder, not a real project cwd.
	return path != string(filepath.Separator) && path != "/"
}

type openCodeProjectFile struct {
	Worktree string `json:"worktree"`
}

func openCodeProjectPath(sessionPath string) string {
	root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(sessionPath))))
	projectID := filepath.Base(filepath.Dir(sessionPath))
	return filepath.Join(root, "storage", "project", projectID+".json")
}

// resolveOpenCodeStorageWorktree applies the file-backed session precedence
// and reads only the project metadata named by the session path.
func resolveOpenCodeStorageWorktree(
	sessionPath, sessionDirectory string,
) (string, error) {
	sessionDirectory = strings.TrimSpace(sessionDirectory)
	if openCodeUsableWorktree(sessionDirectory) {
		return resolveOpenCodeWorktree(sessionDirectory, ""), nil
	}
	projectPath := openCodeProjectPath(sessionPath)
	projectWorktree := ""
	raw, err := os.ReadFile(projectPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf(
				"reading opencode project file %s: %w", projectPath, err,
			)
		}
	} else {
		var project openCodeProjectFile
		if err := json.Unmarshal(raw, &project); err != nil {
			return "", fmt.Errorf(
				"decoding opencode project file %s: %w", projectPath, err,
			)
		}
		projectWorktree = project.Worktree
	}
	return resolveOpenCodeWorktree(sessionDirectory, projectWorktree), nil
}

// parseOpenCodeStorageFile parses a file-backed OpenCode storage
// session rooted at storage/session/<project>/<session>.json. The
// OpenCode-format provider owns this path; Kilo and MiMoCode reuse it
// and relabel the result.
func parseOpenCodeStorageFile(
	sessionPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	snapshot, err := loadOpenCodeStorageSnapshot(sessionPath, true)
	if err != nil {
		return nil, nil, err
	}
	sess, parsed, err := buildOpenCodeParsedSession(
		snapshot.session,
		snapshot.worktree,
		snapshot.worktree,
		snapshot.sessionPath,
		snapshot.fileMtime,
		machine,
		snapshot.messages,
		snapshot.parts,
	)
	if err != nil || sess == nil {
		return sess, parsed, err
	}
	sess.File.Size = snapshot.fileSize
	sess.File.Hash = snapshot.fingerprint()
	return sess, parsed, nil
}

// openCodeStorageSessionFingerprint hashes raw storage rows without parsing messages.
func openCodeStorageSessionFingerprint(sessionPath string) (string, error) {
	snapshot, err := loadOpenCodeStorageSnapshot(sessionPath, false)
	if err != nil {
		return "", err
	}
	return openCodeStorageFingerprintFromSnapshot(snapshot)
}

func openCodeStorageFingerprintFromSnapshot(
	snapshot openCodeStorageSnapshot,
) (string, error) {
	if !snapshot.hasMaterializedMessages() {
		// Empty or still-in-progress sessions intentionally map to SkipNoSession;
		// fingerprinting must preserve that normal outcome.
		return "", nil
	}
	hash := snapshot.fingerprint()
	if !HasOpenCodeStorageFingerprint(hash) {
		return "", fmt.Errorf(
			"opencode storage session %s has no content fingerprint",
			snapshot.sessionPath,
		)
	}
	return hash, nil
}

func openOpenCodeDB(dbPath string) (*sql.DB, error) {
	db, err := openSQLiteReadOnly(dbPath, sqliteReadOptions{busyTimeoutMS: 3000})
	if err != nil {
		return nil, fmt.Errorf(
			"opening opencode db %s: %w", dbPath, err,
		)
	}
	return db, nil
}

// openCodeProject is a row from the opencode project table.
type openCodeProject struct {
	id       string
	worktree string
}

func loadOpenCodeProjects(
	db *sql.DB,
) (map[string]string, error) {
	rows, err := db.Query(
		"SELECT id, worktree FROM project",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	projects := make(map[string]string)
	for rows.Next() {
		var p openCodeProject
		if err := rows.Scan(&p.id, &p.worktree); err != nil {
			return nil, err
		}
		projects[p.id] = p.worktree
	}
	return projects, rows.Err()
}

type openCodeProjectsCacheEntry struct {
	state    SQLiteContainerState
	projects map[string]string
}

// openCodeProjectsCache memoizes the project table per shared SQLite DB,
// keyed by the container's change-detection state. The engine parses each
// session of a container through an independent provider instance, so
// without this cache every parsed session re-queried the full project
// table — the dominant per-session cost when re-verifying a changed
// container. Entries hold only a handful of small maps (one per configured
// container path) and are replaced in place.
var (
	openCodeProjectsCacheMu sync.Mutex
	openCodeProjectsCache   = map[string]openCodeProjectsCacheEntry{}
)

// loadOpenCodeProjectsCached returns the project→worktree map for the
// shared DB at dbPath, reusing the previous load while the container state
// is unchanged. The state is captured before the query, so a write racing
// the load can only make cached data newer than its key — the next capture
// then mismatches and reloads. The returned map is shared and must be
// treated as read-only.
func loadOpenCodeProjectsCached(
	db *sql.DB, dbPath string,
) (map[string]string, error) {
	state, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return loadOpenCodeProjects(db)
	}
	openCodeProjectsCacheMu.Lock()
	entry, hit := openCodeProjectsCache[dbPath]
	openCodeProjectsCacheMu.Unlock()
	if hit && entry.state == state {
		return entry.projects, nil
	}
	projects, err := loadOpenCodeProjects(db)
	if err != nil {
		return nil, err
	}
	openCodeProjectsCacheMu.Lock()
	openCodeProjectsCache[dbPath] = openCodeProjectsCacheEntry{
		state:    state,
		projects: projects,
	}
	openCodeProjectsCacheMu.Unlock()
	return projects, nil
}

// openCodeSessionRow is a row from the opencode session table.
type openCodeSessionRow struct {
	id          string
	projectID   string
	parentID    string
	title       string
	directory   string
	timeCreated int64
	timeUpdated int64
}

// openCodeSessionSchemaCacheEntry memoizes schema probes for one
// container. Each probe has its own "resolved" flag so populating one never
// makes the other report a false negative from its zero value.
type openCodeSessionSchemaCacheEntry struct {
	state             SQLiteContainerState
	directoryColumns  map[string]bool
	hasComposite      bool
	compositeOnce     bool
	hasV2             bool
	v2Once            bool
	sessionTables     []string
	hasTimeIdle       bool
	hasMigrationState bool
}

// openCodeSessionSchemaCache memoizes whether session.directory exists for
// each shared OpenCode SQLite path. Legacy OpenCode-family DBs (older
// OpenCode, Kilo, MiMoCode, ICodeMate) omit the column; probing once per
// container state avoids a PRAGMA on every session parse.
var (
	openCodeSessionSchemaCacheMu sync.Mutex
	openCodeSessionSchemaCache   = map[string]openCodeSessionSchemaCacheEntry{}
)

func openCodeSessionHasDirectoryCached(
	db *sql.DB, dbPath, table string,
) (bool, error) {
	state, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return openCodeTableHasColumn(db, table, "directory")
	}
	openCodeSessionSchemaCacheMu.Lock()
	entry, hit := openCodeSessionSchemaCache[dbPath]
	openCodeSessionSchemaCacheMu.Unlock()
	if hasDirectory, resolved := entry.directoryColumns[table]; hit && entry.state == state && resolved {
		return hasDirectory, nil
	}
	hasDirectory, err := openCodeTableHasColumn(db, table, "directory")
	if err != nil {
		return false, err
	}
	openCodeSessionSchemaCacheMu.Lock()
	prev := openCodeSessionSchemaCache[dbPath]
	if prev.state != state {
		prev = openCodeSessionSchemaCacheEntry{}
	}
	prev.state = state
	prev.directoryColumns = maps.Clone(prev.directoryColumns)
	if prev.directoryColumns == nil {
		prev.directoryColumns = make(map[string]bool)
	}
	prev.directoryColumns[table] = hasDirectory
	openCodeSessionSchemaCache[dbPath] = prev
	openCodeSessionSchemaCacheMu.Unlock()
	return hasDirectory, nil
}

// openCodeCompositeMtimeExpr is the per-session change signal for a
// SQLite-backed OpenCode container. Every session in a root shares one
// physical opencode.db, so the container file's own size and mtime move
// whenever any single session is written and cannot discriminate between
// sessions. These four columns can:
//
//   - session.time_updated  — the session row itself
//   - project.time_updated  — the owning project (worktree renames re-resolve
//     every session in that project, which is the correct scope; verified on a
//     production container that this does not track ordinary session activity)
//   - max(message.time_updated) / max(part.time_updated) — child content,
//     including in-place edits that leave time_created untouched
//
// The child scans read only small columns; OpenCode keeps each part's `data`
// in SQLite overflow pages, so this does not read transcript bytes.
// The streaming form groups the child tables once for the whole container, so
// listing every session costs a single pass over each child table.
const openCodeCompositeMtimeExpr = `MAX(s.time_updated,
		COALESCE(pr.time_updated, 0),
		COALESCE(m.mx, 0),
		COALESCE(p.mx, 0))`

const openCodeCompositeMtimeJoins = `
	LEFT JOIN project pr ON pr.id = s.project_id
	LEFT JOIN (
		SELECT session_id, MAX(time_updated) mx, COUNT(*) n,
		       group_concat(id || ':' || time_updated) ident
		FROM (SELECT session_id, id, time_updated FROM message
		      ORDER BY session_id, id)
		GROUP BY session_id
	) m ON m.session_id = s.id
	LEFT JOIN (
		SELECT session_id, MAX(time_updated) mx, COUNT(*) n,
		       group_concat(id || ':' || time_updated) ident
		FROM (SELECT session_id, id, time_updated FROM part
		      ORDER BY session_id, id)
		GROUP BY session_id
	) p ON p.session_id = s.id`

// openCodeCompositeCountsExpr yields the child row counts that make the
// signal deletion-sensitive. A MAX over timestamps cannot see a delete: on a
// real container the session or project row usually already holds the higher
// value, so removing a message or part leaves the max untouched and the
// session would look fresh with the deleted content still archived.
const openCodeCompositeCountsExpr = `s.time_updated,
	COALESCE(pr.time_updated, 0),
	COALESCE(m.n, 0), COALESCE(p.n, 0),
	COALESCE(m.ident, ''), COALESCE(p.ident, '')`

const openCodeSessionCompositeCountsExpr = `
	s.time_updated,
	COALESCE(pr.time_updated, 0),
	(SELECT COUNT(*) FROM message WHERE session_id = s.id),
	(SELECT COUNT(*) FROM part WHERE session_id = s.id),
	(SELECT COALESCE(group_concat(id || ':' || time_updated), '')
	 FROM (SELECT id, time_updated FROM message
	       WHERE session_id = s.id ORDER BY id)),
	(SELECT COALESCE(group_concat(id || ':' || time_updated), '')
	 FROM (SELECT id, time_updated FROM part
	       WHERE session_id = s.id ORDER BY id))`

// The single-session form must NOT reuse the grouped subqueries above: a
// GROUP BY subquery is materialized over the whole container before the outer
// WHERE narrows to one session, so every per-session lookup would scan every
// message and part in the container. Correlated aggregates filtered by
// session_id ride the message/part session_id indexes instead, which is the
// difference between an index seek and an archive-wide scan on every call.
const openCodeSessionCompositeMtimeExpr = `MAX(s.time_updated,
		COALESCE(pr.time_updated, 0),
		COALESCE((
			SELECT MAX(time_updated) FROM message WHERE session_id = s.id
		), 0),
		COALESCE((
			SELECT MAX(time_updated) FROM part WHERE session_id = s.id
		), 0))`

const openCodeSessionCompositeMtimeJoins = `
	LEFT JOIN project pr ON pr.id = s.project_id`

// openCodeSessionRowWatermarkExpr is the bounded change signal used by
// watermark-only changed-path listings: the session and project rows alone,
// no child aggregation. The session table holds one small row per session, so
// listing every session through this costs a scan of the session and project
// tables only — bounded by session count, never by message/part volume.
const openCodeSessionRowWatermarkExpr = `MAX(s.time_updated,
		COALESCE(pr.time_updated, 0))`

// openCodeCompositeMtimeSupportedCached reports whether this container's schema
// carries every column openCodeCompositeMtimeExpr needs. Older OpenCode-family
// containers (Kilo, MiMoCode, ICodeMate, legacy OpenCode) omit the child
// time_updated columns; those keep the previous session-only mtime and the
// container-stat fallback in Fingerprint.
func openCodeCompositeMtimeSupportedCached(
	db *sql.DB, dbPath string,
) (bool, error) {
	state, ok := StatSQLiteContainerState(dbPath)
	if !ok {
		return openCodeSupportsCompositeMtime(db)
	}
	openCodeSessionSchemaCacheMu.Lock()
	entry, hit := openCodeSessionSchemaCache[dbPath]
	openCodeSessionSchemaCacheMu.Unlock()
	if hit && entry.state == state && entry.compositeOnce {
		return entry.hasComposite, nil
	}
	supported, err := openCodeSupportsCompositeMtime(db)
	if err != nil {
		return false, err
	}
	openCodeSessionSchemaCacheMu.Lock()
	prev := openCodeSessionSchemaCache[dbPath]
	if prev.state != state {
		prev = openCodeSessionSchemaCacheEntry{state: state}
	}
	prev.state = state
	prev.hasComposite = supported
	prev.compositeOnce = true
	openCodeSessionSchemaCache[dbPath] = prev
	openCodeSessionSchemaCacheMu.Unlock()
	return supported, nil
}

func openCodeSupportsCompositeMtime(db *sql.DB) (bool, error) {
	hasV2, err := openCodeTableHasColumn(db, "session_v2", "id")
	if err != nil {
		return false, err
	}
	if hasV2 {
		for _, table := range []string{"session_message", "project"} {
			has, err := openCodeTableHasColumn(db, table, "time_updated")
			if err != nil || !has {
				return false, err
			}
		}
		indexed, err := openCodeTableIndexesColumn(db, "session_message", "session_id")
		if err != nil || !indexed {
			return false, err
		}
		legacy, err := openCodeTableHasColumn(db, "session", "id")
		if err != nil || !legacy {
			return !legacy, err
		}
	}

	for _, probe := range []struct{ table, column string }{
		{"message", "time_updated"},
		{"part", "time_updated"},
		{"project", "time_updated"},
	} {
		has, err := openCodeTableHasColumn(db, probe.table, probe.column)
		if err != nil || !has {
			return false, err
		}
	}
	// The per-session lookups are correlated aggregates keyed on session_id.
	// SQLite does not index a foreign key automatically, so without a
	// session_id index each one degrades to a full child-table scan — and one
	// of these backs the session watcher's 1.5s poll. Fall back to the
	// session-only mtime rather than put an archive scan on that path.
	for _, table := range []string{"message", "part"} {
		indexed, err := openCodeTableIndexesColumn(db, table, "session_id")
		if err != nil || !indexed {
			return false, err
		}
	}
	return true, nil
}

// openCodeTableIndexesColumn reports whether table has an index whose leftmost
// column is column, which is what makes a WHERE column = ? lookup a seek.
func openCodeTableIndexesColumn(
	db *sql.DB, table, column string,
) (bool, error) {
	rows, err := db.Query(
		`SELECT 1 FROM pragma_index_list(?) il
		 JOIN pragma_index_info(il.name) ii
		 WHERE ii.seqno = 0 AND ii.name = ?`,
		table, column,
	)
	if err != nil {
		return false, fmt.Errorf(
			"listing opencode %s indexes: %w", table, err,
		)
	}
	defer rows.Close()
	if rows.Next() {
		return true, rows.Err()
	}
	return false, rows.Err()
}

// openCodeTableHasColumn reports whether table carries column. An unknown
// table yields no PRAGMA rows and reports false rather than erroring, so a
// container missing an optional table degrades to the legacy signal.
func openCodeTableHasColumn(
	db *sql.DB, table, column string,
) (bool, error) {
	rows, err := db.Query(`SELECT 1 FROM pragma_table_info(?) WHERE name = ?`,
		table, column)
	if err != nil {
		return false, fmt.Errorf(
			"listing opencode %s table info: %w", table, err,
		)
	}
	defer rows.Close()
	if rows.Next() {
		return true, rows.Err()
	}
	return false, rows.Err()
}

func loadOneOpenCodeSession(
	db *sql.DB, table, sessionID string, hasDirectory bool,
) (openCodeSessionRow, error) {
	var (
		row *sql.Row
		s   openCodeSessionRow
		err error
	)
	if hasDirectory {
		row = db.QueryRow(`
			SELECT s.id, s.project_id,
			       COALESCE(s.parent_id, ''),
			       COALESCE(s.title, ''),
			       COALESCE(s.directory, ''),
			       s.time_created, s.time_updated
			FROM `+table+` s
			WHERE s.id = ?
		`, sessionID)
		err = row.Scan(
			&s.id, &s.projectID, &s.parentID,
			&s.title, &s.directory,
			&s.timeCreated, &s.timeUpdated,
		)
		return s, err
	}

	// Legacy OpenCode-family schemas omit session.directory; cwd falls
	// back to project.worktree via resolveOpenCodeWorktree.
	row = db.QueryRow(`
		SELECT s.id, s.project_id,
		       COALESCE(s.parent_id, ''),
		       COALESCE(s.title, ''),
		       s.time_created, s.time_updated
		FROM `+table+` s
		WHERE s.id = ?
	`, sessionID)
	err = row.Scan(
		&s.id, &s.projectID, &s.parentID,
		&s.title, &s.timeCreated, &s.timeUpdated,
	)
	return s, err
}

// openCodeMessageRow is a row from the opencode message table.
// The role is extracted from the JSON data column.
type openCodeMessageRow struct {
	id          string
	data        string
	timeCreated int64
	fileMtime   int64
}

// openCodeMessageData holds the scalar fields we extract from
// the message data JSON blob. Token usage lives under `tokens`
// and is read separately via gjson so the parser can
// distinguish explicit zero fields from absent ones.
type openCodeMessageData struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Model      struct {
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
}

// openCodePartRow is a row from the opencode part table.
// The part type is extracted from the JSON data column.
type openCodePartRow struct {
	id          string
	messageID   string
	data        string
	timeCreated int64
	fileMtime   int64
}

type openCodeStorageFingerprint struct {
	Session  *openCodeStorageFingerprintSession  `json:"session,omitempty"`
	Messages []openCodeStorageFingerprintMessage `json:"messages"`
}

type openCodeStorageFingerprintSession struct {
	ID          string `json:"id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Directory   string `json:"directory,omitempty"`
	Worktree    string `json:"worktree,omitempty"`
	TimeCreated int64  `json:"time_created,omitempty"`
	TimeUpdated int64  `json:"time_updated,omitempty"`
}

type openCodeStorageFingerprintMessage struct {
	ID    string                           `json:"id"`
	Time  int64                            `json:"time"`
	Hash  string                           `json:"hash,omitempty"`
	Parts []openCodeStorageFingerprintPart `json:"parts,omitempty"`
}

type openCodeStorageFingerprintPart struct {
	ID   string `json:"id"`
	Time int64  `json:"time"`
	Hash string `json:"hash,omitempty"`
}

func loadOpenCodeMessages(
	db *sql.DB, sessionID string, projections bool,
) ([]openCodeMessageRow, error) {
	query := "SELECT id, data, time_created FROM message WHERE session_id = ?"
	if projections {
		query += " AND NOT EXISTS (SELECT 1 FROM session_message WHERE session_message.id = message.id AND session_message.session_id = message.session_id)"
	}
	rows, err := db.Query(query+" ORDER BY time_created, id", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []openCodeMessageRow
	for rows.Next() {
		var m openCodeMessageRow
		if err := rows.Scan(
			&m.id, &m.data, &m.timeCreated,
		); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func loadOpenCodeParts(
	db *sql.DB, sessionID string,
) (map[string][]openCodePartRow, error) {
	rows, err := db.Query(`
		SELECT p.id, p.message_id,
		       COALESCE(p.data, '{}'),
		       p.time_created
		FROM part p
		WHERE p.session_id = ?
		ORDER BY p.time_created
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	parts := make(map[string][]openCodePartRow)
	for rows.Next() {
		var p openCodePartRow
		if err := rows.Scan(
			&p.id, &p.messageID,
			&p.data, &p.timeCreated,
		); err != nil {
			return nil, err
		}
		parts[p.messageID] = append(
			parts[p.messageID], p,
		)
	}
	return parts, rows.Err()
}

func buildOpenCodeSession(
	db *sql.DB,
	s openCodeSessionRow,
	cwd, projectWorktree, dbPath, machine string,
) (*ParsedSession, []ParsedMessage, error) {
	// Capture the watermark BEFORE reading children. Messages and parts are
	// read through separate autocommit queries, so a concurrent write landing
	// between them would otherwise be stamped with a watermark newer than the
	// content actually read, and every later sync would skip the session as
	// fresh — permanently archiving a torn transcript. Reading the watermark
	// first inverts the race: the stamp is never newer than the content, so a
	// concurrent change leaves the stored value behind the source and the next
	// pass re-syncs it.
	//
	// Stamp the same composite the fingerprint reports, so the stored
	// file_mtime is directly comparable to it. Falling back to the session
	// row's own time_updated keeps legacy containers on their prior value.
	fileMtime := s.timeUpdated
	if composite, _, err := openCodeSessionWatermark(
		db, dbPath, s.id,
	); err != nil {
		return nil, nil, err
	} else if composite != 0 {
		fileMtime = composite
	}

	v2, err := openCodeV2SupportedCached(db, dbPath)
	if err != nil {
		return nil, nil, err
	}
	table, err := openCodeSessionTableCached(db, dbPath, s.id)
	if err != nil {
		return nil, nil, err
	}
	var projected []ParsedMessage
	var projectionHash string
	if v2 {
		projected, _, projectionHash, err = loadOpenCodeV2Messages(db, s.id, cwd)
		if err != nil {
			return nil, nil, err
		}
	}
	var msgs []openCodeMessageRow
	var parts map[string][]openCodePartRow
	parsed := projected
	if table == "session" {
		msgs, err = loadOpenCodeMessages(db, s.id, v2)
		if err != nil {
			return nil, nil, fmt.Errorf("loading messages for %s: %w", s.id, err)
		}
		parts, err = loadOpenCodeParts(db, s.id)
		if err != nil {
			return nil, nil, fmt.Errorf("loading parts for %s: %w", s.id, err)
		}
		_, legacy, err := buildOpenCodeParsedSession(s, cwd, projectWorktree, dbPath+"#"+s.id, fileMtime*1_000_000, machine, msgs, parts)
		if err != nil {
			return nil, nil, err
		}
		// Cross-path reuse in 1.18.25 appends projections without converting old
		// messages. Keep v2 seq order and insert unmatched v1 rows by creation time.
		parsed = make([]ParsedMessage, 0, len(legacy)+len(projected))
		for _, message := range projected {
			for len(legacy) > 0 && !legacy[0].Timestamp.After(message.Timestamp) {
				parsed = append(parsed, legacy[0])
				legacy = legacy[1:]
			}
			parsed = append(parsed, message)
		}
		parsed = append(parsed, legacy...)
	}
	for i := range parsed {
		parsed[i].Ordinal = i
	}
	sess, parsed, err := finishOpenCodeSession(s, cwd, projectWorktree, dbPath+"#"+s.id, fileMtime*1_000_000, machine, parsed)
	if err != nil || sess == nil {
		return sess, parsed, err
	}
	metadata := buildOpenCodeSessionFingerprint(s, cwd, projectWorktree, msgs, parts)
	sess.File.Hash = metadata
	if v2 {
		sess.File.Hash = fmt.Sprintf("opencode-v2:%x", sha256.Sum256([]byte(metadata+projectionHash)))
	}
	return sess, parsed, nil
}

func buildOpenCodeParsedSession(
	s openCodeSessionRow,
	cwd, projectWorktree, filePath string,
	fileMtime int64,
	machine string,
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) (*ParsedSession, []ParsedMessage, error) {

	var (
		parsed       []ParsedMessage
		hasUserOrAst bool
		ordinal      int
	)

	for _, m := range msgs {
		var md openCodeMessageData
		if json.Unmarshal([]byte(m.data), &md) != nil {
			continue
		}
		role := normalizeOpenCodeRole(md.Role)
		if role == "" {
			continue
		}
		hasUserOrAst = true

		msgParts := parts[m.id]
		sort.Slice(msgParts, func(a, b int) bool {
			if msgParts[a].timeCreated ==
				msgParts[b].timeCreated {
				return msgParts[a].id < msgParts[b].id
			}
			return msgParts[a].timeCreated <
				msgParts[b].timeCreated
		})

		pm := buildOpenCodeMessage(
			ordinal, m.id, role, m.timeCreated, msgParts, cwd,
		)
		applyOpenCodeTokenUsage(&pm, md, m.data, msgParts)
		if strings.TrimSpace(pm.Content) == "" &&
			!pm.HasToolUse {
			continue
		}

		parsed = append(parsed, pm)
		ordinal++
	}

	if !hasUserOrAst || len(parsed) == 0 {
		return nil, nil, nil
	}

	return finishOpenCodeSession(s, cwd, projectWorktree, filePath, fileMtime, machine, parsed)
}

func finishOpenCodeSession(
	s openCodeSessionRow, cwd, projectWorktree, filePath string,
	fileMtime int64, machine string, parsed []ParsedMessage,
) (*ParsedSession, []ParsedMessage, error) {
	if len(parsed) == 0 {
		return nil, nil, nil
	}
	firstMsg := ""
	if s.title != "" && !isOpenCodeDefaultTitle(s.title) {
		firstMsg = truncate(s.title, 300)
	} else {
		for _, m := range parsed {
			if m.Role == RoleUser && !m.IsSystem {
				firstMsg = truncate(strings.ReplaceAll(m.Content, "\n", " "), 300)
				break
			}
		}
	}

	project := ExtractProjectFromCwd(projectWorktree)
	if project == "" {
		project = "unknown"
	}

	parentID := ""
	if s.parentID != "" {
		parentID = "opencode:" + s.parentID
	}

	startedAt := millisToTime(s.timeCreated)
	endedAt := millisToTime(s.timeUpdated)

	userCount := 0
	for _, m := range parsed {
		if m.Role == RoleUser && !m.IsSystem && m.Content != "" {
			userCount++
		}
	}

	sess := &ParsedSession{
		ID:               "opencode:" + s.id,
		Project:          project,
		Machine:          machine,
		Agent:            AgentOpenCode,
		Cwd:              cwd,
		ParentSessionID:  parentID,
		FirstMessage:     firstMsg,
		StartedAt:        startedAt,
		EndedAt:          endedAt,
		MessageCount:     len(parsed),
		UserMessageCount: userCount,
		File: FileInfo{
			Path:  filePath,
			Mtime: fileMtime,
		},
	}

	accumulateMessageTokenUsage(sess, parsed)

	return sess, parsed, nil
}

// applyOpenCodeTokenUsage copies the assistant message's model
// id and per-message token counts into pm so the usage
// dashboard can attribute cost. OpenCode's token field names
// use a nested `cache.{read,write}` shape; this maps them onto
// the agentsview-native `cache_{read,creation}_input_tokens`
// keys that internal/db/usage.go expects.
//
// Coverage semantics match the claude parser contract: a field
// that is present at zero is preserved as "known zero" and
// sets its coverage flag, while a tokens object with no
// recognized fields (empty `{}` or a foreign schema) leaves
// TokenUsage empty so the usage query filter skips the row.
func applyOpenCodeTokenUsage(
	pm *ParsedMessage,
	md openCodeMessageData,
	dataRaw string,
	parts []openCodePartRow,
) {
	if md.ModelID != "" {
		pm.Model = md.ModelID
	} else if md.Model.ModelID != "" {
		pm.Model = md.Model.ModelID
	}
	raws := []string{dataRaw}
	for _, part := range parts {
		if extractOpenCodePartType(part.data) == "step-finish" {
			raws = append(raws, part.data)
		}
	}
	fields, ok := collectOpenCodeTokenFields(raws...)
	if !ok {
		return
	}

	normalized := map[string]int{
		"input_tokens":                fields.input,
		"output_tokens":               fields.output,
		"cache_read_input_tokens":     fields.cacheRead,
		"cache_creation_input_tokens": fields.cacheCreate,
	}
	j, err := json.Marshal(normalized, json.Deterministic(true))
	if err != nil {
		return
	}
	pm.TokenUsage = j
	pm.OutputTokens = fields.output
	pm.HasOutputTokens = fields.hasOutput
	pm.ContextTokens = fields.input +
		fields.cacheRead + fields.cacheCreate
	pm.HasContextTokens = fields.hasInput ||
		fields.hasCacheRead || fields.hasCacheCreate
}

type openCodeTokenFields struct {
	input          int
	output         int
	cacheRead      int
	cacheCreate    int
	hasInput       bool
	hasOutput      bool
	hasCacheRead   bool
	hasCacheCreate bool
}

func collectOpenCodeTokenFields(
	raws ...string,
) (openCodeTokenFields, bool) {
	var (
		fields openCodeTokenFields
		any    bool
	)

	for _, raw := range raws {
		tokens := gjson.Get(raw, "tokens")
		if !tokens.Exists() {
			continue
		}
		if field := tokens.Get("input"); field.Exists() {
			fields.input = int(field.Int())
			fields.hasInput = true
			any = true
		}
		if field := tokens.Get("output"); field.Exists() {
			fields.output = int(field.Int())
			fields.hasOutput = true
			any = true
		}
		if field := tokens.Get("cache.read"); field.Exists() {
			fields.cacheRead = int(field.Int())
			fields.hasCacheRead = true
			any = true
		}
		if field := tokens.Get("cache.write"); field.Exists() {
			fields.cacheCreate = int(field.Int())
			fields.hasCacheCreate = true
			any = true
		}
	}

	return fields, any
}

// openCodeDefaultTitleRe matches the exact placeholder format
// OpenCode uses before the LLM generates a real title:
// "New session - 2026-03-22T10:00:00.000Z" or
// "Child session - 2026-03-22T10:00:00.000Z".
var openCodeDefaultTitleRe = regexp.MustCompile(
	`^(New session|Child session) - \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$`,
)

func isOpenCodeDefaultTitle(title string) bool {
	return openCodeDefaultTitleRe.MatchString(title)
}

func normalizeOpenCodeRole(role string) RoleType {
	switch role {
	case "user":
		return RoleUser
	case "assistant":
		return RoleAssistant
	default:
		return ""
	}
}

func buildOpenCodeMessage(
	ordinal int,
	messageID string,
	role RoleType,
	timeCreatedMs int64,
	parts []openCodePartRow,
	cwd string,
) ParsedMessage {
	var (
		texts       []string
		toolCalls   []ParsedToolCall
		hasThinking bool
		hasToolUse  bool
	)

	for _, p := range parts {
		partType := extractOpenCodePartType(p.data)
		switch partType {
		case "text":
			text := extractOpenCodeText(p.data)
			if text != "" {
				texts = append(texts, text)
			}
		case "tool":
			hasToolUse = true
			tc := extractOpenCodeToolCall(p.data, cwd)
			if tc.ToolName != "" {
				toolCalls = append(toolCalls, tc)
			}
		case "reasoning":
			text := extractOpenCodeText(p.data)
			if text != "" {
				hasThinking = true
				texts = append(texts,
					"[Thinking]\n"+text+"\n[/Thinking]")
			}
		}
		// skip step-start, step-finish, patch, etc.
	}

	content := strings.Join(texts, "\n")
	return ParsedMessage{
		Ordinal: ordinal,
		Role:    role,
		Content: content,
		// The storage message ID is the stable identity archive guards use
		// to match stored rows when ordinals shift or are sparse.
		SourceUUID:    messageID,
		Timestamp:     millisToTime(timeCreatedMs),
		HasThinking:   hasThinking,
		HasToolUse:    hasToolUse,
		ContentLength: len(content),
		ToolCalls:     toolCalls,
	}
}

// openCodePartTypeData extracts just the type from a part's
// JSON data blob.
type openCodePartTypeData struct {
	Type string `json:"type"`
}

func extractOpenCodePartType(data string) string {
	var d openCodePartTypeData
	if json.Unmarshal([]byte(data), &d) != nil {
		return ""
	}
	return d.Type
}

// openCodeTextData is the JSON structure for a text part's data.
type openCodeTextData struct {
	Content string `json:"content"`
	Text    string `json:"text"`
}

func extractOpenCodeText(data string) string {
	var d openCodeTextData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return ""
	}
	if d.Content != "" {
		return d.Content
	}
	return d.Text
}

// openCodeToolData is the JSON structure for a tool part's data.
type openCodeToolData struct {
	ToolName string         `json:"tool"`
	CallID   string         `json:"callID"`
	State    jsontext.Value `json:"state"`
}

// openCodeToolState holds the nested state of a tool call.
type openCodeToolState struct {
	Input    jsontext.Value `json:"input"`
	Metadata jsontext.Value `json:"metadata"`
}

// openCodeToolMetadata holds the optional metadata from a tool state.
type openCodeToolMetadata struct {
	Exit int `json:"exit"`
}

func extractOpenCodeToolCall(data, cwd string) ParsedToolCall {
	var d openCodeToolData
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return ParsedToolCall{}
	}

	var (
		inputJSON string
		isFailure bool
	)
	if len(d.State) > 0 {
		var state openCodeToolState
		if err := json.Unmarshal(d.State, &state); err == nil {
			if len(state.Input) > 0 {
				inputJSON = string(state.Input)
			}
			// OpenCode records the shell exit code in the tool
			// state metadata. On Windows the output text carries
			// no "exit status N" marker, so metadata.exit is the
			// only reliable failure signal.
			if d.ToolName == "bash" && len(state.Metadata) > 0 {
				var m openCodeToolMetadata
				if err := json.Unmarshal(state.Metadata, &m); err == nil && m.Exit > 0 {
					isFailure = true
				}
			}
		}
	}

	var skillName string
	switch d.ToolName {
	case "skill":
		skillName = gjson.Get(inputJSON, "skill").Str
		if skillName == "" {
			skillName = gjson.Get(inputJSON, "name").Str
		}
	default:
		skillName = inferOpenCodeSkillName(d.ToolName, inputJSON, cwd)
	}

	tc := ParsedToolCall{
		ToolUseID: d.CallID,
		ToolName:  d.ToolName,
		Category:  NormalizeToolCategory(d.ToolName),
		InputJSON: inputJSON,
		SkillName: skillName,
	}

	// OpenCode records model calls to unknown or malformed tools as a
	// synthetic "invalid" tool whose execute succeeds, so state.status
	// is "completed" and carries no error signal. Attach an errored
	// result event so tool health counts these as failures.
	if d.ToolName == "invalid" {
		isFailure = true
	}

	if isFailure {
		tc.ResultEvents = append(tc.ResultEvents, ParsedToolResultEvent{
			ToolUseID: d.CallID,
			Status:    "errored",
		})
	}

	return tc
}

func inferOpenCodeSkillName(toolName, inputJSON, cwd string) string {
	if isCursorSkillReadTool(toolName) {
		// OpenCode's read-tool input carries no cwd/workdir key, so
		// inferSkillNameFromJSONPaths can't resolve relative SKILL.md
		// paths and falls back to the parent directory name. Try the
		// file_path directly against the session worktree first.
		if fp := gjson.Get(inputJSON, "file_path").Str; fp != "" && cwd != "" {
			if name := skillNameFromPath(context.Background(), fp, cwd); name != "" {
				return name
			}
		}
		return inferSkillNameFromJSONPaths(context.Background(), inputJSON)
	}
	return inferCodexSkillNameWithBase(context.Background(), toolName, inputJSON, cwd)
}

type openCodeStorageTime struct {
	Created int64 `json:"created"`
	Start   int64 `json:"start"`
	End     int64 `json:"end"`
	Updated int64 `json:"updated"`
}

func (t openCodeStorageTime) messageSortTime() int64 {
	switch {
	case t.Created != 0:
		return t.Created
	case t.Start != 0:
		return t.Start
	case t.End != 0:
		return t.End
	default:
		return t.Updated
	}
}

func (t openCodeStorageTime) partSortTime() int64 {
	switch {
	case t.Start != 0:
		return t.Start
	case t.Created != 0:
		return t.Created
	case t.End != 0:
		return t.End
	default:
		return t.Updated
	}
}

type openCodeStorageSessionFile struct {
	ID        string              `json:"id"`
	Directory string              `json:"directory"`
	ParentID  string              `json:"parentID"`
	Title     string              `json:"title"`
	Time      openCodeStorageTime `json:"time"`
}

type openCodeStorageSnapshot struct {
	sessionPath string
	fileSize    int64
	fileMtime   int64
	session     openCodeSessionRow
	directory   string
	worktree    string
	messages    []openCodeMessageRow
	parts       map[string][]openCodePartRow
}

func loadOpenCodeStorageSnapshot(
	sessionPath string,
	includeMtime bool,
) (openCodeStorageSnapshot, error) {
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		return openCodeStorageSnapshot{}, fmt.Errorf(
			"reading opencode session file %s: %w",
			sessionPath, err,
		)
	}

	var sf openCodeStorageSessionFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return openCodeStorageSnapshot{}, fmt.Errorf(
			"decoding opencode session file %s: %w",
			sessionPath, err,
		)
	}
	if sf.ID == "" {
		return openCodeStorageSnapshot{}, fmt.Errorf(
			"opencode session file %s missing id",
			sessionPath,
		)
	}

	root := filepath.Dir(filepath.Dir(filepath.Dir(
		filepath.Dir(sessionPath),
	)))
	msgs, err := loadOpenCodeStorageMessages(root, sf.ID)
	if err != nil {
		return openCodeStorageSnapshot{}, err
	}
	parts, err := loadOpenCodeStorageParts(root, msgs)
	if err != nil {
		return openCodeStorageSnapshot{}, err
	}
	var fileMtime int64
	if includeMtime {
		fileMtime, err = OpenCodeSourceMtime(sessionPath)
		if err != nil {
			return openCodeStorageSnapshot{}, err
		}
	}
	worktree, err := resolveOpenCodeStorageWorktree(sessionPath, sf.Directory)
	if err != nil {
		return openCodeStorageSnapshot{}, err
	}

	return openCodeStorageSnapshot{
		sessionPath: sessionPath,
		fileSize:    int64(len(raw)),
		fileMtime:   fileMtime,
		session: openCodeSessionRow{
			id:          sf.ID,
			parentID:    sf.ParentID,
			title:       sf.Title,
			timeCreated: sf.Time.Created,
			timeUpdated: sf.Time.Updated,
		},
		directory: sf.Directory,
		worktree:  worktree,
		messages:  msgs,
		parts:     parts,
	}, nil
}

func (s openCodeStorageSnapshot) fingerprint() string {
	return buildOpenCodeSessionFingerprint(
		s.session,
		s.directory,
		s.worktree,
		s.messages,
		s.parts,
	)
}

func (s openCodeStorageSnapshot) hasMaterializedMessages() bool {
	for _, msg := range s.messages {
		var md openCodeMessageData
		if json.Unmarshal([]byte(msg.data), &md) != nil ||
			normalizeOpenCodeRole(md.Role) == "" {
			continue
		}
		for _, part := range s.parts[msg.id] {
			switch extractOpenCodePartType(part.data) {
			case "tool":
				return true
			case "text":
				if strings.TrimSpace(extractOpenCodeText(part.data)) != "" {
					return true
				}
			case "reasoning":
				if extractOpenCodeText(part.data) != "" {
					return true
				}
			}
		}
	}
	return false
}

type openCodeStorageMessageFile struct {
	ID         string `json:"id"`
	SessionID  string `json:"sessionID"`
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Model      struct {
		ModelID    string `json:"modelID"`
		ProviderID string `json:"providerID"`
	} `json:"model"`
	Time openCodeStorageTime `json:"time"`
}

type openCodeStoragePartFile struct {
	ID        string              `json:"id"`
	SessionID string              `json:"sessionID"`
	MessageID string              `json:"messageID"`
	Time      openCodeStorageTime `json:"time"`
}

func loadOpenCodeStorageMessages(
	root, sessionID string,
) ([]openCodeMessageRow, error) {
	dir := filepath.Join(root, "storage", "message", sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"reading opencode message dir %s: %w", dir, err,
		)
	}

	var msgs []openCodeMessageRow
	for _, entry := range entries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf(
				"reading opencode message file %s: %w",
				path, err,
			)
		}
		var mf openCodeStorageMessageFile
		if err := json.Unmarshal(raw, &mf); err != nil {
			return nil, fmt.Errorf(
				"decoding opencode message file %s: %w",
				path, err,
			)
		}
		if mf.ID == "" {
			return nil, fmt.Errorf(
				"opencode message file %s missing id",
				path,
			)
		}
		msgs = append(msgs, openCodeMessageRow{
			id:          mf.ID,
			data:        string(raw),
			timeCreated: mf.Time.messageSortTime(),
			fileMtime:   mustEntryMtime(entry),
		})
	}

	sort.Slice(msgs, func(i, j int) bool {
		if msgs[i].timeCreated == msgs[j].timeCreated {
			return msgs[i].id < msgs[j].id
		}
		return msgs[i].timeCreated < msgs[j].timeCreated
	})
	return msgs, nil
}

func loadOpenCodeStorageParts(
	root string, msgs []openCodeMessageRow,
) (map[string][]openCodePartRow, error) {
	parts := make(map[string][]openCodePartRow, len(msgs))
	for _, msg := range msgs {
		dir := filepath.Join(root, "storage", "part", msg.id)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf(
				"reading opencode part dir %s: %w", dir, err,
			)
		}
		for _, entry := range entries {
			if entry.IsDir() ||
				!strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf(
					"reading opencode part file %s: %w",
					path, err,
				)
			}
			var pf openCodeStoragePartFile
			if err := json.Unmarshal(raw, &pf); err != nil {
				return nil, fmt.Errorf(
					"decoding opencode part file %s: %w",
					path, err,
				)
			}
			if pf.ID == "" {
				return nil, fmt.Errorf(
					"opencode part file %s missing id",
					path,
				)
			}
			if pf.MessageID == "" {
				pf.MessageID = msg.id
			}
			parts[msg.id] = append(parts[msg.id], openCodePartRow{
				id:          pf.ID,
				messageID:   pf.MessageID,
				data:        string(raw),
				timeCreated: pf.Time.partSortTime(),
				fileMtime:   mustEntryMtime(entry),
			})
		}
	}
	return parts, nil
}

// OpenCodeSourceMtime returns a composite mtime for either an
// OpenCode storage session JSON path or a legacy SQLite virtual
// path in the form opencode.db#<sessionID>.
func OpenCodeSourceMtime(sourcePath string) (int64, error) {
	if sourcePath == "" {
		return 0, nil
	}
	if dbPath, sessionID, ok := parseOpenCodeFormatVirtualPath(
		openCodeFmt.dbName, sourcePath,
	); ok {
		return openCodeSQLiteSessionMtime(dbPath, sessionID)
	}
	return openCodeStorageSessionMtime(sourcePath)
}

func OpenCodeStorageFingerprintMissing(
	storedHash, currentHash string,
) bool {
	stored, ok := decodeOpenCodeStorageFingerprint(storedHash)
	if !ok {
		return false
	}
	current, ok := decodeOpenCodeStorageFingerprint(currentHash)
	if !ok {
		return false
	}
	if stored.Session != nil {
		if current.Session == nil ||
			*current.Session != *stored.Session {
			return true
		}
	}

	currentMsgs := make(map[string]openCodeStorageFingerprintMessage, len(current.Messages))
	for _, msg := range current.Messages {
		currentMsgs[msg.ID] = msg
	}
	for _, storedMsg := range stored.Messages {
		currentMsg, ok := currentMsgs[storedMsg.ID]
		if !ok || currentMsg.Time < storedMsg.Time ||
			currentMsg.Hash != storedMsg.Hash {
			return true
		}
		currentParts := make(map[string]openCodeStorageFingerprintPart, len(currentMsg.Parts))
		for _, part := range currentMsg.Parts {
			currentParts[part.ID] = part
		}
		for _, storedPart := range storedMsg.Parts {
			currentPart, ok := currentParts[storedPart.ID]
			if !ok || currentPart.Time < storedPart.Time ||
				currentPart.Hash != storedPart.Hash {
				return true
			}
		}
	}
	return false
}

func HasOpenCodeStorageFingerprint(hash string) bool {
	return strings.HasPrefix(hash, openCodeStorageFingerprintPrefix)
}

func buildOpenCodeStorageFingerprint(
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) string {
	return buildOpenCodeStorageFingerprintWithSession(nil, msgs, parts)
}

func buildOpenCodeSessionFingerprint(
	s openCodeSessionRow,
	directory, worktree string,
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) string {
	return buildOpenCodeStorageFingerprintWithSession(
		&openCodeStorageFingerprintSession{
			ID:          s.id,
			ProjectID:   s.projectID,
			ParentID:    s.parentID,
			Title:       s.title,
			Directory:   directory,
			Worktree:    worktree,
			TimeCreated: s.timeCreated,
			TimeUpdated: s.timeUpdated,
		},
		msgs,
		parts,
	)
}

func buildOpenCodeStorageFingerprintWithSession(
	session *openCodeStorageFingerprintSession,
	msgs []openCodeMessageRow,
	parts map[string][]openCodePartRow,
) string {
	fp := openCodeStorageFingerprint{
		Session: session,
		Messages: make(
			[]openCodeStorageFingerprintMessage,
			0, len(msgs),
		),
	}
	for _, msg := range msgs {
		partRows := append([]openCodePartRow(nil), parts[msg.id]...)
		sort.Slice(partRows, func(i, j int) bool {
			if partRows[i].timeCreated == partRows[j].timeCreated {
				return partRows[i].id < partRows[j].id
			}
			return partRows[i].timeCreated < partRows[j].timeCreated
		})
		fpMsg := openCodeStorageFingerprintMessage{
			ID:   msg.id,
			Time: msg.timeCreated,
			Hash: openCodeStorageFingerprintHash(msg.data),
		}
		for _, part := range partRows {
			fpMsg.Parts = append(fpMsg.Parts,
				openCodeStorageFingerprintPart{
					ID:   part.id,
					Time: part.timeCreated,
					Hash: openCodeStorageFingerprintHash(part.data),
				},
			)
		}
		fp.Messages = append(fp.Messages, fpMsg)
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return ""
	}
	return openCodeStorageFingerprintPrefix + string(raw)
}

func decodeOpenCodeStorageFingerprint(
	hash string,
) (openCodeStorageFingerprint, bool) {
	if !strings.HasPrefix(hash, openCodeStorageFingerprintPrefix) {
		return openCodeStorageFingerprint{}, false
	}
	raw := strings.TrimPrefix(hash, openCodeStorageFingerprintPrefix)
	var fp openCodeStorageFingerprint
	if err := json.Unmarshal([]byte(raw), &fp); err != nil {
		return openCodeStorageFingerprint{}, false
	}
	return fp, true
}

func openCodeStorageFingerprintHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum)
}

// openCodeSQLiteSessionMtimeComposite is openCodeSQLiteSessionMtime with the
// schema-support flag the fingerprint needs to decide whether the shared
// container's size still has to act as a fallback change signal.
func openCodeSQLiteSessionMtimeComposite(
	dbPath, sessionID string,
) (int64, string, bool, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return 0, "", false, nil
		}
		return 0, "", false, fmt.Errorf(
			"stat opencode db %s: %w", dbPath, err,
		)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return 0, "", false, err
	}
	defer db.Close()

	timeUpdated, digest, composite, err := openCodeSessionCompositeMtime(
		db, dbPath, sessionID,
	)
	if err != nil {
		return 0, "", false, err
	}
	if timeUpdated == 0 {
		return 0, digest, composite, nil
	}
	return timeUpdated * 1_000_000, digest, composite, nil
}

func openCodeSQLiteSessionMtime(
	dbPath, sessionID string,
) (int64, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"stat opencode db %s: %w", dbPath, err,
		)
	}

	db, err := openOpenCodeDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	timeUpdated, _, err := openCodeSessionWatermark(db, dbPath, sessionID)
	if err != nil {
		return 0, err
	}
	if timeUpdated == 0 {
		return 0, nil
	}
	return timeUpdated * 1_000_000, nil
}

func openCodeStorageSessionMtime(
	sessionPath string,
) (int64, error) {
	info, err := os.Stat(sessionPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"stat opencode session file %s: %w",
			sessionPath, err,
		)
	}

	root := filepath.Dir(filepath.Dir(filepath.Dir(
		filepath.Dir(sessionPath),
	)))
	messageRoot := filepath.Join(root, "storage", "message")
	partRoot := filepath.Join(root, "storage", "part")
	sessionID := strings.TrimSuffix(
		filepath.Base(sessionPath), filepath.Ext(sessionPath),
	)
	fileMtime := info.ModTime().UnixNano()

	messageDir := filepath.Join(root, "storage", "message", sessionID)
	fileMtime = max(fileMtime, statMtime(messageDir))
	var session struct {
		Directory string `json:"directory"`
	}
	if raw, readErr := os.ReadFile(sessionPath); readErr == nil {
		if json.Unmarshal(raw, &session) == nil &&
			!openCodeUsableWorktree(strings.TrimSpace(session.Directory)) {
			fileMtime = max(fileMtime, statMtime(openCodeProjectPath(sessionPath)))
		}
	}
	msgEntries, err := os.ReadDir(messageDir)
	if err != nil {
		if os.IsNotExist(err) {
			fileMtime = max(fileMtime, statMtime(messageRoot))
			return fileMtime, nil
		}
		return 0, fmt.Errorf(
			"reading opencode message dir %s: %w",
			messageDir, err,
		)
	}
	for _, entry := range msgEntries {
		if entry.IsDir() ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fileMtime = max(fileMtime, mustEntryMtime(entry))
		messageID := strings.TrimSuffix(
			entry.Name(), filepath.Ext(entry.Name()),
		)
		partDir := filepath.Join(root, "storage", "part", messageID)
		fileMtime = max(fileMtime, statMtime(partDir))
		partEntries, err := os.ReadDir(partDir)
		if err != nil {
			if os.IsNotExist(err) {
				fileMtime = max(fileMtime, statMtime(partRoot))
				continue
			}
			return 0, fmt.Errorf(
				"reading opencode part dir %s: %w",
				partDir, err,
			)
		}
		for _, partEntry := range partEntries {
			if partEntry.IsDir() ||
				!strings.HasSuffix(partEntry.Name(), ".json") {
				continue
			}
			fileMtime = max(fileMtime, mustEntryMtime(partEntry))
		}
	}

	return fileMtime, nil
}

func mustEntryMtime(entry os.DirEntry) int64 {
	info, err := entry.Info()
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func statMtime(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.ModTime().UnixNano()
}

func millisToTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
