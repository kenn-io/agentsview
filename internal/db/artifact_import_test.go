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
	"go.kenn.io/agentsview/internal/config"
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
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 2)
	require.NoError(database.EnqueueArtifactImport(ctx, work))
	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)

	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(work.Name, pending[0].Name)

	future := pending[0]
	future.RequiredManifestVersion = 3
	require.NoError(database.EnqueueArtifactImport(ctx, future))
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	assert.Empty(pending)

	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 3, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(3, pending[0].RequiredManifestVersion)
	acknowledged, err := database.AcknowledgeArtifactImport(ctx, pending[0])
	require.NoError(err)
	assert.True(acknowledged)
}

func TestArtifactImportQueueIdentityAndSequenceAuthority(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 1)

	require.NoError(database.EnqueueArtifactImport(ctx, work))
	require.NoError(database.EnqueueArtifactImport(ctx, work))

	conflict := work
	conflict.SHA256 = strings.Repeat("b", 64)
	err := database.EnqueueArtifactImport(ctx, conflict)
	require.ErrorIs(err, ErrArtifactImportConflict)

	for sequence := 2; sequence <= 3; sequence++ {
		next := artifactImportTestWork(work.Origin, sequence)
		next.SHA256 = strings.Repeat(string(rune('a'+sequence)), 64)
		require.NoError(database.EnqueueArtifactImport(ctx, next))
	}
	require.NoError(database.EnqueueArtifactImport(
		ctx, artifactImportTestWork(work.Origin, 2),
	))

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)
	assert.Equal(t, "cp-0000000003.json", pending[0].Name)
}

func TestArtifactImportQueueAttemptAndStaleAcknowledgement(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	require.NoError(database.EnqueueArtifactImport(
		ctx, artifactImportTestWork("peer-a1b2c3", 1),
	))

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)
	claim := pending[0]

	staleTime := claim
	staleTime.EnqueuedAt = "2000-01-01T00:00:00Z"
	acknowledged, err := database.AcknowledgeArtifactImport(ctx, staleTime)
	require.NoError(err)
	assert.False(acknowledged)

	staleIdentity := claim
	staleIdentity.SHA256 = strings.Repeat("b", 64)
	acknowledged, err = database.AcknowledgeArtifactImport(ctx, staleIdentity)
	require.NoError(err)
	assert.False(acknowledged)

	marked, err := database.MarkArtifactImportAttempted(ctx, claim, attempt)
	require.NoError(err)
	assert.True(marked)
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	assert.Empty(pending)

	nextAttempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	assert.Greater(nextAttempt, attempt)
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		nextAttempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)
}

func TestArtifactImportQuarantineIntentIsDurableAndExact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 1)
	require.NoError(database.EnqueueArtifactImport(ctx, work))
	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	pending, err := database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		attempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)

	marked, err := database.MarkArtifactImportQuarantinePending(
		ctx, pending[0],
	)
	require.NoError(err)
	require.True(marked)
	nextAttempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	pending, err = database.PendingArtifactImports(
		ctx,
		ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1},
		nextAttempt,
		10,
	)
	require.NoError(err)
	require.Len(pending, 1)
	assert.True(pending[0].QuarantinePending)

	stale := pending[0]
	stale.SHA256 = strings.Repeat("b", 64)
	marked, err = database.MarkArtifactImportQuarantinePending(ctx, stale)
	require.NoError(err)
	assert.False(marked)
}

func TestArtifactImportQueuePaginationDoesNotRetryAttemptedPage(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	for i := 1; i <= 129; i++ {
		work := artifactImportTestWork(fmt.Sprintf("peer-%04d", i), 1)
		work.SHA256 = fmt.Sprintf("%064x", i)
		require.NoError(database.EnqueueArtifactImport(ctx, work))
	}

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	versions := ArtifactImportVersions{Checkpoint: 1, Manifest: 2, Segment: 1}
	first, err := database.PendingArtifactImports(ctx, versions, attempt, 128)
	require.NoError(err)
	require.Len(first, 128)
	for _, claim := range first {
		marked, markErr := database.MarkArtifactImportAttempted(
			ctx, claim, attempt,
		)
		require.NoError(markErr)
		require.True(marked)
	}

	second, err := database.PendingArtifactImports(ctx, versions, attempt, 128)
	require.NoError(err)
	require.Len(second, 1)
	assert.NotEqual(t, first[0].Origin, second[0].Origin)
}

