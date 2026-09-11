package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// HostedTable describes a tenant-owned table. Keys omit tenant_id; the installer
// always prefixes it. Extend the inventory with projection tables when their
// schema lands. Unknown tables (including vector storage) fail closed.
type HostedTable struct {
	Name        string
	Key         []string
	ForeignKeys []HostedForeignKey
}

// HostedForeignKey describes the complete relationship, excluding tenant_id.
// Delete and Update must be one of the PostgreSQL referential actions below.
type HostedForeignKey struct {
	Columns    []string
	Table      string
	References []string
	Delete     string
	Update     string
}

var hostedTables = append([]HostedTable{
	{Name: "vector_generations", Key: []string{"id"}},
	{Name: "vector_generation_machines", Key: []string{"generation_id", "machine"}},
	{Name: "vector_documents", Key: []string{"doc_key"}},
	{Name: "vector_push_state", Key: []string{"generation_id", "session_id"}},
	{Name: "hosted_tenant_binding", Key: []string{"singleton"}},
	{Name: "sync_metadata", Key: []string{"key"}},
	{Name: "sessions", Key: []string{"id"}},
	{Name: "messages", Key: []string{"session_id", "ordinal"}, ForeignKeys: sessionHostedFK()},
	{Name: "usage_events", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
	{Name: "cursor_usage_events", Key: []string{"id"}},
	{Name: "starred_sessions", Key: []string{"session_id"}, ForeignKeys: sessionHostedFK()},
	{Name: "excluded_sessions", Key: []string{"id"}},
	{Name: "session_aliases", Key: []string{"session_id", "alias_id"}, ForeignKeys: sessionHostedFK()},
	{Name: "pinned_messages", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
	{Name: "tool_calls", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
	{Name: "tool_result_events", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
	{Name: "secret_findings", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
	{Name: "insights", Key: []string{"id"}},
	{Name: "model_pricing", Key: []string{"model_pattern"}},
	{Name: "model_pricing_bands", Key: []string{"model_pattern", "above_input_tokens"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"model_pattern"}, Table: "model_pricing", References: []string{"model_pattern"}, Delete: "CASCADE"}}},
	{Name: "genai_pricing", Key: []string{"singleton"}},
	{Name: "source_archives", Key: []string{"source_archive_id"}},
	{Name: "source_project_identity_observations", Key: []string{"source_archive_id", "project", "machine", "root_path", "git_remote"}},
	{Name: "source_project_identity_observation_scopes", Key: []string{"source_archive_id", "project", "machine", "root_path", "git_remote", "publication_scope"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"source_archive_id", "project", "machine", "root_path", "git_remote"}, Table: "source_project_identity_observations", References: []string{"source_archive_id", "project", "machine", "root_path", "git_remote"}, Delete: "CASCADE"}}},
	{Name: "source_session_project_identity_snapshots", Key: []string{"source_archive_id", "source_database_generation", "source_session_id"}},
	{Name: "source_session_project_identity_snapshot_scopes", Key: []string{"source_archive_id", "source_database_generation", "source_session_id", "publication_scope"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"source_archive_id", "source_database_generation", "source_session_id"}, Table: "source_session_project_identity_snapshots", References: []string{"source_archive_id", "source_database_generation", "source_session_id"}, Delete: "CASCADE"}}},
	{Name: "source_worktree_project_mappings", Key: []string{"source_archive_id", "machine", "path_prefix"}},
	{Name: "source_worktree_project_mapping_scopes", Key: []string{"source_archive_id", "machine", "path_prefix", "publication_scope"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"source_archive_id", "machine", "path_prefix"}, Table: "source_worktree_project_mappings", References: []string{"source_archive_id", "machine", "path_prefix"}, Delete: "CASCADE"}}},
	{Name: "raw_devices", Key: []string{"device_id"}},
	{Name: "raw_device_tokens", Key: []string{"token_sha256"}, ForeignKeys: deviceHostedFK()},
	{Name: "raw_upload_sessions", Key: []string{"upload_id"}, ForeignKeys: deviceHostedFK()},
	{Name: "raw_objects", Key: []string{"sha256"}},
	{Name: "raw_manifests", Key: []string{"manifest_id"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"device_id"}, Table: "raw_devices", References: []string{"device_id"}, Delete: "RESTRICT"},
		{Columns: []string{"device_id", "provider", "configured_root_id", "source_key_sha256"}, Table: "raw_source_heads", References: []string{"device_id", "provider", "configured_root_id", "source_key_sha256"}, Delete: "RESTRICT"},
	}},
	{Name: "raw_manifest_entries", Key: []string{"manifest_id", "entry_index"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"}}},
	{Name: "raw_manifest_objects", Key: []string{"manifest_id", "entry_index", "object_index"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"manifest_id", "entry_index"}, Table: "raw_manifest_entries", References: []string{"manifest_id", "entry_index"}, Delete: "RESTRICT"},
		{Columns: []string{"sha256", "size_bytes"}, Table: "raw_objects", References: []string{"sha256", "size_bytes"}, Delete: "RESTRICT"},
	}},
	{Name: "raw_source_heads", Key: []string{"device_id", "provider", "configured_root_id", "source_key_sha256"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"},
		{Columns: []string{"device_id"}, Table: "raw_devices", References: []string{"device_id"}, Delete: "RESTRICT"},
	}},
	{Name: "raw_ingest_jobs", Key: []string{"id"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"}}},
}, rawProjectionTables...)

