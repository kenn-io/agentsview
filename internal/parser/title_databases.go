package parser

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Shared title databases hold the human-facing conversation title that the
// IDE shows, but live outside the provider's session roots: Antigravity keeps
// one conversation_summaries.db per IDE root, and Qoder keeps one chat_sessions
// table inside the application's main.sqlite under Application Support. Both
// are shared by every session of that client, so a write to one of them is a
// title-only change for an unknown set of already-imported sessions.
//
// This file owns the two facts every caller needs: how to recognize such a
// path, and how to read its (id, title) pairs. Discovery and parsing callers
// read rows through ReadSharedTitles; the sync engine plans a refresh task
// from the recognized path and re-reads under its write lock.
const (
	antigravitySummariesDBName = "conversation_summaries.db"
	qoderAppDatabaseName       = "main.sqlite"
)

// Qoder application directories. The CN build has its own database, so a
// session must be read from the database of the client that owns it.
const (
	qoderCNClientApp   = "com.qodercn.app.stable"
	qoderIntlClientApp = "com.qoder.app.stable"
)

// qoderClientDirs lists the Qoder client session directories, in a stable
// order, each mapped to the application directory that owns its titles. The CN
// build is listed first so a machine with both installed resolves
// deterministically.
//
// These are matched as whole path segments, never as substrings: .qoderwork is
// a separate legacy export directory and must not be read as .qoder. Note the
// client directory is usually not the last segment of a configured root --
// every default root ends in "projects" (<client>/projects).
var qoderClientDirs = []struct {
	Dir string
	App string
}{
	{Dir: ".qoder-cn", App: qoderCNClientApp},
	{Dir: ".qoder", App: qoderIntlClientApp},
}

// SharedTitleDatabase identifies one recognized shared title database.
type SharedTitleDatabase struct {
	Agent AgentType
	// DBPath is the normalized main database file. A "-wal" event resolves to
	// this same path so the caller never opens the write-ahead log itself.
	DBPath string
}

// IDPrefix returns the provider's session ID prefix, which must be applied to
// the bare database id before it can be compared with sessions.id.
func (d SharedTitleDatabase) IDPrefix() string {
	switch d.Agent {
	case AgentAntigravity:
		return antigravityIDPrefix
	case AgentQoder:
		return qoderIDPrefix
	default:
		return ""
	}
}

// SharedTitleRecord is one (bare id, title) pair read from a shared title
// database. Title is nil when the stored value is SQL NULL, which is not a
// signal to clear an existing name.
type SharedTitleRecord struct {
	ID    string
	Title *string
}

// AntigravityTitleDatabasePath returns the summaries database path for an
// Antigravity IDE root.
func AntigravityTitleDatabasePath(root string) string {
	if root == "" {
		return ""
	}
	return filepath.Join(root, antigravitySummariesDBName)
}

// QoderTitleDatabasePaths returns the existing Qoder application databases, in
// qoderClientDirs order. Missing builds are omitted rather than guessed, so
// unsupported platforms contribute nothing.
func QoderTitleDatabasePaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var paths []string
	for _, client := range qoderClientDirs {
		path := filepath.Join(
			home, "Library", "Application Support", client.App,
			qoderAppDatabaseName,
		)
		if !IsRegularFile(path) {
			continue
		}
		paths = append(paths, path)
	}
	return paths
}

// qoderTitleDatabasePathsForRoots returns the title databases reachable from
// the given Qoder session roots, deduplicated in root order. A root that
// belongs to neither client directory contributes nothing: a custom or
// mirrored root must not read this machine's application database.
func qoderTitleDatabasePathsForRoots(roots []string) []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var paths []string
	for _, root := range roots {
		app := qoderAppForRoot(root)
		if app == "" {
			continue
		}
		path := filepath.Join(
			home, "Library", "Application Support", app, qoderAppDatabaseName,
		)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	return paths
}

// qoderAppForRoot maps a configured Qoder session root to the application
// directory that owns its titles, or "" when the root belongs to neither
// client. See QoderClientAppForPath for the binding rule.
func qoderAppForRoot(root string) string {
	return QoderLocalClientAppForPath(root)
}

// QoderClientAppForPath returns the Qoder application directory that owns the
// titles for a session root or transcript path, or "" when the path is under
// neither client directory.
//
// The binding walks the whole path from the deepest segment outward and takes
// the nearest exact match, because the client directory is not the last
// segment: every default root is <client>/projects. Matching is by whole
// segment, never by substring, so .qoderwork is not read as .qoder.
func QoderClientAppForPath(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	for {
		segment := filepath.Base(clean)
		for _, client := range qoderClientDirs {
			if segment == client.Dir {
				return client.App
			}
		}
		parent := filepath.Dir(clean)
		if parent == clean {
			return ""
		}
		clean = parent
	}
}

// QoderLocalClientAppForPath binds only this user's native client tree.
// A mirrored .qoder directory or a logical host:path is not a local owner.
func QoderLocalClientAppForPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(path) {
		return ""
	}
	clean := filepath.Clean(path)
	for _, client := range qoderClientDirs {
		root := filepath.Join(home, client.Dir)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			return client.App
		}
	}
	return ""
}

// QoderTitleDatabaseForLocalSource returns the database of a verified native
// local source, or empty when the source belongs to a mirror or another host.
func QoderTitleDatabaseForLocalSource(path string) string {
	app := QoderLocalClientAppForPath(path)
	if app == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", app, qoderAppDatabaseName)
}

