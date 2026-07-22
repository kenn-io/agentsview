package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/parser"
)

const reconciliationPageSize = 256

type reconciliationCandidate struct {
	Provider       parser.AgentType
	Identity       string
	Path           string
	StoredPath     string
	MemberIdentity string
	WatchRoot      string
	Project        string
	Preference1    int64
	Preference2    int64
	Preference3    int64
}

func (candidate reconciliationCandidate) Cursor() reconciliationCursor {
	return reconciliationCursor{
		Provider: candidate.Provider,
		Identity: candidate.Identity,
	}
}

type reconciliationCursor struct {
	Provider parser.AgentType
	Identity string
}

// ReconciliationMetrics reports the largest bounded batches retained while a
// watcher-forced reconciliation was running.
type ReconciliationMetrics struct {
	MaxSpoolPageRows         int
	MaxProviderBuffered      int
	MaxRehydratedSources     int
	MaxWorkerResults         int
	MaxPendingWrites         int
	ExcludedRemoteRoots      int
	GlobalLinkPasses         int
	MaxProviderRetainedBytes int64
	SharedContainerScans     int
	OpenCodeSQLiteParses     int
	// CodexReplacementIndexBuilds counts uuid-to-path replacement index
	// builds during missing-path tombstoning. A streamed pass answers
	// replacement lookups from its discovery spool, so this stays zero;
	// only a spool-less pass builds the fallback index, at most once
	// regardless of how many Codex sources vanished.
	CodexReplacementIndexBuilds int
}

type reconciliationSpool struct {
	path string
	db   *sql.DB

	mu      sync.Mutex
	closed  bool
	sealed  bool
	metrics ReconciliationMetrics
}

type reconciliationSpoolStore interface {
	Add(context.Context, reconciliationCandidate) error
	Candidate(context.Context, parser.AgentType, string) (reconciliationCandidate, bool, error)
	ContainsSource(context.Context, parser.AgentType, string) (bool, error)
	ContainsSourceIdentity(context.Context, parser.AgentType, string, string) (bool, error)
	Page(context.Context, reconciliationCursor, int) ([]reconciliationCandidate, error)
	Metrics() ReconciliationMetrics
	CloseAndRemove() error
}

func (spool *reconciliationSpool) Candidate(
	ctx context.Context, provider parser.AgentType, identity string,
) (reconciliationCandidate, bool, error) {
	if err := spool.seal(ctx); err != nil {
		return reconciliationCandidate{}, false, err
	}
	var candidate reconciliationCandidate
	var providerName string
	err := spool.db.QueryRowContext(ctx, `
		SELECT provider, identity, path, stored_path, member_identity, watch_root, project,
		       preference_1, preference_2, preference_3
		FROM candidates
		WHERE provider = ? AND identity = ?
	`, string(provider), identity).Scan(
		&providerName, &candidate.Identity, &candidate.Path, &candidate.StoredPath,
		&candidate.MemberIdentity,
		&candidate.WatchRoot, &candidate.Project,
		&candidate.Preference1, &candidate.Preference2,
		&candidate.Preference3,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return reconciliationCandidate{}, false, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return reconciliationCandidate{}, false, ctxErr
		}
		return reconciliationCandidate{}, false, fmt.Errorf(
			"query reconciliation candidate: %w", err,
		)
	}
	candidate.Provider = parser.AgentType(providerName)
	return candidate, true, nil
}

func (spool *reconciliationSpool) ContainsSource(
	ctx context.Context, provider parser.AgentType, storedPath string,
) (bool, error) {
	if err := spool.seal(ctx); err != nil {
		return false, err
	}
	var found int
	err := spool.db.QueryRowContext(ctx, `
		SELECT 1
		FROM candidates
		WHERE provider = ? AND stored_path = ?
		LIMIT 1
	`, string(provider), storedPath).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("query reconciliation source membership: %w", err)
	}
	return true, nil
}

