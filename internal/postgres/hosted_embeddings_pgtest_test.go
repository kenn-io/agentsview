//go:build pgtest

package postgres

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func embeddingRecipe() HostedEmbeddingRecipe {
	return HostedEmbeddingRecipe{ProfileName: "test", Model: "synthetic", Dimensions: 3, BuilderVersion: "conversation-v1", ChunkerVersion: "rune-v1", MaxInputChars: 64, DocumentPrefix: "doc: ", QueryPrefix: "query: ", EncodingType: HostedEmbeddingEncoding, CorpusScope: "conversation-v1"}
}
func embeddingFixture(t *testing.T) (hostedFixture, *HostedEmbeddingStore, HostedEmbeddingGeneration) {
	t.Helper()
	f := newHostedFixture(t, "tenant-embedding")
	g, err := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	require.NoError(t, err)
	s, err := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant, Limits: HostedEmbeddingLimits{RowPageSize: 2, MaxOccurrenceBytes: 1}})
	require.NoError(t, err)
	return f, s, g
}
func reconcileEmbedding(t *testing.T, s *HostedEmbeddingStore) {
	t.Helper()
	for range 4 {
		_, err := s.Reconcile(t.Context(), 256)
		require.NoError(t, err)
	}
}
func TestHostedEmbeddingSourceAndPublication(t *testing.T) {
	f, s, g := embeddingFixture(t)
	_, err := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES ('s:1','p','m','codex');
 INSERT INTO messages(session_id,ordinal,role,source_uuid,content) VALUES
 ('s:1',0,'user','dup','hello'),('s:1',1,'assistant','dup','é'),
 ('s:1',2,'user','hidden','<system-reminder> hidden</system-reminder>'),('s:1',3,'assistant','','猫'),
 ('s:1',4,'user','dup','next');`)
	require.NoError(t, err)
	reconcileEmbedding(t, s)
	leases, err := s.Claim(t.Context(), "worker", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	snap, err := s.ReadSession(t.Context(), leases[0])
	require.NoError(t, err)
	require.Len(t, snap.Documents, 3)
	assert.Equal(t, "u:s%3A1:dup", snap.Documents[0].Key)
	assert.Equal(t, "r:s%3A1:dup#2", snap.Documents[1].Key)
	assert.Equal(t, "u:s%3A1:dup#3", snap.Documents[2].Key)
	assert.Equal(t, db.EmbeddableUnit{SessionID: "s:1", Kind: "run", SourceUUID: "dup", Ordinal: 1, OrdinalEnd: 3, Content: "é\n\n猫", Offsets: []db.UnitOffset{{Ordinal: 1}, {Ordinal: 3, RuneStart: 3, ByteStart: 4}}}, snap.Documents[1].Unit)
	assert.Equal(t, "é\n\n猫", snap.Documents[1].Chunks[0].Text)
	local, err := db.Open(filepath.Join(t.TempDir(), "parity.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, local.Close()) })
	require.NoError(t, local.UpsertSession(db.Session{ID: "s:1", Project: "p", Machine: "m", Agent: "codex"}))
	require.NoError(t, local.InsertMessages([]db.Message{{SessionID: "s:1", Ordinal: 0, Role: "user", SourceUUID: "dup", Content: "hello"}, {SessionID: "s:1", Ordinal: 1, Role: "assistant", SourceUUID: "dup", Content: "é"}, {SessionID: "s:1", Ordinal: 2, Role: "user", SourceUUID: "hidden", Content: "<system-reminder> hidden</system-reminder>"}, {SessionID: "s:1", Ordinal: 3, Role: "assistant", Content: "猫"}, {SessionID: "s:1", Ordinal: 4, Role: "user", SourceUUID: "dup", Content: "next"}}))
	var localUnits []db.EmbeddableUnit
	_, err = local.ScanEmbeddableUnits(t.Context(), "", false, func(u db.EmbeddableUnit) error { localUnits = append(localUnits, u); return nil })
	require.NoError(t, err)
	expected := []db.EmbeddableUnit{{SessionID: "s:1", Kind: "user", SourceUUID: "dup", Ordinal: 0, OrdinalEnd: 0, Content: "hello"}, {SessionID: "s:1", Kind: "run", SourceUUID: "dup", Ordinal: 1, OrdinalEnd: 3, Content: "é\n\n猫", Offsets: []db.UnitOffset{{Ordinal: 1}, {Ordinal: 3, RuneStart: 3, ByteStart: 4}}}, {SessionID: "s:1", Kind: "user", SourceUUID: "dup", Ordinal: 4, OrdinalEnd: 4, Content: "next"}}
	assert.Equal(t, expected, localUnits)
	assert.Equal(t, expected, []db.EmbeddableUnit{snap.Documents[0].Unit, snap.Documents[1].Unit, snap.Documents[2].Unit})

	ok, err := s.Activate(t.Context(), g.ID)
	require.NoError(t, err)
	assert.False(t, ok)
	vectors := []HostedEmbeddingVector{{DocumentKey: "u:s%3A1:dup", ChunkIndex: 0, Values: []float32{1, 0, 0}}, {DocumentKey: "r:s%3A1:dup#2", ChunkIndex: 0, Values: []float32{0, 1, 0}}, {DocumentKey: "u:s%3A1:dup#3", ChunkIndex: 0, Values: []float32{0, 0, 1}}}
	assert.ErrorIs(t, s.Publish(t.Context(), snap, vectors[:2]), ErrHostedEmbeddingInvalidResults)
	require.NoError(t, s.Publish(t.Context(), snap, vectors))
	ok, err = s.Activate(t.Context(), g.ID)
	require.NoError(t, err)
	assert.True(t, ok)
	var content string
	require.NoError(t, f.runtime.QueryRow(`SELECT content FROM hosted_embedding_documents WHERE doc_key='r:s%3A1:dup#2'`).Scan(&content))
	assert.Equal(t, "é\n\n猫", content)
	_, err = f.runtime.Exec(`UPDATE messages SET content='edited' WHERE session_id='s:1' AND ordinal=0`)
	require.NoError(t, err)
	assert.ErrorIs(t, s.Publish(t.Context(), snap, vectors), ErrHostedEmbeddingStale)
	var visible int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_documents d JOIN hosted_embedding_sources j ON j.tenant_id=d.tenant_id AND j.session_id=d.session_id AND j.revision=d.source_revision`).Scan(&visible))
	assert.Zero(t, visible)
}
func TestHostedEmbeddingEmptyAndProtected(t *testing.T) {
	f, s, g := embeddingFixture(t)
	ok, err := s.Activate(t.Context(), g.ID)
	require.NoError(t, err)
	assert.False(t, ok)
	reconcileEmbedding(t, s)
	ok, err = s.Activate(t.Context(), g.ID)
	require.NoError(t, err)
	assert.True(t, ok)
	_, err = f.runtime.Exec(`UPDATE hosted_embedding_generations SET recipe_json='{}'`)
	assert.Error(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	_, err = f.admin.Exec(`CREATE TABLE hosted_embedding_chunks_g999 (id integer)`)
	require.NoError(t, err)
	assert.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedEmbeddingWriteGrantPreflight(t *testing.T) {
	f, s, _ := embeddingFixture(t)
	require.NoError(t, s.CheckWritable(t.Context()))
	_, err := f.admin.Exec(`REVOKE UPDATE ON hosted_embedding_requirements FROM "` + f.role + `"`)
	require.NoError(t, err)
	assert.Error(t, s.CheckWritable(t.Context()))
	_, err = NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant})
	assert.NoError(t, err)
}

