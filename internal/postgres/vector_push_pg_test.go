//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

// fakeVectorSource is an in-memory VectorPushSource whose generation, aggregate
// hashes, and per-session docs are mutated between pushes to drive the delta,
// eviction, and cross-generation cases. genOverride, when set, replaces the
// nth Generation answer (1-based call count) so a test can change the source's
// state between the push's initial Generation call and its mid-push
// re-checks. exportHashes, when set, overrides the per-session hash SessionDocs
// returns (simulating a local index that changed between the delta scan and
// the export); by default SessionDocs echoes the delta-scan hash so pushes
// proceed.
type fakeVectorSource struct {
	gen          VectorGenerationInfo
	hasGen       bool
	hashes       map[string]string
	docs         map[string][]VectorPushDoc
	exportHashes map[string]string
	docsCalls    map[string]int // per-session SessionDocs invocation count
	genCalls     int
	genOverride  func(call int) (VectorGenerationInfo, bool, error)
	hashScopes   [][]string // sessionIDs of each SessionDocHashes call
}

func (f *fakeVectorSource) Generation(
	ctx context.Context,
) (VectorGenerationInfo, bool, error) {
	f.genCalls++
	if f.genOverride != nil {
		return f.genOverride(f.genCalls)
	}
	return f.gen, f.hasGen, nil
}

func (f *fakeVectorSource) SessionDocHashes(
	ctx context.Context, sessionIDs []string,
) (map[string]string, error) {
	f.hashScopes = append(f.hashScopes, sessionIDs)
	if sessionIDs == nil {
		return f.hashes, nil
	}
	out := make(map[string]string, len(sessionIDs))
	for _, id := range sessionIDs {
		if h, ok := f.hashes[id]; ok {
			out[id] = h
		}
	}
	return out, nil
}

func (f *fakeVectorSource) SessionDocs(
	ctx context.Context, id string,
) ([]VectorPushDoc, string, error) {
	if f.docsCalls == nil {
		f.docsCalls = make(map[string]int)
	}
	f.docsCalls[id]++
	if hash, ok := f.exportHashes[id]; ok {
		return f.docs[id], hash, nil
	}
	return f.docs[id], f.hashes[id], nil
}

// newVectorPushTestSync creates a fresh schema with the base + vector tables
// and returns a Sync wired to a local SQLite DB. It skips the test when the
// pgvector extension is unavailable, since the whole phase no-ops there.
func newVectorPushTestSync(
	t *testing.T, pgURL, schema string,
) (*Sync, *db.DB, *sql.DB) {
	t.Helper()
	pg, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open")
	t.Cleanup(func() { _ = pg.Close() })

	ctx := context.Background()
	_, err = pg.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, pg, schema), "EnsureSchema")

	available, err := VectorExtensionAvailable(ctx, pg)
	require.NoError(t, err, "VectorExtensionAvailable")
	if !available {
		t.Skip("pgvector extension unavailable")
	}

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	t.Cleanup(func() { _ = localDB.Close() })

	sync := &Sync{
		pg:         pg,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}
	return sync, localDB, pg
}

// seedVectorSession inserts a minimal session with one message so that a Push
// creates a PG sessions row carrying this pusher's owner marker.
func seedVectorSession(t *testing.T, localDB *db.DB, id string) {
	t.Helper()
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:               id,
		Project:          "proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}), "UpsertSession "+id)
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     id,
		Ordinal:       0,
		Role:          "user",
		Content:       id,
		ContentLength: len(id),
	}}), "InsertMessages "+id)
}

// seedVectorSessionProject inserts a minimal session under the given project so
// the PG sessions row carries a project the vector push filter can scope on.
func seedVectorSessionProject(t *testing.T, localDB *db.DB, id, project string) {
	t.Helper()
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:               id,
		Project:          project,
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}), "UpsertSession "+id)
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     id,
		Ordinal:       0,
		Role:          "user",
		Content:       id,
		ContentLength: len(id),
	}}), "InsertMessages "+id)
}

// vdoc builds a VectorPushDoc with the supplied embeddings as consecutive
// chunks (chunk_index 0..n-1).
func vdoc(
	sessionID, docKey string, ordinal int,
	content, hash string, chunks ...[]float32,
) VectorPushDoc {
	d := VectorPushDoc{
		DocKey:      docKey,
		SessionID:   sessionID,
		Ordinal:     ordinal,
		OrdinalEnd:  ordinal,
		OffsetsJSON: "[]",
		Content:     content,
		ContentHash: hash,
	}
	for i, emb := range chunks {
		d.Chunks = append(d.Chunks, VectorPushChunk{ChunkIndex: i, Embedding: emb})
	}
	return d
}

func countRows(t *testing.T, pg *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pg.QueryRow(query, args...).Scan(&n), "count: "+query)
	return n
}

// docOrdinal returns the current ordinal of the doc_key's vector_documents row.
func docOrdinal(t *testing.T, pg *sql.DB, docKey string) int {
	t.Helper()
	var ord int
	require.NoError(t, pg.QueryRow(
		`SELECT ordinal FROM vector_documents WHERE doc_key = $1`, docKey,
	).Scan(&ord), "ordinal of "+docKey)
	return ord
}

