//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/storage"
)

func recipeColumnExists(t *testing.T, pg *sql.DB) bool {
	t.Helper()
	var present bool
	require.NoError(t, pg.QueryRow(`
SELECT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_schema = current_schema()
      AND table_name = 'vector_generations' AND column_name = 'params'
)`).Scan(&present))
	return present
}

func recipeSource(fingerprint string, params map[string]string) *fakeVectorSource {
	return &fakeVectorSource{
		gen: storage.VectorGenerationInfo{
			Fingerprint: fingerprint, Model: "m", Dimension: 4, Params: params,
		},
		hasGen: true,
		hashes: map[string]string{"A": "ha"},
		docs: map[string][]storage.VectorPushDoc{
			"A": {vdoc("A", "A#0", 0, "c1", "h1", []float32{1, 0, 0, 0})},
		},
	}
}

func TestVectorRecipePublishedAndBackfilled(t *testing.T) {
	pgURL := testPGURL(t)
	sync, localDB, pg := newVectorPushTestSync(t, pgURL, "agentsview_vector_recipe_test")
	ctx := context.Background()
	seedVectorSession(t, localDB, "A")

	// A generation registered before recipe publication has NULL params.
	unavailable, err := ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err)
	require.Empty(t, unavailable)
	_, err = ensureVectorGeneration(ctx, pg, "fp-recipe", "m", 4)
	require.NoError(t, err)
	gens, err := ListVectorGenerationInfo(ctx, pg)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	assert.Nil(t, gens[0].Params, "legacy row")

	published := map[string]string{"max_input_chars": "8192", "query_prefix": "query: "}
	sync.vectorSource = recipeSource("fp-recipe", published)
	res, err := sync.Push(ctx, false, nil)
	require.NoError(t, err)
	assert.False(t, res.Vectors.Skipped)
	gens, err = ListVectorGenerationInfo(ctx, pg)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	assert.Equal(t, published, gens[0].Params, "the push backfills the NULL row")

	// Stored params are immutable.
	sync.vectorSource = recipeSource("fp-recipe", map[string]string{"query_prefix": "other: "})
	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err)
	gens, err = ListVectorGenerationInfo(ctx, pg)
	require.NoError(t, err)
	assert.Equal(t, published, gens[0].Params, "existing params are never overwritten")

	// A pre-column schema reads as unpublished, and read paths add nothing.
	_, err = pg.Exec(`ALTER TABLE vector_generations DROP COLUMN params`)
	require.NoError(t, err)
	gens, err = ListVectorGenerationInfo(ctx, pg)
	require.NoError(t, err)
	require.Len(t, gens, 1)
	assert.Nil(t, gens[0].Params)
	_, _, ok, err := LookupVectorGeneration(ctx, pg, "fp-recipe")
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, EnsureSchema(ctx, pg, "agentsview_vector_recipe_test"))
	_, err = ensureVectorBaseSchemaPG(ctx, pg)
	require.NoError(t, err)
	assert.False(t, recipeColumnExists(t, pg), "serve-side schema preparation adds no column")

	// The next writer push migrates the column and publishes again.
	sync.vectorSource = recipeSource("fp-recipe", published)
	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err)
	assert.True(t, recipeColumnExists(t, pg))
	gens, err = ListVectorGenerationInfo(ctx, pg)
	require.NoError(t, err)
	assert.Equal(t, published, gens[0].Params)

	// A generation exported without params (a non-matching local recipe)
	// registers with NULL params.
	sync.vectorSource = recipeSource("fp-unpublished", nil)
	_, err = sync.Push(ctx, true, nil)
	require.NoError(t, err)
	gens, err = ListVectorGenerationInfo(ctx, pg)
	require.NoError(t, err)
	require.Len(t, gens, 2)
	assert.Nil(t, gens[1].Params)
}

