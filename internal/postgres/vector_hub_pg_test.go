//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"path/filepath"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT data_version, include_automated FROM vector_generation_machines`).Scan(&version, &automated))
	assert.Equal(t, db.CurrentDataVersion(), version)
	assert.False(t, automated)
	source := hubRecipeSource("hub-metadata", map[string]string{"max_input_chars": "8192"})
	includeAutomated := true
	source.gen.IncludeAutomated = &includeAutomated
	sync.vectorSource = source
	_, err = sync.Push(t.Context(), true, nil)
	require.NoError(t, err)
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT data_version, include_automated FROM vector_generation_machines`).Scan(&version, &automated))
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
		for i := range 64 {
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
			doc := vdoc(session, key, i, "content", strconv.Itoa(i), values)
			doc.SourceUUID = uuid
			source.docs[session] = append(source.docs[session], doc)
			// PG halfvec rounds 0.1 to this exact float32 value.
			fixtures = append(fixtures, fixture{vector.HubIdentity{SessionID: session, UnitKind: kind, SourceUUID: uuid, Occurrence: occurrence, Ordinal: i, ContentHash: strconv.Itoa(i)}, []float32{values[0], values[1], 0.0999755859375, 1}})
		}
	}
	sync.vectorSource = source
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `DELETE FROM vector_push_state WHERE session_id='B'`)
	require.NoError(t, err)
	require.Equal(t, 64, countRows(t, pg, `SELECT count(*) FROM vector_documents WHERE session_id='B'`))
	id, _, ok, err := LookupVectorGeneration(t.Context(), pg, "hub-lookup")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 64, countRows(t, pg, `SELECT count(*) FROM `+vectorChunkTable(id)+` c JOIN vector_documents d ON d.doc_key=c.doc_key WHERE d.session_id='B'`))
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, id)
	for _, f := range fixtures {
		for _, stale := range []bool{false, true} {
			identity := f.identity
			if stale {
				identity.ContentHash = "stale"
			}
			reply, err := store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "hub-lookup", Identities: []vector.HubIdentity{identity}})
			require.NoError(t, err)
			require.Equal(t, vector.HubVectorVersion, reply.Version)
			if stale || identity.SessionID == "B" {
				require.Equal(t, []int{0}, reply.Absent)
				require.Empty(t, reply.Chunks)
				continue
			}
			require.Empty(t, reply.Absent)
			require.Len(t, reply.Chunks, 1)
			require.Equal(t, 0, reply.Chunks[0].Index)
			require.Equal(t, 1, reply.Chunks[0].ChunkCount)
			got, err := vector.DecodeHubVector(reply.Chunks[0].Vector, 4)
			require.NoError(t, err)
			require.Equal(t, f.values, got)
		}
	}
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
	require.NoError(t, local.SetSessionDataVersion(t.Context(), "A", db.CurrentDataVersion()))
	// A real producer build supplies the chunk count and recipe together.
	content := strings.Repeat("a", 20000)
	require.NoError(t, local.ReplaceSessionMessages(t.Context(), "A", []db.Message{{SessionID: "A", Ordinal: 0, Role: "user", Content: content, ContentLength: len(content)}}))
	ix, err := vector.Open(t.Context(), filepath.Join(t.TempDir(), "vectors.db"), false, 8192)
	require.NoError(t, err)
	defer ix.Close()
	source := builtVectorSource(t, ix, local, "pages")
	require.Len(t, source.docs["A"], 1)
	require.Len(t, source.docs["A"][0].Chunks, 3)
	sync.vectorSource = source
	_, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	id, _, _, err := LookupVectorGeneration(t.Context(), pg, source.gen.Fingerprint)
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, id)
	req := vector.HubLookupRequest{Fingerprint: source.gen.Fingerprint, Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", ContentHash: source.docs["A"][0].ContentHash}}}
	total := 0
	pages := 0
	for {
		reply, err := store.lookupHubVectors(t.Context(), req, 2150)
		require.NoError(t, err)
		encoded, err := json.Marshal(reply)
		require.NoError(t, err)
		require.LessOrEqual(t, len(encoded), 2150)
		for _, chunk := range reply.Chunks {
			require.Equal(t, total, chunk.ChunkIndex)
			require.Equal(t, 3, chunk.ChunkCount)
			total++
		}
		pages++
		require.Less(t, pages, 4)
		if reply.NextCursor == "" {
			break
		}
		req.Cursor = reply.NextCursor
	}
	assert.Equal(t, 3, total)
	assert.Greater(t, pages, 1)
	for _, offset := range []int{3, 4, 1000000} {
		invalid := req
		invalid.Cursor = makeHubLookupCursor(invalid, 0, offset)
		_, err := store.LookupHubVectors(t.Context(), invalid)
		require.ErrorIs(t, err, vector.ErrHubLookupInvalid)
	}
	req.Identities[0].ContentHash = "changed"
	_, err = store.LookupHubVectors(t.Context(), req)
	require.ErrorIs(t, err, vector.ErrHubLookupInvalid)
	generation, err := store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.Empty(t, generation.Unavailable)
	_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines(generation_id,machine,data_version,include_automated) VALUES($1,'other',$2,true)`, id, db.CurrentDataVersion())
	require.NoError(t, err)
	generation, err = store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, generation.Unavailable)
}

