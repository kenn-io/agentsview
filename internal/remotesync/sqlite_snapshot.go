package remotesync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
	"go.kenn.io/agentsview/internal/parser"
)

const sqliteSnapshotBusyTimeoutMS = 5000

// sqliteBackupPagesPerStep bounds how long one backup step runs, so a
// cancelled context is observed between steps.
const sqliteBackupPagesPerStep = 1024

// sqliteBackupStallSteps bounds consecutive backup steps that copy no
// net pages. The source read transaction pins one snapshot, so a healthy
// backup shrinks Remaining on every step; a step that does not is a BUSY
// or LOCKED retry, or a restart if the snapshot was lost. A thousand
// such steps rides out brief lock contention, yet stops a restart loop
// after re-copying at most about 4 GiB of 4 KiB pages instead of never.
const sqliteBackupStallSteps = 1000

// afterSQLiteBackupStep is a test hook called after each completed step.
var afterSQLiteBackupStep func()

// sqliteBackupSteps is the part of *sqlite3.SQLiteBackup the copy loop
// drives.
type sqliteBackupSteps interface {
	Step(pages int) (bool, error)
	Remaining() int
	Finish() error
}

func writeSQLiteSnapshot(ctx context.Context, dstPath, srcPath string) (err error) {
	if err := os.Remove(dstPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("replace sqlite snapshot: %w", err)
	}
	src, err := sql.Open("sqlite3", sqliteReadOnlyDSN(srcPath))
	if err != nil {
		return fmt.Errorf("open sqlite snapshot source: %w", err)
	}
	defer func() { err = errors.Join(err, src.Close()) }()
	dst, err := sql.Open("sqlite3", dstPath)
	if err != nil {
		return fmt.Errorf("open sqlite snapshot destination: %w", err)
	}
	complete := false
	defer func() {
		err = errors.Join(err, dst.Close())
		if !complete {
			_ = os.Remove(dstPath)
		}
	}()

	srcConn, err := src.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect sqlite snapshot source: %w", err)
	}
	defer func() { err = errors.Join(err, srcConn.Close()) }()
	dstConn, err := dst.Conn(ctx)
	if err != nil {
		return fmt.Errorf("connect sqlite snapshot destination: %w", err)
	}
	defer func() { err = errors.Join(err, dstConn.Close()) }()

	if err := backupPinnedSQLiteSource(ctx, dstConn, srcConn); err != nil {
		return err
	}
	if _, err := dstConn.ExecContext(
		ctx, `PRAGMA journal_mode = DELETE`,
	); err != nil {
		return fmt.Errorf("finalize standalone sqlite snapshot: %w", err)
	}
	complete = true
	return nil
}

// backupPinnedSQLiteSource copies srcConn into dstConn inside one source
// read transaction. Backup steps reuse that transaction, so writes from
// other connections no longer restart the copy: in WAL mode writers keep
// committing past the pinned snapshot, and in rollback-journal mode they
// wait on the shared lock, as with a single-step backup.
func backupPinnedSQLiteSource(ctx context.Context, dstConn, srcConn *sql.Conn) (err error) {
	if _, err := srcConn.ExecContext(ctx, `BEGIN`); err != nil {
		return fmt.Errorf("begin sqlite snapshot read: %w", err)
	}
	defer func() {
		_, rollbackErr := srcConn.ExecContext(context.WithoutCancel(ctx), `ROLLBACK`)
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("end sqlite snapshot read: %w", rollbackErr))
		}
	}()
	var schemaRows int64
	if err := srcConn.QueryRowContext(
		ctx, `SELECT count(*) FROM sqlite_master`,
	).Scan(&schemaRows); err != nil {
		return fmt.Errorf("pin sqlite snapshot read: %w", err)
	}
	return dstConn.Raw(func(dstDriver any) error {
		dstSQLite, ok := dstDriver.(*sqlite3.SQLiteConn)
		if !ok {
			return fmt.Errorf("sqlite snapshot destination is %T", dstDriver)
		}
		return srcConn.Raw(func(srcDriver any) error {
			srcSQLite, ok := srcDriver.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("sqlite snapshot source is %T", srcDriver)
			}
			backup, err := dstSQLite.Backup("main", srcSQLite, "main")
			if err != nil {
				return fmt.Errorf("start sqlite online backup: %w", err)
			}
			return copySQLiteBackup(ctx, backup)
		})
	})
}

