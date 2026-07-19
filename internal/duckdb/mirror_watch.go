package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"
)

// WatchMirrorReplacement polls the Store's mirror file for a rebuild-driven
// atomic replacement (see swapMirrorFile in rebuild.go) and transparently
// reopens the serving handle when a schema-compatible replacement appears.
// Without this, a long-lived serve process would keep querying the old
// inode forever once a rebuild renamed a fresh file over it.
//
// It is a no-op when the Store was not opened from a local file path
// (NewStoreFromDB, remote/Quack stores have nothing to watch). onEvent, if
// non-nil, is called with a non-nil error whenever a stat, open, or schema
// compat check on a candidate replacement fails; the watcher keeps polling
// afterward, so a subsequent good file is still picked up. WatchMirrorReplacement
// returns immediately; the poll loop runs in a background goroutine until
// ctx is done.
func (s *Store) WatchMirrorReplacement(
	ctx context.Context, interval time.Duration, onEvent func(err error),
) {
	if s.path == "" {
		return
	}
	go s.watchMirrorReplacementLoop(ctx, interval, onEvent)
}

func (s *Store) watchMirrorReplacementLoop(
	ctx context.Context, interval time.Duration, onEvent func(err error),
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkMirrorReplacement(ctx, onEvent)
		}
	}
}

// checkMirrorReplacement stats the mirror path once and, if the inode
// changed since the currently open handle was opened, tries to open and
// validate the new file, then adopt it. A stat error (the file is briefly
// missing mid-rename) is treated as "no change yet" rather than an error:
// the watcher keeps serving the old handle and checks again on the next
// tick.
func (s *Store) checkMirrorReplacement(ctx context.Context, onEvent func(err error)) {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}

	s.handleMu.RLock()
	prev := s.fileInfo
	s.handleMu.RUnlock()
	if prev != nil && os.SameFile(prev, info) {
		return
	}

	conn, alias, err := openMirrorAlias(s.path)
	if err != nil {
		reportMirrorReplacementEvent(
			onEvent, fmt.Errorf("opening replaced duckdb mirror: %w", err),
		)
		return
	}
	if err := CheckSchemaCompat(ctx, conn); err != nil {
		_ = conn.Close()
		removeMirrorAlias(alias)
		reportMirrorReplacementEvent(
			onEvent, fmt.Errorf("replaced duckdb mirror incompatible: %w", err),
		)
		return
	}
	s.swapHandle(conn, alias, info)
}

// openMirrorAlias opens path's *current* contents through a private
// hardlink instead of path itself.
//
// This works around a DuckDB constraint verified empirically against the
// duckdb-go driver: a process may not have two independent connections
// open on the same literal path string at once. Opening path again while
// the Store's original connection is still open either errors ("Can't
// open a connection to same database file with a different configuration
// than existing connections") when the DSN differs, or, when it matches,
// silently returns the *already open, stale* database instance instead of
// re-reading the file rebuildMirror just renamed into place. A hardlink
// gives the identical bytes a distinct path string, so DuckDB treats it as
// an unrelated database and genuinely reads the current file from disk,
// while the Store's existing connection (opened on the literal path
// before the rename) keeps serving whatever it already had open,
// undisturbed.
//
// The caller must Close the returned *sql.DB and then call
// removeMirrorAlias(alias) once it is done with the connection.
func openMirrorAlias(path string) (conn *sql.DB, alias string, err error) {
	alias = fmt.Sprintf("%s.reopen-%d", path, time.Now().UnixNano())
	if err := os.Link(path, alias); err != nil {
		return nil, "", fmt.Errorf("hardlinking duckdb mirror for reopen: %w", err)
	}
	conn, err = Open(alias)
	if err != nil {
		_ = os.Remove(alias)
		return nil, "", err
	}
	return conn, alias, nil
}

// removeMirrorAlias best-effort removes a hardlink created by
// openMirrorAlias. alias is "" for a Store's original path-opened
// connection (NewStore never aliases its own first open), which is a
// deliberate no-op: that path is the mirror file itself and must never be
// removed.
func removeMirrorAlias(alias string) {
	if alias == "" {
		return
	}
	if err := os.Remove(alias); err != nil && !os.IsNotExist(err) {
		log.Printf("duckdb serve: removing mirror reopen alias %s: %v", alias, err)
	}
}

// swapHandle installs the new handle and its backing alias under a write
// lock, then closes the old handle and removes its alias (if any) after
// releasing the lock. sql.DB.Close waits for connections currently in use
// to be released before returning, so any query already running against
// the old handle completes normally; only new queries see the
// replacement.
func (s *Store) swapHandle(conn *sql.DB, alias string, info os.FileInfo) {
	s.handleMu.Lock()
	oldConn := s.duck
	oldAlias := s.aliasPath
	s.duck = conn
	s.aliasPath = alias
	s.fileInfo = info
	s.handleMu.Unlock()

	if err := oldConn.Close(); err != nil {
		log.Printf("duckdb serve: closing replaced mirror handle: %v", err)
	}
	removeMirrorAlias(oldAlias)
}

func reportMirrorReplacementEvent(onEvent func(err error), err error) {
	if onEvent != nil {
		onEvent(err)
	}
}