func TestArtifactImportQueueStatsIncludeFutureRowsAndLimitsAreBounded(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	work := artifactImportTestWork("peer-a1b2c3", 1)
	work.RequiredManifestVersion = 3
	require.NoError(database.EnqueueArtifactImport(ctx, work))

	count, oldest, err := database.ArtifactImportQueueStats(ctx)
	require.NoError(err)
	assert.Equal(1, count)
	assert.NotEmpty(oldest)

	attempt, err := database.ReserveArtifactImportAttemptGeneration(ctx)
	require.NoError(err)
	for _, limit := range []int{0, 1025} {
		_, err := database.PendingArtifactImports(
			ctx,
			ArtifactImportVersions{Checkpoint: 1, Manifest: 3, Segment: 1},
			attempt,
			limit,
		)
		require.Error(err)
	}
}

func TestArtifactPeerCheckpointHeadIsMonotonic(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   42,
	}

	advanced, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(err)
	assert.True(advanced)
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(err)
	assert.False(advanced)

	older := head
	older.Sequence = 1
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, older)
	require.NoError(err)
	assert.False(advanced)

	conflict := head
	conflict.CheckpointSHA256 = strings.Repeat("b", 64)
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, conflict)
	require.ErrorIs(err, ErrArtifactImportConflict)
	assert.False(advanced)

	newer := head
	newer.Sequence = 3
	newer.CheckpointSHA256 = strings.Repeat("c", 64)
	advanced, err = database.RecordArtifactPeerCheckpointHead(ctx, newer)
	require.NoError(err)
	assert.True(advanced)

	got, found, err := database.GetArtifactPeerCheckpointHead(ctx, head.Origin)
	require.NoError(err)
	require.True(found)
	assert.Equal(newer, got)

	_, found, err = database.GetArtifactPeerCheckpointHead(ctx, "missing")
	require.NoError(err)
	assert.False(found)
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
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   99,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(err)

	landing := ArtifactCheckpointLanding(head)
	want := map[string]string{
		head.Origin + "~one": strings.Repeat("b", 64),
		head.Origin + "~two": strings.Repeat("c", 64),
	}
	require.NoError(database.RecordArtifactCheckpointLanding(ctx, landing, want))

	gotLanding, got, found, err :=
		database.GetArtifactCheckpointLanding(ctx, head.Origin)
	require.NoError(err)
	require.True(found)
	assert.Equal(landing, gotLanding)
	assert.Equal(want, got)

	require.NoError(database.RecordArtifactCheckpointLanding(ctx, landing, want))
}

func TestArtifactCheckpointLandingIdentityReadDoesNotMaterializeSessionMap(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   99,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(err)
	const sessionCount = 2_000
	sessionMap := make(map[string]string, sessionCount)
	for i := range sessionCount {
		sessionMap[fmt.Sprintf("%s~session-%04d", head.Origin, i)] =
			fmt.Sprintf("%064x", i+1)
	}
	require.NoError(database.RecordArtifactCheckpointLanding(
		ctx, ArtifactCheckpointLanding(head), sessionMap,
	))

	var got ArtifactCheckpointLanding
	var found bool
	allocations := testing.AllocsPerRun(3, func() {
		var readErr error
		got, found, readErr =
			database.GetArtifactCheckpointLandingIdentity(ctx, head.Origin)
		require.NoError(readErr)
	})
	require.True(found)
	assert.Equal(ArtifactCheckpointLanding(head), got)
	assert.Less(allocations, 500.0)
}

