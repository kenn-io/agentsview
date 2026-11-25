package parser

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrUnsupportedCodexStateIndex marks a newer index or schema migration.
var ErrUnsupportedCodexStateIndex = errors.New("unsupported Codex state index")

// CodexIndexedSessionKey reads a thread's current rollout from the supported
// state_5.sqlite thread index in a Codex home and returns its session key.
// It returns an error for a newer index, os.ErrNotExist when the home has no
// initialized supported index, and sql.ErrNoRows when the index has no row
// for the thread.
func CodexIndexedSessionKey(ctx context.Context, home, thread string) (string, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "state_") || !strings.HasSuffix(name, ".sqlite") {
			continue
		}
		version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "state_"), ".sqlite"))
		if err == nil && version > 5 {
			return "", fmt.Errorf("%w %s", ErrUnsupportedCodexStateIndex, name)
		}
	}
	state := filepath.Join(home, "state_5.sqlite")
	if _, err := os.Stat(state); err != nil {
		return "", err
	}
	conn, err := openSQLiteReadOnly(state, sqliteReadOptions{busyTimeoutMS: 1000})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	hasMigrations, hasThreads, err := codexStateTables(ctx, conn)
	if err != nil {
		return "", err
	}
	if !hasMigrations {
		return "", fmt.Errorf("codex state index not initialized: %w", os.ErrNotExist)
	}
	// The index filename does not change for each SQLx schema migration.
	// Codex revision 0adee0d includes migrations through version 64.
	var version int
	if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM _sqlx_migrations").Scan(&version); err != nil {
		return "", err
	}
	if version > 64 {
		return "", fmt.Errorf("%w: unsupported Codex state migration %d", ErrUnsupportedCodexStateIndex, version)
	}
	if !hasThreads {
		return "", fmt.Errorf("codex state index not initialized: %w", os.ErrNotExist)
	}
	var rollout string
	if err := conn.QueryRowContext(ctx, "SELECT rollout_path FROM threads WHERE id = ?", thread).Scan(&rollout); err != nil {
		return "", err
	}
	key := CodexSessionUUIDFromPath(rollout)
	if CodexThreadIDFromSessionKey(key) != thread {
		return "", nil
	}
	return key, nil
}

func codexStateTables(ctx context.Context, conn *sql.DB) (hasMigrations, hasThreads bool, err error) {
	rows, err := conn.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name IN ('_sqlx_migrations', 'threads')")
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, false, err
		}
		switch name {
		case "_sqlx_migrations":
			hasMigrations = true
		case "threads":
			hasThreads = true
		}
	}
	if err := rows.Err(); err != nil {
		return false, false, err
	}
	return hasMigrations, hasThreads, nil
}
