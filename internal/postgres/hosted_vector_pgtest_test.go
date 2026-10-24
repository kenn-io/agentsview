//go:build pgtest

package postgres

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	kitvec "go.kenn.io/kit/vector"
	"hegel.dev/go/hegel"
)

// Provisioning must leave a generation usable by a restricted runtime while
// keeping unknown vector tables outside the hosted catalog.
func TestHostedVectorProvisioning(t *testing.T) {
	f := newHostedFixture(t, "tenant-vectors")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	gen := kitvec.Generation{Model: "synthetic-model", Dimensions: 4, Params: map[string]string{"max_input_chars": "64", "doc_unit_scheme": "run_v1", "chunk_overlap_chars": "9"}}
	id, err := EnsureHostedVectorGeneration(t.Context(), f.admin, f.schema, f.tenant, gen)
	require.NoError(t, err)
	table := vectorChunkTable(id)
	// Owner-managed grants remain explicit, as for every other hosted table.
	require.ErrorContains(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull), table)
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT,INSERT,UPDATE,DELETE ON `+table+` TO "`+f.role+`"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	require.NoError(t, CheckHostedRuntimeWritable(t.Context(), f.runtime, f.schema, config.ArchiveContentFull))
	var state string
	var revision int64
	var done bool
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT state,mirror_revision,bootstrap_done FROM hosted_vector_generations WHERE generation_id=$1`, id).Scan(&state, &revision, &done))
	assert.Equal(t, "building", state)
	assert.Zero(t, revision)
	assert.False(t, done)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO hosted_vector_documents(generation_id,doc_key,session_id,ordinal,ordinal_end,content,content_hash) VALUES($1,'doc','session',0,0,'content','hash')`, id)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), fmt.Sprintf(`INSERT INTO %s(doc_key,chunk_index,embedding) VALUES('doc',0,'[1,0,0,0]')`, table))
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `DELETE FROM hosted_vector_documents WHERE generation_id=$1`, id)
	require.NoError(t, err)
	var count int
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT count(*) FROM `+table).Scan(&count))
	assert.Zero(t, count, "document removal cascades to its chunks")
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO hosted_vector_documents(tenant_id,generation_id,doc_key,session_id,ordinal,ordinal_end,content,content_hash) VALUES('other',$1,'bad','session',0,0,'content','hash')`, id)
	assert.Error(t, err)
	require.ErrorContains(t, DropVectorGeneration(t.Context(), f.admin, id), "hosted-recovery")
	_, err = f.admin.ExecContext(t.Context(), `CREATE TABLE vector_chunks_g999999(doc_key TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	require.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

// Provisioning is idempotent across arbitrary recipe changes and revisits. The
// model records published params and state independently of later provision calls.
func TestHostedVectorProvisionSequences(t *testing.T) {
	f := newHostedFixture(t, "tenant-vectors")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		recipes := make([]kitvec.Generation, 4)
		for i := range recipes {
			chars := hegel.Draw(ht, hegel.Integers(1, math.MaxInt))
			recipes[i] = kitvec.Generation{
				Model:      fmt.Sprintf("synthetic-%x", hegel.Draw(ht, hegel.Integers(uint64(0), uint64(math.MaxUint64)))),
				Dimensions: hegel.Draw(ht, hegel.Integers(1, 4000)), // pgvector's halfvec HNSW dimension range.
				Params:     map[string]string{"max_input_chars": strconv.Itoa(chars), "doc_unit_scheme": "run_v1", "query_prefix": hegel.Draw(ht, hegel.Text().Codec("utf-8").ExcludeCharacters("\x00"))},
			}
		}
		ops := hegel.Draw(ht, hegel.Lists(hegel.Integers(uint32(0), uint32(math.MaxUint32))))
		ops = append([]uint32{0}, ops[:min(len(ops), 5)]...) // Bound the number of materialized schemas per case.
		type snapshot struct {
			id     int64
			state  string
			recipe kitvec.Generation
		}
		want := map[string]snapshot{}
		defer func() {
			for _, s := range want {
				_, err := f.admin.ExecContext(t.Context(), `DROP TABLE `+vectorChunkTable(s.id))
				require.NoError(ht, err)
				_, err = f.admin.ExecContext(t.Context(), `DELETE FROM hosted_vector_generations WHERE generation_id=$1`, s.id)
				require.NoError(ht, err)
				_, err = f.admin.ExecContext(t.Context(), `DELETE FROM vector_generations WHERE id=$1`, s.id)
				require.NoError(ht, err)
			}
		}()
		active := false
		for _, op := range ops {
			recipe := recipes[int(op)%len(recipes)]
			id, err := EnsureHostedVectorGeneration(ht.Context(), f.admin, f.schema, f.tenant, recipe)
			require.NoError(ht, err)
			fp := recipe.Fingerprint()
			if s, ok := want[fp]; ok {
				require.Equal(ht, s.id, id)
			} else {
				state := "building"
				if op&4 != 0 && !active {
					state = "active"
					active = true
				} else if op&8 != 0 {
					state = "retired"
				}
				_, err = f.admin.ExecContext(ht.Context(), `UPDATE hosted_vector_generations SET state=$2 WHERE generation_id=$1`, id, state)
				require.NoError(ht, err)
				_, err = f.admin.ExecContext(ht.Context(), `UPDATE hosted_vector_generations SET mirror_revision=7,bootstrap_revision=5,bootstrap_after_session_id='saved',bootstrap_done=true WHERE generation_id=$1`, id)
				require.NoError(ht, err)
				_, err = f.admin.ExecContext(ht.Context(), `INSERT INTO hosted_vector_documents(tenant_id,generation_id,doc_key,session_id,ordinal,ordinal_end,content,content_hash) VALUES($2,$1,'saved','session',0,0,'preserved','hash')`, id, f.tenant)
				require.NoError(ht, err)
				_, err = f.admin.ExecContext(ht.Context(), `INSERT INTO `+vectorChunkTable(id)+`(tenant_id,doc_key,chunk_index,embedding) SELECT $2,'saved',0,('['||'1'||repeat(',0',$1-1)||']')::public.halfvec`, recipe.Dimensions, f.tenant)
				require.NoError(ht, err)
				want[fp] = snapshot{id, state, recipe}
			}
			again, err := EnsureHostedVectorGeneration(ht.Context(), f.admin, f.schema, f.tenant, recipe)
			require.NoError(ht, err)
			require.Equal(ht, id, again)
			for _, s := range want {
				var preserved bool
				require.NoError(ht, f.admin.QueryRowContext(ht.Context(), `SELECT mirror_revision=7 AND bootstrap_revision=5 AND bootstrap_after_session_id='saved' AND bootstrap_done AND EXISTS(SELECT 1 FROM hosted_vector_documents WHERE generation_id=$1 AND content='preserved' AND content_hash='hash') AND EXISTS(SELECT 1 FROM `+vectorChunkTable(s.id)+` WHERE doc_key='saved') FROM hosted_vector_generations WHERE generation_id=$1`, s.id).Scan(&preserved))
				assert.True(ht, preserved, "reprovisioning preserves progress and stored content")
				var state, model string
				var params []byte
				var dim int
				require.NoError(ht, f.runtime.QueryRowContext(ht.Context(), `SELECT h.state,g.model,g.dimension,g.params FROM hosted_vector_generations h JOIN vector_generations g ON g.id=h.generation_id WHERE h.generation_id=$1`, s.id).Scan(&state, &model, &dim, &params))
				var decoded map[string]string
				require.NoError(ht, json.Unmarshal(params, &decoded))
				assert.Equal(ht, s.state, state)
				assert.Equal(ht, s.recipe.Model, model)
				assert.Equal(ht, s.recipe.Dimensions, dim)
				assert.Equal(ht, s.recipe.Params, decoded)
			}
			var count int
			require.NoError(ht, f.runtime.QueryRowContext(ht.Context(), `SELECT count(*) FROM hosted_vector_generations WHERE state='active'`).Scan(&count))
			assert.LessOrEqual(ht, count, 1)
			require.NoError(ht, CheckHostedTenant(ht.Context(), f.runtime, f.schema, f.tenant))
		}
		// Real PostgreSQL DDL and catalog checks exceed the in-memory speed budget.
	}, hegel.WithTestCases(100), hegel.SuppressHealthCheck(hegel.TooSlow))
}

func TestHostedVectorCatalogRejectsAlteredGenerationBinding(t *testing.T) {
	f := newHostedFixture(t, "tenant-vectors")
	id, err := EnsureHostedVectorGeneration(t.Context(), f.admin, f.schema, f.tenant, kitvec.Generation{Model: "synthetic-model", Dimensions: 4, Params: map[string]string{"max_input_chars": "64"}})
	require.NoError(t, err)
	table := vectorChunkTable(id)
	_, err = f.admin.ExecContext(t.Context(), `ALTER TABLE `+table+` ALTER COLUMN generation_id DROP NOT NULL`)
	require.NoError(t, err)
	require.Error(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
}

func TestHostedVectorAdoptsMirrorWithoutRecipeColumn(t *testing.T) {
	pg := newHostedLegacyFixture(t)
	var schema string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT current_schema()`).Scan(&schema))
	// Restore the pre-recipe mirror shape created before params was published.
	_, err := pg.ExecContext(t.Context(), `ALTER TABLE vector_generations DROP COLUMN params;
 INSERT INTO vector_generations(fingerprint,model,dimension) VALUES('legacy','legacy-model',4)`)
	require.NoError(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), pg, schema, "tenant-adoption"))
	gen := kitvec.Generation{Model: "synthetic", Dimensions: 4, Params: map[string]string{"max_input_chars": "64"}}
	id, err := EnsureHostedVectorGeneration(t.Context(), pg, schema, "tenant-adoption", gen)
	require.NoError(t, err)
	var params []byte
	var state string
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT g.params,h.state FROM vector_generations g JOIN hosted_vector_generations h ON h.generation_id=g.id WHERE g.id=$1`, id).Scan(&params, &state))
	var decoded map[string]string
	require.NoError(t, json.Unmarshal(params, &decoded))
	assert.Equal(t, gen.Params, decoded)
	assert.Equal(t, "building", state)
	var model string
	var dimension int
	require.NoError(t, pg.QueryRowContext(t.Context(), `SELECT model,dimension FROM vector_generations WHERE fingerprint='legacy'`).Scan(&model, &dimension))
	assert.Equal(t, "legacy-model", model)
	assert.Equal(t, 4, dimension)
}

func TestHostedVectorUpgradePreservesExistingTenant(t *testing.T) {
	f := newHostedFixture(t, "tenant-upgrade")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('saved','project','device','claude')`)
	require.NoError(t, err)
	_, err = f.admin.ExecContext(t.Context(), `DROP TABLE hosted_vector_documents,hosted_vector_generations; ALTER TABLE vector_generations DROP COLUMN params`)
	require.NoError(t, err)
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	require.NoError(t, EnsureHostedTenant(t.Context(), f.admin, f.schema, f.tenant))
	_, err = f.admin.ExecContext(t.Context(), `GRANT SELECT ON hosted_vector_generations,hosted_vector_documents TO "`+f.role+`"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	var project string
	require.NoError(t, f.runtime.QueryRowContext(t.Context(), `SELECT project FROM sessions WHERE id='saved'`).Scan(&project))
	assert.Equal(t, "project", project)
	_, err = EnsureHostedVectorGeneration(t.Context(), f.admin, f.schema, f.tenant, kitvec.Generation{Model: "synthetic", Dimensions: 4, Params: map[string]string{"max_input_chars": "64"}})
	require.NoError(t, err)
}

func TestHostedVectorRelocatesExtensionFromMirror(t *testing.T) {
	f := newHostedFixture(t, "tenant-extension")
	_, err := f.admin.ExecContext(t.Context(), `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public; CREATE SCHEMA vector_mirror; ALTER EXTENSION vector SET SCHEMA vector_mirror`)
	require.NoError(t, err)
	t.Cleanup(func() {
		var schema string
		err := f.admin.QueryRowContext(context.Background(), `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&schema)
		assert.NoError(t, err)
		if schema != "public" {
			_, err = f.admin.ExecContext(context.Background(), `ALTER EXTENSION vector SET SCHEMA public`)
			assert.NoError(t, err)
		}
		_, err = f.admin.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS vector_mirror CASCADE`)
		assert.NoError(t, err)
	})
	id, err := EnsureHostedVectorGeneration(t.Context(), f.admin, f.schema, f.tenant, kitvec.Generation{Model: "synthetic", Dimensions: 4, Params: map[string]string{"max_input_chars": "64"}})
	require.NoError(t, err)
	var namespace string
	require.NoError(t, f.admin.QueryRowContext(t.Context(), `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&namespace))
	require.Equal(t, "public", namespace)
	_, err = f.admin.ExecContext(t.Context(), `DROP SCHEMA vector_mirror CASCADE; GRANT SELECT,INSERT,UPDATE,DELETE ON `+vectorChunkTable(id)+` TO "`+f.role+`"`)
	require.NoError(t, err)
	require.NoError(t, CheckHostedTenant(t.Context(), f.runtime, f.schema, f.tenant))
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO hosted_vector_documents(generation_id,doc_key,session_id,ordinal,ordinal_end,content,content_hash) VALUES($1,'doc','session',0,0,'content','hash')`, id)
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `INSERT INTO `+vectorChunkTable(id)+`(doc_key,chunk_index,embedding) VALUES('doc',0,'[1,0,0,0]'::public.halfvec)`)
	require.NoError(t, err)
}
