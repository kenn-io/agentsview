package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var migrationParityCheckDigests = map[string]string{
	"migration_parity_identity":     "2aa48a3d5982f1ed0034aa217d043a41f71a85877a097b1fd8602a3edccebb02",
	"migration_parity_runs":         "5384486b5c7d3e4dc93fd185fa1b358e84ea7f7fae3bba99456c63739b3f4d44",
	"migration_parity_sources":      "fa50ba2df8174375fd4ae65b5d64ce3d4d12827063ba214a046fec4b92791fc3",
	"migration_parity_members":      "62d4ec80669fcd6f25d2ee3b28094b00e1ec6e0bf10f4597eee53b73f1be389a",
	"migration_parity_dependencies": "04379c092535faab7a4ea8adad663b8a87bd527de05ca61bd19c6abb670c5147",
}

type migrationParityColumn struct {
	table, name, typ string
	nullable         bool
}

var migrationParityColumns = func() []migrationParityColumn {
	var out []migrationParityColumn
	add := func(table, typ string, nullable bool, names ...string) {
		for _, name := range names {
			out = append(out, migrationParityColumn{table, name, typ, nullable})
		}
	}
	for _, table := range migrationParityTables {
		add(table.Name, "text", false, "tenant_id")
	}
	add("migration_parity_identity", "smallint", false, "singleton")
	add("migration_parity_identity", "uuid", false, "target_id")
	add("migration_parity_runs", "uuid", false, "run_id")
	add("migration_parity_runs", "bytea", false, "request", "request_digest")
	add("migration_parity_runs", "bytea", true, "binding", "binding_digest", "inventory_digest", "baseline_digest", "validation_digest")
	add("migration_parity_runs", "bigint", false, "init_epoch", "request_generation", "completed_generation", "validation_epoch")
	add("migration_parity_runs", "boolean", false, "baseline_sealed")
	add("migration_parity_runs", "text", false, "state", "validation_state", "last_code", "lease_owner")
	add("migration_parity_runs", "integer", false, "batch_size", "budget_sources")
	add("migration_parity_runs", "timestamp with time zone", true, "baseline_observed_at", "runtime_observed_at", "observed_at", "lease_expires_at")
	add("migration_parity_runs", "uuid", true, "lease_token")
	add("migration_parity_sources", "uuid", false, "run_id")
	add("migration_parity_sources", "bigint", false, "init_epoch", "head_generation", "evidence_generation", "validation_epoch")
	add("migration_parity_sources", "text", false, "source_id", "source_kind", "device_id", "provider", "root_id", "head_manifest", "head_receipt", "source_key_sha256", "captured_code", "code", "inventory_kind")
	add("migration_parity_sources", "bytea", false, "dependency_digest")
	add("migration_parity_sources", "text", true, "captured_verdict", "verdict")
	add("migration_parity_sources", "boolean", false, "candidate_complete", "required")
	add("migration_parity_members", "uuid", false, "run_id")
	add("migration_parity_members", "bigint", false, "init_epoch", "validation_epoch")
	add("migration_parity_members", "text", false, "source_id", "kind", "mapping_state", "inventory_kind")
	add("migration_parity_members", "bytea", false, "member_key", "member_digest")
	add("migration_parity_members", "text", true, "baseline_ref", "captured_verdict", "verdict")
	add("migration_parity_members", "boolean", false, "required")
	add("migration_parity_members", "bytea", true, "baseline_fingerprint", "physical_fingerprint", "candidate_fingerprint")
	add("migration_parity_members", "integer", false, "different")
	add("migration_parity_dependencies", "uuid", false, "run_id")
	add("migration_parity_dependencies", "bigint", false, "init_epoch", "sequence", "validation_epoch")
	add("migration_parity_dependencies", "text", false, "source_id", "kind")
	add("migration_parity_dependencies", "bytea", false, "dependency_key", "key_digest", "expected")
	add("migration_parity_dependencies", "bytea", true, "observed")
	sort.Slice(out, func(i, j int) bool {
		if out[i].table == out[j].table {
			return out[i].name < out[j].name
		}
		return out[i].table < out[j].table
	})
	return out
}()

