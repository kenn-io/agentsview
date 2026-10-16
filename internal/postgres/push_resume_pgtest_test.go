//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

// resumeFixture is a local archive of three sessions, oldest to newest, and
// an empty PG schema to push them to.
type resumeFixture struct {
	pg     *sql.DB
	local  *db.DB
	schema string
}

func newResumeFixture(t *testing.T, schema string) resumeFixture {
	t.Helper()
	pgURL := testPGURL(t)
	cleanNamedPGSchema(t, pgURL, schema)
	t.Cleanup(func() { cleanNamedPGSchema(t, pgURL, schema) })
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = pg.Close() })
	require.NoError(t, EnsureSchema(t.Context(), pg, schema))

	local, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = local.Close() })
	for id, ended := range map[string]string{
		"old": "2026-01-01T00:00:00Z",
		"mid": "2026-02-01T00:00:00Z",
		"new": "2026-03-01T00:00:00Z",
	} {
		require.NoError(t, local.UpsertSession(t.Context(), db.Session{
			ID: id, Project: "proj", Machine: "machine", Agent: "claude",
			EndedAt: &ended, MessageCount: 1, UserMessageCount: 1,
		}))
		require.NoError(t, local.InsertMessages(t.Context(), []db.Message{{
			SessionID: id, Ordinal: 0, Role: "user", Content: id,
			ContentLength: len(id), Timestamp: ended,
		}}))
	}
	return resumeFixture{pg: pg, local: local, schema: schema}
}

// sync returns a pusher that commits one session per batch and records its
// run progress after every batch.
func (f resumeFixture) sync() *Sync {
	return &Sync{
		pg: f.pg, local: f.local, machine: "machine", schema: f.schema,
		schemaDone: true, pushBatchSize: 1,
		fullRunCheckpointInterval: time.Nanosecond,
	}
}