func (spool *reconciliationSpool) ContainsSourceIdentity(
	ctx context.Context, provider parser.AgentType, storedPath, identity string,
) (bool, error) {
	if err := spool.seal(ctx); err != nil {
		return false, err
	}
	var found int
	err := spool.db.QueryRowContext(ctx, `
		SELECT 1
		FROM candidates
		WHERE provider = ? AND stored_path = ? AND member_identity = ?
		LIMIT 1
	`, string(provider), storedPath, identity).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		return false, fmt.Errorf("query reconciliation source identity membership: %w", err)
	}
	return true, nil
}

func newReconciliationSpool(archivePath string) (*reconciliationSpool, error) {
	dir := filepath.Dir(archivePath)
	file, err := os.CreateTemp(dir, ".agentsview-reconcile-*.db")
	if err != nil {
		return nil, fmt.Errorf("create reconciliation spool: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("close reconciliation spool placeholder: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("secure reconciliation spool: %w", err)
	}

	dsn := reconciliationSpoolDSN(path)
	database, err := sql.Open("sqlite3", dsn)
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("open reconciliation spool: %w", err)
	}
	database.SetMaxOpenConns(1)
	spool := &reconciliationSpool{path: path, db: database}
	if err := spool.initialize(); err != nil {
		_ = spool.CloseAndRemove()
		return nil, err
	}
	return spool, nil
}