// TestVectorPushChangeScoped pins the change-scoped contract end to end: a
// scoped push reads local hashes and PG state only for its changed
// relational sessions, updates exactly those, and leaves vector-only
// changes and PG-only orphan rows for the next generation-wide push.
func TestVectorPushChangeScoped(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_change_scoped_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")
	src := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-scoped", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha1", "B": "hb1"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "ha1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "c2", "hb1", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = src

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "baseline Push")
	require.Equal(t, 2, res.Vectors.SessionsPushed)

	var genID int64
	require.NoError(t, pg.QueryRow(
		`SELECT id FROM vector_generations WHERE fingerprint = $1`,
		"fp-scoped",
	).Scan(&genID), "generation id")

	// A PG-only state row no local session backs: eviction fodder that a
	// scoped push must not touch.
	_, err = pg.Exec(`
INSERT INTO vector_push_state (generation_id, session_id, doc_agg_hash)
VALUES ($1, 'orphan', 'stale')`, genID)
	require.NoError(t, err, "seed orphan state row")

	// A changes relationally and in the vector source; B changes only in
	// the vector source (e.g. an embeddings build finishing later).
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:               "A",
		Project:          "proj",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     2,
		UserMessageCount: 2,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}), "reupsert A")
	require.NoError(t, localDB.InsertMessages([]db.Message{{
		SessionID:     "A",
		Ordinal:       1,
		Role:          "user",
		Content:       "A follow-up",
		ContentLength: 11,
	}}), "second message A")
	// InsertMessages touches no sync_marker signal, so advance A's marker
	// the way real ingestion does for a relational change; without this A
	// stays below the baseline watermark and the scoped push selects nothing.
	require.NoError(t, localDB.BumpLocalModifiedAt("A"), "advance A sync marker")
	src.hashes = map[string]string{"A": "ha2", "B": "hb2"}
	src.docs = map[string][]VectorPushDoc{
		"A": {vdoc("A", "A#0", 0, "c1b", "ha2", []float32{0, 0, 1, 0})},
		"B": {vdoc("B", "B#0", 0, "c2b", "hb2", []float32{0, 0, 0, 1})},
	}
	src.hashScopes = nil

	res, err = sync.PushWithOptions(ctx, PushOptions{
		ScopeVectorsToChangedSessions: true,
	}, nil)
	require.NoError(t, err, "scoped Push")
	assert.False(t, res.Vectors.Skipped)
	assert.Equal(t, 1, res.Vectors.SessionsPushed,
		"only the relationally changed session pushes")

	require.Len(t, src.hashScopes, 1, "one scoped hash read")
	assert.Equal(t, []string{"A"}, src.hashScopes[0],
		"local hash read must be scoped to the changed session")

	var aggHash string
	require.NoError(t, pg.QueryRow(`
SELECT doc_agg_hash FROM vector_push_state
 WHERE generation_id = $1 AND session_id = 'A'`, genID,
	).Scan(&aggHash))
	assert.Equal(t, "ha2", aggHash, "A's state row updates")
	require.NoError(t, pg.QueryRow(`
SELECT doc_agg_hash FROM vector_push_state
 WHERE generation_id = $1 AND session_id = 'B'`, genID,
	).Scan(&aggHash))
	assert.Equal(t, "hb1", aggHash,
		"B's vector-only change waits for the next generation-wide push")
	assert.Equal(t, 1, countRows(t, pg, `
SELECT COUNT(*) FROM vector_push_state
 WHERE generation_id = $1 AND session_id = 'orphan'`, genID),
		"a scoped push must not evict PG-only rows")

	res, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "generation-wide Push")
	require.NoError(t, pg.QueryRow(`
SELECT doc_agg_hash FROM vector_push_state
 WHERE generation_id = $1 AND session_id = 'B'`, genID,
	).Scan(&aggHash))
	assert.Equal(t, "hb2", aggHash,
		"the generation-wide push reconciles the vector-only change")
	assert.Equal(t, 0, countRows(t, pg, `
SELECT COUNT(*) FROM vector_push_state
 WHERE generation_id = $1 AND session_id = 'orphan'`, genID),
		"the generation-wide push evicts the orphan row")
}

// TestVectorPushGenerationSwitchPromotesScopedPush pins that a scoped push
// whose last-reconciled fingerprint differs from the active generation
// promotes to a generation-wide reconciliation. Without it a re-embed would
// register the new generation and write only the changed session's chunks,
// leaving search reading an incomplete generation until the interval floor.
func TestVectorPushGenerationSwitchPromotesScopedPush(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_gen_switch_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")
	src := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp1", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "a1", "B": "b1"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "ca1", "a1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "cb1", "b1", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = src

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "baseline Push")
	require.Equal(t, 2, res.Vectors.SessionsPushed)
	gen1 := res.Vectors.GenerationID
	require.NotZero(t, gen1, "baseline records the reconciled generation id")

	// The embedding generation changes: a new fingerprint with fresh
	// per-session docs across the whole corpus.
	src.gen = VectorGenerationInfo{Fingerprint: "fp2", Model: "m", Dimension: 4}
	src.hashes = map[string]string{"A": "a2", "B": "b2"}
	src.docs = map[string][]VectorPushDoc{
		"A": {vdoc("A", "A#0", 0, "ca2", "a2", []float32{0, 0, 1, 0})},
		"B": {vdoc("B", "B#0", 0, "cb2", "b2", []float32{0, 0, 0, 1})},
	}
	src.hashScopes = nil

	// A scoped push limited to the one relationally changed session, still
	// carrying the old generation id, must promote to generation-wide and
	// fill the whole fp2 generation rather than register it with only A's
	// chunks.
	vres, err := sync.pushVectors(ctx, false, []string{"A"}, gen1, nil, nil)
	require.NoError(t, err, "scoped push across a generation switch")
	assert.Equal(t, 2, vres.SessionsPushed,
		"the whole new generation is reconciled, not just the changed session")

	require.Len(t, src.hashScopes, 1, "one hash read")
	assert.Nil(t, src.hashScopes[0],
		"promotion reads the whole generation, not the changed subset")

	var gen2 int64
	require.NoError(t, pg.QueryRow(
		`SELECT id FROM vector_generations WHERE fingerprint = $1`, "fp2",
	).Scan(&gen2), "fp2 generation id")
	assert.NotEqual(t, gen1, gen2, "the new fingerprint got a new generation id")
	assert.Equal(t, gen2, vres.GenerationID, "the push reconciled the new generation")
	assert.Equal(t, 2, countRows(t, pg, `
SELECT COUNT(*) FROM vector_push_state WHERE generation_id = $1`, gen2),
		"both sessions have state rows under the new generation")
}

// TestVectorPushRecreatedGenerationPromotesScopedPush pins that a scoped push
// promotes to generation-wide when PG has lost the active generation (a reset
// or admin drop) even though the source fingerprint is unchanged, so an
// in-memory fingerprint memo cannot leave the recreated generation populated
// with only the changed session's chunks.
func TestVectorPushRecreatedGenerationPromotesScopedPush(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_gen_recreated_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")
	src := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp1", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "a1", "B": "b1"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "ca1", "a1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "cb1", "b1", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = src

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "baseline Push")

	var gen1 int64
	require.NoError(t, pg.QueryRow(
		`SELECT id FROM vector_generations WHERE fingerprint = $1`, "fp1",
	).Scan(&gen1), "gen1 id")

	// PG loses the generation while the source keeps the same fingerprint, so
	// a fingerprint memo would still match; only the generation id changes.
	for _, q := range []string{
		`DELETE FROM vector_push_state WHERE generation_id = $1`,
		`DELETE FROM vector_generation_machines WHERE generation_id = $1`,
		`DELETE FROM vector_generations WHERE id = $1`,
	} {
		_, err := pg.Exec(q, gen1)
		require.NoError(t, err, q)
	}
	src.hashScopes = nil

	// A scoped push still carrying the old generation id must notice the
	// active id changed and reconcile the whole corpus, not just the one
	// changed session.
	vres, err := sync.pushVectors(ctx, false, []string{"A"}, gen1, nil, nil)
	require.NoError(t, err, "scoped push after generation drop")
	assert.Equal(t, 2, vres.SessionsPushed,
		"the recreated generation is fully populated, not just the changed session")
	require.Len(t, src.hashScopes, 1)
	assert.Nil(t, src.hashScopes[0],
		"promotion reads the whole generation, not the changed subset")

	var gen2 int64
	require.NoError(t, pg.QueryRow(
		`SELECT id FROM vector_generations WHERE fingerprint = $1`, "fp1",
	).Scan(&gen2), "recreated generation id")
	assert.NotEqual(t, gen1, gen2, "the generation row was recreated with a new id")
	assert.Equal(t, gen2, vres.GenerationID, "the push reconciled the recreated generation")
	assert.Equal(t, 2, countRows(t, pg, `
SELECT COUNT(*) FROM vector_push_state WHERE generation_id = $1`, gen2),
		"both sessions have state rows under the recreated generation")
}

