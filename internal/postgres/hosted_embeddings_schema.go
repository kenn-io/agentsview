package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const hostedEmbeddingDDL = `
CREATE TABLE hosted_embedding_sources(tenant_id text NOT NULL,session_id text NOT NULL,revision bigint NOT NULL CHECK(revision>0),dirty boolean NOT NULL DEFAULT true,PRIMARY KEY(tenant_id,session_id));
CREATE TABLE hosted_embedding_generations(tenant_id text NOT NULL,id bigint NOT NULL CHECK(id>0),instance_key text NOT NULL,recipe_fingerprint text NOT NULL,recipe_json jsonb NOT NULL,dimensions integer NOT NULL CHECK(dimensions BETWEEN 1 AND 4000),backfill_after_session_id text NOT NULL DEFAULT '',backfill_finished boolean NOT NULL DEFAULT false,PRIMARY KEY(tenant_id,id),UNIQUE(tenant_id,instance_key));
CREATE TABLE hosted_embedding_state(tenant_id text NOT NULL,singleton smallint NOT NULL CHECK(singleton=1),active_generation_id bigint,desired_generation_id bigint,active_valid boolean NOT NULL DEFAULT false,claim_after_generation_id bigint NOT NULL DEFAULT 0,PRIMARY KEY(tenant_id,singleton));
CREATE TABLE hosted_embedding_requirements(tenant_id text NOT NULL,generation_id bigint NOT NULL,session_id text NOT NULL,required_revision bigint NOT NULL,completed_revision bigint,eligible boolean NOT NULL,state text NOT NULL CHECK(state IN ('ready','leased','retry','failed','complete')),attempt_fence bigint NOT NULL DEFAULT 0,attempts integer NOT NULL DEFAULT 0,lease_owner text,lease_token text,lease_expires_at timestamptz,available_at timestamptz NOT NULL DEFAULT clock_timestamp(),error_code text NOT NULL DEFAULT '',snapshot_manifest_hash text,expected_documents integer,expected_chunks integer,PRIMARY KEY(tenant_id,generation_id,session_id));
CREATE TABLE hosted_embedding_documents(tenant_id text NOT NULL,generation_id bigint NOT NULL,doc_key text NOT NULL,session_id text NOT NULL,source_revision bigint NOT NULL,kind text NOT NULL,source_uuid text NOT NULL,ordinal integer NOT NULL,ordinal_end integer NOT NULL,subordinate boolean NOT NULL,offsets jsonb NOT NULL,content text NOT NULL,content_hash text NOT NULL,chunk_manifest jsonb NOT NULL,PRIMARY KEY(tenant_id,generation_id,doc_key));
ALTER TABLE raw_embedding_outbox ADD COLUMN embedding_consumed boolean NOT NULL DEFAULT false;
CREATE INDEX hosted_embedding_dirty ON hosted_embedding_sources(tenant_id,session_id) WHERE dirty;
CREATE INDEX hosted_embedding_due ON hosted_embedding_requirements(tenant_id,generation_id,available_at,session_id) WHERE state IN ('ready','retry');
CREATE INDEX hosted_embedding_expired ON hosted_embedding_requirements(tenant_id,generation_id,lease_expires_at,session_id) WHERE state='leased';
CREATE INDEX hosted_embedding_incomplete ON hosted_embedding_requirements(tenant_id,generation_id,session_id) WHERE state<>'complete' OR completed_revision IS DISTINCT FROM required_revision;
CREATE INDEX hosted_embedding_document_session ON hosted_embedding_documents(tenant_id,generation_id,session_id);
CREATE INDEX hosted_embedding_outbox_pending ON raw_embedding_outbox(tenant_id,session_id,selection_revision,corpus_revision) WHERE NOT embedding_consumed;
`

var hostedEmbeddingTables = []HostedTable{
	{Name: "hosted_embedding_sources", Key: []string{"session_id"}},
	{Name: "hosted_embedding_generations", Key: []string{"id"}},
	{Name: "hosted_embedding_state", Key: []string{"singleton"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"active_generation_id"}, Table: "hosted_embedding_generations", References: []string{"id"}}, {Columns: []string{"desired_generation_id"}, Table: "hosted_embedding_generations", References: []string{"id"}}}},
	{Name: "hosted_embedding_requirements", Key: []string{"generation_id", "session_id"}, ForeignKeys: embeddingDocumentFK()},
	{Name: "hosted_embedding_documents", Key: []string{"generation_id", "doc_key"}, ForeignKeys: embeddingDocumentFK()},
}