func TestVectorHubLookupSparseChunks(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_sparse_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("sparse", map[string]string{"max_input_chars": "8192"})
	source.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "content", "hash", []float32{1, 0, 0, 0})}
	sync.vectorSource = source
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	id, _, _, err := LookupVectorGeneration(t.Context(), pg, "sparse")
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, id)
	table := vectorChunkTable(id)
	identity := vector.HubIdentity{SessionID: "A", UnitKind: "user", ContentHash: "hash"}
	for _, indices := range [][]int{{0, 2}, {3, 7, 15}, {42}, {0, 1, 2, 3, 4, 5, 6, 7}, {math.MaxInt32 - 1, math.MaxInt32}} {
		t.Run(fmt.Sprint(indices), func(t *testing.T) {
			_, err := pg.ExecContext(t.Context(), "DELETE FROM "+table)
			require.NoError(t, err)
			expected := make([]vector.HubChunk, len(indices))
			for i, index := range indices {
				_, err = pg.ExecContext(t.Context(), fmt.Sprintf("INSERT INTO %s (doc_key,chunk_index,embedding,source_hash) VALUES ('o:A:0',$1,$2,'hash')", table), index, fmt.Sprintf("[%d,0,0,0]", i+1))
				require.NoError(t, err)
				encoded, err := vector.EncodeHubVector([]float32{float32(i + 1), 0, 0, 0})
				require.NoError(t, err)
				expected[i] = vector.HubChunk{Index: 0, ChunkIndex: index, ChunkCount: len(indices), Vector: encoded}
			}
			req := vector.HubLookupRequest{Fingerprint: "sparse", Identities: []vector.HubIdentity{identity}}
			full, err := store.LookupHubVectors(t.Context(), req)
			require.NoError(t, err)
			require.Equal(t, expected, full.Chunks)
			// Repeated and absent identities exercise resets between sparse documents.
			missing := identity
			missing.ContentHash = "missing"
			req.Identities = []vector.HubIdentity{missing, identity, missing, identity}
			var actual []vector.HubChunk
			var absent []int
			for pages := 0; ; pages++ {
				require.Less(t, pages, 2*len(indices)+4)
				page, err := store.lookupHubVectors(t.Context(), req, 2200)
				require.NoError(t, err)
				actual = append(actual, page.Chunks...)
				absent = append(absent, page.Absent...)
				if page.NextCursor == "" {
					break
				}
				require.NotEqual(t, req.Cursor, page.NextCursor)
				req.Cursor = page.NextCursor
			}
			want := make([]vector.HubChunk, 0, 2*len(expected))
			for _, position := range []int{1, 3} {
				for _, chunk := range expected {
					chunk.Index = position
					want = append(want, chunk)
				}
			}
			require.Equal(t, want, actual)
			require.Equal(t, []int{0, 2}, absent)
		})
	}
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

