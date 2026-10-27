package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	kitvec "go.kenn.io/kit/vector"
)

//nolint:kennlint // Persisted generation states form a fixed schema contract.
const hostedVectorDDL = `
CREATE TABLE IF NOT EXISTS hosted_vector_generations (
 generation_id BIGINT PRIMARY KEY,
 state TEXT NOT NULL CHECK (state IN ('building','active','retired')),
 mirror_revision BIGINT NOT NULL DEFAULT 0,
 bootstrap_revision BIGINT,
 bootstrap_after_session_id TEXT NOT NULL DEFAULT '',
 bootstrap_done BOOLEAN NOT NULL DEFAULT false,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS hosted_vector_documents (
 generation_id BIGINT NOT NULL, doc_key TEXT NOT NULL, session_id TEXT NOT NULL,
 source_uuid TEXT NOT NULL DEFAULT '', ordinal INTEGER NOT NULL,
 ordinal_end INTEGER NOT NULL, subordinate BOOLEAN NOT NULL DEFAULT false,
 offsets TEXT NOT NULL DEFAULT '[]', content TEXT NOT NULL, content_hash TEXT NOT NULL,
 embedded_hash TEXT, skipped BOOLEAN NOT NULL DEFAULT false,
 PRIMARY KEY (generation_id,doc_key)
);
`

const hostedVectorIndexesDDL = `
CREATE UNIQUE INDEX IF NOT EXISTS hosted_vector_one_active ON hosted_vector_generations(tenant_id) WHERE state='active';
CREATE INDEX IF NOT EXISTS hosted_vector_pending ON hosted_vector_documents(tenant_id,generation_id,doc_key) WHERE embedded_hash IS DISTINCT FROM content_hash;
CREATE INDEX IF NOT EXISTS hosted_vector_session ON hosted_vector_documents(tenant_id,generation_id,session_id);
CREATE INDEX IF NOT EXISTS raw_embedding_revision ON raw_embedding_outbox(tenant_id,corpus_revision);
`

var hostedVectorTables = []HostedTable{
	{Name: "hosted_vector_generations", Key: []string{"generation_id"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"generation_id"}, Table: "vector_generations", References: []string{"id"}}}},
	{Name: "hosted_vector_documents", Key: []string{"generation_id", "doc_key"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"generation_id"}, Table: "hosted_vector_generations", References: []string{"generation_id"}, Delete: "CASCADE"}}},
}