func embeddingDocumentFK() []HostedForeignKey {
	return []HostedForeignKey{{Columns: []string{"generation_id"}, Table: "hosted_embedding_generations", References: []string{"id"}}, {Columns: []string{"session_id"}, Table: "hosted_embedding_sources", References: []string{"session_id"}}}
}
func embeddingTypedTable(id int64) HostedTable {
	return HostedTable{Name: embeddingChunkTable(id), Key: []string{"generation_id", "doc_key", "chunk_index"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"generation_id", "doc_key"}, Table: "hosted_embedding_documents", References: []string{"generation_id", "doc_key"}, Delete: "CASCADE"}}}
}
func embeddingTables(ctx context.Context, q hostedQuerier, schema string) ([]HostedTable, error) {
	var exists bool
	if e := q.QueryRowContext(ctx, `SELECT to_regclass(format('%I.hosted_embedding_generations',$1::text)) IS NOT NULL`, schema).Scan(&exists); e != nil {
		return nil, e
	}
	if !exists {
		return nil, nil
	}
	qs, _ := quoteIdentifier(schema)
	rows, e := q.QueryContext(ctx, `SELECT id FROM `+qs+`.hosted_embedding_generations ORDER BY id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := append([]HostedTable(nil), hostedEmbeddingTables...)
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		if id <= 0 {
			return nil, fmt.Errorf("invalid embedding generation registry")
		}
		out = append(out, embeddingTypedTable(id), embeddingValueTableDefinition(id))
	}
	return out, rows.Err()
}

// ProvisionHostedEmbeddings is an explicit owner operation. Source DDL locks
// precede the corpus fence; force rebuilds touch only new generation tables.
func ProvisionHostedEmbeddings(ctx context.Context, owner *sql.DB, schema, tenant string, recipe HostedEmbeddingRecipe, instanceKey, runtimeRole string) (HostedEmbeddingGeneration, error) {
	var g HostedEmbeddingGeneration
	var err error
	recipe, err = CanonicalHostedEmbeddingRecipe(recipe)
	if err != nil {
		return g, err
	}
	if instanceKey == "" || len(instanceKey) > 256 {
		return g, fmt.Errorf("invalid embedding instance key")
	}
	if err = validateHostedBinding(schema, tenant); err != nil {
		return g, err
	}
	qr, err := quoteIdentifier(runtimeRole)
	if err != nil {
		return g, err
	}
	qs, _ := quoteIdentifier(schema)
	tx, err := owner.BeginTx(ctx, nil)
	if err != nil {
		return g, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-hosted-embedding:"+schema); err != nil {
		return g, err
	}
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true),set_config('standard_conforming_strings','on',true)`, qs, tenant); err != nil {
		return g, err
	}
	if err = checkHostedBinding(ctx, tx, schema, tenant); err != nil {
		return g, err
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return g, err
	}
	var ext string
	if err = tx.QueryRowContext(ctx, `SELECT n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&ext); err != nil {
		return g, fmt.Errorf("pgvector must be installed by owner: %w", err)
	}
	if ext != "public" && ext != schema {
		return g, fmt.Errorf("hosted embeddings require pgvector in public or the bound schema; provision a supported extension schema with the owner role")
	}
	var usable bool
	if err = tx.QueryRowContext(ctx, `SELECT has_schema_privilege($1,$2,'USAGE')`, runtimeRole, ext).Scan(&usable); err != nil {
		return g, err
	}
	if !usable {
		return g, fmt.Errorf("hosted embedding runtime requires USAGE on the supported pgvector schema")
	}
	qe, _ := quoteIdentifier(ext)
	tables, err := embeddingTables(ctx, tx, schema)
	if err != nil {
		return g, err
	}
	if len(tables) == 0 {
		if _, err = tx.ExecContext(ctx, `LOCK TABLE sessions,messages,raw_embedding_outbox IN ACCESS EXCLUSIVE MODE`); err != nil {
			return g, err
		}
		if _, err = tx.ExecContext(ctx, hostedEmbeddingDDL); err != nil {
			return g, err
		}
		if err = InstallHostedTables(ctx, tx, schema, tenant, hostedEmbeddingTables); err != nil {
			return g, err
		}
		if _, err = tx.ExecContext(ctx, embeddingNotifierDDL(tenant)); err != nil {
			return g, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO raw_corpus_state(tenant_id,singleton) VALUES($1,1) ON CONFLICT DO NOTHING`, tenant); err != nil {
		return g, err
	}
	var one int
	if err = tx.QueryRowContext(ctx, `SELECT singleton FROM raw_corpus_state WHERE tenant_id=$1 AND singleton=1 FOR UPDATE`, tenant).Scan(&one); err != nil {
		return g, err
	}
	var existing int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM hosted_embedding_generations WHERE tenant_id=$1 AND instance_key=$2`, tenant, instanceKey).Scan(&existing)
	switch err {
	case nil:
		g, err = loadEmbeddingGeneration(ctx, tx, existing)
		if err != nil {
			return g, err
		}
		if g.Recipe != recipe {
			return g, fmt.Errorf("embedding instance recipe mismatch")
		}
	case sql.ErrNoRows:
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(id),0)+1 FROM hosted_embedding_generations`).Scan(&g.ID); err != nil {
			return g, err
		}
		g.InstanceKey = instanceKey
		g.Recipe = recipe
		b, _ := json.Marshal(recipe)
		if _, err = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_generations(tenant_id,id,instance_key,recipe_fingerprint,recipe_json,dimensions) VALUES($1,$2,$3,$4,$5,$6)`, tenant, g.ID, instanceKey, recipe.Fingerprint, b, recipe.Dimensions); err != nil {
			return g, err
		}
		name := embeddingChunkTable(g.ID)
		ddl := fmt.Sprintf(`CREATE TABLE %s(tenant_id text NOT NULL,generation_id bigint NOT NULL CHECK(generation_id=%d),doc_key text NOT NULL,chunk_index integer NOT NULL CHECK(chunk_index>=0),input_hash text NOT NULL,embedding %s.halfvec(%d) NOT NULL,PRIMARY KEY(tenant_id,generation_id,doc_key,chunk_index)); CREATE INDEX %s_knn ON %s USING hnsw(embedding %s.halfvec_cosine_ops); CREATE INDEX %s_reuse ON %s(tenant_id,input_hash);`, name, g.ID, qe, recipe.Dimensions, name, name, qe, name, name)
		ddl += fmt.Sprintf(`CREATE TABLE %s(tenant_id text NOT NULL,generation_id bigint NOT NULL CHECK(generation_id=%d),input_hash text NOT NULL,embedding %s.halfvec(%d) NOT NULL,PRIMARY KEY(tenant_id,input_hash));`, embeddingValueTable(g.ID), g.ID, qe, recipe.Dimensions)
		if _, err = tx.ExecContext(ctx, ddl); err != nil {
			return g, err
		}
		if err = InstallHostedTables(ctx, tx, schema, tenant, []HostedTable{embeddingTypedTable(g.ID), embeddingValueTableDefinition(g.ID)}); err != nil {
			return g, err
		}
	default:
		return g, err
	}
	// Registry rows cannot be forged by runtime. Only cursor updates are granted.
	for _, table := range []string{"hosted_embedding_sources", "hosted_embedding_requirements", "hosted_embedding_documents", "hosted_embedding_state", embeddingChunkTable(g.ID), embeddingValueTable(g.ID)} {
		if _, err = tx.ExecContext(ctx, `GRANT SELECT,INSERT,UPDATE,DELETE ON `+table+` TO `+qr); err != nil {
			return g, err
		}
	}
	if _, err = tx.ExecContext(ctx, `GRANT SELECT ON hosted_embedding_generations TO `+qr+`; GRANT UPDATE(backfill_after_session_id,backfill_finished) ON hosted_embedding_generations TO `+qr+`; GRANT UPDATE(embedding_consumed) ON raw_embedding_outbox TO `+qr); err != nil {
		return g, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO hosted_embedding_state(tenant_id,singleton) VALUES($1,1) ON CONFLICT DO NOTHING`, tenant); err != nil {
		return g, err
	}
	if err = selectEmbeddingDesired(ctx, tx, tenant, g.ID); err != nil {
		return g, err
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return g, err
	}
	return g, tx.Commit()
}
func embeddingNotifierBody(tenant string, messages bool) string {
	literal := "'" + strings.ReplaceAll(tenant, "'", "''") + "'"
	id := "id"
	cols := []string{"id", "deleted_at", "is_automated", "prompt_evidence_discarded", "relationship_type", "parent_session_id", "parser_parent_session_id", "provenance_kind", "raw_content_revision"}
	if messages {
		id = "session_id"
		cols = []string{"session_id", "ordinal", "role", "source_uuid", "content", "is_sidechain", "is_system"}
	}
	old, new := []string{}, []string{}
	for _, c := range cols {
		old = append(old, "OLD."+c)
		new = append(new, "NEW."+c)
	}
	return `DECLARE ids text[]:='{}'; sid text; BEGIN
 IF TG_OP<>'INSERT' AND OLD.tenant_id IS DISTINCT FROM ` + literal + ` THEN RAISE EXCEPTION 'embedding tenant mismatch'; END IF;
 IF TG_OP<>'DELETE' AND NEW.tenant_id IS DISTINCT FROM ` + literal + ` THEN RAISE EXCEPTION 'embedding tenant mismatch'; END IF;
 IF TG_OP='UPDATE' AND ROW(` + strings.Join(old, ",") + `) IS NOT DISTINCT FROM ROW(` + strings.Join(new, ",") + `) THEN RETURN NULL; END IF;
 IF TG_OP<>'INSERT' THEN ids:=array_append(ids,OLD.` + id + `); END IF;
 IF TG_OP<>'DELETE' THEN ids:=array_append(ids,NEW.` + id + `); END IF;
 PERFORM singleton FROM raw_corpus_state WHERE tenant_id=` + literal + ` AND singleton=1 FOR UPDATE;
 UPDATE raw_corpus_state SET corpus_revision=corpus_revision+1 WHERE tenant_id=` + literal + ` AND singleton=1;
 FOR sid IN SELECT DISTINCT v FROM unnest(ids) v ORDER BY v LOOP
 INSERT INTO hosted_embedding_sources(tenant_id,session_id,revision,dirty) VALUES(` + literal + `,sid,1,true) ON CONFLICT(tenant_id,session_id) DO UPDATE SET revision=hosted_embedding_sources.revision+1,dirty=true;
 END LOOP; RETURN NULL; END; `
}