func TestArtifactCheckpointLandingReadUsesOneSnapshot(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	firstHead := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         1,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   41,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, firstHead)
	require.NoError(err)
	firstMap := map[string]string{
		firstHead.Origin + "~one": strings.Repeat("b", 64),
	}
	require.NoError(database.RecordArtifactCheckpointLanding(
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
				require.NoError(recordErr)
				require.True(advanced)
				require.NoError(database.RecordArtifactCheckpointLanding(
					context.WithoutCancel(ctx),
					ArtifactCheckpointLanding(secondHead),
					secondMap,
				))
			})
		},
	)
	require.NoError(err)
	require.True(found)
	assert.Equal(ArtifactCheckpointLanding(firstHead), gotLanding)
	assert.Equal(firstMap, gotMap)

	gotLanding, gotMap, found, err = database.GetArtifactCheckpointLanding(
		ctx, firstHead.Origin,
	)
	require.NoError(err)
	require.True(found)
	assert.Equal(ArtifactCheckpointLanding(secondHead), gotLanding)
	assert.Equal(secondMap, gotMap)
}

func TestArtifactCheckpointLandingRejectsUnrecordedAndRegressedAuthority(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	head := ArtifactPeerCheckpointHead{
		Origin:           "peer-a1b2c3",
		Sequence:         2,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   99,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(ctx, head)
	require.NoError(err)
	landing := ArtifactCheckpointLanding(head)
	sessionMap := map[string]string{
		head.Origin + "~one": strings.Repeat("b", 64),
	}
	require.NoError(database.RecordArtifactCheckpointLanding(
		ctx, landing, sessionMap,
	))

	wrongIdentity := landing
	wrongIdentity.CheckpointSHA256 = strings.Repeat("c", 64)
	err = database.RecordArtifactCheckpointLanding(
		ctx, wrongIdentity, sessionMap,
	)
	require.ErrorIs(err, ErrArtifactImportConflict)

	newerHead := head
	newerHead.Sequence = 3
	newerHead.CheckpointSHA256 = strings.Repeat("d", 64)
	advanced, err := database.RecordArtifactPeerCheckpointHead(ctx, newerHead)
	require.NoError(err)
	require.True(advanced)
	newerLanding := ArtifactCheckpointLanding(newerHead)
	newerMap := map[string]string{
		head.Origin + "~two": strings.Repeat("e", 64),
	}
	require.NoError(database.RecordArtifactCheckpointLanding(
		ctx, newerLanding, newerMap,
	))

	err = database.RecordArtifactCheckpointLanding(ctx, landing, sessionMap)
	require.ErrorIs(err, ErrArtifactImportConflict)
	gotLanding, got, found, err := database.GetArtifactCheckpointLanding(
		ctx, head.Origin,
	)
	require.NoError(err)
	require.True(found)
	assert.Equal(newerLanding, gotLanding)
	assert.Equal(newerMap, got)
}

func TestArtifactImportedSessionProvenanceIsBoundedAndAdvances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

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
	require.NoError(database.RecordArtifactImportedSession(ctx, one))
	require.NoError(database.RecordArtifactImportedSession(ctx, two))
	require.NoError(database.RecordArtifactImportedSession(ctx, one))

	got, err := database.ArtifactImportedManifestHashes(
		ctx, origin, []string{two.GID, two.GID},
	)
	require.NoError(err)
	assert.Equal(map[string]string{two.GID: two.ManifestHash}, got)

	one.ManifestHash = strings.Repeat("c", 64)
	require.NoError(database.RecordArtifactImportedSession(ctx, one))
	got, err = database.ArtifactImportedManifestHashes(
		ctx, origin, []string{one.GID, two.GID},
	)
	require.NoError(err)
	assert.Equal(map[string]string{
		one.GID: one.ManifestHash,
		two.GID: two.ManifestHash,
	}, got)

	tooMany := make([]string, 1025)
	for i := range tooMany {
		tooMany[i] = fmt.Sprintf("%s~%04d", origin, i)
	}
	_, err = database.ArtifactImportedManifestHashes(ctx, origin, tooMany)
	require.Error(err)
}