// TestVectorPushRecreatedTablesReusedIDPromotesScopedPush pins the promotion
// when a reset recreates the vector tables themselves, not just their rows:
// the restarted id sequence hands the recreated generation the memoized id,
// so the id comparison passes and only the machine push record — wiped in
// the same reset — reveals the new incarnation.
func TestVectorPushRecreatedTablesReusedIDPromotesScopedPush(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_tables_recreated_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")
	src := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp1", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "a1", "B": "b1"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "ca1", "a1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "cb1", "b1", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = src

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "baseline Push")

	var gen1 int64
	require.NoError(t, pg.QueryRow(
		`SELECT id FROM vector_generations WHERE fingerprint = $1`, "fp1",
	).Scan(&gen1), "gen1 id")

	// The vector tables are dropped and recreated (the relational sync
	// marker is untouched), restarting the id sequence so the unchanged
	// fingerprint re-registers under the memoized id.
	for _, q := range []string{
		`DROP TABLE IF EXISTS ` + vectorChunkTable(gen1),
		`DROP TABLE IF EXISTS vector_push_state`,
		`DROP TABLE IF EXISTS vector_generation_machines`,
		`DROP TABLE IF EXISTS vector_documents`,
		`DROP TABLE IF EXISTS vector_generations`,
	} {
		_, err := pg.Exec(q)
		require.NoError(t, err, q)
	}
	src.hashScopes = nil

	vres, err := sync.pushVectors(ctx, false, []string{"A"}, gen1, nil, nil)
	require.NoError(t, err, "scoped push after table recreation")

	var gen2 int64
	require.NoError(t, pg.QueryRow(
		`SELECT id FROM vector_generations WHERE fingerprint = $1`, "fp1",
	).Scan(&gen2), "recreated generation id")
	require.Equal(t, gen1, gen2,
		"the restarted sequence reuses the id, which is the case under test")

	assert.Equal(t, 2, vres.SessionsPushed,
		"the recreated generation is fully populated, not just the changed session")
	require.Len(t, src.hashScopes, 1)
	assert.Nil(t, src.hashScopes[0],
		"promotion reads the whole generation, not the changed subset")
	assert.Equal(t, 2, countRows(t, pg, `
SELECT COUNT(*) FROM vector_push_state WHERE generation_id = $1`, gen2),
		"both sessions have state rows under the recreated generation")
}

func TestVectorPushRoundTrip(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_roundtrip_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	sync.vectorSource = &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-rt", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {
				vdoc("A", "A#0", 0, "c1", "h1",
					[]float32{1, 0, 0, 0}, []float32{0, 1, 0, 0}),
				vdoc("A", "A#1", 1, "c2", "h2", []float32{0, 0, 1, 0}),
			},
			"B": {vdoc("B", "B#0", 0, "c3", "h3", []float32{0, 0, 0, 1})},
		},
	}

	var vectorReports, prepareReports []PushProgress
	res, err := sync.Push(ctx, false, func(p PushProgress) {
		switch p.Phase {
		case "vectors":
			vectorReports = append(vectorReports, p)
		case "preparing":
			prepareReports = append(prepareReports, p)
		}
	})
	require.NoError(t, err, "Push")
	assert.False(t, res.Vectors.Skipped)
	assert.Equal(t, 2, res.Vectors.SessionsPushed)
	assert.Equal(t, 3, res.Vectors.DocsPushed)
	assert.Equal(t, 4, res.Vectors.ChunksPushed)

	require.NotEmpty(t, prepareReports, "fingerprint phase must report progress")
	prepared := prepareReports[len(prepareReports)-1]
	assert.Equal(t, 2, prepared.SessionsDone)
	assert.Equal(t, 2, prepared.SessionsTotal)

	require.NotEmpty(t, vectorReports, "vector phase must report progress")
	final := vectorReports[len(vectorReports)-1]
	assert.Equal(t, 2, final.VectorSessionsDone)
	assert.Equal(t, 2, final.VectorSessionsTotal)
	assert.Equal(t, 4, final.VectorChunksPushed)

	genID, dim, ok, err := LookupVectorGeneration(ctx, pg, "fp-rt")
	require.NoError(t, err, "LookupVectorGeneration")
	require.True(t, ok, "generation registered")
	assert.Equal(t, 4, dim)

	assert.Equal(t, 3,
		countRows(t, pg, `SELECT COUNT(*) FROM vector_documents`))
	assert.Equal(t, 4,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)))
	assert.Equal(t, 2, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_push_state WHERE generation_id = $1`, genID))
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_generation_machines
		 WHERE generation_id = $1 AND machine = $2`, genID, "test-machine"))
}

func TestVectorPushDeltaNoop(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, _ := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_noop_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	sync.vectorSource = &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-noop", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "c3", "h3", []float32{0, 0, 0, 1})},
		},
	}

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Equal(t, 2, res.Vectors.SessionsUnchanged)
	assert.Equal(t, 0, res.Vectors.SessionsPushed)
	assert.Equal(t, 0, res.Vectors.DocsPushed)
}

// TestVectorPushFullRepairsSilentCorruption pins --full's repair contract for
// the vector phase: a chunk row missing from PG while vector_push_state still
// reports the session current is invisible to a normal delta push, but a full
// push bypasses the unchanged-hash skip and restores it.
func TestVectorPushFullRepairsSilentCorruption(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_full_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")

	sync.vectorSource = &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-full", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
		},
	}

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "seed Push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-full")
	require.NoError(t, err)
	require.True(t, ok)

	// Silently lose the chunk while push state still says "current".
	_, err = pg.Exec(`DELETE FROM ` + vectorChunkTable(genID))
	require.NoError(t, err, "corrupting chunk table")

	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "delta pushVectors")
	assert.Equal(t, 1, res.SessionsUnchanged, "delta push cannot see the loss")
	assert.Equal(t, 0,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)))

	res, err = sync.pushVectors(ctx, true, nil, 0, nil, nil)
	require.NoError(t, err, "full pushVectors")
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 0, res.SessionsUnchanged)
	assert.Equal(t, 1,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)),
		"full push restores the missing chunk")
}

