//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
	"go.kenn.io/agentsview/internal/vector"
)

func TestVectorHubPushPublishesCompatibility(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_metadata_test")
	seedVectorSession(t, local, "A")
	sync.vectorSource = hubRecipeSource("hub-metadata", map[string]string{"max_input_chars": "8192"})
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	var version int
	var automated bool
	require.NoError(t, pg.QueryRow(`SELECT data_version, include_automated FROM vector_generation_machines`).Scan(&version, &automated))
	assert.Equal(t, db.CurrentDataVersion(), version)
	assert.False(t, automated)
	source := hubRecipeSource("hub-metadata", map[string]string{"max_input_chars": "8192"})
	includeAutomated := true
	source.gen.IncludeAutomated = &includeAutomated
	sync.vectorSource = source
	_, err = sync.Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.NoError(t, pg.QueryRow(`SELECT data_version, include_automated FROM vector_generation_machines`).Scan(&version, &automated))
	assert.True(t, automated)
}

func TestVectorHubLookupRoundTrip(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_lookup_test")
	source := hubRecipeSource("hub-lookup", map[string]string{"max_input_chars": "8192"})
	type fixture struct {
		identity vector.HubIdentity
		values   []float32
	}
	var fixtures []fixture
	for _, session := range []string{"A", "B"} {
		seedVectorSession(t, local, session)
		source.hashes[session] = "corpus"
		source.docs[session] = nil
		for i := 0; i < 64; i++ {
			kind, prefix, uuid, occurrence := "user", "u:", fmt.Sprintf("unit-%d", i), 1
			if i%2 == 1 {
				kind, prefix = "run", "r:"
			}
			if i%4 == 2 {
				occurrence = 2
			}
			key := fmt.Sprintf("%s%s:%s", prefix, session, uuid)
			if occurrence == 2 {
				key += "#2"
			}
			if i%4 == 3 {
				uuid = ""
				key = fmt.Sprintf("ro:%s:%d", session, i)
			}
			values := []float32{float32(i) / 32, -float32(i) / 32, 0.1, 1}
			doc := vdoc(session, key, i, "content", fmt.Sprint(i), values)
			doc.SourceUUID = uuid
			source.docs[session] = append(source.docs[session], doc)
			// PG halfvec rounds 0.1 to this exact float32 value.
			fixtures = append(fixtures, fixture{vector.HubIdentity{SessionID: session, UnitKind: kind, SourceUUID: uuid, Occurrence: occurrence, Ordinal: i, ContentHash: fmt.Sprint(i)}, []float32{values[0], values[1], 0.0999755859375, 1}})
		}
	}
	sync.vectorSource = source
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = pg.Exec(`DELETE FROM vector_push_state WHERE session_id='B'`)
	require.NoError(t, err)
	id, _, ok, err := LookupVectorGeneration(t.Context(), pg, "hub-lookup")
	require.NoError(t, err)
	require.True(t, ok)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, id)
	hegel.Test(t, func(ht *hegel.T) {
		f := fixtures[hegel.Draw(ht, hegel.Integers(0, len(fixtures)-1))]
		stale := hegel.Draw(ht, hegel.Booleans())
		identity := f.identity
		if stale {
			identity.ContentHash = "stale"
		}
		reply, err := store.LookupHubVectors(ht.Context(), vector.HubLookupRequest{Fingerprint: "hub-lookup", Identities: []vector.HubIdentity{identity}})
		require.NoError(ht, err)
		require.Equal(ht, vector.HubVectorVersion, reply.Version)
		if stale || identity.SessionID == "B" {
			require.Equal(ht, []int{0}, reply.Absent)
			require.Empty(ht, reply.Chunks)
			return
		}
		require.Empty(ht, reply.Absent)
		require.Len(ht, reply.Chunks, 1)
		require.Equal(ht, 0, reply.Chunks[0].Index)
		require.Equal(ht, 1, reply.Chunks[0].ChunkCount)
		got, err := vector.DecodeHubVector(reply.Chunks[0].Vector, 4)
		require.NoError(ht, err)
		require.Equal(ht, f.values, got)
	}, hegel.WithTestCases(1000))
	generation, err := store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "hub-lookup", generation.Fingerprint)
	assert.Equal(t, db.CurrentDataVersion(), generation.DataVersion)
	_, err = store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "other", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", ContentHash: "x"}}})
	assert.ErrorIs(t, err, vector.ErrHubGenerationChanged)
}