func TestApplyArtifactImportedSessionPreservesLocalCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	origin := "peer-a1b2c3"
	gid := origin + "~native"
	localWrite := SessionBatchWrite{
		Session: Session{
			ID: gid, Project: "local-project", Machine: "developer-host",
			Agent: "claude",
		},
		Messages: []Message{{
			SessionID: gid, Ordinal: 0, Role: "user", Content: "local",
		}},
		ReplaceMessages: true,
	}
	result, err := database.WriteSessionBatchAtomic(
		[]SessionBatchWrite{localWrite},
	)
	require.NoError(err)
	require.Equal(1, result.WrittenSessions)

	imported := ArtifactImportedSession{
		Origin:            origin,
		GID:               gid,
		ManifestHash:      strings.Repeat("a", 64),
		ImportedSessionID: gid,
	}
	peerWrite := localWrite
	peerWrite.Session.Project = "peer-project"
	peerWrite.Session.Machine = origin
	peerWrite.Messages[0].Content = "peer"
	applied, err := database.ApplyArtifactImportedSession(
		ctx, imported, peerWrite,
	)
	require.NoError(err)
	assert.True(applied.Suppressed)
	assert.False(applied.Written)

	session, err := database.GetSession(ctx, gid)
	require.NoError(err)
	require.NotNil(session)
	assert.Equal("local-project", session.Project)
	assert.Equal("developer-host", session.Machine)
	messages, err := database.GetAllMessages(ctx, gid)
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal("local", messages[0].Content)
	provenance, err := database.ArtifactImportedManifestHashes(
		ctx, origin, []string{gid},
	)
	require.NoError(err)
	assert.Equal(map[string]string{gid: imported.ManifestHash}, provenance)
}

func TestApplyArtifactImportedSessionProjectsToolResultImages(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	database.SetToolResultImages(config.ToolResultImagesOffload)
	database.SetAssetsDir(t.TempDir())
	ctx := t.Context()
	origin := "peer-a1b2c3"
	gid := origin + "~image"
	imported := ArtifactImportedSession{
		Origin:            origin,
		GID:               gid,
		ManifestHash:      strings.Repeat("a", 64),
		ImportedSessionID: gid,
	}
	write := SessionBatchWrite{
		Session: Session{
			ID: gid, Project: "project", Machine: origin, Agent: "codex",
		},
		Messages:        []Message{testImageMessage(gid)},
		ReplaceMessages: true,
	}

	result, err := database.ApplyArtifactImportedSession(ctx, imported, write)
	require.NoError(err)
	require.True(result.Written)

	messages, err := database.GetAllMessages(ctx, gid)
	require.NoError(err)
	require.Len(messages, 1)
	call := messages[0].ToolCalls[0]
	assertOffloadedImage(t, call.ResultContent, database.AssetsDir())
	require.Len(call.ResultEvents, 1)
	assertOffloadedImage(t, call.ResultEvents[0].Content, database.AssetsDir())
}

func TestArtifactImportedManifestHashesChunksWithinSQLiteVariableLimit(
	t *testing.T,
) {
	require := require.New(t)

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
	require.NoError(database.RecordArtifactImportedSession(ctx, first))
	require.NoError(database.RecordArtifactImportedSession(ctx, last))
	forceReaderVarLimit(t, database, 999)

	got, err := database.ArtifactImportedManifestHashes(ctx, origin, gids)
	require.NoError(err)
	assert.Equal(t, map[string]string{
		first.GID: first.ManifestHash,
		last.GID:  last.ManifestHash,
	}, got)
}

