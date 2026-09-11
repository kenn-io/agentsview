package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func checkEmbeddingCatalog(ctx context.Context, q hostedQuerier, schema, tenant string, tables []HostedTable) error {
	if len(tables) == 0 {
		return nil
	}
	qs, _ := quoteIdentifier(schema)
	if e := checkEmbeddingColumns(ctx, q, schema, tables); e != nil {
		return e
	}
	if e := checkEmbeddingVectorAccess(ctx, q, schema); e != nil {
		return e
	}

	var unsafeRegistry bool
	if e := q.QueryRowContext(ctx, `SELECT c.relowner<>(SELECT oid FROM pg_roles WHERE rolname=current_user) AND (has_table_privilege(c.oid,'INSERT,DELETE') OR has_column_privilege(c.oid,'recipe_json','UPDATE') OR has_column_privilege(c.oid,'recipe_fingerprint','UPDATE') OR has_column_privilege(c.oid,'dimensions','UPDATE') OR has_column_privilege(c.oid,'id','UPDATE') OR has_column_privilege(c.oid,'instance_key','UPDATE')) FROM pg_class c WHERE c.oid=to_regclass(format('%I.hosted_embedding_generations',$1::text))`, schema).Scan(&unsafeRegistry); e != nil {
		return e
	}
	if unsafeRegistry {
		return fmt.Errorf("hosted embedding registry requires owner-only immutable fields")
	}
	for _, v := range []struct{ table, expr string }{{"hosted_embedding_sources", "(revision > 0)"}, {"hosted_embedding_generations", "(id > 0)"}, {"hosted_embedding_generations", "((dimensions >= 1) AND (dimensions <= 4000))"}, {"hosted_embedding_state", "(singleton = 1)"}, {"hosted_embedding_requirements", "(state = ANY (ARRAY['ready'::text, 'leased'::text, 'retry'::text, 'failed'::text, 'complete'::text]))"}} {
		var valid bool
		e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND c.contype='c' AND c.convalidated AND pg_get_expr(c.conbin,c.conrelid)=$3)`, schema, v.table, v.expr).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return fmt.Errorf("hosted embedding constraint altered on %s", v.table)
		}
	}

	for _, v := range []struct {
		name, table, body string
		kind              int
	}{{"hosted_embedding_session_notify", "sessions", embeddingNotifierBody(tenant, false), 29}, {"hosted_embedding_message_notify", "messages", embeddingNotifierBody(tenant, true), 29}, {"hosted_embedding_recipe_immutable", "hosted_embedding_generations", embeddingImmutableBody, 27}} {
		var valid bool
		e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_namespace n ON n.oid=p.pronamespace WHERE t.tgrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND t.tgname=$3 AND t.tgenabled='O' AND t.tgtype=$4 AND NOT t.tgisinternal AND t.tgqual IS NULL AND t.tgnargs=0 AND p.prosrc=$5 AND NOT p.prosecdef AND p.proconfig IS NULL AND n.nspname=$1 AND p.proname=$3)`, schema, v.table, v.name, v.kind, v.body).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return fmt.Errorf("embedding notifier altered: %s", v.name)
		}
	}
	for _, v := range []struct{ name, table, cols, pred, method string }{
		{"hosted_embedding_dirty", "hosted_embedding_sources", "tenant_id,session_id", "dirty", "btree"},
		{"hosted_embedding_due", "hosted_embedding_requirements", "tenant_id,generation_id,available_at,session_id", "state = ANY (ARRAY['ready'::text, 'retry'::text])", "btree"},
		{"hosted_embedding_expired", "hosted_embedding_requirements", "tenant_id,generation_id,lease_expires_at,session_id", "state = 'leased'::text", "btree"},
		{"hosted_embedding_incomplete", "hosted_embedding_requirements", "tenant_id,generation_id,session_id", "(state <> 'complete'::text) OR (completed_revision IS DISTINCT FROM required_revision)", "btree"},
		{"hosted_embedding_document_session", "hosted_embedding_documents", "tenant_id,generation_id,session_id", "", "btree"},
		{"hosted_embedding_outbox_pending", "raw_embedding_outbox", "tenant_id,session_id,selection_revision,corpus_revision", "NOT embedding_consumed", "btree"},
	} {
		if e := checkEmbeddingIndex(ctx, q, schema, v.name, v.table, v.cols, v.pred, v.method); e != nil {
			return e
		}
	}
	rows, e := q.QueryContext(ctx, `SELECT id,dimensions,recipe_fingerprint,recipe_json FROM `+qs+`.hosted_embedding_generations ORDER BY id`)
	if e != nil {
		return e
	}
	type gen struct {
		id  int64
		dim int
		fp  string
		b   []byte
	}
	var gens []gen
	for rows.Next() {
		var g gen
		if e = rows.Scan(&g.id, &g.dim, &g.fp, &g.b); e != nil {
			rows.Close()
			return e
		}
		gens = append(gens, g)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, g := range gens {
		var r HostedEmbeddingRecipe
		if e = json.Unmarshal(g.b, &r); e != nil {
			return e
		}
		r, e = CanonicalHostedEmbeddingRecipe(r)
		if e != nil {
			return e
		}
		if r.Dimensions != g.dim || r.Fingerprint != g.fp {
			return fmt.Errorf("embedding registry recipe mismatch")
		}
		name := embeddingChunkTable(g.id)
		var valid bool
		for _, typedName := range []string{name, embeddingValueTable(g.id)} {
			e = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute a JOIN pg_type t ON t.oid=a.atttypid JOIN pg_extension e ON e.extnamespace=t.typnamespace WHERE a.attrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND a.attname='embedding' AND a.attnotnull AND a.atttypmod=$3 AND t.typname='halfvec' AND e.extname='vector') AND EXISTS(SELECT 1 FROM pg_constraint c WHERE c.conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND c.contype='c' AND c.convalidated AND pg_get_expr(c.conbin,c.conrelid)=format('(generation_id = %s)',$4::bigint))`, schema, typedName, g.dim, g.id).Scan(&valid)
			if e != nil {
				return e
			}
			if !valid {
				return fmt.Errorf("embedding typed table mismatch")
			}
		}
		if e = checkEmbeddingIndex(ctx, q, schema, name+"_knn", name, "embedding", "", "hnsw"); e != nil {
			return e
		}
		if e = checkEmbeddingIndex(ctx, q, schema, name+"_reuse", name, "tenant_id,input_hash", "", "btree"); e != nil {
			return e
		}
		e = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_index i JOIN pg_opclass o ON o.oid=i.indclass[0] JOIN pg_extension e ON e.extnamespace=o.opcnamespace WHERE i.indexrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND o.opcname='halfvec_cosine_ops' AND e.extname='vector')`, schema, name+"_knn").Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return fmt.Errorf("embedding cosine index altered")
		}
	}
	return nil
}
func checkEmbeddingIndex(ctx context.Context, q hostedQuerier, schema, name, table, cols, pred, method string) error {
	var gotCols, gotPred, gotMethod string
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT i.indisvalid AND i.indisready AND i.indexprs IS NULL AND i.indnatts=i.indnkeyatts, array_to_string(ARRAY(SELECT a.attname FROM unnest(i.indkey) WITH ORDINALITY k(n,p) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.n ORDER BY k.p),','), COALESCE(pg_get_expr(i.indpred,i.indrelid),''), m.amname FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am m ON m.oid=c.relam WHERE i.indexrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND i.indrelid=to_regclass(format('%I.%I',$1::text,$3::text))`, schema, name, table).Scan(&valid, &gotCols, &gotPred, &gotMethod)
	if e != nil {
		return fmt.Errorf("embedding index %s missing: %w", name, e)
	}
	normalize := func(s string) string { return strings.NewReplacer("(", "", ")", "", " ", "").Replace(s) }
	if !valid || gotCols != cols || normalize(gotPred) != normalize(pred) || gotMethod != method {
		return fmt.Errorf("embedding index %s altered", name)
	}
	return nil
}

func checkEmbeddingColumns(ctx context.Context, q hostedQuerier, schema string, tables []HostedTable) error {
	type column struct {
		Table string `json:"table_name"`
		Name  string `json:"column_name"`
		Type  string `json:"type_name"`
	}
	var columns []column
	add := func(table, typ, names string) {
		for name := range strings.FieldsSeq(names) {
			columns = append(columns, column{table, name, typ})
		}
	}
	add("hosted_embedding_sources", "text", "session_id")
	add("hosted_embedding_sources", "bigint", "revision")
	add("hosted_embedding_sources", "boolean", "dirty")
	add("hosted_embedding_generations", "bigint", "id")
	add("hosted_embedding_generations", "text", "instance_key recipe_fingerprint backfill_after_session_id")
	add("hosted_embedding_generations", "jsonb", "recipe_json")
	add("hosted_embedding_generations", "integer", "dimensions")
	add("hosted_embedding_generations", "boolean", "backfill_finished")
	add("hosted_embedding_state", "boolean", "active_valid")
	add("hosted_embedding_state", "bigint", "claim_after_generation_id")
	add("hosted_embedding_requirements", "bigint", "generation_id required_revision attempt_fence")
	add("hosted_embedding_requirements", "text", "session_id state error_code")
	add("hosted_embedding_requirements", "boolean", "eligible")
	add("hosted_embedding_requirements", "integer", "attempts")
	add("hosted_embedding_requirements", "timestamp with time zone", "available_at")
	add("hosted_embedding_documents", "bigint", "generation_id source_revision")
	add("hosted_embedding_documents", "text", "doc_key session_id kind source_uuid content content_hash")
	add("hosted_embedding_documents", "integer", "ordinal ordinal_end")
	add("hosted_embedding_documents", "boolean", "subordinate")
	add("hosted_embedding_documents", "jsonb", "offsets chunk_manifest")
	add("raw_embedding_outbox", "boolean", "embedding_consumed")
	for _, table := range tables[len(hostedEmbeddingTables):] {
		add(table.Name, "bigint", "generation_id")
		add(table.Name, "text", "input_hash")
		if strings.HasPrefix(table.Name, "hosted_embedding_chunks_g") {
			add(table.Name, "text", "doc_key")
			add(table.Name, "integer", "chunk_index")
		}
	}
	b, _ := json.Marshal(columns)
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT NOT EXISTS(SELECT 1 FROM jsonb_to_recordset($2::jsonb) AS e(table_name text,column_name text,type_name text) WHERE NOT EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=to_regclass(format('%I.%I',$1::text,e.table_name)) AND a.attname=e.column_name AND a.atttypid=to_regtype(e.type_name) AND a.attnotnull AND NOT a.attisdropped))`, schema, b).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return fmt.Errorf("hosted embedding column contract altered")
	}
	return nil
}