func TestVectorHubLookupPagination(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_pages_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("pages", map[string]string{"max_input_chars": "8192"})
	doc := vdoc("A", "o:A:0", 0, "content", "hash")
	for i := 0; i < 40; i++ {
		doc.Chunks = append(doc.Chunks, storage.VectorPushChunk{ChunkIndex: i, Embedding: []float32{1, 0, 0, 0}})
	}
	source.docs["A"] = []storage.VectorPushDoc{doc}
	sync.vectorSource = source
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	id, _, _, err := LookupVectorGeneration(t.Context(), pg, "pages")
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, id)
	req := vector.HubLookupRequest{Fingerprint: "pages", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", ContentHash: "hash"}}}
	total := 0
	pages := 0
	for {
		reply, err := store.lookupHubVectors(t.Context(), req, 2300)
		require.NoError(t, err)
		encoded, err := json.Marshal(reply)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), 2300)
		for _, chunk := range reply.Chunks {
			require.Equal(t, total, chunk.ChunkIndex)
			require.Equal(t, 40, chunk.ChunkCount)
			total++
		}
		pages++
		require.Less(t, pages, 41)
		if reply.NextCursor == "" {
			break
		}
		req.Cursor = reply.NextCursor
	}
	assert.Equal(t, 40, total)
	assert.Greater(t, pages, 1)
	req.Identities[0].ContentHash = "changed"
	_, err = store.LookupHubVectors(t.Context(), req)
	assert.ErrorIs(t, err, vector.ErrHubLookupInvalid)
	generation, err := store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Empty(t, generation.Unavailable)
	_, err = pg.Exec(`INSERT INTO vector_generation_machines(generation_id,machine,data_version,include_automated) VALUES($1,'other',$2,true)`, id, db.CurrentDataVersion())
	require.NoError(t, err)
	generation, err = store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, generation.Unavailable)
}

func TestVectorHubRejectsCrossGenerationRevision(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_revision_test")
	seedVectorSession(t, local, "A")
	identities := []vector.HubIdentity{
		{SessionID: "A", UnitKind: "user", Ordinal: 0, ContentHash: "new-hash"},
		{SessionID: "A", UnitKind: "user", SourceUUID: "uuid", Occurrence: 2, Ordinal: 1, ContentHash: "new-hash"},
		{SessionID: "A", UnitKind: "run", SourceUUID: "run", Occurrence: 1, Ordinal: 2, ContentHash: "new-hash"},
	}
	first := hubRecipeSource("old", map[string]string{"max_input_chars": "8192"})
	first.docs["A"] = nil
	for i, key := range []string{"o:A:0", "u:A:uuid#2", "r:A:run"} {
		doc := vdoc("A", key, i, "old-content", "old-hash", []float32{1, 0, 0, 0})
		doc.SourceUUID = identities[i].SourceUUID
		first.docs["A"] = append(first.docs["A"], doc)
	}
	sync.vectorSource = first
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	id, _, _, err := LookupVectorGeneration(t.Context(), pg, "old")
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, id)
	second := hubRecipeSource("new", map[string]string{"max_input_chars": "8192"})
	second.docs = first.docs
	sync.vectorSource = second
	_, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	identity := identities[0]
	identity.ContentHash = "old-hash"
	reply, err := store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "old", Identities: []vector.HubIdentity{identity}})
	require.NoError(t, err)
	require.Len(t, reply.Chunks, 1, "equal corpus remains covered in both generations")
	var oldChunks int
	require.NoError(t, pg.QueryRow(`SELECT count(*) FROM `+vectorChunkTable(id)).Scan(&oldChunks))
	assert.Equal(t, len(identities), oldChunks, "unchanged coverage preserves old generation chunks")
	second.hashes["A"] = "changed-corpus"
	for i := range second.docs["A"] {
		second.docs["A"][i].ContentHash = "new-hash"
		second.docs["A"][i].Chunks[0].Embedding = []float32{0, 1, 0, 0}
	}
	_, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)

	for _, identity := range identities {
		for _, hash := range []string{"old-hash", "new-hash"} {
			identity.ContentHash = hash
			reply, err := store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "old", Identities: []vector.HubIdentity{identity}})
			require.NoError(t, err)
			assert.Empty(t, reply.Chunks)
			assert.Equal(t, []int{0}, reply.Absent)
		}
	}

	require.NoError(t, pg.QueryRow(`SELECT count(*) FROM `+vectorChunkTable(id)).Scan(&oldChunks))
	assert.Zero(t, oldChunks, "invalidated coverage must not leave searchable old chunks")
	newID, _, _, err := LookupVectorGeneration(t.Context(), pg, "new")
	require.NoError(t, err)
	var newChunks int
	require.NoError(t, pg.QueryRow(`SELECT count(*) FROM `+vectorChunkTable(newID)).Scan(&newChunks))
	assert.Equal(t, len(identities), newChunks, "current generation chunks remain intact")
	delete(second.hashes, "A")
	delete(second.docs, "A")
	_, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	var documents int
	require.NoError(t, pg.QueryRow(`SELECT count(*) FROM vector_documents WHERE session_id='A'`).Scan(&documents))
	assert.Zero(t, documents, "scope eviction removes shared documents once all chunks are gone")

}

