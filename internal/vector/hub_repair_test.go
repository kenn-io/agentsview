package vector

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
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

// Repair reads only the queued sessions, including their duplicate occurrences.
type queuedRepairSource struct {
	*fakeUnitSource
	sessions []string
}

func (*queuedRepairSource) ScanEmbeddableUnits(context.Context, string, bool, int, func(db.EmbeddableUnit) error) (string, error) {
	return "", errors.New("repair must not scan the entire archive")
}

func (s *queuedRepairSource) WithEmbeddableSnapshot(_ context.Context, fn func(db.EmbeddableSnapshot) error) error {
	return fn(s)
}

func (s *queuedRepairSource) ScanEmbeddableUnitsForSession(ctx context.Context, id string, scope bool, limit int, fn func(db.EmbeddableUnit) error) error {
	s.sessions = append(s.sessions, id)
	return s.fakeUnitSource.ScanEmbeddableUnitsForSession(ctx, id, scope, limit, fn)
}

func TestPullRepairPinsQueuedSessionIdentities(t *testing.T) {
	for _, outcome := range []string{"ready", "changed before fetch", "changed during fetch", "canceled"} {
		t.Run(outcome, func(t *testing.T) {
			ix := openTestIndex(t)
			src := &queuedRepairSource{fakeUnitSource: &fakeUnitSource{rows: []fakeUnit{
				{unit: userDoc("s", "duplicate", 0, "first"), endedAt: "2024-01-01"},
				{unit: userDoc("s", "duplicate", 1, "second"), endedAt: "2024-01-01"},
				{unit: userDoc("unrelated", "other", 0, "other"), endedAt: "2024-01-01"},
			}}}
			gen := fakeGeneration("hub")
			require.NoError(t, buildWithoutResult(ix, t.Context(), src.fakeUnitSource, gen))
			_, err := ix.db.ExecContext(t.Context(), `UPDATE message_vectors_v1 SET embedding=zeroblob(12) WHERE rowid=(SELECT vec_rowid FROM message_vectors_chunks WHERE doc_key='u:s:duplicate#2')`)
			require.NoError(t, err)
			if outcome == "changed before fetch" {
				src.rows[1].unit.Content = "changed"
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
				calls++
				require.Len(t, r.Identities, 1)
				id := r.Identities[0]
				assert.Equal(t, "hub/s", id.SessionID)
				assert.Equal(t, "duplicate", id.SourceUUID)
				assert.Equal(t, 2, id.Occurrence)
				assert.Equal(t, 1, id.Ordinal)
				if outcome == "changed during fetch" {
					src.rows[1].unit.Content = "changed"
				}
				if outcome == "canceled" {
					cancel()
				}
				return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}}, nil
			})
			result, err := ix.Pull(ctx, src, hub, gen, PullOptions{RepairInvalid: true, MapSession: func(s string) string { return "hub/" + s }})
			if outcome == "ready" {
				require.NoError(t, err)
				assert.Equal(t, 1, result.Fill.Documents)
				assert.Zero(t, result.Repair.Remaining)
			} else {
				require.Error(t, err)
				assert.Zero(t, result.Fill.Documents)
				assert.Equal(t, 1, result.Repair.Remaining)
			}
			switch outcome {
			case "changed before fetch":
				require.ErrorContains(t, err, "local document changed before invalid vector repair")
				assert.Zero(t, calls)
				assert.Equal(t, []string{"s"}, src.sessions)
			case "canceled":
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, 1, calls)
			default:
				assert.Equal(t, 1, calls)
				assert.Equal(t, []string{"s", "s"}, src.sessions)
			}
			var queued int
			require.NoError(t, ix.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM message_vectors_repair_queue`).Scan(&queued))
			if outcome == "ready" {
				assert.Zero(t, queued)
			} else {
				assert.Equal(t, 1, queued)
			}
		})
	}
}

func TestPullRepairUsesPendingProjection(t *testing.T) {
	ix := openTestIndex(t)
	src := twoDocSource()
	old, next := fakeGeneration("local"), fakeGeneration("hub")
	require.NoError(t, buildWithoutResult(ix, t.Context(), src, old))
	hub := lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}, Absent: []int{1}}, nil
	})
	result, err := ix.Pull(t.Context(), src, hub, next, PullOptions{})
	require.NoError(t, err)
	assert.False(t, result.Activated)
	stage, err := ix.pendingPullStage(t.Context(), next.Fingerprint())
	require.NoError(t, err)
	ordinal, err := stage.ordinalForFingerprint(t.Context(), next.Fingerprint())
	require.NoError(t, err)
	_, err = ix.db.ExecContext(t.Context(), fmt.Sprintf(`UPDATE %s_v%d SET embedding=zeroblob(12)`, stage.spec.VectorsPrefix, ordinal))
	require.NoError(t, err)
	result, err = ix.Pull(t.Context(), src, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		require.Len(t, r.Identities, 1)
		assert.Equal(t, "u1", r.Identities[0].SourceUUID)
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}}, nil
	}), next, PullOptions{RepairInvalid: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Fill.Documents)
	assert.True(t, result.Repair.RemainingKnown)
	assert.Zero(t, result.Repair.Remaining)
	assert.False(t, result.Activated)
	active, ok, err := ix.ActiveFingerprint(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, old.Fingerprint(), active)
	missing, err := ix.MissingEmbeddedDocs(t.Context(), next.Fingerprint(), nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1, missing, "repair leaves the unrelated absent target pending")
	result, err = ix.Pull(t.Context(), src, lookupFunc(func(_ context.Context, r HubLookupRequest) (HubLookupReply, error) {
		require.Len(t, r.Identities, 1)
		assert.Equal(t, "u2", r.Identities[0].SourceUUID)
		return HubLookupReply{Version: HubVectorVersion, Fingerprint: r.Fingerprint, Chunks: []HubChunk{{Index: 0, ChunkCount: 1, Vector: "AACAPwAAgD8AAIA/"}}}, nil
	}), next, PullOptions{})
	require.NoError(t, err)
	assert.True(t, result.Activated)
	active, ok, err = ix.ActiveFingerprint(t.Context())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, next.Fingerprint(), active)
}