func TestArtifactCheckpointStagePagesDeferredSessionsAndLands(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	landing := ArtifactCheckpointLanding{
		Origin:           "peer-a1b2c3",
		Sequence:         7,
		CheckpointSHA256: strings.Repeat("c", 64),
		CheckpointSize:   321,
	}
	entries := []ArtifactCheckpointSession{
		{
			GID:          landing.Origin + "~one",
			ManifestHash: strings.Repeat("1", 64),
		},
		{
			GID:          landing.Origin + "~two",
			ManifestHash: strings.Repeat("2", 64),
		},
		{
			GID:          landing.Origin + "~three",
			ManifestHash: strings.Repeat("3", 64),
		},
	}
	advanced, err := database.RecordArtifactPeerCheckpointHead(
		ctx,
		ArtifactPeerCheckpointHead(landing),
	)
	require.NoError(err)
	require.True(advanced)
	require.NoError(database.BeginArtifactCheckpointStage(ctx, landing, 1))
	require.NoError(database.StageArtifactCheckpointSessions(
		ctx, landing, entries[:2],
	))
	require.NoError(database.StageArtifactCheckpointSessions(
		ctx, landing, entries[2:],
	))
	require.NoError(database.CompleteArtifactCheckpointStage(
		ctx, landing, len(entries),
	))
	require.NoError(database.RecordArtifactImportedSession(
		ctx,
		ArtifactImportedSession{
			Origin:            landing.Origin,
			GID:               entries[0].GID,
			ManifestHash:      entries[0].ManifestHash,
			ImportedSessionID: entries[0].GID,
		},
	))

	pending, err := database.PendingArtifactCheckpointSessions(
		ctx, landing, 11, 1,
	)
	require.NoError(err)
	require.Equal([]ArtifactCheckpointSession{entries[2]}, pending)
	marked, err := database.MarkArtifactCheckpointSessionAttempted(
		ctx, landing, entries[2], 11,
	)
	require.NoError(err)
	require.True(marked)

	pending, err = database.PendingArtifactCheckpointSessions(
		ctx, landing, 11, 10,
	)
	require.NoError(err)
	assert.Equal([]ArtifactCheckpointSession{entries[1]}, pending)
	pending, err = database.PendingArtifactCheckpointSessions(
		ctx, landing, 12, 10,
	)
	require.NoError(err)
	assert.Equal([]ArtifactCheckpointSession{entries[1], entries[2]}, pending)

	err = database.RecordArtifactCheckpointLandingFromStage(ctx, landing)
	require.ErrorIs(err, ErrArtifactImportConflict)
	for _, entry := range entries[1:] {
		require.NoError(database.RecordArtifactImportedSession(
			ctx,
			ArtifactImportedSession{
				Origin:            landing.Origin,
				GID:               entry.GID,
				ManifestHash:      entry.ManifestHash,
				ImportedSessionID: entry.GID,
			},
		))
	}
	require.NoError(database.RecordArtifactCheckpointLandingFromStage(
		ctx, landing,
	))

	gotLanding, gotMap, found, err := database.GetArtifactCheckpointLanding(
		ctx, landing.Origin,
	)
	require.NoError(err)
	require.True(found)
	assert.Equal(landing, gotLanding)
	assert.Equal(map[string]string{
		entries[0].GID: entries[0].ManifestHash,
		entries[1].GID: entries[1].ManifestHash,
		entries[2].GID: entries[2].ManifestHash,
	}, gotMap)
}

func TestPendingArtifactCheckpointSessionsUsesBoundedPendingOrder(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	landing := ArtifactCheckpointLanding{
		Origin:           "peer-a1b2c3",
		Sequence:         9,
		CheckpointSHA256: strings.Repeat("d", 64),
		CheckpointSize:   654,
	}
	require.NoError(database.BeginArtifactCheckpointStage(ctx, landing, 1))

	const satisfiedCount = 512
	entries := make([]ArtifactCheckpointSession, 0, satisfiedCount+2)
	for i := range satisfiedCount {
		entry := ArtifactCheckpointSession{
			GID:          fmt.Sprintf("%s~a-satisfied-%04d", landing.Origin, i),
			ManifestHash: fmt.Sprintf("%064x", i+1),
		}
		require.NoError(database.RecordArtifactImportedSession(
			ctx,
			ArtifactImportedSession{
				Origin:            landing.Origin,
				GID:               entry.GID,
				ManifestHash:      entry.ManifestHash,
				ImportedSessionID: entry.GID,
			},
		))
		entries = append(entries, entry)
	}
	newerAttempt := ArtifactCheckpointSession{
		GID:          landing.Origin + "~z-newer-attempt",
		ManifestHash: strings.Repeat("e", 64),
	}
	olderAttempt := ArtifactCheckpointSession{
		GID:          landing.Origin + "~zz-older-attempt",
		ManifestHash: strings.Repeat("f", 64),
	}
	entries = append(entries, newerAttempt, olderAttempt)
	for start := 0; start < len(entries); start += maxArtifactImportSessionPageSize {
		end := min(start+maxArtifactImportSessionPageSize, len(entries))
		require.NoError(database.StageArtifactCheckpointSessions(
			ctx, landing, entries[start:end],
		))
	}
	require.NoError(database.CompleteArtifactCheckpointStage(
		ctx, landing, len(entries),
	))
	marked, err := database.MarkArtifactCheckpointSessionAttempted(
		ctx, landing, newerAttempt, 5,
	)
	require.NoError(err)
	require.True(marked)

	pending, err := database.PendingArtifactCheckpointSessions(
		ctx, landing, 10, 1,
	)
	require.NoError(err)
	require.Equal([]ArtifactCheckpointSession{olderAttempt}, pending)

	rows, err := database.getReader().Query(
		`PRAGMA index_info('idx_artifact_checkpoint_stage_ready')`,
	)
	require.NoError(err)
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var sequence, columnID int
		var name string
		require.NoError(rows.Scan(&sequence, &columnID, &name))
		columns = append(columns, name)
	}
	require.NoError(rows.Err())
	assert.Equal(t, []string{
		"origin", "sequence", "satisfied",
		"attempt_generation", "gid", "manifest_hash",
	}, columns)
}