func hubRecipeSource(fingerprint string, params map[string]string) *fakeVectorSource {
	source := recipeSource(fingerprint, params)
	value := false
	source.gen.IncludeAutomated = &value
	return source
}

func TestVectorHubScopedCompatibilityChangePromotesReconciliation(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_scope_test")
	seedVectorSession(t, local, "A")
	seedVectorSession(t, local, "B")
	source := hubRecipeSource("scope", map[string]string{"max_input_chars": "8192"})
	source.hashes["B"] = "b1"
	source.docs["B"] = []storage.VectorPushDoc{vdoc("B", "o:B:0", 0, "b1", "b1", []float32{1, 0, 0, 0})}
	sync.vectorSource = source
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	automated := true
	source.gen.IncludeAutomated = &automated
	source.hashes["A"] = "a2"
	source.docs["A"][0].ContentHash = "a2"
	source.hashes["B"] = "b2"
	source.docs["B"][0].ContentHash = "b2"
	_, err = sync.pushVectors(t.Context(), false, []string{"A"}, 0, nil, nil)
	require.NoError(t, err)
	var scope bool
	require.NoError(t, pg.QueryRow(`SELECT include_automated FROM vector_generation_machines`).Scan(&scope))
	assert.True(t, scope)
	var hash string
	require.NoError(t, pg.QueryRow(`SELECT content_hash FROM vector_documents WHERE session_id='B'`).Scan(&hash))
	assert.Equal(t, "b2", hash, "scope change reconciles the whole corpus")
}

func TestVectorHubMachineRenameRetiresCompatibilityWitness(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_rename_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("rename", map[string]string{"max_input_chars": "8192"})
	sync.vectorSource = source
	res, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, res.Vectors.GenerationID)
	sync.machine = "renamed-machine"
	automated := true
	source.gen.IncludeAutomated = &automated
	_, err = sync.pushVectors(t.Context(), false, nil, 0, nil, nil)
	require.NoError(t, err)
	generation, err := store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Empty(t, generation.Unavailable)
	assert.True(t, generation.IncludeAutomated)
	assert.Equal(t, 1, countRows(t, pg, `SELECT count(*) FROM vector_generation_machines`))
}

func TestVectorHubIncompleteCompatibilityPushIsUnavailable(t *testing.T) {
	for _, first := range []bool{false, true} {
		for _, canceled := range []bool{false, true} {
			t.Run(fmt.Sprintf("first=%t/canceled=%t", first, canceled), func(t *testing.T) {
				sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_partial_test")
				seedVectorSession(t, local, "A")
				seedVectorSession(t, local, "B")
				source := hubRecipeSource("partial", map[string]string{"max_input_chars": "8192"})
				source.hashes["B"] = "b1"
				source.docs["B"] = []storage.VectorPushDoc{vdoc("B", "o:B:0", 0, "b1", "b1", []float32{1, 0, 0, 0})}
				sync.vectorSource = source
				res, err := sync.Push(t.Context(), false, nil)
				require.NoError(t, err)
				store := &Store{pg: pg}
				setHubTestSearcher(t, store, res.Vectors.GenerationID)
				if first {
					_, err = pg.Exec(`UPDATE vector_generation_machines SET machine='other-pusher'`)
					require.NoError(t, err)
				}
				automated := true
				source.gen.IncludeAutomated = &automated
				for _, id := range []string{"A", "B"} {
					source.hashes[id] = id + "2"
					source.docs[id][0].ContentHash = id + "2"
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				var observed bool
				progress := func(p storage.PushProgress) {
					if p.VectorChunksPushed == 0 {
						return
					}
					observed = true
					generation, err := store.HubVectorGeneration(t.Context())
					require.NoError(t, err)
					assert.NotEmpty(t, generation.Unavailable, "committed chunks must not retain old compatibility")
					if canceled {
						cancel()
					}
				}
				var failed map[string]struct{}
				if !canceled {
					failed = map[string]struct{}{"B": {}}
				}
				res.Vectors, err = sync.pushVectors(ctx, false, nil, 0, failed, progress)
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
					assert.Equal(t, 1, res.Vectors.SessionsDeferred)
				}
				require.True(t, observed)
				generation, err := store.HubVectorGeneration(t.Context())
				require.NoError(t, err)
				assert.NotEmpty(t, generation.Unavailable, "incomplete pushes must remain unavailable")
			})
		}
	}
}