// TestVectorPushDeferredOnSessionError pins that a changed session named in
// failedSessions is deferred — its newer local vectors must not land ahead of
// sessions/messages rows its session-phase push failed to write — and that
// the untouched delta state sends it on the next successful push.
func TestVectorPushDeferredOnSessionError(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_deferred_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-def", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha1", "B": "hb1"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "old", "h-old", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "cb", "hb", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "seed Push")

	// A's docs change locally, but its session push failed this run.
	fake.hashes["A"] = "ha2"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "A#0", 0, "new", "h-new", []float32{0, 1, 0, 0}),
	}
	res, err := sync.pushVectors(
		ctx, false, nil, 0, map[string]struct{}{"A": {}}, nil,
	)
	require.NoError(t, err, "deferred pushVectors")
	assert.Equal(t, 1, res.SessionsDeferred)
	assert.Equal(t, 0, res.SessionsPushed)
	assert.Equal(t, 1, res.SessionsUnchanged, "B is unchanged")

	var content string
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "A#0",
	).Scan(&content))
	assert.Equal(t, "old", content, "deferred session keeps its prior vectors")

	// Next push with no failures sends the deferred session.
	res, err = sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "healing pushVectors")
	assert.Equal(t, 0, res.SessionsDeferred)
	assert.Equal(t, 1, res.SessionsPushed)
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "A#0",
	).Scan(&content))
	assert.Equal(t, "new", content)
}

func TestVectorPushContentChange(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_content_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-cc", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "c3", "h3", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	fake.hashes["A"] = "ha2"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "A#0", 0, "c1-updated", "h1b", []float32{0, 1, 0, 0}),
	}

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
	assert.Equal(t, 1, res.Vectors.SessionsUnchanged)

	var content, hash string
	require.NoError(t, pg.QueryRow(
		`SELECT content, content_hash FROM vector_documents WHERE doc_key = $1`,
		"A#0").Scan(&content, &hash), "read updated doc")
	assert.Equal(t, "c1-updated", content)
	assert.Equal(t, "h1b", hash)
}

func TestVectorPushEviction(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_evict_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-ev", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "c3", "h3", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-ev")
	require.NoError(t, err)
	require.True(t, ok)

	delete(fake.hashes, "B")
	delete(fake.docs, "B")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "second Push")
	assert.Equal(t, 1, res.Vectors.SessionsEvicted)
	assert.Equal(t, 1, res.Vectors.DocsDeleted)

	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genID)+` WHERE doc_key = $1`,
		"B#0"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = $2`, genID, "B"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "B"))
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "A"))
}

func TestVectorPushNoActiveGeneration(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_nogen_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	sync.vectorSource = &fakeVectorSource{hasGen: false}

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")
	assert.True(t, res.Vectors.Skipped)
	assert.NotEmpty(t, res.Vectors.SkippedReason)
	assert.Equal(t, 0,
		countRows(t, pg, `SELECT COUNT(*) FROM vector_generations`))
}

func TestVectorPushSharedDocAcrossGenerations(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_sharedgen_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "X")
	seedVectorSession(t, localDB, "Y")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-a", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"X": "hx", "Y": "hy"},
		docs: map[string][]VectorPushDoc{
			"X": {vdoc("X", "X#0", 0, "cx", "hx1", []float32{1, 0, 0, 0})},
			"Y": {vdoc("Y", "Y#0", 0, "cy", "hy1", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = fake

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "push gen A")
	genA, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-a")
	require.NoError(t, err)
	require.True(t, ok)

	// Switch to a second generation (new fingerprint) sharing the same docs.
	fake.gen.Fingerprint = "fp-b"
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "push gen B")
	genB, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-b")
	require.NoError(t, err)
	require.True(t, ok)

	// Drop Y from gen B's source, triggering a gen-B eviction of Y.
	delete(fake.hashes, "Y")
	delete(fake.docs, "Y")

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "push gen B without Y")
	assert.Equal(t, 1, res.Vectors.SessionsEvicted)
	assert.Equal(t, 0, res.Vectors.DocsDeleted,
		"Y's doc is still referenced by gen A, so it must survive")

	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genA)+` WHERE doc_key = $1`,
		"Y#0"), "gen A chunks for Y survive")
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genB)+` WHERE doc_key = $1`,
		"Y#0"), "gen B chunks for Y evicted")
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE doc_key = $1`, "Y#0"),
		"shared doc row survives")
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = $2`, genB, "Y"))
}

// TestVectorPushOrdinalShift pins the slot-safe replacement: stable doc_keys
// whose ordinals shift up (0/1 -> 1/2 with a new doc taking slot 0) re-push
// without a UNIQUE (session_id, ordinal) violation and land on their final
// ordinals.
func TestVectorPushOrdinalShift(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_shift_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-sh", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "h1"},
		docs: map[string][]VectorPushDoc{
			"A": {
				vdoc("A", "k0", 0, "c0", "hc0", []float32{1, 0, 0, 0}),
				vdoc("A", "k1", 1, "c1", "hc1", []float32{0, 1, 0, 0}),
			},
		},
	}
	sync.vectorSource = fake

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	// Shift k0/k1 up to ordinals 1/2 and insert kNew at ordinal 0.
	fake.hashes["A"] = "h2"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "kNew", 0, "cn", "hcn", []float32{0, 0, 1, 0}),
		vdoc("A", "k0", 1, "c0", "hc0", []float32{1, 0, 0, 0}),
		vdoc("A", "k1", 2, "c1", "hc1", []float32{0, 1, 0, 0}),
	}

	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "shift Push")
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
	assert.Equal(t, 0, res.Vectors.DocsDeleted)

	assert.Equal(t, 0, docOrdinal(t, pg, "kNew"))
	assert.Equal(t, 1, docOrdinal(t, pg, "k0"))
	assert.Equal(t, 2, docOrdinal(t, pg, "k1"))
	assert.Equal(t, 3,
		countRows(t, pg, `SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "A"))
}