// SharedTitleDatabaseForChangedPath recognizes a changed filesystem path as
// one of the shared title databases and returns the database to refresh.
// A "-wal" event resolves to its main database; "-shm" and unrelated files are
// rejected so an index rewrite never schedules a refresh.
func SharedTitleDatabaseForChangedPath(path string) (SharedTitleDatabase, bool) {
	if path == "" {
		return SharedTitleDatabase{}, false
	}
	clean := filepath.Clean(path)
	if strings.HasSuffix(clean, "-shm") {
		return SharedTitleDatabase{}, false
	}
	main := strings.TrimSuffix(clean, "-wal")
	base := filepath.Base(main)
	switch base {
	case antigravitySummariesDBName:
		if !IsRegularFile(main) {
			return SharedTitleDatabase{}, false
		}
		return SharedTitleDatabase{Agent: AgentAntigravity, DBPath: main}, true
	case qoderAppDatabaseName:
		if !isQoderTitleDatabaseDir(filepath.Dir(main)) || !IsRegularFile(main) {
			return SharedTitleDatabase{}, false
		}
		return SharedTitleDatabase{Agent: AgentQoder, DBPath: main}, true
	default:
		return SharedTitleDatabase{}, false
	}
}

// isQoderTitleDatabaseDir reports whether dir is one of the Qoder application
// directories whose main.sqlite carries conversation titles.
func isQoderTitleDatabaseDir(dir string) bool {
	clean := filepath.Clean(dir)
	for _, client := range qoderClientDirs {
		if strings.HasSuffix(clean, string(filepath.Separator)+client.App) {
			return true
		}
	}
	return false
}

// SharedTitleDatabasesForRoots returns the shared title databases owned by the
// given provider's configured roots: one conversation_summaries.db per
// Antigravity root, and one application database per recognized Qoder root.
//
// Only databases that exist right now are returned, so a periodic sweep can
// treat "absent" as no signal and pick the database up on the cycle after the
// client creates it. The result is deduplicated and sorted so two sweeps over
// the same roots agree on order.
func SharedTitleDatabasesForRoots(
	agent AgentType, roots []string,
) []SharedTitleDatabase {
	var paths []string
	switch agent {
	case AgentAntigravity:
		for _, root := range roots {
			if path := AntigravityTitleDatabasePath(root); path != "" {
				paths = append(paths, path)
			}
		}
	case AgentQoder:
		paths = qoderTitleDatabasePathsForRoots(roots)
	default:
		return nil
	}
	seen := make(map[string]struct{}, len(paths))
	databases := make([]SharedTitleDatabase, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if !IsRegularFile(path) {
			continue
		}
		databases = append(databases, SharedTitleDatabase{
			Agent: agent, DBPath: path,
		})
	}
	slices.SortFunc(databases, func(a, b SharedTitleDatabase) int {
		return strings.Compare(a.DBPath, b.DBPath)
	})
	return databases
}

// ReadSharedTitles reads every (id, title) row from a shared title database.
// A read error is returned so the caller can keep the stored names and retry;
// an empty result is a valid "no rows" answer, not an error.
func ReadSharedTitles(
	ctx context.Context, database SharedTitleDatabase,
) ([]SharedTitleRecord, error) {
	if database.DBPath == "" {
		return nil, nil
	}
	db, err := openSQLiteReadOnly(database.DBPath, sqliteReadOptions{busyTimeoutMS: 200})
	if err != nil {
		return nil, err
	}
	defer db.Close()

	query := ""
	switch database.Agent {
	case AgentAntigravity:
		query = "SELECT conversation_id, title FROM conversation_summaries"
	case AgentQoder:
		query = "SELECT session_id, title FROM chat_sessions"
	default:
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []SharedTitleRecord
	for rows.Next() {
		var (
			id    string
			title sql.NullString
		)
		if err := rows.Scan(&id, &title); err != nil {
			return nil, err
		}
		if id == "" {
			continue
		}
		record := SharedTitleRecord{ID: id}
		if title.Valid {
			clean := strings.TrimSpace(title.String)
			record.Title = &clean
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// readTitleFromDatabase reads one session's title from a shared title
// database, implementing the shared four-state contract: a non-empty title
// (or an explicitly present blank) reports present=true; a missing database,
// missing row, or SQL NULL reports present=false with a nil error so the
// caller preserves the stored name.
func readTitleFromDatabase(
	ctx context.Context, database SharedTitleDatabase, bareID string,
) (title string, present bool, err error) {
	if database.DBPath == "" || bareID == "" {
		return "", false, nil
	}
	if _, statErr := os.Stat(database.DBPath); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, statErr
	}
	db, err := openSQLiteReadOnly(database.DBPath, sqliteReadOptions{busyTimeoutMS: 200})
	if err != nil {
		return "", false, err
	}
	defer db.Close()

	query := ""
	switch database.Agent {
	case AgentAntigravity:
		query = "SELECT title FROM conversation_summaries WHERE conversation_id = ? LIMIT 1"
	case AgentQoder:
		query = "SELECT title FROM chat_sessions WHERE session_id = ? LIMIT 1"
	default:
		return "", false, nil
	}
	var stored sql.NullString
	err = db.QueryRowContext(ctx, query, bareID).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !stored.Valid {
		return "", false, nil
	}
	return strings.TrimSpace(stored.String), true, nil
}
