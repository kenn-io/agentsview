package db

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFileBackedSessionCount_ExcludesNonDevinNonFileBackedAgents(
	t *testing.T,
) {
	d := testDB(t)
	ctx := context.Background()

	// Insert a claude-ai session (non-file-backed).
	insertSession(t, d, "claude-ai:test-1", "claude.ai",
		func(s *Session) { s.Agent = "claude-ai" })

	// Insert a warp session (non-Devin, non-file-backed).
	insertSession(t, d, "warp:test-1", "myproject",
		func(s *Session) { s.Agent = "warp" })

	// Insert a devin session (provider-backed local source; FileBased=false but
	// still protected by resync counting).
	insertSession(t, d, "devin:test-1", "myproject",
		func(s *Session) { s.Agent = "devin" })

	// Insert a claude session (file-backed).
	insertSession(t, d, "test-file-session", "myproject")

	count, err := d.FileBackedSessionCount(ctx)
	require.NoError(t, err, "FileBackedSessionCount")
	assert.Equal(t, 2, count,
		"FileBackedSessionCount should include claude plus devin, but exclude other non-file-backed agents")
}
