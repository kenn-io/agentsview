package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/db"
)

// A canonical Codex page must look up exclusions under the bare rollout id it
// held before data version 134, because a purged old row leaves only that
// exclusion behind.
func TestPGSessionTombstoneIDsIncludeRetiredCodexRollout(t *testing.T) {
	const thread = "11111111-1111-4111-8111-111111111111"
	const rollout = "22222222-2222-4222-8222-222222222222"
	page := db.Session{ID: "host~codex:" + thread + "_" + rollout, Agent: "codex"}
	assert.ElementsMatch(t,
		[]string{page.ID, "host~codex:" + rollout},
		pgSessionTombstoneIDs(page))

	thr := db.Session{ID: "codex:" + thread, Agent: "codex"}
	assert.Equal(t, []string{thr.ID}, pgSessionTombstoneIDs(thr))

	other := db.Session{ID: "claude:" + thread + "_" + rollout, Agent: "claude"}
	assert.Equal(t, []string{other.ID}, pgSessionTombstoneIDs(other))
}