func TestArtifactCheckpointStageRestartsForNewDecoderVersion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	landing := ArtifactCheckpointLanding{
		Origin:           "peer-a1b2c3",
		Sequence:         10,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   777,
	}
	entry := ArtifactCheckpointSession{
		GID:          landing.Origin + "~session",
		ManifestHash: strings.Repeat("b", 64),
	}
	require.NoError(database.BeginArtifactCheckpointStage(
		ctx, landing, 1,
	))
	require.NoError(database.StageArtifactCheckpointSessionPage(
		ctx, landing, []ArtifactCheckpointSession{entry}, 0, 42,
	))
	require.NoError(database.BeginArtifactCheckpointStage(
		ctx, landing, 1,
	))
	progress, err := database.ArtifactCheckpointStageProgress(ctx, landing)
	require.NoError(err)
	assert.Equal(1, progress.DecodedCount)
	assert.Equal(int64(42), progress.DecodeOffset)

	require.NoError(database.BeginArtifactCheckpointStage(
		ctx, landing, 2,
	))
	progress, err = database.ArtifactCheckpointStageProgress(ctx, landing)
	require.NoError(err)
	assert.False(progress.Complete)
	assert.Zero(progress.DecodedCount)
	assert.Zero(progress.DecodeOffset)
	var staged int
	require.NoError(database.getReader().QueryRowContext(ctx, `
		SELECT count(*)
		FROM artifact_checkpoint_stage_sessions
		WHERE origin = ? AND sequence = ?`,
		landing.Origin, landing.Sequence,
	).Scan(&staged))
	assert.Zero(staged)
}