func installHostedVectorUpgrade(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	if err := ensureVectorRecipeColumn(ctx, tx); err != nil {
		return err
	}
	var missing []HostedTable
	for _, table := range hostedVectorTables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass(format('%I.%I',$1::text,$2::text)) IS NOT NULL`, schema, table.Name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		if _, err := tx.ExecContext(ctx, hostedVectorDDL); err != nil {
			return err
		}
		if err := InstallHostedTables(ctx, tx, schema, tenant, missing); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, hostedVectorIndexesDDL)
	return err
}

func hostedChunkInventory(id int64) HostedTable {
	return HostedTable{Name: vectorChunkTable(id), Key: []string{"doc_key", "chunk_index"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"generation_id", "doc_key"}, Table: "hosted_vector_documents", References: []string{"generation_id", "doc_key"}, Delete: "CASCADE"}}}
}

// Only registered hosted generations may add chunk tables to the inventory.
// The ordinary catalog gate checks their tenant keys, RLS and complete FKs.
func hostedCatalogTables(ctx context.Context, q hostedQuerier, schema string) ([]HostedTable, error) {
	qs, _ := quoteIdentifier(schema)
	rows, err := q.QueryContext(ctx, `SELECT g.generation_id,
 EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=to_regclass(format('%I.vector_chunks_g%s',$1::text,g.generation_id)) AND a.attname='generation_id' AND a.attnotnull AND a.atttypid='bigint'::regtype AND NOT a.attisdropped)
 AND EXISTS(SELECT 1 FROM pg_constraint k WHERE k.conrelid=to_regclass(format('%I.vector_chunks_g%s',$1::text,g.generation_id)) AND k.contype='c' AND k.convalidated AND k.conname='hosted_generation_check' AND pg_get_expr(k.conbin,k.conrelid)=format('(generation_id = %L::bigint)',g.generation_id))
 FROM `+qs+`.hosted_vector_generations g ORDER BY generation_id`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := slices.Clone(hostedTables)
	for rows.Next() {
		var id int64
		var bound bool
		if err = rows.Scan(&id, &bound); err != nil {
			return nil, err
		}
		if id <= 0 {
			return nil, errors.New("invalid hosted vector generation")
		}
		if !bound {
			return nil, fmt.Errorf("hosted generation binding missing or altered on %s", vectorChunkTable(id))
		}
		tables = append(tables, hostedChunkInventory(id))
	}
	return tables, rows.Err()
}

// EnsureHostedVectorGeneration is an owner-only operation after tenant
// provisioning. It publishes an immutable recipe and atomically installs a
// building generation and protected chunk table. Existing states are retained.
// Runtime grants remain an explicit owner step, as for other hosted tables.
func EnsureHostedVectorGeneration(ctx context.Context, pg *sql.DB, schema, tenant string, gen kitvec.Generation) (int64, error) {
	if err := validateHostedBinding(schema, tenant); err != nil {
		return 0, err
	}
	if gen.Model == "" || gen.Dimensions <= 0 || len(gen.Params) == 0 {
		return 0, errors.New("hosted vector provisioning requires a complete recipe")
	}
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-hosted:"+schema); err != nil {
		return 0, err
	}
	qs, _ := quoteIdentifier(schema)
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true)`, qs, tenant); err != nil {
		return 0, err
	}
	if err = checkHostedBinding(ctx, tx, schema, tenant); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `CREATE EXTENSION IF NOT EXISTS vector SCHEMA public`); err != nil {
		return 0, fmt.Errorf("hosted vectors require pgvector: %w", err)
	}
	// Keep database-wide types outside disposable tenant and mirror schemas.
	extensionSchema, err := vectorExtensionSchema(ctx, tx)
	if err != nil {
		return 0, err
	}
	if extensionSchema != `"public"` {
		if _, err = tx.ExecContext(ctx, `ALTER EXTENSION vector SET SCHEMA public`); err != nil {
			return 0, fmt.Errorf("pgvector is installed in schema %s; its owner must run ALTER EXTENSION vector SET SCHEMA public, then provision again: %w", extensionSchema, err)
		}
	}
	id, err := ensureVectorGeneration(ctx, tx, gen.Fingerprint(), gen.Model, gen.Dimensions)
	if err != nil {
		return 0, err
	}
	if err = publishVectorRecipe(ctx, tx, gen.Fingerprint(), gen.Params); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO hosted_vector_generations(generation_id,state) VALUES($1,'building') ON CONFLICT(generation_id) DO NOTHING`, id); err != nil {
		return 0, err
	}
	table := vectorChunkTable(id)
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		ext, err := vectorExtensionSchema(ctx, tx)
		if err != nil {
			return 0, err
		}
		//nolint:kennlint // Chunk rows must remain bound to this physical generation table.
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE %s (
   generation_id BIGINT NOT NULL DEFAULT %d CONSTRAINT hosted_generation_check CHECK(generation_id='%d'::bigint),
   doc_key TEXT NOT NULL, chunk_index INTEGER NOT NULL,
   embedding %s.halfvec(%d) NOT NULL, PRIMARY KEY(doc_key,chunk_index));
   CREATE INDEX idx_%s_hnsw ON %s USING hnsw(embedding %s.halfvec_cosine_ops)`, table, id, id, ext, gen.Dimensions, table, table, ext)); err != nil {
			return 0, err
		}
		if err = InstallHostedTables(ctx, tx, schema, tenant, []HostedTable{hostedChunkInventory(id)}); err != nil {
			return 0, err
		}
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}
