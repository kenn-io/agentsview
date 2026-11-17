package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"go.kenn.io/agentsview/internal/storage"
)

// ProvisionHostedVector registers and protects a generation under the owner
// connection. The runtime never creates or modifies its schema.
func ProvisionHostedVector(ctx context.Context, pg *sql.DB, schema, tenant string, generation storage.VectorGenerationInfo) error {
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-hosted:"+schema); err != nil {
		return err
	}
	quoted, err := quoteIdentifier(schema)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true),set_config('standard_conforming_strings','on',true)`, quoted, tenant); err != nil {
		return err
	}
	if err = checkHostedBinding(ctx, tx, schema, tenant); err != nil {
		return err
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return err
	}
	id, err := ensureVectorGeneration(ctx, tx, generation.Fingerprint, generation.Model, generation.Dimension)
	if err != nil {
		return err
	}
	if err = publishVectorRecipe(ctx, tx, generation.Fingerprint, generation.Params); err != nil {
		return err
	}
	name := vectorChunkTable(id)
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		return err
	}
	if err = ensureVectorChunkTable(ctx, tx, id, generation.Dimension); err != nil {
		return fmt.Errorf("pgvector required for hosted-provision: %w", err)
	}
	if !exists {
		if err = InstallHostedTables(ctx, tx, schema, tenant, []HostedTable{{Name: name, Key: []string{"doc_key", "chunk_index"}}}); err != nil {
			return err
		}
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return err
	}
	return tx.Commit()
}
