package parser

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CodexIndexedSessionKey reads a thread's current rollout from the newest
// state_N.sqlite thread index in a Codex home and returns its session key.
// It returns os.ErrNotExist when the home has no index and sql.ErrNoRows when
// the index has no row for the thread.
func CodexIndexedSessionKey(ctx context.Context, home, thread string) (string, error) {
	paths, _ := filepath.Glob(filepath.Join(home, "state_*.sqlite"))
	latest := -1
	var state string
	for _, path := range paths {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "state_"), ".sqlite")
		version, err := strconv.Atoi(name)
		if err == nil && version > latest {
			latest, state = version, path
		}
	}
	if state == "" {
		return "", os.ErrNotExist
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