func TestVectorHubLookupCompatibilityGate(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_lookup_gate_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("lookup-gate", map[string]string{"max_input_chars": "8192"})
	source.docs["A"] = []storage.VectorPushDoc{vdoc("A", "u:A:unit", 0, "content", "hash", []float32{1, 0, 0, 0})}
	source.docs["A"][0].SourceUUID = "unit"
	sync.vectorSource = source
	result, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, result.Vectors.GenerationID)
	for _, tc := range []struct {
		name, params, witnesses string
		available               bool
	}{
		{"current", `{"max_input_chars":"8192"}`, "(1,false,1)", true},
		{"legacy null hash", `{"max_input_chars":"8192"}`, "(1,false,NULL)", true},
		{"future hash", `{"max_input_chars":"8192"}`, "(1,false,2)", false},
		{"mixed hash versions", `{"max_input_chars":"8192"}`, "(1,false,1),(1,false,2)", false},
		{"no witnesses", `{"max_input_chars":"8192"}`, "", false},
		{"null version", `{"max_input_chars":"8192"}`, "(NULL,false,1)", false},
		{"zero version", `{"max_input_chars":"8192"}`, "(0,false,1)", false},
		{"null scope", `{"max_input_chars":"8192"}`, "(1,NULL,1)", false},
		{"different versions", `{"max_input_chars":"8192"}`, "(1,false,1),(2,false,1)", false},
		{"different scopes", `{"max_input_chars":"8192"}`, "(1,false,1),(1,true,1)", false},
		{"null params", "null", "(1,false,1)", false},
		{"array params", "[]", "(1,false,1)", false},
		{"numeric param", `{"max_input_chars":8192}`, "(1,false,1)", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pg.ExecContext(t.Context(), "DELETE FROM vector_generation_machines")
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "UPDATE vector_generations SET params=$1::jsonb", tc.params)
			require.NoError(t, err)
			if tc.witnesses != "" {
				_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines
                (generation_id,machine,data_version,include_automated,chunk_hash_version)
                SELECT $1,(row_number() OVER())::text,v.version::integer,v.scope::boolean,v.hash::integer FROM (VALUES `+tc.witnesses+`) v(version,scope,hash)`, result.Vectors.GenerationID)
				require.NoError(t, err)
			}
			for _, cursor := range []bool{false, true} {
				req := vector.HubLookupRequest{Fingerprint: "lookup-gate", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", SourceUUID: "unit", Occurrence: 1, ContentHash: "hash"}}}
				if cursor {
					req.Cursor = makeHubLookupCursor(req, 0, 0)
				}
				reply, err := store.LookupHubVectors(t.Context(), req)
				if tc.available {
					require.NoError(t, err)
					require.Len(t, reply.Chunks, 1)
					values, err := vector.DecodeHubVector(reply.Chunks[0].Vector, 4)
					require.NoError(t, err)
					assert.Equal(t, []float32{1, 0, 0, 0}, values)
				} else {
					require.ErrorIs(t, err, vector.ErrHubGenerationChanged)
					assert.Empty(t, reply.Chunks)
				}
			}
			generation, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tc.available, generation.Unavailable == "")
		})
	}
}

func TestVectorHubCompatibilityWitnessTrust(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_witness_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("witness-trust", map[string]string{"max_input_chars": "8192"})
	sync.vectorSource = source
	result, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	key, err := sync.vectorGenerationWitnessKey(t.Context())
	require.NoError(t, err)
	version, hashVersion := db.CurrentDataVersion(), 1
	oldVersion, futureHash := version-1, 2
	yes, no := true, false
	for _, tc := range []struct {
		name                             string
		storedVersion, storedHashVersion *int
		storedScope, requestedScope      *bool
		missing, expected                bool
	}{
		{"current", &version, &hashVersion, &no, &no, false, true},
		{"missing", &version, &hashVersion, &no, &no, true, false},
		{"old version", &oldVersion, &hashVersion, &no, &no, false, false},
		{"null version", nil, &hashVersion, &no, &no, false, false},
		{"future hash", &version, &futureHash, &no, &no, false, false},
		{"null hash", &version, nil, &no, &no, false, false},
		{"changed scope", &version, &hashVersion, &no, &yes, false, false},
		{"unknown scope", &version, &hashVersion, nil, nil, false, true},
		{"missing stored scope", &version, &hashVersion, nil, &no, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storedVersion, storedHashVersion, storedScope, requestedScope, missing := tc.storedVersion, tc.storedHashVersion, tc.storedScope, tc.requestedScope, tc.missing
			_, err := pg.ExecContext(t.Context(), "DELETE FROM vector_generation_machines")
			require.NoError(t, err)
			if !missing {
				_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines
			(generation_id,machine,last_push_at,data_version,include_automated,chunk_hash_version) VALUES ($1,$2,now(),$3,$4,$5)`,
					result.Vectors.GenerationID, key, storedVersion, storedScope, storedHashVersion)
				require.NoError(t, err)
			}
			info := source.gen
			info.IncludeAutomated = requestedScope
			resolved, err := sync.resolveVectorGeneration(t.Context(), info, key)
			require.NoError(t, err)
			lookedUp, found, err := sync.lookupVectorGeneration(t.Context(), info.Fingerprint, key, requestedScope)
			require.NoError(t, err)
			require.True(t, found)
			expected := tc.expected
			require.Equal(t, expected, resolved.machineRecorded)
			require.Equal(t, expected, lookedUp.machineRecorded)
		})
	}
}

