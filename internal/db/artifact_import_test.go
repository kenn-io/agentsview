package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func artifactImportTestWork(origin string, sequence int) ArtifactImportWork {
	return ArtifactImportWork{
		Origin:                    origin,
		Kind:                      "checkpoints",
		Name:                      fmt.Sprintf("cp-%010d.json", sequence),
		SHA256:                    strings.Repeat("a", 64),
		Size:                      42,
		RequiredCheckpointVersion: 1,
		RequiredManifestVersion:   2,
		RequiredSegmentVersion:    1,
	}
}

func TestArtifactImportQueueExactClaimsAndVersionGates(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 2)
	require.NoError(t, database.EnqueueArtifactImport(ctx, work))
	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)

	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, work.Name, pending[0].Name)

	future := pending[0]
	future.RequiredManifestVersion = 3
	require.NoError(t, database.EnqueueArtifactImport(ctx, future))
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	assert.Empty(t, pending)

	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 3, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, 3, pending[0].RequiredManifestVersion)
	acknowledged, err := database.AcknowledgeArtifactImport(ctx, pending[0])
	require.NoError(t, err)
	assert.True(t, acknowledged)
}

func TestArtifactImportQueueIdentityAndSequenceAuthority(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 1)

	require.NoError(t, database.EnqueueArtifactImport(ctx, work))
	require.NoError(t, database.EnqueueArtifactImport(ctx, work))

	conflict := work
	conflict.SHA256 = strings.Repeat("b", 64)
	err := database.EnqueueArtifactImport(ctx, conflict)
	require.ErrorIs(t, err, ErrArtifactImportConflict)

	for sequence := 2; sequence <= 3; sequence++ {
		next := artifactImportTestWork(work.Origin, sequence)
		next.SHA256 = strings.Repeat(string(rune('a'+sequence)), 64)
		require.NoError(t, database.EnqueueArtifactImport(ctx, next))
	}
	require.NoError(t, database.EnqueueArtifactImport(
		ctx, artifactImportTestWork(work.Origin, 2),
	))

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "cp-0000000003.json", pending[0].Name)
}

func TestArtifactImportQueueAttemptAndStaleAcknowledgement(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	require.NoError(t, database.EnqueueArtifactImport(
		ctx, artifactImportTestWork("peer-a1b2c3", 1),
	))

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	claim := pending[0]

	staleTime := claim
	staleTime.EnqueuedAt = "2000-01-01T00:00:00Z"
	acknowledged, err := database.AcknowledgeArtifactImport(ctx, staleTime)
	require.NoError(t, err)
	assert.False(t, acknowledged)

	staleIdentity := claim
	staleIdentity.SHA256 = strings.Repeat("b", 64)
	acknowledged, err = database.AcknowledgeArtifactImport(ctx, staleIdentity)
	require.NoError(t, err)
	assert.False(t, acknowledged)

	marked, err := database.MarkArtifactImportAttempted(ctx, claim, attempt)
	require.NoError(t, err)
	assert.True(t, marked)
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(t, err)
	assert.Empty(t, pending)

	nextAttempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	assert.Greater(t, nextAttempt, attempt)
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		nextAttempt,
		10,
	)
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestArtifactImportQueuePaginationDoesNotRetryAttemptedPage(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	for i := 1; i <= 129; i++ {
		work := artifactImportTestWork(fmt.Sprintf("peer-%04d", i), 1)
		work.SHA256 = fmt.Sprintf("%064x", i)
		require.NoError(t, database.EnqueueArtifactImport(ctx, work))
	}

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	versions := ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1}
	first, err := database.PendingArtifactImports(ctx, versions, attempt, 128)
	require.NoError(t, err)
	require.Len(t, first, 128)
	for _, claim := range first {
		marked, markErr := database.MarkArtifactImportAttempted(
			ctx, claim, attempt,
		)
		require.NoError(t, markErr)
		require.True(t, marked)
	}

	second, err := database.PendingArtifactImports(ctx, versions, attempt, 128)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.NotEqual(t, first[0].Origin, second[0].Origin)
}

func TestArtifactImportQueueStatsIncludeFutureRowsAndLimitsAreBounded(
	t *testing.T,
) {
	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 1)
	work.RequiredManifestVersion = 3
	require.NoError(t, database.EnqueueArtifactImport(ctx, work))

	count, oldest, err := database.ArtifactImportQueueStats(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.NotEmpty(t, oldest)

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(t, err)
	for _, limit := range []int{0, 1025} {
		_, err := database.PendingArtifactImports(
			ctx,
			ArtifactImportVersions{Checkpoint: 1, Manifest: 3, Segment: 1},
			attempt,
			limit,
		)
		require.Error(t, err)
	}
}