func setHubTestSearcher(t *testing.T, store *Store, generationID int64) {
	t.Helper()
	var fingerprint string
	require.NoError(t, store.pg.QueryRow(`SELECT fingerprint FROM vector_generations WHERE id=$1`, generationID).Scan(&fingerprint))
	searcher, reason, err := (Backend{}).OpenVectorSearcher(t.Context(), store, storage.VectorGenerationInfo{Fingerprint: fingerprint}, 8192, nil)
	require.NoError(t, err)
	require.Empty(t, reason)
	require.NotNil(t, searcher)
	store.SetVectorSearcher(searcher)
}

func TestVectorHubRejectsRecreatedSelectedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, fingerprint string
		dimension         int
	}{
		{"same recipe", "selected", 4},
		{"changed recipe", "replacement", 4},
		{"changed dimension", "replacement", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_recreated_test")
			seedVectorSession(t, local, "A")
			source := hubRecipeSource("selected", map[string]string{"max_input_chars": "8192"})
			source.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "content", "hash", []float32{1, 0, 0, 0})}
			sync.vectorSource = source
			res, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, res.Vectors.GenerationID)
			var before string
			require.NoError(t, pg.QueryRow(`SELECT created_at::text FROM vector_generations WHERE id=$1`, res.Vectors.GenerationID).Scan(&before))
			// Recreate before the first discovery call: pinning cannot be lazy.
			for _, table := range []string{vectorChunkTable(res.Vectors.GenerationID), "vector_push_state", "vector_generation_machines", "vector_documents", "vector_generations"} {
				_, err = pg.Exec(`DROP TABLE ` + table)
				require.NoError(t, err)
			}
			source.gen.Fingerprint = tc.fingerprint
			source.gen.Dimension = tc.dimension
			source.docs["A"][0].Chunks[0].Embedding = make([]float32, tc.dimension)
			source.docs["A"][0].Chunks[0].Embedding[0] = 1
			pushed, err := sync.pushVectors(t.Context(), false, nil, 0, nil, nil)
			require.NoError(t, err)
			require.Equal(t, res.Vectors.GenerationID, pushed.GenerationID, "table recreation reuses the selected ID")
			var after string
			require.NoError(t, pg.QueryRow(`SELECT created_at::text FROM vector_generations WHERE id=$1`, pushed.GenerationID).Scan(&after))
			require.NotEqual(t, before, after)
			generation, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.NotEmpty(t, generation.Unavailable)
			for _, fingerprint := range []string{"selected", tc.fingerprint} {
				_, err = store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: fingerprint, Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", Ordinal: 0, ContentHash: "hash"}}})
				assert.ErrorIs(t, err, vector.ErrHubGenerationChanged)
			}
		})
	}
}

func TestVectorHubRejectsMissingSelectedChunks(t *testing.T) {
	for _, discovered := range []bool{false, true} {
		t.Run(fmt.Sprintf("discovered=%t", discovered), func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_missing_chunks_test")
			seedVectorSession(t, local, "A")
			source := hubRecipeSource("selected", map[string]string{"max_input_chars": "8192"})
			source.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "content", "hash", []float32{1, 0, 0, 0})}
			sync.vectorSource = source
			res, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, res.Vectors.GenerationID)
			if discovered {
				generation, err := store.HubVectorGeneration(t.Context())
				require.NoError(t, err)
				require.Empty(t, generation.Unavailable)
			}
			_, err = pg.Exec(`DROP TABLE ` + vectorChunkTable(res.Vectors.GenerationID))
			require.NoError(t, err)
			generation, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.NotEmpty(t, generation.Unavailable)
			_, err = store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "selected", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", Ordinal: 0, ContentHash: "hash"}}})
			assert.ErrorIs(t, err, vector.ErrHubGenerationChanged)
		})
	}
}