// TestVectorPushDocRemovedAndShared pins that a doc removed from a re-pushed
// session disappears from vector_documents in the same push, cascading its
// chunks out of every generation — including one that still embedded it.
// Regression: preserving the shared row parked at a negative ordinal left the
// older generation with chunks the read path could never hydrate (the
// ordinal >= 0 tombstone guard), silently shrinking its results. Local kit
// removal deletes a vanished doc's vectors from all generations, and PG must
// match.
func TestVectorPushDocRemovedAndShared(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_shrink_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")

	// Gen A embeds kShared + kDropShared (not kDropSolo).
	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-ga", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha"},
		docs: map[string][]VectorPushDoc{
			"A": {
				vdoc("A", "kShared", 0, "cs", "hcs", []float32{1, 0, 0, 0}),
				vdoc("A", "kDropShared", 1, "cds", "hcds", []float32{0, 1, 0, 0}),
			},
		},
	}
	sync.vectorSource = fake
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "push gen A")
	genA, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-ga")
	require.NoError(t, err)
	require.True(t, ok)

	// Gen B embeds all three docs for A.
	fake.gen.Fingerprint = "fp-gb"
	fake.hashes["A"] = "hb1"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "kShared", 0, "cs", "hcs", []float32{1, 0, 0, 0}),
		vdoc("A", "kDropShared", 1, "cds", "hcds", []float32{0, 1, 0, 0}),
		vdoc("A", "kDropSolo", 2, "cdo", "hcdo", []float32{0, 0, 1, 0}),
	}
	_, err = sync.Push(ctx, false, nil)
	require.NoError(t, err, "push gen B")
	genB, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-gb")
	require.NoError(t, err)
	require.True(t, ok)

	// Shrink gen B's A to only kShared: kDropShared + kDropSolo vanish locally.
	fake.hashes["A"] = "hb2"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "kShared", 0, "cs", "hcs", []float32{1, 0, 0, 0}),
	}
	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "shrink push gen B")
	assert.Equal(t, 1, res.Vectors.SessionsPushed)
	assert.Equal(t, 2, res.Vectors.DocsDeleted,
		"kDropSolo and kDropShared both vanished locally")

	// Both vanished docs are gone, whether or not another generation embedded
	// them.
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE doc_key = $1`, "kDropSolo"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE doc_key = $1`, "kDropShared"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genA)+` WHERE doc_key = $1`,
		"kDropShared"), "gen A chunks for kDropShared cascade-deleted")
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genB)+` WHERE doc_key = $1`,
		"kDropShared"), "gen B chunks for kDropShared cleared")
	// No parked tombstones survive the push, and every remaining gen A chunk
	// hydrates: the older generation must not retain chunks pointing at rows
	// the read path's ordinal >= 0 guard hides.
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE ordinal < 0`))
	assert.Equal(t, 0, countRows(t, pg, `
		SELECT COUNT(*) FROM `+vectorChunkTable(genA)+` c
		LEFT JOIN vector_documents d
		  ON d.doc_key = c.doc_key AND d.ordinal >= 0
		WHERE d.doc_key IS NULL`),
		"gen A has no chunks that cannot hydrate")
	// kShared stays live at its positive ordinal in both generations.
	assert.Equal(t, 0, docOrdinal(t, pg, "kShared"))
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genA)+` WHERE doc_key = $1`,
		"kShared"), "gen A keeps its chunks for the still-present doc")
}

// TestVectorPushConflictSkipped pins finding 3: a session whose PG owner marker
// names another machine is skipped on push (counted in Conflicts) and left
// untouched on evict (also counted).
func TestVectorPushConflictSkipped(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_conflict_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-cf", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "cA0", 0, "ca", "hca", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "cB0", 0, "cb", "hcb", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	// Hand A to another machine on the PG side.
	_, err = pg.Exec(
		`UPDATE sessions SET owner_marker = 'other-machine' WHERE id = $1`, "A")
	require.NoError(t, err, "reassign owner")

	// A changed locally: the vector phase must skip it as a conflict, leaving
	// its pushed content intact.
	fake.hashes["A"] = "ha2"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "cA0", 0, "ca-new", "hca2", []float32{0, 1, 0, 0}),
	}
	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "push conflict")
	assert.Equal(t, 1, res.Conflicts)
	assert.Equal(t, 0, res.SessionsPushed)
	var content string
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "cA0",
	).Scan(&content))
	assert.Equal(t, "ca", content, "conflicted doc left untouched")

	// A now vanishes from local: the evict path must leave the other machine's
	// session in place and count the conflict.
	delete(fake.hashes, "A")
	delete(fake.docs, "A")
	res, err = sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "evict conflict")
	assert.Equal(t, 1, res.Conflicts)
	assert.Equal(t, 0, res.SessionsEvicted)
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-cf")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = $2`, genID, "A"))
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "A"))
}

// TestVectorPushLegacyMachineOwnership pins machine-aware ownership for
// legacy PG rows whose owner_marker is empty: a foreign machine name blocks
// both the push and evict paths (counted as conflicts), while this pusher's
// own machine name self-heals — its vectors push and evict normally.
func TestVectorPushLegacyMachineOwnership(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_legacy_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-lg", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "lA0", 0, "ca", "hca", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "lB0", 0, "cb", "hcb", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	// Rewrite both PG rows as legacy (empty marker): A owned by a foreign
	// machine, B carrying this pusher's own machine name.
	_, err = pg.Exec(`UPDATE sessions
	   SET owner_marker = '', machine = 'other-machine' WHERE id = $1`, "A")
	require.NoError(t, err, "make A a foreign legacy row")
	_, err = pg.Exec(`UPDATE sessions
	   SET owner_marker = '', machine = 'test-machine' WHERE id = $1`, "B")
	require.NoError(t, err, "make B an owned legacy row")

	// Both change locally: only B may push.
	fake.hashes["A"], fake.hashes["B"] = "ha2", "hb2"
	fake.docs["A"] = []VectorPushDoc{
		vdoc("A", "lA0", 0, "ca-new", "hca2", []float32{0, 1, 0, 0}),
	}
	fake.docs["B"] = []VectorPushDoc{
		vdoc("B", "lB0", 0, "cb-new", "hcb2", []float32{0, 0, 1, 0}),
	}
	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "legacy push")
	assert.Equal(t, 1, res.Conflicts, "foreign legacy row is a conflict")
	assert.Equal(t, 1, res.SessionsPushed, "owned legacy row self-heals")
	var content string
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "lA0",
	).Scan(&content))
	assert.Equal(t, "ca", content, "foreign legacy doc left untouched")
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "lB0",
	).Scan(&content))
	assert.Equal(t, "cb-new", content)

	// Both vanish locally: only B may be evicted.
	fake.hashes = map[string]string{}
	fake.docs = map[string][]VectorPushDoc{}
	res, err = sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "legacy evict")
	assert.Equal(t, 1, res.Conflicts, "foreign legacy row is not evicted")
	assert.Equal(t, 1, res.SessionsEvicted)
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "A"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "B"))
}

// TestVectorPushEvictionSkippedWhenSourceTurnsUnready pins the pre-eviction
// re-check: when the local source becomes unready between the delta scan and
// the eviction pass (an embeddings rebuild started mid-push), the eviction
// list — computed from a now-partial local view — must be dropped, and the
// next healthy push re-derives and applies it.
func TestVectorPushEvictionSkippedWhenSourceTurnsUnready(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_unready_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-ur", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "uA0", 0, "ca", "hca", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "uB0", 0, "cb", "hcb", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")

	// B vanishes locally, but the source turns unready before the evict pass.
	delete(fake.hashes, "B")
	delete(fake.docs, "B")
	fake.genCalls = 0
	fake.genOverride = func(call int) (VectorGenerationInfo, bool, error) {
		if call == 1 {
			return fake.gen, true, nil
		}
		return VectorGenerationInfo{}, false, fmt.Errorf(
			"%w: rebuild started", ErrVectorSourceNotReady)
	}
	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "push with mid-push rebuild")
	assert.Equal(t, 0, res.SessionsEvicted, "eviction must be dropped")
	assert.Equal(t, 1, res.SessionsDeferred,
		"abandoned evictions must surface as deferred so the watch loop schedules a generation-wide reconciliation")
	assert.Equal(t, 1, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "B"),
		"B's docs survive the aborted eviction")

	// The next healthy push evicts B.
	fake.genOverride = nil
	res, err = sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "healthy push")
	assert.Equal(t, 1, res.SessionsEvicted)
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "B"))
}