func TestVectorRecipeWriterWithoutOwnershipKeepsPushingVectors(t *testing.T) {
	pgURL := testPGURL(t)
	const schema = "agentsview_vector_recipe_role_test"
	const role = "agentsview_vector_recipe_writer"
	const rolePassword = "agentsview_vector_recipe_writer_pw"
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		// dropColumn simulates tables created before recipe publication.
		dropColumn bool
		// revokeUpdate denies UPDATE on vector_generations.
		revokeUpdate bool
	}{
		{name: "cannot add the column", dropColumn: true},
		{name: "cannot update the row", revokeUpdate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admin, err := Open(pgURL, schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { _ = admin.Close() })
			_, err = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
			require.NoError(t, err)
			require.NoError(t, EnsureSchema(ctx, admin, schema))
			unavailable, err := ensureVectorBaseSchemaPG(ctx, admin)
			require.NoError(t, err)
			if unavailable != "" {
				t.Skip(unavailable)
			}
			genID, err := ensureVectorGeneration(ctx, admin, "fp-role", "m", 4)
			require.NoError(t, err)
			require.NoError(t, ensureVectorChunkTable(ctx, admin, genID, 4))
			if tc.dropColumn {
				_, err = admin.Exec(`ALTER TABLE vector_generations DROP COLUMN params`)
				require.NoError(t, err)
			}

			_, _ = admin.Exec(`DROP OWNED BY ` + role)
			_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
			_, err = admin.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + rolePassword + `'`)
			require.NoError(t, err)
			t.Cleanup(func() {
				_, _ = admin.Exec(`DROP SCHEMA IF EXISTS ` + schema + ` CASCADE`)
				_, _ = admin.Exec(`DROP OWNED BY ` + role)
				_, _ = admin.Exec(`DROP ROLE IF EXISTS ` + role)
			})
			grants := []string{
				`GRANT USAGE, CREATE ON SCHEMA ` + schema + ` TO ` + role,
				`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA ` + schema + ` TO ` + role,
				`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA ` + schema + ` TO ` + role,
			}
			// The writer owns the tables whose indexes every vector push
			// re-declares (CREATE INDEX IF NOT EXISTS needs ownership), as a
			// writer that pushes vectors on main must; vector_generations
			// stays with the admin, so the writer cannot alter it.
			for _, table := range []string{
				"vector_documents", "vector_push_state",
				"vector_generation_machines", vectorChunkTable(genID),
			} {
				grants = append(grants, `ALTER TABLE `+table+` OWNER TO `+role)
			}
			// pgvector's types and operator classes live wherever the
			// extension was first installed in this database.
			extSchema, err := vectorExtensionSchema(ctx, admin)
			require.NoError(t, err)
			grants = append(grants, `GRANT USAGE ON SCHEMA `+extSchema+` TO `+role)
			if tc.revokeUpdate {
				grants = append(grants, `REVOKE UPDATE ON vector_generations FROM `+role)
			}
			for _, grant := range grants {
				_, err = admin.Exec(grant)
				require.NoError(t, err, grant)
			}

			restrictedURL, err := url.Parse(pgURL)
			require.NoError(t, err)
			restrictedURL.User = url.UserPassword(role, rolePassword)
			restricted, err := Open(restrictedURL.String(), schema, true)
			require.NoError(t, err)
			t.Cleanup(func() { _ = restricted.Close() })

			localDB, err := db.Open(t.Context(), filepath.Join(t.TempDir(), "local.db"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = localDB.Close() })
			seedVectorSession(t, localDB, "A")
			sync := &Sync{
				pg: restricted, local: localDB, machine: "test-machine",
				schema: schema, schemaDone: true,
			}
			sync.vectorSource = recipeSource("fp-role", map[string]string{"query_prefix": "query: "})

			pushed, err := sync.Push(ctx, false, nil)
			require.NoError(t, err)
			res := pushed.Vectors
			assert.False(t, res.Skipped, res.SkippedReason)
			assert.Equal(t, 1, res.SessionsPushed)
			assert.Equal(t, 1, countRows(t, admin,
				`SELECT COUNT(*) FROM `+vectorChunkTable(genID)))
			gens, err := ListVectorGenerationInfo(ctx, admin)
			require.NoError(t, err)
			require.Len(t, gens, 1)
			assert.Nil(t, gens[0].Params, "params stay NULL for a writer that cannot publish")
			assert.Equal(t, !tc.dropColumn, recipeColumnExists(t, admin))
		})
	}
}