func TestArtifactCheckpointStageRejectsNestedNativeSessionID(t *testing.T) {
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	landing := ArtifactCheckpointLanding{
		Origin:           "peer-a1b2c3",
		Sequence:         1,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   100,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(
		ctx, ArtifactPeerCheckpointHead(landing),
	)
	require.NoError(err)
	require.NoError(database.BeginArtifactCheckpointStage(ctx, landing, 1))

	err = database.StageArtifactCheckpointSessions(
		ctx,
		landing,
		[]ArtifactCheckpointSession{{
			GID:          landing.Origin + "~session~nested",
			ManifestHash: strings.Repeat("b", 64),
		}},
	)
	require.Error(err)
	assert.NotErrorIs(t, err, ErrArtifactImportConflict)
}

func TestPruneArtifactCheckpointStagesUsesPeerHeadAndKeepsLanding(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	unlanded := ArtifactCheckpointLanding{
		Origin:           "unlanded-a1b2c3",
		Sequence:         1,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   100,
	}
	landed := ArtifactCheckpointLanding{
		Origin:           "landed-a1b2c3",
		Sequence:         1,
		CheckpointSHA256: strings.Repeat("b", 64),
		CheckpointSize:   101,
	}
	for _, stage := range []ArtifactCheckpointLanding{unlanded, landed} {
		_, err := database.RecordArtifactPeerCheckpointHead(
			ctx, ArtifactPeerCheckpointHead(stage),
		)
		require.NoError(err)
		require.NoError(database.BeginArtifactCheckpointStage(ctx, stage, 1))
		require.NoError(database.CompleteArtifactCheckpointStage(ctx, stage, 0))
	}
	require.NoError(database.RecordArtifactCheckpointLandingFromStage(
		ctx, landed,
	))
	for _, stage := range []ArtifactCheckpointLanding{unlanded, landed} {
		next := ArtifactPeerCheckpointHead(stage)
		next.Sequence = 2
		next.CheckpointSHA256 = strings.Repeat("c", 64)
		next.CheckpointSize = 102
		advanced, err := database.RecordArtifactPeerCheckpointHead(ctx, next)
		require.NoError(err)
		require.True(advanced)
	}

	pruned, more, err := database.PruneArtifactCheckpointStages(ctx, 10)
	require.NoError(err)
	assert.Equal(1, pruned)
	assert.False(more)
	_, err = database.ArtifactCheckpointStageProgress(ctx, unlanded)
	require.ErrorIs(err, ErrArtifactImportConflict)
	state, err := database.ArtifactCheckpointStageProgress(ctx, landed)
	require.NoError(err)
	assert.True(state.Complete)
}

func TestPruneArtifactCheckpointStagesIsBoundedAndKeepsCurrentLanding(
	t *testing.T,
) {
	assert := assert.New(t)
	require := require.New(t)

	database := testDB(t)
	ctx := t.Context()
	origin := "peer-a1b2c3"
	first := ArtifactCheckpointLanding{
		Origin: origin, Sequence: 1,
		CheckpointSHA256: strings.Repeat("a", 64),
		CheckpointSize:   100,
	}
	_, err := database.RecordArtifactPeerCheckpointHead(
		ctx, ArtifactPeerCheckpointHead(first),
	)
	require.NoError(err)
	require.NoError(database.BeginArtifactCheckpointStage(ctx, first, 1))
	for i := range 3 {
		entry := ArtifactCheckpointSession{
			GID:          fmt.Sprintf("%s~old-%d", origin, i),
			ManifestHash: fmt.Sprintf("%064x", i+1),
		}
		require.NoError(database.StageArtifactCheckpointSessions(
			ctx, first, []ArtifactCheckpointSession{entry},
		))
		require.NoError(database.RecordArtifactImportedSession(
			ctx,
			ArtifactImportedSession{
				Origin: origin, GID: entry.GID,
				ManifestHash:      entry.ManifestHash,
				ImportedSessionID: entry.GID,
			},
		))
	}
	require.NoError(database.CompleteArtifactCheckpointStage(ctx, first, 3))
	require.NoError(database.RecordArtifactCheckpointLandingFromStage(
		ctx, first,
	))

	current := ArtifactCheckpointLanding{
		Origin: origin, Sequence: 2,
		CheckpointSHA256: strings.Repeat("b", 64),
		CheckpointSize:   50,
	}
	_, err = database.RecordArtifactPeerCheckpointHead(
		ctx, ArtifactPeerCheckpointHead(current),
	)
	require.NoError(err)
	require.NoError(database.BeginArtifactCheckpointStage(ctx, current, 1))
	require.NoError(database.CompleteArtifactCheckpointStage(ctx, current, 0))
	require.NoError(database.RecordArtifactCheckpointLanding(
		ctx, current,
		map[string]string{
			origin + "~legacy": strings.Repeat("c", 64),
		},
	))
	require.NoError(database.RecordArtifactCheckpointLandingFromStage(
		ctx, current,
	))

	pruned, more, err := database.PruneArtifactCheckpointStages(ctx, 2)
	require.NoError(err)
	assert.Equal(2, pruned)
	assert.True(more)
	pruned, more, err = database.PruneArtifactCheckpointStages(ctx, 2)
	require.NoError(err)
	assert.Equal(2, pruned)
	assert.True(more)
	pruned, more, err = database.PruneArtifactCheckpointStages(ctx, 2)
	require.NoError(err)
	assert.Equal(1, pruned)
	assert.False(more)

	got, sessionMap, found, err := database.GetArtifactCheckpointLanding(
		ctx, origin,
	)
	require.NoError(err)
	require.True(found)
	assert.Equal(current, got)
	assert.Empty(sessionMap)
}
