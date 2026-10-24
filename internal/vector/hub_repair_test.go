package vector

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPullRepairRequiresExistingGeneration(t *testing.T) {
	ix := openTestIndex(t)
	hub := lookupFunc(func(context.Context, HubLookupRequest) (HubLookupReply, error) {
		require.FailNow(t, "repair cannot fetch vectors for an uncreated generation")
		return HubLookupReply{}, nil
	})
	_, err := ix.Pull(t.Context(), twoDocSource(), hub, fakeGeneration("hub"), PullOptions{RepairInvalid: true})
	require.ErrorContains(t, err, "lookup generation")
	gens, err := ix.Generations(t.Context())
	require.NoError(t, err)
	assert.Empty(t, gens)
	var docs int
	require.NoError(t, ix.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_messages").Scan(&docs))
	assert.Zero(t, docs)
}

func TestPullRepairOwnsOnlyQueuedTargets(t *testing.T) {
	for _, outcome := range []string{"ready", "absent", "failure", "statistics"} {
		t.Run(outcome, func(t *testing.T) {
			ix := openTestIndex(t)
			src := twoDocSource()
			gen := fakeGeneration("hub")
			require.NoError(t, buildWithoutResult(ix, t.Context(), src, gen))
			// One malformed vector is a repair target. The ordinary pending change
			// and newly discovered source document are outside the repair scope.
			_, err := ix.db.ExecContext(t.Context(), `UPDATE message_vectors_v1 SET embedding=zeroblob(12) WHERE rowid=(SELECT vec_rowid FROM message_vectors_chunks WHERE doc_key='u:s1:u1')`)
			require.NoError(t, err)
			if outcome != "statistics" {
				src.rows[1].unit.Content = "changed"
				_, err = ix.Refresh(t.Context(), src, true, false)
				require.NoError(t, err)
				src.rows = append(src.rows, fakeUnit{unit: userDoc("s1", "new", 2, "new"), endedAt: "2024-01-02"})
			}
			before, err := ix.Generations(t.Context())
			require.NoError(t, err)
			calls := 0
			hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				require.Len(t, r.Identities, 1)
				assert.Equal(t, "u1", r.Identities[0].SourceUUID)
				if outcome == "failure" || outcome == "statistics" {
					return HubLookupReply{}, errors.New("lookup failed")
				}
				reply := HubLookupReply{Version: HubVectorVersion, Fingerprint: gen.Fingerprint()}
				if outcome == "absent" {
					reply.Absent = []int{0}
				} else {
					reply.Chunks = []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}
				}
				return reply, nil
			})
			result, err := ix.Pull(t.Context(), src, hub, gen, PullOptions{RepairInvalid: true})
			if outcome == "ready" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			assert.Equal(t, 1, calls)
			assert.False(t, result.Activated)
			assert.Equal(t, 1, result.Repair.Documents)
			assert.True(t, result.Repair.RemainingKnown)
			remaining := 1
			if outcome == "ready" {
				remaining = 0
				assert.Equal(t, 1, result.Fill.Documents)
				assert.Equal(t, 1, result.Fill.Chunks)
			}
			assert.Equal(t, remaining, result.Repair.Remaining)
			after, err := ix.Generations(t.Context())
			require.NoError(t, err)
			require.Len(t, after, 1)
			assert.Equal(t, before[0].State, after[0].State)
			missing := remaining
			if outcome != "statistics" {
				missing++
			}
			assert.EqualValues(t, missing, after[0].Missing)
			var discovered int
			require.NoError(t, ix.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM vector_messages WHERE doc_key='u:s1:new'").Scan(&discovered))
			assert.Zero(t, discovered)
		})
	}
}