func TestArtifactPeerCheckpointHeadIsMonotonic(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   42,
	}

	advanced, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	assert.True(t, advanced)
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	assert.False(t, advanced)

	older := head
	older.Sequence = 1
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, older)
	require.NoError(t, err)
	assert.False(t, advanced)

	conflict := head
	conflict.CheckpointSHA256 = strings.Repeat("b", 64)
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, conflict)
	require.ErrorIs(t, err, ErrArtifactImportConflict)
	assert.False(t, advanced)

	newer := head
	newer.Sequence = 3
	newer.CheckpointSHA256 = strings.Repeat("c", 64)
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, newer)
	require.NoError(t, err)
	assert.True(t, advanced)

	got, found, err := database.GetArtifactPeerCheckpointHead(ctx, head.Origin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, newer, got)

	_, found, err = database.GetArtifactPeerCheckpointHead(ctx, "missing")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestArtifactImportQueueRejectsInvalidClaims(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	tests := []struct {
		name   string
		mutate func(*ArtifactImportWork)
	}{
		{"blank origin", func(work *ArtifactImportWork) { work.Origin = "" }},
		{"wrong kind", func(work *ArtifactImportWork) { work.Kind = "manifests" }},
		{"bad name", func(work *ArtifactImportWork) { work.Name = "cp-1.json" }},
		{"bad hash", func(work *ArtifactImportWork) { work.SHA256 = "ABC" }},
		{"negative size", func(work *ArtifactImportWork) { work.Size = -1 }},
		{"zero version", func(work *ArtifactImportWork) {
			work.RequiredSegmentVersion = 0
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			work := artifactImportTestWork("peer-a1b2c3", 1)
			tc.mutate(&work)
			err := database.EnqueueArtifactImport(ctx, work)
			require.Error(t, err)
			assert.False(t, errors.Is(err, ErrArtifactImportConflict))
		})
	}
}

func TestArtifactCheckpointLandingBindsPeerIdentity(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   99,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)

	landing := ArtifactCheckpointLanding(head)
	want := map[string]string{
		head.Origin + "~one": strings.Repeat("b", 64),
		head.Origin + "~two": strings.Repeat("c", 64),
	}
	require.NoError(t,
		database.RecordArtifactCheckpointLanding(ctx, landing, want))

	gotLanding, got, found, err :=
		database.GetArtifactCheckpointLanding(ctx, head.Origin)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, landing, gotLanding)
	assert.Equal(t, want, got)

	require.NoError(t,
		database.RecordArtifactCheckpointLanding(ctx, landing, want))
}

func TestArtifactCheckpointLandingReadUsesOneSnapshot(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	firstHead := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         1,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   41,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, firstHead)
	require.NoError(t, err)
	firstMap := map[string]string{
		firstHead.Origin + "~one": strings.Repeat("b", 64),
	}
	require.NoError(t, database.RecordArtifactCheckpointLanding(
		ctx, ArtifactCheckpointLanding(firstHead), firstMap,
	))

	secondHead := firstHead
	secondHead.Sequence = 2
	secondHead.CheckpointSHA256 = strings.Repeat("c", 64)
	secondHead.CheckpointSize = 42
	secondMap := map[string]string{
		firstHead.Origin + "~two": strings.Repeat("d", 64),
	}
	var once sync.Once
	gotLanding, gotMap, found, err := database.getArtifactCheckpointLanding(
		ctx, firstHead.Origin, func() {
			once.Do(func() {
				advanced, recordErr := database.RecordArtifactPeerCheckpointHead(
					context.WithoutCancel(ctx), secondHead,
				)
				require.NoError(t, recordErr)
				require.True(t, advanced)
				require.NoError(t, database.RecordArtifactCheckpointLanding(
					context.WithoutCancel(ctx),
					ArtifactCheckpointLanding(secondHead),
					secondMap,
				))
			})
		},
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ArtifactCheckpointLanding(firstHead), gotLanding)
	assert.Equal(t, firstMap, gotMap)

	gotLanding, gotMap, found, err = database.GetArtifactCheckpointLanding(
		ctx, firstHead.Origin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ArtifactCheckpointLanding(secondHead), gotLanding)
	assert.Equal(t, secondMap, gotMap)
}