// killedPush runs a push that is killed after its first session batch, the
// way a restart kills a long full push.
func (f resumeFixture) killedPush(t *testing.T) {
	t.Helper()
	s := f.sync()
	ctx, kill := context.WithCancel(t.Context())
	defer kill()
	s.afterPushBatch = kill
	_, err := s.Push(ctx, false, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func (f resumeFixture) sessionIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.pg.QueryContext(t.Context(), `SELECT id FROM sessions ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func (f resumeFixture) messageCTID(t *testing.T, sessionID string) string {
	t.Helper()
	var ctid string
	require.NoError(t, f.pg.QueryRowContext(t.Context(),
		`SELECT ctid::text FROM messages WHERE session_id = $1`, sessionID,
	).Scan(&ctid))
	return ctid
}

// Each process is killed after committing one session. Every restart must
// keep the sessions earlier processes committed, so the push converges, and
// the run must end with every session in PG and nothing left pending.
func TestFullPushResumesAcrossRestarts(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_resume_test")

	f.killedPush(t)
	assert.Equal(t, []string{"new"}, f.sessionIDs(t), "newest session goes first")
	newCTID := f.messageCTID(t, "new")

	f.killedPush(t)
	assert.Equal(t, []string{"mid", "new"}, f.sessionIDs(t),
		"the second process kept the first process's session")
	midCTID := f.messageCTID(t, "mid")

	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, "resuming an unfinished full push, started because: backfilling session aliases", res.FullReason)
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 2, res.SkippedResumed)
	assert.Zero(t, res.Errors)
	assert.Equal(t, []string{"mid", "new", "old"}, f.sessionIDs(t))
	assert.Equal(t, newCTID, f.messageCTID(t, "new"), "committed sessions are not rewritten")
	assert.Equal(t, midCTID, f.messageCTID(t, "mid"), "committed sessions are not rewritten")

	next, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.False(t, next.Full, "the resumed push recorded its completion")
	assert.Zero(t, next.SessionsPushed)
}

// A run recorded against another archive generation is abandoned: sessions
// it committed may predate the change, so every session is pushed again.
func TestFullPushRestartsWhenArchiveChangedSinceInterruption(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_resume_archive_test")
	f.killedPush(t)

	state := f.sync().effectiveSyncState()
	stored, err := state.GetSyncState(t.Context(), fullPushRunStateKey)
	require.NoError(t, err)
	var run fullPushRun
	require.NoError(t, json.Unmarshal([]byte(stored), &run))
	run.ArchiveID = "previous-archive"
	edited, err := json.Marshal(run)
	require.NoError(t, err)
	require.NoError(t, state.SetSyncState(t.Context(), fullPushRunStateKey, string(edited)))

	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, "backfilling session aliases", res.FullReason)
	assert.Equal(t, 3, res.SessionsPushed)
}

// A session the interrupted run committed but that PG lost while the pusher
// was down is pushed again instead of being skipped.
func TestFullPushResumeRepushesSessionsMissingFromPG(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_resume_missing_test")
	f.killedPush(t)
	_, err := f.pg.ExecContext(t.Context(), `DELETE FROM messages WHERE session_id = 'new'`)
	require.NoError(t, err)
	_, err = f.pg.ExecContext(t.Context(), `DELETE FROM sessions WHERE id = 'new'`)
	require.NoError(t, err)

	res, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, res.SessionsPushed)
	assert.Zero(t, res.SkippedResumed)
	assert.Equal(t, []string{"mid", "new", "old"}, f.sessionIDs(t))
}

// interruptingVectorSource runs onDocs before each session export so a test
// can kill a push partway through its vector phase.
type interruptingVectorSource struct {
	*fakeVectorSource
	onDocs func(sessionID string) error
}

func (s interruptingVectorSource) BeginExport(
	ctx context.Context, sessionIDs []string,
) (storage.VectorExport, bool, error) {
	export, ok, err := s.fakeVectorSource.BeginExport(ctx, sessionIDs)
	if err != nil || !ok {
		return export, ok, err
	}
	return interruptingVectorExport{VectorExport: export, onDocs: s.onDocs}, true, nil
}

type interruptingVectorExport struct {
	storage.VectorExport
	onDocs func(sessionID string) error
}

func (e interruptingVectorExport) SessionDocs(
	ctx context.Context, sessionID string,
) ([]storage.VectorPushDoc, string, error) {
	if err := e.onDocs(sessionID); err != nil {
		return nil, "", err
	}
	return e.VectorExport.SessionDocs(ctx, sessionID)
}

// A full push killed during its vector phase resumes there: the sessions and
// the vectors it already pushed are not sent again.
func TestFullPushResumesVectorPhase(t *testing.T) {
	s, local, _ := newVectorPushTestSync(t, testPGURL(t), "agentsview_push_resume_vector_test")
	seedVectorSession(t, local, "A")
	seedVectorSession(t, local, "B")
	ctx, kill := context.WithCancel(t.Context())
	defer kill()
	var exported []string
	source := interruptingVectorSource{
		fakeVectorSource: &fakeVectorSource{
			gen:    storage.VectorGenerationInfo{Fingerprint: "fp-resume", Model: "m", Dimension: 4},
			hasGen: true,
			hashes: map[string]string{"A": "ha", "B": "hb"},
			docs: map[string][]storage.VectorPushDoc{
				"A": {vdoc("A", "A#0", 0, "a", "h1", []float32{1, 0, 0, 0})},
				"B": {vdoc("B", "B#0", 0, "b", "h2", []float32{0, 1, 0, 0})},
			},
		},
		onDocs: func(id string) error {
			if len(exported) == 1 {
				kill()
				return ctx.Err()
			}
			exported = append(exported, id)
			return nil
		},
	}
	s.vectorSource = source
	_, err := s.Push(ctx, false, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, exported, 1)
	interrupted := exported[0]

	exported = nil
	source.onDocs = func(id string) error {
		exported = append(exported, id)
		return nil
	}
	s.vectorSource = source
	res, err := s.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, "resuming an unfinished full push, started because: backfilling session aliases", res.FullReason)
	assert.Zero(t, res.SessionsPushed)
	assert.Equal(t, 2, res.SkippedResumed)
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
	assert.Equal(t, 1, res.Vectors.SessionsUnchanged)
	assert.Len(t, exported, 1)
	assert.NotContains(t, exported, interrupted)
}

// An explicit full push starts over instead of resuming an unfinished one.
func TestRequestedFullPushDiscardsUnfinishedRun(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_resume_requested_test")
	f.killedPush(t)

	res, err := f.sync().Push(t.Context(), true, nil)
	require.NoError(t, err)
	assert.True(t, res.Full)
	assert.Equal(t, 3, res.SessionsPushed)
	assert.Zero(t, res.SkippedResumed)
}

// A full push that ended with session errors does not keep later pushes
// full once nothing else requires one; they go back to incremental.
func TestErroredFullPushRunDroppedWhenNoFullPushRequired(t *testing.T) {
	f := newResumeFixture(t, "agentsview_push_resume_errored_test")
	first, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	require.Zero(t, first.Errors)

	// NOT VALID leaves the existing row alone and rejects only the full
	// push's rewrite of it.
	_, err = f.pg.ExecContext(t.Context(),
		`ALTER TABLE sessions ADD CONSTRAINT reject_old CHECK (id <> 'old') NOT VALID`)
	require.NoError(t, err)
	errored, err := f.sync().Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.NotZero(t, errored.Errors)
	state := f.sync().effectiveSyncState()
	stored, err := state.GetSyncState(t.Context(), fullPushRunStateKey)
	require.NoError(t, err)
	require.NotEmpty(t, stored, "the errored run stays recorded")

	_, err = f.pg.ExecContext(t.Context(), `ALTER TABLE sessions DROP CONSTRAINT reject_old`)
	require.NoError(t, err)
	next, err := f.sync().Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.False(t, next.Full)
	assert.Empty(t, next.FullReason)
	stored, err = state.GetSyncState(t.Context(), fullPushRunStateKey)
	require.NoError(t, err)
	assert.Empty(t, stored, "the errored run record is cleared")
}