func TestVectorHubLegacyCompatibilityRewritesCoverage(t *testing.T) {
	for _, witness := range []string{"unknown", "missing", "old version", "changed scope", "resumed full", "legacy machine"} {
		t.Run(witness, func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_upgrade_test")
			seedVectorSession(t, local, "A")
			first := hubRecipeSource("legacy-a", map[string]string{"max_input_chars": "8192"})
			first.hashes["A"] = "aggregate-a"
			first.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "old content", "old-hash", []float32{1, 0, 0, 0})}
			sync.vectorSource = first
			result, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			generation := result.Vectors.GenerationID
			table := vectorChunkTable(generation)
			_, err = pg.ExecContext(t.Context(), "CREATE TABLE legacy_chunks AS SELECT * FROM "+table)
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "CREATE TABLE legacy_state AS SELECT * FROM vector_push_state WHERE generation_id=$1", generation)
			require.NoError(t, err)
			second := hubRecipeSource("legacy-b", map[string]string{"max_input_chars": "8192"})
			second.hashes["A"] = "aggregate-b"
			second.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "new content", "new-hash", []float32{0, 1, 0, 0})}
			sync.vectorSource = second
			_, err = sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			// Older writers updated shared documents without retiring other generations'
			// chunks and delta hashes. Restore that reachable pre-upgrade database state.
			_, err = pg.ExecContext(t.Context(), "INSERT INTO "+table+" SELECT * FROM legacy_chunks")
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "INSERT INTO vector_push_state SELECT * FROM legacy_state")
			require.NoError(t, err)
			switch witness {
			case "legacy machine":
				_, err = pg.ExecContext(t.Context(), "DELETE FROM vector_generation_machines WHERE generation_id=$1", generation)
				require.NoError(t, err)
				_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines
				(generation_id,machine,last_push_at) VALUES ($1,$2,now())`, generation, sync.machine)
			case "unknown":
				_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET data_version=NULL,include_automated=NULL WHERE generation_id=$1", generation)
			case "missing":
				_, err = pg.ExecContext(t.Context(), "DELETE FROM vector_generation_machines WHERE generation_id=$1", generation)
			case "changed scope":
				_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET include_automated=true WHERE generation_id=$1", generation)
			case "resumed full":
				_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET data_version=NULL,include_automated=NULL WHERE generation_id=$1", generation)
				sync.fullRun = &fullPushRunTracker{run: fullPushRun{VectorGeneration: generation, Vectors: map[string]string{"A": "aggregate-a"}}, lastSaved: time.Now(), interval: time.Hour}
			case "old version":
				_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET data_version=$2 WHERE generation_id=$1", generation, db.CurrentDataVersion()-1)
			}
			require.NoError(t, err)
			sync.vectorSource = first
			// A failed session must not be hidden by the legacy equal-hash skip.
			deferred, err := sync.pushVectors(t.Context(), witness == "resumed full", nil, 0, map[string]struct{}{"A": {}}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, deferred.SessionsDeferred)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, generation)
			pending, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.NotEmpty(t, pending.Unavailable)
			if witness == "legacy machine" {
				assert.Equal(t, 1, countRows(t, pg, "SELECT count(*) FROM vector_generation_machines WHERE generation_id=$1 AND machine=$2", generation, sync.machine), "deferred reconciliation retains the legacy witness")
			}
			blocked, lookupErr := store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "legacy-a", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", ContentHash: "new-hash"}}})
			require.ErrorIs(t, lookupErr, vector.ErrHubGenerationChanged)
			assert.Empty(t, blocked.Chunks, "unverified legacy chunks cannot bypass discovery")
			result.Vectors, err = sync.pushVectors(t.Context(), witness == "resumed full", nil, 0, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, result.Vectors.SessionsPushed, "first certification repairs historical coverage even when its aggregate hash matches")
			recipe, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.Empty(t, recipe.Unavailable)
			identity := vector.HubIdentity{SessionID: "A", UnitKind: "user", ContentHash: "new-hash"}
			reply, err := store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "legacy-a", Identities: []vector.HubIdentity{identity}})
			require.NoError(t, err)
			assert.Empty(t, reply.Chunks, "old vectors must never be served under another generation's newer content hash")
			assert.Equal(t, []int{0}, reply.Absent)
			identity.ContentHash = "old-hash"
			reply, err = store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "legacy-a", Identities: []vector.HubIdentity{identity}})
			require.NoError(t, err)
			require.Len(t, reply.Chunks, 1)
			values, err := vector.DecodeHubVector(reply.Chunks[0].Vector, 4)
			require.NoError(t, err)
			assert.Equal(t, []float32{1, 0, 0, 0}, values)
			again, err := sync.pushVectors(t.Context(), false, nil, 0, nil, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, again.SessionsUnchanged, "verified publishers retain ordinary delta skips")
			failed, err := sync.pushVectors(t.Context(), false, nil, 0, map[string]struct{}{"A": {}}, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, failed.SessionsDeferred, "failed session writes cannot be hidden by verified delta skips")
		})
	}
}

func hubRecipeSource(fingerprint string, params map[string]string) *fakeVectorSource {
	source := recipeSource(fingerprint, params)
	value := false
	source.gen.IncludeAutomated = &value
	source.gen.DataVersion = db.CurrentDataVersion()
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
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT include_automated FROM vector_generation_machines`).Scan(&scope))
	assert.True(t, scope)
	var hash string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT content_hash FROM vector_documents WHERE session_id='B'`).Scan(&hash))
	assert.Equal(t, "b2", hash, "scope change reconciles the whole corpus")
}

func TestVectorHubFilteredLegacyWitnessRemainsUnknown(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		t.Run(fmt.Sprintf("exclude=%t", exclude), func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_legacy_filter_test")
			seedVectorSessionProject(t, local, "A", "alpha")
			seedVectorSessionProject(t, local, "B", "beta")
			first := hubRecipeSource("legacy-filter", map[string]string{"max_input_chars": "8192"})
			first.docs["A"] = []storage.VectorPushDoc{vdoc("A", "u:A:a", 0, "a", "hash-a", []float32{1, 0, 0, 0})}
			first.docs["A"][0].SourceUUID = "a"
			first.hashes["B"] = "old-b"
			first.docs["B"] = []storage.VectorPushDoc{vdoc("B", "u:B:b", 0, "old b", "old-b", []float32{1, 0, 0, 0})}
			first.docs["B"][0].SourceUUID = "b"
			sync.vectorSource = first
			result, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			generation := result.Vectors.GenerationID
			table := vectorChunkTable(generation)
			_, err = pg.ExecContext(t.Context(), "CREATE TABLE legacy_filter_chunks AS SELECT * FROM "+table+" WHERE doc_key='u:B:b'")
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "CREATE TABLE legacy_filter_state AS SELECT * FROM vector_push_state WHERE generation_id=$1 AND session_id='B'", generation)
			require.NoError(t, err)
			second := hubRecipeSource("legacy-filter-new", map[string]string{"max_input_chars": "8192"})
			second.docs["A"] = first.docs["A"]
			second.hashes["B"] = "new-b"
			second.docs["B"] = []storage.VectorPushDoc{vdoc("B", "u:B:b", 0, "new b", "new-b", []float32{0, 1, 0, 0})}
			second.docs["B"][0].SourceUUID = "b"
			sync.vectorSource = second
			_, err = sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "INSERT INTO "+table+" SELECT * FROM legacy_filter_chunks")
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "INSERT INTO vector_push_state SELECT * FROM legacy_filter_state")
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET machine=$2,data_version=NULL,include_automated=NULL WHERE generation_id=$1", generation, sync.machine)
			require.NoError(t, err)
			if exclude {
				sync.excludeProjects = []string{"beta"}
			} else {
				sync.projects = []string{"alpha"}
			}
			// The vector source supplies only included projects, as a real scoped export does.
			delete(first.hashes, "B")
			delete(first.docs, "B")
			sync.vectorSource = first
			_, err = sync.pushVectors(t.Context(), false, nil, 0, nil, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, generation)
			info, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.NotEmpty(t, info.Unavailable, "excluded legacy coverage has not been reconciled")
			assert.Equal(t, 1, countRows(t, pg, "SELECT count(*) FROM vector_generation_machines WHERE generation_id=$1 AND machine=$2 AND data_version IS NULL", generation, sync.machine))
			reply, err := store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "legacy-filter", Identities: []vector.HubIdentity{{SessionID: "B", UnitKind: "user", SourceUUID: "b", Occurrence: 1, ContentHash: "new-b"}}})
			require.ErrorIs(t, err, vector.ErrHubGenerationChanged)
			assert.Empty(t, reply.Chunks, "excluded stale vectors cannot be served under the newer shared hash")
			// An unfiltered retry reconciles the omitted coverage before retiring its witness.
			sync.projects = nil
			sync.excludeProjects = nil
			first.hashes["B"] = "old-b"
			first.docs["B"] = []storage.VectorPushDoc{vdoc("B", "u:B:b", 0, "old b", "old-b", []float32{1, 0, 0, 0})}
			first.docs["B"][0].SourceUUID = "b"
			_, err = sync.Push(t.Context(), true, nil)
			require.NoError(t, err)
			info, err = store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.Empty(t, info.Unavailable)
			assert.Zero(t, countRows(t, pg, "SELECT count(*) FROM vector_generation_machines WHERE generation_id=$1 AND machine=$2", generation, sync.machine))
		})
	}
}

func TestVectorHubLegacyChunkHashUpgradeRewritesUnchanged(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_hash_upgrade_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("hash-upgrade", map[string]string{"max_input_chars": "8192"})
	sync.vectorSource = source
	result, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), "ALTER TABLE "+vectorChunkTable(result.Vectors.GenerationID)+" DROP COLUMN IF EXISTS source_hash")
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), "ALTER TABLE vector_generation_machines DROP COLUMN IF EXISTS chunk_hash_version")
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, result.Vectors.GenerationID)
	info, err := store.HubVectorGeneration(t.Context())
	require.NoError(t, err)
	assert.NotEmpty(t, info.Unavailable, "unmigrated chunk tables have no provenance")
	_, err = store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "hash-upgrade", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", Occurrence: 1, ContentHash: "h1"}}})
	require.ErrorIs(t, err, vector.ErrHubGenerationChanged)
	result, err = sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, result.Vectors.SessionsPushed, "unchanged legacy coverage needs provenance before reuse")
	assert.Zero(t, result.Vectors.SessionsUnchanged)
}

func TestVectorHubChunkSourceHashProof(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_hash_proof_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("hash-proof", map[string]string{"max_input_chars": "8192"})
	source.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "a", "a", []float32{1, 0, 0, 0})}
	sync.vectorSource = source
	result, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	table := vectorChunkTable(result.Vectors.GenerationID)
	_, err = pg.ExecContext(t.Context(), "CREATE TABLE hash_proof_template AS SELECT embedding FROM "+table+" LIMIT 1")
	require.NoError(t, err)
	store := &Store{pg: pg}
	setHubTestSearcher(t, store, result.Vectors.GenerationID)
	for _, count := range []int{0, 1, 3, 16} {
		for mode := range 4 {
			t.Run(fmt.Sprintf("count=%d/mode=%d", count, mode), func(t *testing.T) {
				hash := "hash-proof"
				_, err := pg.ExecContext(t.Context(), "UPDATE vector_documents SET content_hash=$1 WHERE session_id='A'", hash)
				require.NoError(t, err)
				_, err = pg.ExecContext(t.Context(), "DELETE FROM "+table)
				require.NoError(t, err)
				_, err = pg.ExecContext(t.Context(), "INSERT INTO "+table+` (doc_key,chunk_index,embedding,source_hash)
 SELECT 'o:A:0',i,template.embedding,
 CASE WHEN $2=0 OR ($2 IN (1,2) AND i>0) OR ($2=3 AND i<$1-1) THEN $3
      WHEN $2=2 THEN $3||'-other' ELSE NULL END
 FROM generate_series(0,$1-1) i CROSS JOIN hash_proof_template template`, count, mode, hash)
				require.NoError(t, err)
				req := vector.HubLookupRequest{Fingerprint: "hash-proof", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", Occurrence: 1, ContentHash: hash}}}
				reply, err := store.LookupHubVectors(t.Context(), req)
				require.NoError(t, err)
				if mode != 0 || count == 0 {
					require.Empty(t, reply.Chunks, "one unproven chunk quarantines the entire document")
					require.Equal(t, []int{0}, reply.Absent)
					req.Cursor = makeHubLookupCursor(req, 0, count/2)
					page, err := store.LookupHubVectors(t.Context(), req)
					require.NoError(t, err)
					require.Empty(t, page.Chunks)
					require.Equal(t, []int{0}, page.Absent, "a cursor cannot skip an unproven sibling")
					return
				}
				require.Len(t, reply.Chunks, count)
				require.Empty(t, reply.Absent)
				for i, chunk := range reply.Chunks {
					require.Equal(t, i, chunk.ChunkIndex)
					require.Equal(t, count, chunk.ChunkCount)
					values, err := vector.DecodeHubVector(chunk.Vector, 4)
					require.NoError(t, err)
					require.Equal(t, []float32{1, 0, 0, 0}, values)
				}
				if count > 1 {
					firstPage, err := store.lookupHubVectors(t.Context(), req, 2200)
					require.NoError(t, err)
					require.NotEmpty(t, firstPage.Chunks)
					require.NotEmpty(t, firstPage.NextCursor)
					_, err = pg.ExecContext(t.Context(), "UPDATE "+table+" SET source_hash=NULL WHERE chunk_index=0")
					require.NoError(t, err)
					req.Cursor = firstPage.NextCursor
					page, err := store.LookupHubVectors(t.Context(), req)
					require.NoError(t, err)
					require.Empty(t, page.Chunks)
					require.Equal(t, []int{0}, page.Absent, "each page rechecks proof for previously returned siblings")
					require.Empty(t, page.NextCursor)
				}
			})
		}
	}
}

func TestVectorHubInterruptedLegacyPublisherIsQuarantined(t *testing.T) {
	for _, changedHash := range []bool{false, true} {
		t.Run(fmt.Sprintf("changedHash=%t", changedHash), func(t *testing.T) {
			pgURL := testPGURL(t)
			first, local, pg := newVectorPushTestSync(t, pgURL, "agentsview_vector_hub_legacy_orphan_test")
			seedVectorSession(t, local, "A")
			source := hubRecipeSource("legacy-orphan", map[string]string{"max_input_chars": "8192"})
			source.hashes["A"] = "old-a"
			source.docs["A"] = []storage.VectorPushDoc{vdoc("A", "o:A:0", 0, "old a", "old-a", []float32{1, 0, 0, 0})}
			first.vectorSource = source
			result, err := first.Push(t.Context(), true, nil)
			require.NoError(t, err)
			generation := result.Vectors.GenerationID
			// An old writer committed session coverage, then stopped before its witness.
			_, err = pg.ExecContext(t.Context(), "DELETE FROM vector_generation_machines WHERE generation_id=$1", generation)
			require.NoError(t, err)
			_, err = pg.ExecContext(t.Context(), "ALTER TABLE "+vectorChunkTable(generation)+" DROP COLUMN IF EXISTS source_hash")
			require.NoError(t, err)
			hash := "old-a"
			if changedHash {
				hash = "new-a"
				// Legacy cross-generation writes could update the shared document alone.
				_, err = pg.ExecContext(t.Context(), "UPDATE vector_documents SET content_hash=$1 WHERE session_id='A'", hash)
				require.NoError(t, err)
			}
			otherLocal := testDB(t)
			seedVectorSession(t, otherLocal, "B")
			otherSource := hubRecipeSource("legacy-orphan", map[string]string{"max_input_chars": "8192"})
			otherSource.hashes = map[string]string{"B": "b"}
			otherSource.docs = map[string][]storage.VectorPushDoc{"B": {vdoc("B", "o:B:0", 0, "b", "b", []float32{0, 1, 0, 0})}}
			other, err := New(pgURL, first.schema, otherLocal, "other-machine", true, storage.PusherOptions{VectorSource: otherSource})
			require.NoError(t, err)
			t.Cleanup(func() { _ = other.Close() })
			_, err = other.Push(t.Context(), true, nil)
			require.NoError(t, err)
			assert.Equal(t, 1, countRows(t, pg, "SELECT count(*) FROM vector_push_state WHERE generation_id=$1 AND session_id='A'", generation), "foreign legacy coverage is preserved")
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, generation)
			info, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.Empty(t, info.Unavailable, "the completed publisher can still supply certified vectors")
			req := vector.HubLookupRequest{Fingerprint: "legacy-orphan", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", Occurrence: 1, ContentHash: hash}}}
			reply, err := store.LookupHubVectors(t.Context(), req)
			require.NoError(t, err)
			assert.Empty(t, reply.Chunks, "another publisher cannot certify interrupted legacy coverage")
			assert.Equal(t, []int{0}, reply.Absent)
			// Its own upgraded retry rewrites coverage and makes it reusable again.
			source.hashes["A"] = hash
			source.docs["A"][0].ContentHash = hash
			source.docs["A"][0].Chunks[0].Embedding = []float32{0, 0, 1, 0}
			_, err = first.Push(t.Context(), true, nil)
			require.NoError(t, err)
			reply, err = store.LookupHubVectors(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, reply.Chunks, 1)
			values, err := vector.DecodeHubVector(reply.Chunks[0].Vector, 4)
			require.NoError(t, err)
			assert.Equal(t, []float32{0, 0, 1, 0}, values)
			assert.Empty(t, reply.Absent)
		})
	}
}

func TestVectorHubUnfilteredPushRetiresFilteredWitnesses(t *testing.T) {
	for _, exclude := range []bool{false, true} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("exclude=%t/legacy=%t", exclude, legacy), func(t *testing.T) {
				pgURL := testPGURL(t)
				const schema = "agentsview_vector_hub_filter_recovery_test"
				_, local, pg := newVectorPushTestSync(t, pgURL, schema)
				seedVectorSessionProject(t, local, "A", "alpha")
				seedVectorSessionProject(t, local, "B", "beta")
				filteredSource := hubRecipeSource("filter-recovery", map[string]string{"max_input_chars": "8192"})
				opts := storage.PusherOptions{SyncStateTarget: "hub", VectorSource: filteredSource}
				if exclude {
					opts.ExcludeProjects = []string{"beta"}
				} else {
					opts.Projects = []string{"alpha"}
				}
				filtered, err := New(pgURL, schema, local, "before-rename", true, opts)
				require.NoError(t, err)
				t.Cleanup(func() { _ = filtered.Close() })
				result, err := filtered.Push(t.Context(), true, nil)
				require.NoError(t, err)
				oldKey, err := filtered.vectorGenerationWitnessKey(t.Context())
				require.NoError(t, err)
				if legacy {
					_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET data_version=NULL,include_automated=NULL WHERE generation_id=$1", result.Vectors.GenerationID)
					require.NoError(t, err)
				}
				fullSource := hubRecipeSource("filter-recovery", map[string]string{"max_input_chars": "8192"})
				automated := true
				fullSource.gen.IncludeAutomated = &automated
				fullSource.hashes["B"] = "b-full"
				fullSource.docs["B"] = []storage.VectorPushDoc{vdoc("B", "o:B:0", 0, "b", "b-full", []float32{0, 1, 0, 0})}
				full, err := New(pgURL, schema, local, "after-rename", true, storage.PusherOptions{SyncStateTarget: "hub", VectorSource: fullSource})
				require.NoError(t, err)
				t.Cleanup(func() { _ = full.Close() })
				require.NotEqual(t, filtered.syncStateTarget, full.syncStateTarget)
				// Another archive's scoped witness must survive this publisher's recovery.
				foreign := "other|push_marker:another-archive:scope:0123456789abcdef"
				_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines
				(generation_id,machine,last_push_at,data_version,include_automated) VALUES ($1,$2,now(),$3,true)`, result.Vectors.GenerationID, foreign, db.CurrentDataVersion())
				require.NoError(t, err)
				_, err = full.Push(t.Context(), true, nil)
				require.NoError(t, err)
				assert.Zero(t, countRows(t, pg, "SELECT count(*) FROM vector_generation_machines WHERE generation_id=$1 AND machine=$2", result.Vectors.GenerationID, oldKey))
				assert.Equal(t, 1, countRows(t, pg, "SELECT count(*) FROM vector_generation_machines WHERE generation_id=$1 AND machine=$2", result.Vectors.GenerationID, foreign))
				assert.Equal(t, 1, countRows(t, pg, "SELECT count(*) FROM vector_push_state WHERE generation_id=$1 AND session_id='B' AND doc_agg_hash='b-full'", result.Vectors.GenerationID))
				store := &Store{pg: pg}
				setHubTestSearcher(t, store, result.Vectors.GenerationID)
				info, err := store.HubVectorGeneration(t.Context())
				require.NoError(t, err)
				assert.Empty(t, info.Unavailable)
				assert.True(t, info.IncludeAutomated)
			})
		}
	}
}