// TestVectorPushProjectScope pins the project-scope gate: a filtered push
// (projects=[alpha]) must leave vector state for an out-of-scope beta session
// already in PG untouched, while still pushing the in-scope alpha session's
// changes. The vectors.db source carries every session regardless of scope, so
// without the gate the beta doc would be re-pushed under a filtered push.
func TestVectorPushProjectScope(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_scope_test")
	ctx := context.Background()

	seedVectorSessionProject(t, localDB, "alpha", "alpha")
	seedVectorSessionProject(t, localDB, "beta", "beta")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-scope", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"alpha": "ha", "beta": "hb"},
		docs: map[string][]VectorPushDoc{
			"alpha": {vdoc("alpha", "alpha#0", 0, "a-orig", "ha1", []float32{1, 0, 0, 0})},
			"beta":  {vdoc("beta", "beta#0", 0, "b-orig", "hb1", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake

	// Unfiltered push lands both alpha and beta vectors in PG.
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "unfiltered push")

	// Restrict the push to project alpha and change both sessions locally.
	sync.projects = []string{"alpha"}
	fake.hashes["alpha"] = "ha2"
	fake.docs["alpha"] = []VectorPushDoc{
		vdoc("alpha", "alpha#0", 0, "a-updated", "ha2", []float32{0, 1, 0, 0}),
	}
	fake.hashes["beta"] = "hb2"
	fake.docs["beta"] = []VectorPushDoc{
		vdoc("beta", "beta#0", 0, "b-updated", "hb2", []float32{0, 1, 0, 0}),
	}

	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "filtered vector push")
	assert.Equal(t, 1, res.SessionsPushed, "only in-scope alpha pushed")
	assert.Equal(t, 0, res.Conflicts, "beta is out of scope, not a conflict")
	assert.Equal(t, 0, res.SessionsEvicted, "beta must not be evicted")

	var alphaContent, betaContent string
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "alpha#0",
	).Scan(&alphaContent))
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "beta#0",
	).Scan(&betaContent))
	assert.Equal(t, "a-updated", alphaContent, "in-scope alpha updated")
	assert.Equal(t, "b-orig", betaContent, "out-of-scope beta untouched")

	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-scope")
	require.NoError(t, err)
	require.True(t, ok)
	var betaHash string
	require.NoError(t, pg.QueryRow(
		`SELECT doc_agg_hash FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = $2`, genID, "beta",
	).Scan(&betaHash))
	assert.Equal(t, "hb", betaHash, "beta push state left at its original hash")
}

// TestVectorPushLocalProjectMoveOutOfScope pins that filtered pushes scope
// local candidates by their LIVE local project, not the (possibly stale) PG
// project. A session pushed in scope (project alpha) then moved out of scope
// locally (alpha->beta) keeps its old PG project because the filtered session
// push skips it; scoping by PG would let the next `pg push --projects alpha`
// re-push or evict its vectors. Scoping by the local project must leave them
// untouched.
func TestVectorPushLocalProjectMoveOutOfScope(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_localmove_test")
	ctx := context.Background()

	seedVectorSessionProject(t, localDB, "mover", "alpha")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-move", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"mover": "h1"},
		docs: map[string][]VectorPushDoc{
			"mover": {vdoc("mover", "mover#0", 0, "orig", "hc1", []float32{1, 0, 0, 0})},
		},
	}
	sync.vectorSource = fake

	// Unfiltered push lands the alpha vector in PG (PG project = alpha).
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "unfiltered push")

	// The session moves to beta locally, but its PG sessions.project stays alpha
	// (a filtered session push skips it). The local doc content also changes.
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID:               "mover",
		Project:          "beta",
		Machine:          "test-machine",
		Agent:            "claude",
		MessageCount:     1,
		UserMessageCount: 1,
		CreatedAt:        "2026-01-01T00:00:00Z",
	}), "move mover to beta")
	fake.hashes["mover"] = "h2"
	fake.docs["mover"] = []VectorPushDoc{
		vdoc("mover", "mover#0", 0, "updated", "hc2", []float32{0, 1, 0, 0}),
	}

	var pgProject string
	require.NoError(t, pg.QueryRow(
		`SELECT project FROM sessions WHERE id = $1`, "mover",
	).Scan(&pgProject))
	require.Equal(t, "alpha", pgProject, "PG project is still stale alpha")

	// A push filtered to alpha must treat mover (now local beta) as out of scope.
	sync.projects = []string{"alpha"}
	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "filtered vector push after local move")
	assert.Equal(t, 0, res.SessionsPushed, "moved-out session not pushed")
	assert.Equal(t, 0, res.SessionsEvicted, "moved-out session not evicted")
	assert.Equal(t, 0, res.Conflicts, "moved-out session is not a conflict")

	var content string
	require.NoError(t, pg.QueryRow(
		`SELECT content FROM vector_documents WHERE doc_key = $1`, "mover#0",
	).Scan(&content))
	assert.Equal(t, "orig", content, "out-of-scope vector left untouched")

	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-move")
	require.NoError(t, err)
	require.True(t, ok)
	var stateHash string
	require.NoError(t, pg.QueryRow(
		`SELECT doc_agg_hash FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = $2`, genID, "mover",
	).Scan(&stateHash))
	assert.Equal(t, "h1", stateHash, "push state left at its original hash")
}

// TestVectorPushSkipsDocExportForMissingSession pins that a local-only session
// with no PG sessions row is skipped without exporting its docs. Doc export
// (content + chunk decode) must run only after the in-tx existence probe
// passes, so a session that always skips never re-exports docs each push.
func TestVectorPushSkipsDocExportForMissingSession(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, _ := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_skipexport_test")
	ctx := context.Background()

	// "A" is a real session (pushed to PG). "ghost" is local-only: it never gets
	// a PG sessions row, so the vector push must skip it.
	seedVectorSession(t, localDB, "A")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-skip", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "ghost": "hg"},
		docs: map[string][]VectorPushDoc{
			"A":     {vdoc("A", "A#0", 0, "ca", "hca", []float32{1, 0, 0, 0})},
			"ghost": {vdoc("ghost", "g#0", 0, "cg", "hcg", []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = fake

	// Push seeds A's PG sessions row (session phase) then runs the vector phase.
	// ghost is absent from localDB, so it never gets a PG sessions row.
	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "Push")
	assert.Equal(t, 1, res.Vectors.SessionsPushed, "only A lands")
	assert.Equal(t, 0, res.Vectors.Conflicts, "missing session is not a conflict")

	assert.Equal(t, 1, fake.docsCalls["A"], "A's docs exported once")
	assert.Zero(t, fake.docsCalls["ghost"],
		"missing session's docs never exported")
}

// TestVectorPushOrphanStateEvicted pins finding-3 rule d: a vector_push_state
// row whose sessions row has vanished is evicted regardless of owner marker.
func TestVectorPushOrphanStateEvicted(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_orphan_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-or", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "oA0", 0, "ca", "hca", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "oB0", 0, "cb", "hcb", []float32{0, 0, 0, 1})},
		},
	}
	sync.vectorSource = fake
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "first Push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-or")
	require.NoError(t, err)
	require.True(t, ok)

	// Session A's row disappears (e.g. deleted) but its vector state lingers,
	// and A leaves local so it is an eviction candidate.
	_, err = pg.Exec(`DELETE FROM sessions WHERE id = $1`, "A")
	require.NoError(t, err, "delete session row")
	delete(fake.hashes, "A")
	delete(fake.docs, "A")

	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "evict orphan")
	assert.Equal(t, 1, res.SessionsEvicted)
	assert.Equal(t, 0, res.Conflicts, "orphaned state is not a conflict")

	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = $2`, genID, "A"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM `+vectorChunkTable(genID)+` WHERE doc_key = $1`, "oA0"))
	assert.Equal(t, 0, countRows(t, pg,
		`SELECT COUNT(*) FROM vector_documents WHERE session_id = $1`, "A"))
}

