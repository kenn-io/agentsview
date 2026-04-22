package db

import (
	"context"
	"testing"
)

func TestFileBackedSessionCount_ExcludesNonFileAgents(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	// Insert a claude-ai session (non-file-backed).
	insertSession(t, d, "claude-ai:test-1", "claude.ai",
		func(s *Session) { s.Agent = "claude-ai" })

	// Insert a warp session (non-file-backed).
	insertSession(t, d, "warp:test-1", "myproject",
		func(s *Session) { s.Agent = "warp" })

	// Insert a claude session (file-backed).
	insertSession(t, d, "test-file-session", "myproject")

	count, err := d.FileBackedSessionCount(ctx)
	requireNoError(t, err, "FileBackedSessionCount")
	if count != 1 {
		t.Errorf(
			"FileBackedSessionCount = %d, want 1 "+
				"(only claude session)", count,
		)
	}
}