func TestVectorHubLegacyWitnessRetirement(t *testing.T) {
	sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_alias_test")
	seedVectorSession(t, local, "A")
	source := hubRecipeSource("legacy-alias", map[string]string{"max_input_chars": "8192"})
	sync.vectorSource = source
	_, err := sync.Push(t.Context(), false, nil)
	require.NoError(t, err)
	initialKey, err := sync.vectorGenerationWitnessKey(t.Context())
	require.NoError(t, err)
	suffix := strings.TrimPrefix(initialKey, sync.machine)
	generation, err := sync.resolveVectorGeneration(t.Context(), source.gen, initialKey)
	require.NoError(t, err)
	foreignGeneration, err := ensureVectorGeneration(t.Context(), pg, "foreign-alias", "m", 4)
	require.NoError(t, err)
	for _, hostileName := range []bool{false, true} {
		for _, filtered := range []bool{false, true} {
			for _, canceled := range []bool{false, true} {
				t.Run(fmt.Sprintf("hostile=%t/filtered=%t/canceled=%t", hostileName, filtered, canceled), func(t *testing.T) {
					current, previous := "current", "previous-λ|1"
					if hostileName {
						current += "|push_marker:foreign-archive:scope:0123456789abcdef"
					}
					sync.machine = current
					unrelated := previous + "-other"
					otherScope := previous + suffix + "-other"
					filteredScope := previous + suffix + ":scope:0123456789abcdef"
					before := []string{previous, current, previous + suffix, unrelated, otherScope, filteredScope}
					_, err := pg.ExecContext(t.Context(), "DELETE FROM vector_generation_machines")
					require.NoError(t, err)
					for _, name := range before {
						_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines
			(generation_id,machine,last_push_at) VALUES ($1,$2,now())`, generation.id, name)
						require.NoError(t, err)
					}
					_, err = pg.ExecContext(t.Context(), `INSERT INTO vector_generation_machines
		(generation_id,machine,last_push_at) VALUES ($1,$2,now())`, foreignGeneration, previous)
					require.NoError(t, err)
					ctx := t.Context()
					sync.projects = nil
					if filtered {
						sync.projects = []string{"alpha"}
					}
					key, err := sync.vectorGenerationWitnessKey(t.Context())
					require.NoError(t, err)
					if canceled {
						var cancel context.CancelFunc
						ctx, cancel = context.WithCancel(ctx)
						cancel()
					}
					ok, err := sync.recordVectorGenerationMachine(ctx, source.gen.Fingerprint, generation, key, source.gen.IncludeAutomated)
					want := []string{key, unrelated, otherScope}
					if filtered {
						want = append(want, previous, current, previous+suffix, filteredScope)
					}
					if !filtered && hostileName {
						want = append(want, current)
					}
					if canceled {
						require.Error(t, err)
						require.False(t, ok)
						want = before
					} else {
						require.NoError(t, err)
						require.True(t, ok)
						ok, err = sync.recordVectorGenerationMachine(ctx, source.gen.Fingerprint, generation, key, source.gen.IncludeAutomated)
						require.NoError(t, err)
						require.True(t, ok, "repeated certification is idempotent")
					}
					rows, err := pg.QueryContext(t.Context(), "SELECT machine FROM vector_generation_machines WHERE generation_id=$1", generation.id)
					require.NoError(t, err)
					defer rows.Close()
					var got []string
					for rows.Next() {
						var name string
						require.NoError(t, rows.Scan(&name))
						got = append(got, name)
					}
					require.NoError(t, rows.Err())
					require.ElementsMatch(t, want, got)
					var foreignCount int
					require.NoError(t, pg.QueryRowContext(t.Context(), "SELECT count(*) FROM vector_generation_machines WHERE generation_id=$1 AND machine=$2", foreignGeneration, previous).Scan(&foreignCount))
					require.Equal(t, 1, foreignCount, "another generation's plain witness remains untouched")
				})
			}
		}
	}
}

func TestVectorHubMachineRenameRetiresCompatibilityWitness(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%t", legacy), func(t *testing.T) {
			sync, local, pg := newVectorPushTestSync(t, testPGURL(t), "agentsview_vector_hub_rename_test")
			seedVectorSession(t, local, "A")
			source := hubRecipeSource("rename", map[string]string{"max_input_chars": "8192"})
			sync.vectorSource = source
			res, err := sync.Push(t.Context(), false, nil)
			require.NoError(t, err)
			store := &Store{pg: pg}
			setHubTestSearcher(t, store, res.Vectors.GenerationID)
			if legacy {
				_, err = pg.ExecContext(t.Context(), "UPDATE vector_generation_machines SET machine=$1,data_version=NULL,include_automated=NULL", sync.machine)
				require.NoError(t, err)
			}
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
		})
	}
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
					_, err = pg.ExecContext(t.Context(), `UPDATE vector_generation_machines SET machine='other-pusher'`)
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
	require.NoError(t, store.pg.QueryRowContext(t.Context(), `SELECT fingerprint FROM vector_generations WHERE id=$1`, generationID).Scan(&fingerprint))
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
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT created_at::text FROM vector_generations WHERE id=$1`, res.Vectors.GenerationID).Scan(&before))
			// Recreate before the first discovery call: pinning cannot be lazy.
			for _, table := range []string{vectorChunkTable(res.Vectors.GenerationID), "vector_push_state", "vector_generation_machines", "vector_documents", "vector_generations"} {
				_, err = pg.ExecContext(t.Context(), `DROP TABLE `+table)
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
			require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT created_at::text FROM vector_generations WHERE id=$1`, pushed.GenerationID).Scan(&after))
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
			_, err = pg.ExecContext(t.Context(), `DROP TABLE `+vectorChunkTable(res.Vectors.GenerationID))
			require.NoError(t, err)
			generation, err := store.HubVectorGeneration(t.Context())
			require.NoError(t, err)
			assert.NotEmpty(t, generation.Unavailable)
			_, err = store.LookupHubVectors(t.Context(), vector.HubLookupRequest{Fingerprint: "selected", Identities: []vector.HubIdentity{{SessionID: "A", UnitKind: "user", Ordinal: 0, ContentHash: "hash"}}})
			assert.ErrorIs(t, err, vector.ErrHubGenerationChanged)
		})
	}
}