func sessionHostedFK() []HostedForeignKey {
	return []HostedForeignKey{{Columns: []string{"session_id"}, Table: "sessions", References: []string{"id"}, Delete: "CASCADE"}}
}
func deviceHostedFK() []HostedForeignKey {
	return []HostedForeignKey{{Columns: []string{"device_id"}, Table: "raw_devices", References: []string{"device_id"}, Delete: "RESTRICT"}}
}
func hostedNames(names []string) (string, error) {
	quoted := make([]string, len(names))
	for i, name := range names {
		var err error
		quoted[i], err = quoteIdentifier(name)
		if err != nil {
			return "", err
		}
	}
	return strings.Join(quoted, ","), nil
}
func hostedKey(names []string) (string, error) {
	return hostedNames(append([]string{"tenant_id"}, names...))
}
func hostedAction(value string) (string, error) {
	switch value {
	case "":
		return "NO ACTION", nil
	case "NO ACTION", "RESTRICT", "CASCADE", "SET NULL", "SET DEFAULT":
		return value, nil
	}
	return "", fmt.Errorf("invalid hosted referential action")
}

// InstallHostedTables protects explicitly inventoried tables in an owner-run
// transaction. Call only after creating their ordinary DDL and binding the
// transaction's schema/tenant. It preserves rows and legacy unique targets,
// replaces actual FK constraints, and never rewrites arbitrary DDL text.
func InstallHostedTables(ctx context.Context, tx *sql.Tx, schema, tenant string, tables []HostedTable) error {
	if err := validateHostedBinding(schema, tenant); err != nil {
		return err
	}
	if err := checkHostedBinding(ctx, tx, schema, tenant); err != nil {
		return err
	}
	qs, _ := quoteIdentifier(schema)
	literal := "'" + strings.ReplaceAll(tenant, "'", "''") + "'"
	// Lock all participating tables before inspecting constraints or modifying rows.
	for _, table := range tables {
		qt, err := quoteIdentifier(table.Name)
		if err != nil {
			return err
		}
		if _, err = hostedKey(table.Key); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `LOCK TABLE `+qs+`.`+qt+` IN ACCESS EXCLUSIVE MODE`); err != nil {
			return err
		}
	}
	// Remove FKs first so old PK indexes can be replaced. Verify each existing
	// relationship against the inventory before removing it (including actions).
	for _, table := range tables {
		rows, err := tx.QueryContext(ctx, `SELECT c.conname, array_to_string(ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(n,i) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.n WHERE a.attname<>'tenant_id' ORDER BY k.i),','), r.relname, rn.nspname, array_to_string(ARRAY(SELECT a.attname FROM unnest(c.confkey) WITH ORDINALITY k(n,i) JOIN pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.n WHERE a.attname<>'tenant_id' ORDER BY k.i),','), c.confdeltype::text, c.confupdtype::text FROM pg_constraint c JOIN pg_class r ON r.oid=c.confrelid JOIN pg_namespace rn ON rn.oid=r.relnamespace WHERE c.conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND c.contype='f'`, schema, table.Name)
		if err != nil {
			return err
		}
		var drop []string
		for rows.Next() {
			var name, cols, ref, refSchema, refs, del, upd string
			if err = rows.Scan(&name, &cols, &ref, &refSchema, &refs, &del, &upd); err != nil {
				rows.Close()
				return err
			}
			found := false
			for _, fk := range table.ForeignKeys {
				d, _ := hostedAction(fk.Delete)
				u, _ := hostedAction(fk.Update)
				if refSchema == schema && cols == strings.Join(fk.Columns, ",") && ref == fk.Table && refs == strings.Join(fk.References, ",") && hostedActionCode(d) == del && hostedActionCode(u) == upd {
					found = true
					break
				}
			}
			if !found {
				rows.Close()
				return fmt.Errorf("uninventoried foreign key on %s", table.Name)
			}
			drop = append(drop, name)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		qt, _ := quoteIdentifier(table.Name)
		for _, name := range drop {
			qn, err := quoteIdentifier(name)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `ALTER TABLE `+qs+`.`+qt+` DROP CONSTRAINT `+qn); err != nil {
				return err
			}
		}
	}
	for _, table := range tables {
		qt, _ := quoteIdentifier(table.Name)
		target := qs + `.` + qt
		var hasTenant bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND attname='tenant_id' AND NOT attisdropped)`, schema, table.Name).Scan(&hasTenant); err != nil {
			return err
		}
		if !hasTenant {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+target+` ADD COLUMN tenant_id TEXT NOT NULL DEFAULT `+literal); err != nil {
				return err
			}
		}
		var bad bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+target+` WHERE tenant_id IS DISTINCT FROM $1)`, tenant).Scan(&bad); err != nil {
			return err
		}
		if bad {
			return fmt.Errorf("existing rows violate hosted tenant binding in %s", table.Name)
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+target+` ALTER COLUMN tenant_id SET NOT NULL, ALTER COLUMN tenant_id SET DEFAULT current_setting('agentsview.tenant_id',true)`); err != nil {
			return err
		}
		var pk, keys string
		if err := tx.QueryRowContext(ctx, `SELECT c.conname,array_to_string(ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(n,i) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.n ORDER BY k.i),',') FROM pg_constraint c WHERE c.conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND c.contype='p'`, schema, table.Name).Scan(&pk, &keys); err != nil {
			return err
		}
		expected := "tenant_id," + strings.Join(table.Key, ",")
		if keys != expected {
			if keys != strings.Join(table.Key, ",") {
				return fmt.Errorf("unexpected hosted primary key on %s", table.Name)
			}
			old, _ := hostedNames(table.Key)
			pkq, err := quoteIdentifier(pk)
			if err != nil {
				return err
			}
			// Existing ON CONFLICT clauses still target the original key.
			if _, err = tx.ExecContext(ctx, `ALTER TABLE `+target+` ADD UNIQUE (`+old+`), DROP CONSTRAINT `+pkq); err != nil {
				return err
			}
			key, _ := hostedKey(table.Key)
			if _, err = tx.ExecContext(ctx, `ALTER TABLE `+target+` ADD PRIMARY KEY (`+key+`)`); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+target+` ADD CONSTRAINT hosted_tenant_check CHECK (tenant_id = `+literal+`); ALTER TABLE `+target+` ENABLE ROW LEVEL SECURITY; ALTER TABLE `+target+` FORCE ROW LEVEL SECURITY; CREATE POLICY hosted_tenant_policy ON `+target+` USING (tenant_id = current_setting('agentsview.tenant_id',true)) WITH CHECK (tenant_id = current_setting('agentsview.tenant_id',true))`); err != nil {
			return err
		}
	}
	for _, table := range tables {
		qt, _ := quoteIdentifier(table.Name)
		for i, fk := range table.ForeignKeys {
			cols, err := hostedKey(fk.Columns)
			if err != nil {
				return err
			}
			refs, err := hostedKey(fk.References)
			if err != nil {
				return err
			}
			ref, err := quoteIdentifier(fk.Table)
			if err != nil {
				return err
			}
			del, err := hostedAction(fk.Delete)
			if err != nil {
				return err
			}
			upd, err := hostedAction(fk.Update)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s.%s ADD CONSTRAINT hosted_fk_%d FOREIGN KEY (%s) REFERENCES %s.%s (%s) ON DELETE %s ON UPDATE %s`, qs, qt, i, cols, qs, ref, refs, del, upd)); err != nil {
				return err
			}
		}
	}
	return nil
}
func hostedActionCode(action string) string {
	return map[string]string{"NO ACTION": "a", "RESTRICT": "r", "CASCADE": "c", "SET NULL": "n", "SET DEFAULT": "d"}[action]
}
