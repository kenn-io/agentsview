package postgres

import (
	"context"
	"fmt"
)

// CheckWritable is the enabled worker's pre-readiness gate. Read-only status
// and active-generation clients do not require job mutation privileges.
func (s *HostedEmbeddingStore) CheckWritable(ctx context.Context) error {
	if e := checkEmbeddingVectorAccess(ctx, s.pg, s.schema); e != nil {
		return e
	}
	tables, e := embeddingTables(ctx, s.pg, s.schema)
	if e != nil {
		return e
	}
	for _, v := range tables {
		privileges := []string{"SELECT"}
		switch v.Name {
		case "hosted_embedding_generations":
		case "hosted_embedding_state":
			privileges = append(privileges, "UPDATE")
		default:
			privileges = append(privileges, "INSERT", "UPDATE", "DELETE")
		}
		for _, p := range privileges {
			var ok bool
			e = s.pg.QueryRowContext(ctx, `SELECT has_table_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),$3)`, s.schema, v.Name, p).Scan(&ok)
			if e != nil {
				return e
			}
			if !ok {
				return fmt.Errorf("hosted embedding worker lacks %s on %s", p, v.Name)
			}
		}
	}
	for _, v := range []struct{ table, column string }{{"hosted_embedding_generations", "backfill_after_session_id"}, {"hosted_embedding_generations", "backfill_finished"}, {"raw_embedding_outbox", "embedding_consumed"}, {"raw_corpus_state", "corpus_revision"}} {
		var ok bool
		e = s.pg.QueryRowContext(ctx, `SELECT has_column_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),$3,'UPDATE')`, s.schema, v.table, v.column).Scan(&ok)
		if e != nil {
			return e
		}
		if !ok {
			return fmt.Errorf("hosted embedding worker lacks column update on %s", v.table)
		}
	}
	return nil
}

func checkEmbeddingVectorAccess(ctx context.Context, q hostedQuerier, schema string) error {
	var extensionSchema string
	var usable bool
	if e := q.QueryRowContext(ctx, `SELECT n.nspname,has_schema_privilege(n.oid,'USAGE') FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='vector'`).Scan(&extensionSchema, &usable); e != nil {
		return e
	}
	if extensionSchema != "public" && extensionSchema != schema {
		return fmt.Errorf("hosted embeddings require pgvector in public or the bound schema")
	}
	if !usable {
		return fmt.Errorf("hosted embedding runtime requires USAGE on the supported pgvector schema")
	}
	return nil
}
