package parser

import (
	"context"
	"os"
	"path/filepath"
)

// CodexIndexedSessionKey reads a thread's current rollout from the supported
// state_5.sqlite thread index in a Codex home and returns its session key.
// It returns os.ErrNotExist when the home has no index and sql.ErrNoRows when
// the index has no row for the thread.
func CodexIndexedSessionKey(ctx context.Context, home, thread string) (string, error) {
	state := filepath.Join(home, "state_5.sqlite")
	if _, err := os.Stat(state); err != nil {
		return "", err
	}
	conn, err := openSQLiteReadOnly(state, sqliteReadOptions{busyTimeoutMS: 1000})
	if err != nil {
		return "", err
	}
	defer conn.Close()
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