func TestHostedEmbeddingEligibilityAndZeroDocuments(t *testing.T) {
	f, s, g := embeddingFixture(t)
	_, e := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent,is_automated,prompt_evidence_discarded,deleted_at) VALUES
 ('visible','p','m','codex',false,false,NULL),('automated','p','m','codex',true,false,NULL),('discarded','p','m','codex',false,true,NULL),('trashed','p','m','codex',false,false,clock_timestamp()),('empty','p','m','codex',false,false,NULL);
 INSERT INTO messages(session_id,ordinal,role,content,is_system) VALUES('visible',0,'user','shown',false),('visible',1,'user','system',true),('visible',2,'tool','tool',false),('automated',0,'user','automated',false),('discarded',0,'user','discarded',false),('trashed',0,'user','trashed',false);`)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	leases, e := s.Claim(t.Context(), "worker", 10, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 2)
	for _, l := range leases {
		snap, e := s.ReadSession(t.Context(), l)
		require.NoError(t, e)
		if l.SessionID == "empty" {
			assert.Empty(t, snap.Documents)
			require.NoError(t, s.Publish(t.Context(), snap, nil))
		} else {
			require.Equal(t, "visible", l.SessionID)
			require.Len(t, snap.Documents, 1)
			assert.Equal(t, "shown", snap.Documents[0].Unit.Content)
			require.NoError(t, s.Publish(t.Context(), snap, []HostedEmbeddingVector{{DocumentKey: "o:visible:0", ChunkIndex: 0, Values: []float32{1, 0, 0}}}))
		}
	}
	ok, e := s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.True(t, ok)
	status, e := s.Status(t.Context())
	require.NoError(t, e)
	assert.Equal(t, int64(2), status.Desired.Complete)
	var before, after int64
	require.NoError(t, f.runtime.QueryRow(`SELECT revision FROM hosted_embedding_sources WHERE session_id='visible'`).Scan(&before))
	_, e = f.runtime.Exec(`UPDATE sessions SET display_name='title',updated_at=clock_timestamp() WHERE id='visible'`)
	require.NoError(t, e)
	require.NoError(t, f.runtime.QueryRow(`SELECT revision FROM hosted_embedding_sources WHERE session_id='visible'`).Scan(&after))
	assert.Equal(t, before, after)
}

func TestHostedEmbeddingUsageOnlyClearsAllGenerations(t *testing.T) {
	f, s, g, snap := embeddingOne(t)
	require.NoError(t, s.Publish(t.Context(), snap, embeddingOneVector()))
	ok, e := s.Activate(t.Context(), g.ID)
	require.NoError(t, e)
	assert.True(t, ok)
	second, e := ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "rebuild", f.role)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	leases, e := s.Claim(t.Context(), "rebuild", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	newSnap, e := s.ReadSession(t.Context(), leases[0])
	require.NoError(t, e)
	reuse, e := s.ReusableVectors(t.Context(), newSnap)
	require.NoError(t, e)
	require.Len(t, reuse, 1)
	require.NoError(t, s.ClearContent(t.Context()))
	reuse, e = s.ReusableVectors(t.Context(), newSnap)
	require.NoError(t, e)
	assert.Empty(t, reuse)
	assert.Error(t, s.Publish(t.Context(), newSnap, embeddingOneVector()))
	ok, e = s.Activate(t.Context(), second.ID)
	require.NoError(t, e)
	assert.False(t, ok)
	active, e := s.Active(t.Context())
	require.NoError(t, e)
	assert.Nil(t, active)
	var count int
	require.NoError(t, f.runtime.QueryRow(`SELECT count(*) FROM hosted_embedding_documents`).Scan(&count))
	assert.Zero(t, count)
	reconcileEmbedding(t, s)
	leases, e = s.Claim(t.Context(), "fresh", 2, time.Minute)
	require.NoError(t, e)
	assert.NotEmpty(t, leases)
	active, e = s.Active(t.Context())
	require.NoError(t, e)
	assert.Nil(t, active)
	for _, lease := range leases {
		if lease.GenerationID == second.ID {
			fresh, e := s.ReadSession(t.Context(), lease)
			require.NoError(t, e)
			require.NoError(t, s.Publish(t.Context(), fresh, embeddingOneVector()))
		}
	}
	ok, e = s.Activate(t.Context(), second.ID)
	require.NoError(t, e)
	assert.True(t, ok)
	active, e = s.Active(t.Context())
	require.NoError(t, e)
	require.NotNil(t, active)
	assert.Equal(t, second.ID, active.ID)
}

func TestHostedEmbeddingBackfillBehindCursorAndBounds(t *testing.T) {
	f := newHostedFixture(t, "tenant-embedding")
	_, e := f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('z','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('z',0,'assistant',''),('z',1,'assistant',''),('z',2,'assistant','')`)
	require.NoError(t, e)
	_, e = ProvisionHostedEmbeddings(t.Context(), f.admin, f.schema, f.tenant, embeddingRecipe(), "initial", f.role)
	require.NoError(t, e)
	s, e := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant, Limits: HostedEmbeddingLimits{RowPageSize: 1, MaxRows: 2}})
	require.NoError(t, e)
	result, e := s.Reconcile(t.Context(), 1)
	require.NoError(t, e)
	assert.Equal(t, 1, result.Backfill)
	_, e = f.runtime.Exec(`INSERT INTO sessions(id,project,machine,agent) VALUES('a','p','m','codex');INSERT INTO messages(session_id,ordinal,role,content) VALUES('a',0,'user','new lower key')`)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	leases, e := s.Claim(t.Context(), "worker", 2, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 2)
	for _, l := range leases {
		snap, e := s.ReadSession(t.Context(), l)
		if l.SessionID == "z" {
			assert.ErrorIs(t, e, ErrHostedEmbeddingWorkLimit)
			assert.Nil(t, snap)
			require.NoError(t, s.Fail(t.Context(), l, "work_limit", false))
		} else {
			require.NoError(t, e)
			assert.Equal(t, "new lower key", snap.Documents[0].Unit.Content)
		}
	}
	status, e := s.Status(t.Context())
	require.NoError(t, e)
	assert.True(t, status.Desired.BackfillFinished)
	assert.False(t, status.ActivationReady)
	assert.Equal(t, int64(1), status.Desired.Failed)
}

