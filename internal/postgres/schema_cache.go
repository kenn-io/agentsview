package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
)

// ensuredSchemas maps each push target, keyed by its targetFingerprint, to
// the schema state it had after this process last prepared it. The daemon
// builds a new Sync for every push request, so the Sync's own schemaDone
// memo never outlives one push there; this lets those Syncs skip the schema
// step while nothing has changed the target's schema since, as one
// long-lived Sync already does.
var ensuredSchemas sync.Map

// pgSchemaStateSQL summarizes the push schema: every relation with its
// columns or index definition, constraints, triggers, functions, and the
// installed extensions. Object IDs are part of it, so a schema that was
// dropped and created again, or restored, differs even when its definitions
// match.
const pgSchemaStateSQL = `
WITH ns AS (SELECT oid FROM pg_namespace WHERE nspname = $1)
SELECT coalesce(md5(string_agg(item, E'\n' ORDER BY item)), '')
FROM (
	SELECT format('schema %s', oid) AS item FROM ns
	UNION ALL
	SELECT format('relation %s %s %s %s', c.oid, c.relname, c.relkind,
		CASE WHEN c.relkind = 'i' THEN pg_get_indexdef(c.oid) ELSE (
			SELECT string_agg(format('%s %s %s', a.attname,
				format_type(a.atttypid, a.atttypmod), a.attnotnull), ',' ORDER BY a.attnum)
			FROM pg_attribute a
			WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped
		) END)
	FROM pg_class c WHERE c.relnamespace = (SELECT oid FROM ns)
	UNION ALL
	SELECT format('constraint %s %s', con.conname, pg_get_constraintdef(con.oid))
	FROM pg_constraint con WHERE con.connamespace = (SELECT oid FROM ns)
	UNION ALL
	SELECT format('trigger %s %s %s', t.tgname, t.tgrelid, t.tgfoid)
	FROM pg_trigger t JOIN pg_class tc ON tc.oid = t.tgrelid
	WHERE tc.relnamespace = (SELECT oid FROM ns) AND NOT t.tgisinternal
	UNION ALL
	SELECT format('function %s(%s) %s', p.proname,
		pg_get_function_identity_arguments(p.oid), md5(p.prosrc))
	FROM pg_proc p WHERE p.pronamespace = (SELECT oid FROM ns)
	UNION ALL
	SELECT format('extension %s %s', e.extname, e.extversion) FROM pg_extension e
) items`

func readSchemaState(ctx context.Context, conn *sql.DB, schema string) (string, error) {
	var hash string
	if err := conn.QueryRowContext(ctx, pgSchemaStateSQL, schema).Scan(&hash); err != nil {
		return "", fmt.Errorf("reading pg schema state: %w", err)
	}
	return hash, nil
}

// schemaUnchangedSinceEnsuredLocked reports whether this process already
// prepared s's target and its schema has not changed since. A state that
// cannot be read counts as changed, so the full schema step runs and
// reports any real failure. Callers hold schemaMu.
func (s *Sync) schemaUnchangedSinceEnsuredLocked(ctx context.Context) bool {
	if s.targetFingerprint == "" {
		return false
	}
	recorded, ok := ensuredSchemas.Load(s.targetFingerprint)
	if !ok {
		return false
	}
	current, err := readSchemaState(ctx, s.pg, s.schema)
	return err == nil && current == recorded
}

// recordEnsuredSchemaLocked records s's target schema as prepared by this
// process. The schema step has already succeeded, so a state that cannot be
// read only leaves the next Sync to run the step again. Callers hold
// schemaMu.
func (s *Sync) recordEnsuredSchemaLocked(ctx context.Context) {
	if s.targetFingerprint == "" {
		return
	}
	state, err := readSchemaState(ctx, s.pg, s.schema)
	if err != nil {
		log.Printf("pg schema: the next push will check the schema again: %v", err)
		return
	}
	ensuredSchemas.Store(s.targetFingerprint, state)
}

// forgetEnsuredSchemaLocked makes the next schema check on s's target, in
// this Sync or a later one, run the full schema step. Callers hold schemaMu.
func (s *Sync) forgetEnsuredSchemaLocked() {
	s.schemaDone = false
	if s.targetFingerprint != "" {
		ensuredSchemas.Delete(s.targetFingerprint)
	}
}