func checkMigrationParityCatalog(ctx context.Context, q hostedQuerier, schema string) error {
	rows, err := q.QueryContext(ctx, `SELECT c.relname,a.attname,format_type(a.atttypid,a.atttypmod),NOT a.attnotnull
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum>0 AND NOT a.attisdropped
WHERE n.nspname=$1 AND c.relkind IN ('r','p') AND c.relname LIKE 'migration_parity_%'
ORDER BY c.relname,a.attname`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	var got []migrationParityColumn
	for rows.Next() {
		var c migrationParityColumn
		if err = rows.Scan(&c.table, &c.name, &c.typ, &c.nullable); err != nil {
			return err
		}
		got = append(got, c)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(got) != len(migrationParityColumns) {
		expected := make(map[string]bool, len(migrationParityColumns))
		for _, c := range migrationParityColumns {
			expected[c.table+"."+c.name] = true
		}
		var unexpected []string
		for _, c := range got {
			if !expected[c.table+"."+c.name] {
				unexpected = append(unexpected, c.table+"."+c.name)
			}
		}
		return fmt.Errorf("migration parity column inventory mismatch: got %d columns, want %d (unexpected %s)", len(got), len(migrationParityColumns), strings.Join(unexpected, ","))
	}
	for i := range got {
		if got[i] != migrationParityColumns[i] {
			return fmt.Errorf("migration parity column contract altered on %s.%s", got[i].table, got[i].name)
		}
	}

	checks := map[string][]string{
		"migration_parity_identity":     {"migration_parity_identity_singleton_check", "hosted_tenant_check"},
		"migration_parity_runs":         {"migration_parity_runs_request_digest_check", "migration_parity_runs_binding_digest_check", "migration_parity_runs_init_epoch_check", "migration_parity_runs_inventory_digest_check", "migration_parity_runs_baseline_digest_check", "migration_parity_runs_state_check", "migration_parity_runs_batch_size_check", "migration_parity_runs_budget_sources_check", "migration_parity_runs_request_generation_check", "migration_parity_runs_completed_generation_check", "migration_parity_runs_validation_epoch_check", "migration_parity_runs_validation_state_check", "migration_parity_runs_validation_digest_check", "migration_parity_runs_code_check", "hosted_tenant_check"},
		"migration_parity_sources":      {"migration_parity_sources_init_epoch_check", "migration_parity_sources_kind_check", "migration_parity_sources_head_generation_check", "migration_parity_sources_dependency_digest_check", "migration_parity_sources_captured_verdict_check", "migration_parity_sources_captured_code_check", "migration_parity_sources_verdict_check", "migration_parity_sources_code_check", "migration_parity_sources_evidence_generation_check", "migration_parity_sources_validation_epoch_check", "migration_parity_sources_inventory_kind_check", "hosted_tenant_check"},
		"migration_parity_members":      {"migration_parity_members_init_epoch_check", "migration_parity_members_member_digest_check", "migration_parity_members_kind_check", "migration_parity_members_mapping_state_check", "migration_parity_members_physical_fingerprint_check", "migration_parity_members_captured_verdict_check", "migration_parity_members_verdict_check", "migration_parity_members_different_check", "migration_parity_members_validation_epoch_check", "migration_parity_members_inventory_kind_check", "hosted_tenant_check"},
		"migration_parity_dependencies": {"migration_parity_dependencies_init_epoch_check", "migration_parity_dependencies_kind_check", "migration_parity_dependencies_key_digest_check", "migration_parity_dependencies_expected_check", "migration_parity_dependencies_sequence_check", "migration_parity_dependencies_validation_epoch_check", "migration_parity_dependencies_observed_check", "hosted_tenant_check"},
	}
	for table, expected := range checks {
		rows, queryErr := q.QueryContext(ctx, `SELECT conname,pg_get_expr(conbin,conrelid) FROM pg_constraint WHERE conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND contype='c' AND convalidated ORDER BY conname`, schema, table)
		if queryErr != nil {
			return queryErr
		}
		var names []string
		digest := sha256.New()
		for rows.Next() {
			var name, expression string
			if err = rows.Scan(&name, &expression); err != nil {
				rows.Close()
				return err
			}
			names = append(names, name)
			if name != "hosted_tenant_check" {
				_, _ = digest.Write([]byte(name))
				_, _ = digest.Write([]byte{0})
				_, _ = digest.Write([]byte(expression))
				_, _ = digest.Write([]byte{0})
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		sort.Strings(expected)
		if strings.Join(names, ",") != strings.Join(expected, ",") {
			return fmt.Errorf("migration parity check inventory mismatch on %s", table)
		}
		gotDigest := hex.EncodeToString(digest.Sum(nil))
		if gotDigest != migrationParityCheckDigests[table] {
			return fmt.Errorf("migration parity check definitions altered on %s", table)
		}
	}
	for _, idx := range []struct{ name, table, cols, predicate string }{
		{"migration_parity_runs_runnable", "migration_parity_runs", "tenant_id,state,run_id", "state = ANY (ARRAY['requested'::text, 'initializing'::text, 'running'::text, 'validating'::text])"},
		{"migration_parity_runs_expired", "migration_parity_runs", "tenant_id,lease_expires_at,run_id", "lease_token IS NOT NULL"},
		{"migration_parity_sources_pending", "migration_parity_sources", "tenant_id,run_id,init_epoch,evidence_generation,source_id", ""},
		{"migration_parity_members_baseline_lookup", "migration_parity_members", "tenant_id,run_id,init_epoch,baseline_ref", ""},
		{"migration_parity_dependencies_reverse", "migration_parity_dependencies", "tenant_id,kind,key_digest,run_id,init_epoch,source_id", ""},
		{"migration_parity_dependencies_history", "migration_parity_dependencies", "tenant_id,run_id,init_epoch,source_id,kind,sequence", ""},
	} {
		if err = checkMigrationParityIndex(ctx, q, schema, idx.name, idx.table, idx.cols, idx.predicate); err != nil {
			return err
		}
	}
	var triggerType int
	var triggerEnabled string
	var triggerInternal, triggerConditional bool
	var triggerArgs int
	var functionSource, functionLanguage, functionVolatility, functionReturn string
	var functionSecurity, functionHasConfig bool
	if err = q.QueryRowContext(ctx, `SELECT t.tgtype::int,t.tgenabled::text,t.tgisinternal,t.tgqual IS NOT NULL,t.tgnargs,
		p.prosrc,l.lanname,p.provolatile::text,p.prorettype::regtype::text,p.prosecdef,p.proconfig IS NOT NULL
		FROM pg_trigger t JOIN pg_proc p ON p.oid=t.tgfoid JOIN pg_language l ON l.oid=p.prolang
		WHERE t.tgrelid=to_regclass(format('%I.migration_parity_identity',$1::text))
		AND t.tgname='migration_parity_identity_immutable' AND p.proname='migration_parity_reject_identity_mutation' AND p.pronargs=0`, schema).Scan(
		&triggerType, &triggerEnabled, &triggerInternal, &triggerConditional, &triggerArgs,
		&functionSource, &functionLanguage, &functionVolatility, &functionReturn, &functionSecurity, &functionHasConfig); err != nil {
		return fmt.Errorf("migration parity identity immutability trigger missing: %w", err)
	}
	if triggerType != 58 || triggerEnabled != "O" || triggerInternal || triggerConditional || triggerArgs != 0 {
		return fmt.Errorf("migration parity identity immutability trigger altered")
	}
	normalizeBody := func(body string) string { return strings.Join(strings.Fields(body), " ") }
	if functionLanguage != "plpgsql" || functionVolatility != "v" || functionReturn != "trigger" || functionSecurity || functionHasConfig ||
		normalizeBody(functionSource) != "BEGIN RAISE EXCEPTION 'migration parity identity is immutable' USING ERRCODE='55000'; END;" {
		return fmt.Errorf("migration parity identity immutability function altered")
	}
	return nil
}

func checkMigrationParityIndex(ctx context.Context, q hostedQuerier, schema, name, table, cols, predicate string) error {
	var gotCols, gotPredicate, method string
	var valid bool
	err := q.QueryRowContext(ctx, `SELECT i.indisvalid AND i.indisready AND i.indexprs IS NULL AND i.indnatts=i.indnkeyatts,
array_to_string(ARRAY(SELECT a.attname FROM unnest(i.indkey) WITH ORDINALITY k(n,p) JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=k.n ORDER BY k.p),','),
COALESCE(pg_get_expr(i.indpred,i.indrelid),''),m.amname
FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid JOIN pg_am m ON m.oid=c.relam
WHERE i.indexrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND i.indrelid=to_regclass(format('%I.%I',$1::text,$3::text))`, schema, name, table).Scan(&valid, &gotCols, &gotPredicate, &method)
	normalize := func(value string) string {
		return strings.NewReplacer("(", "", ")", "", " ", "").Replace(value)
	}
	if err != nil || !valid || gotCols != cols || normalize(gotPredicate) != normalize(predicate) || method != "btree" {
		return fmt.Errorf("migration parity index %s missing or altered", name)
	}
	return nil
}

var migrationParityBaselineTables = []string{
	"sessions", "messages", "usage_events", "starred_sessions", "excluded_sessions", "session_aliases", "pinned_messages",
	"source_archives", "source_project_identity_observations", "source_project_identity_observation_scopes",
	"source_session_project_identity_snapshots", "source_session_project_identity_snapshot_scopes", "source_worktree_project_mappings", "source_worktree_project_mapping_scopes",
	"tool_calls", "tool_result_events", "secret_findings", "raw_objects", "raw_manifests", "raw_manifest_entries", "raw_manifest_objects", "raw_source_heads",
	"raw_source_projections", "raw_projection_generations", "raw_session_groups", "raw_content_revisions", "raw_session_branches", "session_sources", "raw_source_contributions",
	"raw_session_public_aliases", "raw_curation", "raw_pins", "raw_corpus_state", "raw_session_links",
}

var migrationParityEvidenceTables = map[string]bool{
	"migration_parity_identity": true, "migration_parity_runs": true, "migration_parity_sources": true,
	"migration_parity_members": true, "migration_parity_dependencies": true,
}

func checkMigrationParityRuntimePrivileges(ctx context.Context, db *sql.DB, schema string) error {
	if err := checkParityNoDDL(ctx, db, schema); err != nil {
		return err
	}
	for _, grant := range []struct{ table, privileges string }{
		{"migration_parity_identity", "SELECT"},
		{"migration_parity_runs", "SELECT,INSERT,UPDATE"},
		{"migration_parity_sources", "SELECT,INSERT,UPDATE,DELETE"},
		{"migration_parity_members", "SELECT,INSERT,UPDATE,DELETE"},
		{"migration_parity_dependencies", "SELECT,INSERT,UPDATE,DELETE"},
	} {
		if err := checkHostedTablePrivileges(ctx, db, schema, grant.table, grant.privileges); err != nil {
			return fmt.Errorf("migration parity evidence privileges are incomplete: %w", err)
		}
	}
	return nil
}

func checkParityNoDDL(ctx context.Context, db *sql.DB, schema string) error {
	var unsafe bool
	err := db.QueryRowContext(ctx, `SELECT r.rolsuper OR r.rolbypassrls OR r.rolcreatedb OR r.rolcreaterole
 OR has_database_privilege(current_database(),'CREATE') OR has_schema_privilege(to_regnamespace($1),'CREATE')
 OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname LIKE 'migration_parity_%' AND (c.relowner=r.oid OR has_table_privilege(c.oid,'TRUNCATE,TRIGGER,REFERENCES') OR has_any_column_privilege(c.oid,'REFERENCES')))
 FROM pg_roles r WHERE r.rolname=current_user`, schema).Scan(&unsafe)
	if err != nil {
		return err
	}
	if unsafe {
		return fmt.Errorf("migration parity runtime requires a restricted non-owner role without DDL privileges")
	}
	return nil
}

func checkMigrationParityBaseline(ctx context.Context, db *sql.DB, options MigrationParityOptions, expectedIdentity string) error {
	if err := CheckHostedTenant(ctx, db, options.Schema, options.Tenant); err != nil {
		return fmt.Errorf("baseline_unprovisioned: %w", err)
	}
	if err := checkMigrationParityCatalog(ctx, db, options.Schema); err != nil {
		return fmt.Errorf("baseline_unprovisioned: %w", err)
	}
	var identity string
	if err := db.QueryRowContext(ctx, `SELECT target_id::text FROM migration_parity_identity WHERE singleton=1`).Scan(&identity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("baseline_unprovisioned: target identity mismatch")
		}
		return err
	}
	if identity != expectedIdentity {
		return fmt.Errorf("baseline_unprovisioned: target identity mismatch")
	}
	if err := checkParityNoDDL(ctx, db, options.Schema); err != nil {
		return err
	}
	for _, table := range migrationParityBaselineTables {
		var selected, mutated bool
		err := db.QueryRowContext(ctx, `SELECT has_table_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),'SELECT'),
			has_table_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),'INSERT,UPDATE,DELETE,TRUNCATE') OR
			has_any_column_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),'INSERT,UPDATE,REFERENCES')`, options.Schema, table).Scan(&selected, &mutated)
		if err != nil || !selected || mutated {
			return fmt.Errorf("migration parity baseline privileges are unsafe on %s", table)
		}
	}
	for _, table := range hostedTables {
		if migrationParityEvidenceTables[table.Name] {
			continue
		}
		var mutated bool
		err := db.QueryRowContext(ctx, `SELECT has_table_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),'INSERT,UPDATE,DELETE,TRUNCATE') OR
			has_any_column_privilege(to_regclass(format('%I.%I',$1::text,$2::text)),'INSERT,UPDATE,REFERENCES')`, options.Schema, table.Name).Scan(&mutated)
		if err != nil || mutated {
			return fmt.Errorf("migration parity baseline privileges are unsafe on %s", table.Name)
		}
	}
	return nil
}