func TestHostedEmbeddingVectorValidationAndHalfvecNormalization(t *testing.T) {
	f, s, _, snap := embeddingOne(t)
	for _, values := range [][]float32{{0, 0, 0}, {1, 0}, {float32(math.NaN()), 0, 0}, {float32(math.Inf(1)), 0, 0}} {
		assert.ErrorIs(t, s.Publish(t.Context(), snap, []HostedEmbeddingVector{{"o:s:0", 0, values}}), ErrHostedEmbeddingInvalidResults)
	}
	assert.ErrorIs(t, s.Publish(t.Context(), snap, []HostedEmbeddingVector{{"o:s:0", 1, []float32{1, 0, 0}}}), ErrHostedEmbeddingInvalidResults)
	require.NoError(t, s.Publish(t.Context(), snap, []HostedEmbeddingVector{{"o:s:0", 0, []float32{math.MaxFloat32, 0, 0}}}))
	var stored string
	require.NoError(t, f.runtime.QueryRow(`SELECT embedding::text FROM hosted_embedding_chunks_g1`).Scan(&stored))
	assert.Equal(t, "[1,0,0]", stored)
}

func TestHostedEmbeddingFilteredRowsStillBoundSourceWork(t *testing.T) {
	f, s, _, snap := embeddingOne(t)
	_, e := f.runtime.Exec(`INSERT INTO messages(session_id,ordinal,role,content,is_system) VALUES('s',1,'user','hidden',true),('s',2,'tool','tool',false),('s',3,'user','<command-message> hidden',false)`)
	require.NoError(t, e)
	reconcileEmbedding(t, s)
	limited, e := NewHostedEmbeddingStore(t.Context(), f.runtime, HostedEmbeddingOptions{Schema: f.schema, Tenant: f.tenant, Limits: HostedEmbeddingLimits{MaxRows: 2, RowPageSize: 1}})
	require.NoError(t, e)
	leases, e := limited.Claim(t.Context(), "limited", 1, time.Minute)
	require.NoError(t, e)
	require.Len(t, leases, 1)
	_, e = limited.ReadSession(t.Context(), leases[0])
	assert.ErrorIs(t, e, ErrHostedEmbeddingWorkLimit)
	assert.ErrorIs(t, s.Publish(t.Context(), snap, embeddingOneVector()), ErrHostedEmbeddingStale)
}

func TestHostedEmbeddingRuntimeVectorOperator(t *testing.T) {
	f, s, _, snap := embeddingOne(t)
	require.NoError(t, s.Publish(t.Context(), snap, embeddingOneVector()))
	var extensionSchema string
	require.NoError(t, f.admin.QueryRow(`SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&extensionSchema))
	quoted, _ := quoteIdentifier(extensionSchema)
	var distance float64
	e := f.runtime.QueryRow(`SELECT embedding OPERATOR(`+quoted+`.<=>) $1::`+quoted+`.halfvec FROM hosted_embedding_chunks_g1`, `[1,0,0]`).Scan(&distance)
	require.NoError(t, e)
	assert.Equal(t, float64(0), distance)
}