func TestArtifactCheckpointLandingRejectsUnrecordedAndRegressedAuthority(
	t *testing.T,
) {
	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   99,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(t, err)
	landing := ArtifactCheckpointLanding(head)
	sessionMap := map[string]string{
		head.Origin + "~one": strings.Repeat("b", 64),
	}
	require.NoError(t, database.RecordArtifactCheckpointLanding(
		ctx, landing, sessionMap,
	))

	wrongIdentity := landing
	wrongIdentity.CheckpointSHA256 = strings.Repeat("c", 64)
	err = database.RecordArtifactCheckpointLanding(
		ctx, wrongIdentity, sessionMap,
	)
	require.ErrorIs(t, err, ErrArtifactImportConflict)

	newerHead := head
	newerHead.Sequence = 3
	newerHead.CheckpointSHA256 = strings.Repeat("d", 64)
	advanced, err := database.RecordArtifactPeerCheckpointHead(ctx, newerHead)
	require.NoError(t, err)
	require.True(t, advanced)
	newerLanding := ArtifactCheckpointLanding(newerHead)
	newerMap := map[string]string{
		head.Origin + "~two": strings.Repeat("e", 64),
	}
	require.NoError(t, database.RecordArtifactCheckpointLanding(
		ctx, newerLanding, newerMap,
	))

	err = database.RecordArtifactCheckpointLanding(ctx, landing, sessionMap)
	require.ErrorIs(t, err, ErrArtifactImportConflict)
	gotLanding, got, found, err := database.GetArtifactCheckpointLanding(
		ctx, head.Origin,
	)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, newerLanding, gotLanding)
	assert.Equal(t, newerMap, got)
}

func TestArtifactImportedSessionProvenanceIsBoundedAndAdvances(t *testing.T) {
	database := testDB(t)
	ctx := t.Context()
	origin := "peer-a1b2c3"
	one := ArtifactImportedSession{
		Origin:            origin,
		GID:               origin + "~one",
		ManifestHash:      strings.Repeat("a", 64),
		ImportedSessionID: origin + "~one",
	}
	two := ArtifactImportedSession{
		Origin:            origin,
		GID:               origin + "~two",
		ManifestHash:      strings.Repeat("b", 64),
		ImportedSessionID: origin + "~two",
	}
	require.NoError(t, database.RecordArtifactImportedSession(ctx, one))
	require.NoError(t, database.RecordArtifactImportedSession(ctx, two))
	require.NoError(t, database.RecordArtifactImportedSession(ctx, one))

	got, err := database.ArtifactImportedManifestHashes(
		ctx, origin, []string{two.GID, two.GID},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{two.GID: two.ManifestHash}, got)

	one.ManifestHash = strings.Repeat("c", 64)
	require.NoError(t, database.RecordArtifactImportedSession(ctx, one))
	got, err = database.ArtifactImportedManifestHashes(
		ctx, origin, []string{one.GID, two.GID},
	)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		one.GID: one.ManifestHash,
		two.GID: two.ManifestHash,
	}, got)

	tooMany := make([]string, 1025)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%s~%04d", origin, i)
	}
	_, err = database.ArtifactImportedManifestHashes(ctx, origin, tooMany)
	require.Error(t, err)
}

func TestArtifactImportedManifestHashesChunksWithinSQLiteVariableLimit(
	t *testing.T,
) {
	database := testDB(t)
	ctx := t.Context()
	origin := "peer-a1b2c3"
	gids := make([]string, maxArtifactQueuePageSize)
	for i := range gids {
		gids[i] = fmt.Sprintf("%s~%04d", origin, i)
	}
	first := ArtifactImportedSession{
		Origin:            origin,
		GID:               gids[0],
		ManifestHash:      strings.Repeat("a", 64),
		ImportedSessionID: gids[0],
	}
	last := ArtifactImportedSession{
		Origin:            origin,
		GID:               gids[len(gids)-1],
		ManifestHash:      strings.Repeat("b", 64),
		ImportedSessionID: gids[len(gids)-1],
	}
	require.NoError(t, database.RecordArtifactImportedSession(ctx, first))
	require.NoError(t, database.RecordArtifactImportedSession(ctx, last))
	forceReaderVarLimit(t, database, 999)

	got, err := database.ArtifactImportedManifestHashes(ctx, origin, gids)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		first.GID: first.ManifestHash,
		last.GID:  last.ManifestHash,
	}, got)
}