const embeddingImmutableBody = `BEGIN IF TG_OP<>'UPDATE' OR ROW(OLD.tenant_id,OLD.id,OLD.instance_key,OLD.recipe_fingerprint,OLD.recipe_json,OLD.dimensions) IS DISTINCT FROM ROW(NEW.tenant_id,NEW.id,NEW.instance_key,NEW.recipe_fingerprint,NEW.recipe_json,NEW.dimensions) THEN RAISE EXCEPTION 'embedding recipe is immutable'; END IF; RETURN NEW; END; `

func embeddingNotifierDDL(tenant string) string {
	var out strings.Builder
	for _, v := range []struct{ name, table, body string }{{"hosted_embedding_session_notify", "sessions", embeddingNotifierBody(tenant, false)}, {"hosted_embedding_message_notify", "messages", embeddingNotifierBody(tenant, true)}, {"hosted_embedding_recipe_immutable", "hosted_embedding_generations", embeddingImmutableBody}} {
		events := "AFTER INSERT OR UPDATE OR DELETE"
		if v.table == "hosted_embedding_generations" {
			events = "BEFORE UPDATE OR DELETE"
		}
		out.WriteString(`CREATE FUNCTION ` + v.name + `() RETURNS trigger LANGUAGE plpgsql SECURITY INVOKER AS $embed$` + v.body + `$embed$;CREATE TRIGGER ` + v.name + ` ` + events + ` ON ` + v.table + ` FOR EACH ROW EXECUTE FUNCTION ` + v.name + `();`)
	}
	return out.String()
}

func embeddingValueTable(id int64) string { return fmt.Sprintf("hosted_embedding_values_g%d", id) }
func embeddingValueTableDefinition(id int64) HostedTable {
	return HostedTable{Name: embeddingValueTable(id), Key: []string{"input_hash"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"generation_id"}, Table: "hosted_embedding_generations", References: []string{"id"}}}}
}