// copySQLiteBackup steps backup to completion and always finishes it.
func copySQLiteBackup(ctx context.Context, backup sqliteBackupSteps) error {
	lowest, stalled := -1, 0
	for {
		done, stepErr := backup.Step(sqliteBackupPagesPerStep)
		if stepErr != nil {
			return fmt.Errorf("copy sqlite online backup: %w",
				errors.Join(stepErr, backup.Finish()))
		}
		if done {
			break
		}
		if afterSQLiteBackupStep != nil {
			afterSQLiteBackupStep()
		}
		if err := ctx.Err(); err != nil {
			return errors.Join(err, backup.Finish())
		}
		if remaining := backup.Remaining(); lowest < 0 || remaining < lowest {
			lowest, stalled = remaining, 0
			continue
		}
		stalled++
		if stalled >= sqliteBackupStallSteps {
			return errors.Join(fmt.Errorf(
				"copy sqlite online backup: no progress after %d steps with %d pages left",
				stalled, lowest), backup.Finish())
		}
	}
	if err := backup.Finish(); err != nil {
		return fmt.Errorf("finish sqlite online backup: %w", err)
	}
	return nil
}

// sqliteSnapshotIdentity treats an unusable database as an optional
// missing archive component. The SQLite archive preserves imported
// sessions even when their source files disappear, so omitting an
// unreadable database from the manifest costs only the mirror's cached
// copy, and a later sync retries after the writer repairs or replaces
// it. Failing the manifest instead would block every other agent's
// sync behind one bad file.
func sqliteSnapshotIdentity(ctx context.Context, path string) (int64, time.Time, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return 0, time.Time{}, false
	}
	conn, err := sql.Open("sqlite3", sqliteReadOnlyDSN(path))
	if err != nil {
		return 0, time.Time{}, false
	}
	defer conn.Close()
	var pageSize, pageCount int64
	if err := conn.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, time.Time{}, false
	}
	if err := conn.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, time.Time{}, false
	}
	return pageSize * pageCount, sqliteSnapshotModTime(path, info.ModTime()), true
}

func sqliteSnapshotModTime(stateDB string, modTime time.Time) time.Time {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Lstat(stateDB + suffix)
		if err == nil && suffix == "-wal" && !parser.SQLiteWALInfoHasFrames(info) {
			continue
		}
		if err == nil && info.Mode().IsRegular() && info.ModTime().After(modTime) {
			modTime = info.ModTime()
		}
	}
	return modTime
}

func sqliteReadOnlyDSN(path string) string {
	escaped := strings.NewReplacer(
		"%", "%25",
		"?", "%3F",
		"#", "%23",
	).Replace(path)
	return fmt.Sprintf(
		"file:%s?mode=ro&_busy_timeout=%d", escaped, sqliteSnapshotBusyTimeoutMS,
	)
}

func hermesStateDBTargets(targets TargetSet) []string {
	allExtraFiles := targets.AllExtraFiles()
	extraFiles := make(map[string]struct{}, len(allExtraFiles))
	for _, path := range allExtraFiles {
		extraFiles[filepath.Clean(path)] = struct{}{}
	}
	seen := make(map[string]struct{})
	for _, root := range targets.Dirs[parser.AgentHermes] {
		clean := filepath.Clean(root)
		switch filepath.Base(clean) {
		case "state.db":
			seen[clean] = struct{}{}
		case "sessions":
			stateDB := filepath.Join(filepath.Dir(clean), "state.db")
			if _, ok := extraFiles[stateDB]; ok {
				seen[stateDB] = struct{}{}
			}
		}
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// Snapshot stores whose WAL can disappear between manifest and archive transfer.
func sqliteSnapshotTargets(targets TargetSet) map[string]struct{} {
	out := make(map[string]struct{})
	for _, path := range hermesStateDBTargets(targets) {
		out[path] = struct{}{}
	}
	for _, path := range targets.Files[parser.AgentZed] {
		if filepath.Base(filepath.Clean(path)) == "threads.db" {
			out[filepath.Clean(path)] = struct{}{}
		}
	}
	for _, root := range targets.Dirs[parser.AgentAntigravity] {
		out[filepath.Join(root, parser.AntigravitySummariesDBName)] = struct{}{}
	}
	return out
}

func sqliteSnapshotPaths(stateDB string) []string {
	return []string{
		filepath.Clean(stateDB),
		filepath.Clean(stateDB + "-wal"),
		filepath.Clean(stateDB + "-shm"),
		filepath.Clean(stateDB + "-journal"),
	}
}

func sqliteSnapshotForArchivePath(path string) (string, bool) {
	path = filepath.Clean(path)
	switch filepath.Base(path) {
	case "state.db", "threads.db", parser.AntigravitySummariesDBName:
		return path, true
	case "state.db-wal", "threads.db-wal", parser.AntigravitySummariesDBName + "-wal":
		return strings.TrimSuffix(path, "-wal"), true
	case "state.db-shm", "threads.db-shm", parser.AntigravitySummariesDBName + "-shm":
		return strings.TrimSuffix(path, "-shm"), true
	case "state.db-journal", "threads.db-journal", parser.AntigravitySummariesDBName + "-journal":
		return strings.TrimSuffix(path, "-journal"), true
	default:
		return "", false
	}
}