func reconciliationSpoolDSN(path string) string {
	uri := ""
	switch {
	case windowsDrivePath(path):
		slashPath := "/" + strings.ReplaceAll(path, `\`, "/")
		uri = "file:" + (&url.URL{Path: slashPath}).EscapedPath()
	case strings.HasPrefix(path, `\\`):
		// SQLite rejects non-local file URI authorities unless built with
		// SQLITE_ALLOW_URI_AUTHORITY. The ordinary Windows filename preserves
		// UNC semantics without requiring that optional compile-time feature.
		return path
	default:
		uri = "file:" + (&url.URL{Path: path}).EscapedPath()
	}
	return uri
}

func windowsDrivePath(path string) bool {
	if len(path) < 3 || path[1] != ':' || (path[2] != '\\' && path[2] != '/') {
		return false
	}
	return path[0] >= 'A' && path[0] <= 'Z' || path[0] >= 'a' && path[0] <= 'z'
}

func (spool *reconciliationSpool) initialize() error {
	_, err := spool.db.Exec(`
		PRAGMA busy_timeout = 5000;
		PRAGMA journal_mode = WAL;
		PRAGMA synchronous = NORMAL;
		PRAGMA temp_store = FILE;
		CREATE TABLE candidates (
			provider TEXT NOT NULL,
			identity TEXT NOT NULL,
			path TEXT NOT NULL,
			stored_path TEXT NOT NULL,
			member_identity TEXT NOT NULL,
			watch_root TEXT NOT NULL,
			project TEXT NOT NULL,
			preference_1 INTEGER NOT NULL,
			preference_2 INTEGER NOT NULL,
			preference_3 INTEGER NOT NULL,
			PRIMARY KEY (provider, identity)
		) WITHOUT ROWID;
		CREATE INDEX candidates_by_stored_path
			ON candidates(provider, stored_path, member_identity);
		BEGIN IMMEDIATE;
	`)
	if err != nil {
		return fmt.Errorf("initialize reconciliation spool: %w", err)
	}
	return nil
}

func (spool *reconciliationSpool) Add(
	ctx context.Context, candidate reconciliationCandidate,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	spool.mu.Lock()
	sealed := spool.sealed
	spool.mu.Unlock()
	if sealed {
		return errors.New("reconciliation spool is sealed")
	}
	_, err := spool.db.ExecContext(ctx, `
		INSERT INTO candidates (
			provider, identity, path, stored_path, member_identity, watch_root, project,
			preference_1, preference_2, preference_3
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider, identity) DO UPDATE SET
			path = excluded.path,
			stored_path = excluded.stored_path,
			member_identity = excluded.member_identity,
			watch_root = excluded.watch_root,
			project = excluded.project,
			preference_1 = excluded.preference_1,
			preference_2 = excluded.preference_2,
			preference_3 = excluded.preference_3
		WHERE excluded.preference_1 > candidates.preference_1
		   OR (excluded.preference_1 = candidates.preference_1
		       AND excluded.preference_2 > candidates.preference_2)
		   OR (excluded.preference_1 = candidates.preference_1
		       AND excluded.preference_2 = candidates.preference_2
		       AND excluded.preference_3 > candidates.preference_3)
		   OR (excluded.preference_1 = candidates.preference_1
		       AND excluded.preference_2 = candidates.preference_2
		       AND excluded.preference_3 = candidates.preference_3
		       AND excluded.path < candidates.path)
	`, string(candidate.Provider), candidate.Identity, candidate.Path,
		candidate.StoredPath, candidate.MemberIdentity,
		candidate.WatchRoot, candidate.Project, candidate.Preference1,
		candidate.Preference2, candidate.Preference3)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("write reconciliation candidate: %w", err)
	}
	return nil
}

func (spool *reconciliationSpool) Page(
	ctx context.Context, after reconciliationCursor, limit int,
) ([]reconciliationCandidate, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := spool.seal(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > reconciliationPageSize {
		limit = reconciliationPageSize
	}
	rows, err := spool.db.QueryContext(ctx, `
		SELECT provider, identity, path, stored_path, member_identity, watch_root, project,
		       preference_1, preference_2, preference_3
		FROM candidates
		WHERE provider > ? OR (provider = ? AND identity > ?)
		ORDER BY provider, identity
		LIMIT ?
	`, string(after.Provider), string(after.Provider), after.Identity, limit)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("query reconciliation candidates: %w", err)
	}
	defer rows.Close()

	page := make([]reconciliationCandidate, 0, limit)
	for rows.Next() {
		var candidate reconciliationCandidate
		var provider string
		if err := rows.Scan(
			&provider, &candidate.Identity, &candidate.Path, &candidate.StoredPath,
			&candidate.MemberIdentity,
			&candidate.WatchRoot, &candidate.Project,
			&candidate.Preference1, &candidate.Preference2,
			&candidate.Preference3,
		); err != nil {
			return nil, fmt.Errorf("scan reconciliation candidate: %w", err)
		}
		candidate.Provider = parser.AgentType(provider)
		page = append(page, candidate)
	}
	if err := rows.Err(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("iterate reconciliation candidates: %w", err)
	}
	spool.mu.Lock()
	spool.metrics.MaxSpoolPageRows = max(spool.metrics.MaxSpoolPageRows, len(page))
	spool.mu.Unlock()
	return page, nil
}

func (spool *reconciliationSpool) seal(ctx context.Context) error {
	spool.mu.Lock()
	if spool.sealed {
		spool.mu.Unlock()
		return nil
	}
	spool.mu.Unlock()
	if _, err := spool.db.ExecContext(ctx, "COMMIT"); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("commit reconciliation candidates: %w", err)
	}
	spool.mu.Lock()
	spool.sealed = true
	spool.mu.Unlock()
	return nil
}

func (spool *reconciliationSpool) Metrics() ReconciliationMetrics {
	spool.mu.Lock()
	defer spool.mu.Unlock()
	return spool.metrics
}

func (spool *reconciliationSpool) closeDB() error {
	spool.mu.Lock()
	if spool.closed {
		spool.mu.Unlock()
		return nil
	}
	spool.closed = true
	sealed := spool.sealed
	spool.mu.Unlock()
	if !sealed {
		_, _ = spool.db.Exec("ROLLBACK")
	}
	return spool.db.Close()
}

func (spool *reconciliationSpool) CloseAndRemove() error {
	closeErr := spool.closeDB()
	var removeErr error
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(spool.path + suffix); err != nil &&
			!errors.Is(err, os.ErrNotExist) && removeErr == nil {
			removeErr = err
		}
	}
	return errors.Join(closeErr, removeErr)
}