// TestVectorPushDefersSessionWhenExportDiverges pins the replacement-path
// guard against concurrent local rebuilds: when a session's export hash no
// longer matches the hash the delta scan read (an embeddings build rewrote
// the local index between the two reads), the session is deferred with
// nothing written — the previously pushed PG chunks and push state survive
// untouched. Without the guard, the push would replace valid PG vectors with
// the partial view and record the stale scan hash as current, which a
// same-fingerprint rebuild (same content, same hash) would never repair.
func TestVectorPushDefersSessionWhenExportDiverges(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_push_diverge_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")

	source := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-div", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1",
				[]float32{1, 0, 0, 0}, []float32{0, 1, 0, 0})},
		},
	}
	sync.vectorSource = source

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "seed Push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-div")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)))

	// The delta scan sees a changed session, but by export time the local
	// index has moved again: SessionDocs returns a shrunken doc set whose
	// hash differs from the scan's.
	source.hashes = map[string]string{"A": "ha-changed"}
	source.exportHashes = map[string]string{"A": "ha-mid-rebuild"}
	source.docs = map[string][]VectorPushDoc{
		"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{9, 9, 9, 9})},
	}

	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "diverged pushVectors")
	assert.Equal(t, 1, res.SessionsDeferred, "diverged session must defer")
	assert.Equal(t, 0, res.SessionsPushed)

	assert.Equal(t, 2,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)),
		"previously pushed chunks must survive the deferral")
	assert.Equal(t, 1, countRows(t, pg, `
		SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = 'A' AND doc_agg_hash = 'ha'`,
		genID), "push state must keep the last successfully exported hash")

	// Once the local index settles (export hash matches the scan again), the
	// deferred session is re-derived and pushed.
	source.exportHashes = nil
	res, err = sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "settled pushVectors")
	assert.Equal(t, 1, res.SessionsPushed)
	assert.Equal(t, 1,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)))
}

