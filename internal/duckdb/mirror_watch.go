package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
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

// SameMirrorFile reports whether two Stat results describe the same
// unchanged mirror file. It is deliberately stricter than os.SameFile
// alone: on Windows, a FileInfo returned by os.Stat loads its underlying
// file identity LAZILY, at the first os.SameFile call. If the file at the
// path was already rename-replaced before that first call, the "old"
// FileInfo resolves the NEW file's identity and os.SameFile reports the
// two as equal forever, so a replacement watcher polling with os.SameFile
// alone can never observe the swap. ModTime and Size are captured eagerly
// by Stat on every platform, so also requiring them to match detects a
// swapped-in rebuilt mirror (different mtime) even when the lazily loaded
// identity lies.
func SameMirrorFile(a, b os.FileInfo) bool {
	if a == nil || b == nil {
		return false
	}
	return os.SameFile(a, b) &&
		a.ModTime().Equal(b.ModTime()) &&
		a.Size() == b.Size()
}

// PrimeFileIdentity forces info to load and cache its underlying file
// identity immediately. The ModTime/Size fallback in SameMirrorFile is not
// sufficient on its own: Windows stamps file times from the system clock,
// which advances in ~15.6ms ticks, so a file written and replaced within
// one tick carries the original's ModTime, and a same-size replacement
// then matches on every eagerly captured field while the lazily loaded
// identity resolves to the new file (see SameMirrorFile). Self-comparing
// the FileInfo triggers the lazy load while the path still points at the
// file this info describes, making later identity comparisons reliable.
// Call it at every baseline capture (store open, watcher adoption, serve
// loop). Harmless on platforms that fill the identity eagerly.
func PrimeFileIdentity(info os.FileInfo) {
	if info != nil {
		_ = os.SameFile(info, info)
	}
}

// checkMirrorReplacement stats the mirror path once and, if the file's
// identity changed since the currently open handle was opened (see
// SameMirrorFile), tries to open and validate the new file, then adopt it.
// A stat error (the file is briefly missing mid-rename) is treated as "no
// change yet" rather than an error: the watcher keeps serving the old
// handle and checks again on the next tick.
func (s *Store) checkMirrorReplacement(ctx context.Context, onEvent func(err error)) {
	info, err := os.Stat(s.path)
	if err != nil {
		return
	}

	s.handleMu.RLock()
	prev := s.fileInfo
	s.handleMu.RUnlock()
	if prev != nil && SameMirrorFile(prev, info) {
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
	PrimeFileIdentity(info)
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

// SweepStaleMirrorReopenAliases removes every path.reopen-N hardlink left
// next to path. Callers hold each alias only for the lifetime of the *sql.DB
// connection opened on it (see openMirrorAlias/removeMirrorAlias), so any
// alias still present is a leftover from a process that crashed or was
// killed before it could clean up after itself; it is always safe to remove
// unconditionally (there is no in-progress swap to race: an alias belonging
// to a live process's *current* handle is only ever known to that process,
// which will remove it itself on the next successful reopen or on Close).
// This is meant to be called once, before a serve process opens its own
// first handle on path — not from a running Store, which already sweeps its
// own alias in Close and in the mirror-replacement swap.
//
// This walks the parent directory with os.ReadDir and matches names by
// literal prefix instead of filepath.Glob(path+".reopen-*"): path is
// interpolated into the pattern, and glob metacharacters ([, ?, *) in a
// project or archive directory name would otherwise be interpreted as glob
// syntax instead of literal characters, silently breaking or over-matching
// the sweep. The suffix after the prefix must be entirely ASCII digits —
// the exact shape openMirrorAlias generates (time.Now().UnixNano()) — so a
// user file that merely shares the prefix (for example
// "mirror.duckdb.reopen-backup") is never deleted (see
// isGeneratedSweepName).
func SweepStaleMirrorReopenAliases(path string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + ".reopen-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading duckdb mirror directory %s: %w", dir, err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() ||
			!isGeneratedSweepName(entry.Name(), prefix) {
			continue
		}
		m := filepath.Join(dir, entry.Name())
		if err := os.Remove(m); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale duckdb mirror reopen alias %s: %w", m, err)
		}
	}
	return nil
}
