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

func writeSQLiteSnapshot(dstPath, srcPath string) (err error) {
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

	srcConn, err := src.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("connect sqlite snapshot source: %w", err)
	}
	defer func() { err = errors.Join(err, srcConn.Close()) }()
	dstConn, err := dst.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("connect sqlite snapshot destination: %w", err)
	}
	defer func() { err = errors.Join(err, dstConn.Close()) }()

	err = dstConn.Raw(func(dstDriver any) error {
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
			done, stepErr := backup.Step(-1)
			finishErr := backup.Finish()
			if stepErr != nil || finishErr != nil {
				return fmt.Errorf(
					"copy sqlite online backup: %w",
					errors.Join(stepErr, finishErr),
				)
			}
			if !done {
				return fmt.Errorf("copy sqlite online backup: backup incomplete")
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	if _, err := dstConn.ExecContext(
		context.Background(), `PRAGMA journal_mode = DELETE`,
	); err != nil {
		return fmt.Errorf("finalize standalone sqlite snapshot: %w", err)
	}
	complete = true
	return nil
}

func sqliteSnapshotIdentity(path string) (int64, time.Time, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, time.Time{}, false, nil
		}
		return 0, time.Time{}, false, fmt.Errorf("stat sqlite database %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return 0, time.Time{}, false, nil
	}
	conn, err := sql.Open("sqlite3", sqliteReadOnlyDSN(path))
	if err != nil {
		return 0, time.Time{}, false, fmt.Errorf("open sqlite database %q: %w", path, err)
	}
	defer conn.Close()
	var pageSize, pageCount int64
	if err := conn.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read sqlite page size %q: %w", path, err)
	}
	if err := conn.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, time.Time{}, false, fmt.Errorf("read sqlite page count %q: %w", path, err)
	}
	return pageSize * pageCount, sqliteSnapshotModTime(path, info.ModTime()), true, nil
}

// hermesSQLiteSnapshotIdentity treats an unusable state database as an
// optional missing archive component. Hermes transcripts remain independently
// useful, and a later sync can retry the database after the writer repairs or
// replaces it.
func hermesSQLiteSnapshotIdentity(path string) (int64, time.Time, bool) {
	size, modTime, exists, err := sqliteSnapshotIdentity(path)
	if err != nil {
		return 0, time.Time{}, false
	}
	return size, modTime, exists
}

func sqliteSnapshotModTime(stateDB string, modTime time.Time) time.Time {
	for _, suffix := range []string{"-wal", "-journal"} {
		info, err := os.Lstat(stateDB + suffix)
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
	extraFiles := make(map[string]struct{}, len(targets.ExtraFiles))
	for _, path := range targets.ExtraFiles {
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

func hermesSQLitePaths(stateDB string) []string {
	return []string{
		filepath.Clean(stateDB),
		filepath.Clean(stateDB + "-wal"),
		filepath.Clean(stateDB + "-shm"),
		filepath.Clean(stateDB + "-journal"),
	}
}

func hermesStateDBForArchivePath(path string) (string, bool) {
	path = filepath.Clean(path)
	switch filepath.Base(path) {
	case "state.db":
		return path, true
	case "state.db-wal":
		return strings.TrimSuffix(path, "-wal"), true
	case "state.db-shm":
		return strings.TrimSuffix(path, "-shm"), true
	case "state.db-journal":
		return strings.TrimSuffix(path, "-journal"), true
	default:
		return "", false
	}
}