// TestVectorEvictionRechecksOwnershipInTx pins the eviction-path TOCTOU
// guard: ownership read by the delta scan can be minutes stale, so each
// eviction transaction re-probes the sessions row FOR UPDATE. A session
// another pusher claimed after the scan is skipped as a conflict — its new
// owner's chunks and push state survive — while a session whose row vanished
// is still evicted.
func TestVectorEvictionRechecksOwnershipInTx(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_evict_recheck_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")
	seedVectorSession(t, localDB, "B")

	sync.vectorSource = &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-ev", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha", "B": "hb"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
			"B": {vdoc("B", "B#0", 0, "c2", "h2", []float32{0, 1, 0, 0})},
		},
	}

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "seed Push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-ev")
	require.NoError(t, err)
	require.True(t, ok)

	// Simulate a concurrent pusher claiming A after this push's delta scan
	// (which would have seen it as ours), and B's session row vanishing.
	_, err = pg.ExecContext(ctx, `
		UPDATE sessions SET owner_marker = 'other-marker', machine = 'other-machine'
		 WHERE id = 'A'`)
	require.NoError(t, err, "reassign A")
	_, err = pg.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'B'`)
	require.NoError(t, err, "delete B")

	owner, err := sync.vectorOwnerIdentity(ctx)
	require.NoError(t, err)
	allGenIDs, err := sync.allVectorGenerationIDs(ctx)
	require.NoError(t, err)
	genIDs, err := sync.existingChunkGenerations(ctx, allGenIDs)
	require.NoError(t, err)
	scope := vectorPushScope{
		gen:         vectorGeneration{id: genID},
		fingerprint: "fp-ev",
		genIDs:      genIDs,
		owner:       owner,
	}

	var res VectorPushResult
	require.NoError(t,
		sync.evictVectorSessions(ctx, scope, []string{"A", "B"}, &res))

	assert.Equal(t, 1, res.Conflicts, "reclaimed session is a conflict")
	assert.Equal(t, 1, res.SessionsEvicted, "vanished session is evicted")
	assert.Equal(t, 1, countRows(t, pg, `
		SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = 'A'`, genID),
		"the new owner's push state survives")
	assert.Equal(t, 1, countRows(t, pg, `
		SELECT COUNT(*) FROM `+vectorChunkTable(genID)+` c
		 WHERE c.doc_key IN (
			SELECT doc_key FROM vector_documents WHERE session_id = 'A')`),
		"the new owner's chunks survive")
	assert.Equal(t, 0, countRows(t, pg, `
		SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = 'B'`, genID))
}

// TestVectorPushEvictionDefersFailedSession pins that failedSessions gates
// eviction, not just replacement: a failed session with zero embedded docs is
// absent from the local hash map and would otherwise be evicted — vector
// state running ahead of the sessions/messages rows its session-phase push
// failed to write. It must defer instead, and evict on the next clean push.
func TestVectorPushEvictionDefersFailedSession(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_evict_failed_test")
	ctx := context.Background()

	seedVectorSession(t, localDB, "A")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-evf", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"A": "ha"},
		docs: map[string][]VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
		},
	}
	sync.vectorSource = fake

	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "seed Push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-evf")
	require.NoError(t, err)
	require.True(t, ok)

	// A's embedded docs all vanish locally while its session push fails.
	fake.hashes = map[string]string{}
	fake.docs = map[string][]VectorPushDoc{}

	res, err := sync.pushVectors(
		ctx, false, nil, 0, map[string]struct{}{"A": {}}, nil,
	)
	require.NoError(t, err, "failed-session pushVectors")
	assert.Equal(t, 0, res.SessionsEvicted, "failed session must not be evicted")
	assert.Equal(t, 1, res.SessionsDeferred)
	assert.Equal(t, 1,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)),
		"failed session keeps its vectors")
	assert.Equal(t, 1, countRows(t, pg, `
		SELECT COUNT(*) FROM vector_push_state
		 WHERE generation_id = $1 AND session_id = 'A'`, genID))

	res, err = sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "clean pushVectors")
	assert.Equal(t, 1, res.SessionsEvicted, "next clean push evicts")
	assert.Equal(t, 0,
		countRows(t, pg, `SELECT COUNT(*) FROM `+vectorChunkTable(genID)))
}

// TestVectorPushFilteredEvictionScopesByLocalProject pins the eviction-scope
// partition for filtered pushes: an eviction candidate absent from the vector
// hash map can still have a live local sessions row (zero embedded docs), and
// that row's LIVE project — not the stale PG project — decides scope. Only
// candidates genuinely absent from the local sessions table fall back to PG
// project scoping.
func TestVectorPushFilteredEvictionScopesByLocalProject(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(
		t, pgURL, "agentsview_vector_evict_scope_test")
	ctx := context.Background()

	seedVectorSessionProject(t, localDB, "moved", "alpha")
	seedVectorSessionProject(t, localDB, "gone-out", "beta")
	seedVectorSessionProject(t, localDB, "gone-in", "alpha")

	fake := &fakeVectorSource{
		gen:    VectorGenerationInfo{Fingerprint: "fp-evs", Model: "m", Dimension: 4},
		hasGen: true,
		hashes: map[string]string{"moved": "hm", "gone-out": "ho", "gone-in": "hi"},
		docs: map[string][]VectorPushDoc{
			"moved":    {vdoc("moved", "m#0", 0, "cm", "hm1", []float32{1, 0, 0, 0})},
			"gone-out": {vdoc("gone-out", "o#0", 0, "co", "ho1", []float32{0, 1, 0, 0})},
			"gone-in":  {vdoc("gone-in", "i#0", 0, "ci", "hi1", []float32{0, 0, 1, 0})},
		},
	}
	sync.vectorSource = fake

	// Unfiltered push lands all three sessions' vectors and PG rows.
	_, err := sync.Push(ctx, false, nil)
	require.NoError(t, err, "unfiltered push")
	genID, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-evs")
	require.NoError(t, err)
	require.True(t, ok)

	// "moved" changes project locally (alpha -> beta) — its PG project stays
	// alpha because a filtered session push skips it. The others vanish from
	// the local archive entirely. All three drop out of the embedded set.
	require.NoError(t, localDB.UpsertSession(db.Session{
		ID: "moved", Project: "beta", Machine: "test-machine",
		Agent: "claude", MessageCount: 1, UserMessageCount: 1,
		CreatedAt: "2026-01-01T00:00:00Z",
	}), "move session to beta")
	require.NoError(t, localDB.DeleteSession("gone-out"))
	require.NoError(t, localDB.DeleteSession("gone-in"))
	fake.hashes = map[string]string{}
	fake.docs = map[string][]VectorPushDoc{}

	sync.projects = []string{"alpha"}
	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "filtered pushVectors")

	assert.Equal(t, 1, res.SessionsEvicted,
		"only the locally-absent alpha session is evicted")
	chunkCount := func(sessionID string) int {
		return countRows(t, pg, `
			SELECT COUNT(*) FROM `+vectorChunkTable(genID)+` c
			 WHERE c.doc_key IN (
				SELECT doc_key FROM vector_documents WHERE session_id = $1)`,
			sessionID)
	}
	assert.Equal(t, 1, chunkCount("moved"),
		"session that moved out of the filter locally is untouched")
	assert.Equal(t, 1, chunkCount("gone-out"),
		"locally-absent session with out-of-filter PG project is untouched")
	assert.Equal(t, 0, chunkCount("gone-in"),
		"locally-absent session with in-filter PG project is evicted")
}

// TestVectorPushSkipsOnInsufficientPrivilege pins the restricted-role degrade
// path: a schema provisioned by a privileged role (core tables exist) whose
// push role cannot CREATE means the vector base DDL fails with SQLSTATE
// 42501. That must skip the vector phase with a reason — matching a database
// without pgvector — not fail a push whose session phase succeeded.
func TestVectorPushSkipsOnInsufficientPrivilege(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_vector_privilege_test"
	const role = "agentsview_vector_restricted"
	const rolePassword = "agentsview_vector_restricted_pw"

	admin, err := Open(pgURL, schema, true)
	require.NoError(t, err, "Open admin")
	t.Cleanup(func() { _ = admin.Close() })

	ctx := context.Background()
	_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
	require.NoError(t, err, "drop schema")
	require.NoError(t, EnsureSchema(ctx, admin, schema))
	unavailable, err := ensureVectorBaseSchemaPG(ctx, admin)
	require.NoError(t, err)
	if unavailable != "" {
		t.Skip(unavailable)
	}

	// Simulate a schema provisioned by an older, pre-vector agentsview:
	// core tables exist but the vector tables do not, so the restricted
	// role's own setup must CREATE them — and cannot.
	for _, tbl := range []string{
		"vector_push_state", "vector_documents",
		"vector_generation_machines", "vector_generations",
	} {
		_, err = admin.Exec(`DROP TABLE IF EXISTS ` + tbl + ` CASCADE`)
		require.NoError(t, err, tbl)
	}

	_, _ = admin.Exec(`DROP OWNED BY ` + role) // clear grants from prior runs
	_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	_, err = admin.Exec(
		`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + rolePassword + `'`)
	require.NoError(t, err, "create restricted role")
	t.Cleanup(func() {
		_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
		_, _ = admin.Exec(`DROP OWNED BY ` + role)
		_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schema + ` TO ` + role,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` +
			schema + ` TO ` + role,
	} {
		_, err = admin.Exec(grant)
		require.NoError(t, err, grant)
	}

	restrictedURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	restrictedURL.User = url.UserPassword(role, rolePassword)
	restricted, err := Open(restrictedURL.String(), schema, true)
	require.NoError(t, err, "Open restricted")
	t.Cleanup(func() { _ = restricted.Close() })

	localDB, err := db.Open(filepath.Join(t.TempDir(), "local.db"))
	require.NoError(t, err, "db.Open")
	t.Cleanup(func() { _ = localDB.Close() })

	sync := &Sync{
		pg:         restricted,
		local:      localDB,
		machine:    "test-machine",
		schema:     schema,
		schemaDone: true,
	}
	sync.vectorSource = &fakeVectorSource{
		gen: VectorGenerationInfo{
			Fingerprint: "fp-priv", Model: "m", Dimension: 4,
		},
		hasGen: true,
		hashes: map[string]string{},
	}

	res, err := sync.pushVectors(ctx, false, nil, 0, nil, nil)
	require.NoError(t, err, "privilege failure must skip, not fail the push")
	assert.True(t, res.Skipped)
	assert.Contains(t, res.SkippedReason, "privileges")
}
