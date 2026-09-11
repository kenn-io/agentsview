package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	archivedb "go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawsync"
)

func validateHostedBinding(schema, tenant string) error {
	if len(schema) > 63 {
		return fmt.Errorf("hosted schema identifier exceeds PostgreSQL limit")
	}
	if _, err := quoteIdentifier(schema); err != nil {
		return err
	}
	identity, err := rawsync.NewAuthIdentity(tenant, "hosted-validation")
	if err != nil || identity.TenantID != tenant {
		return fmt.Errorf("hosted tenant must be nonblank and canonical")
	}
	return nil
}

// EnsureHostedTenant explicitly adopts a schema for exactly one tenant using a
// separate owner credential. Ordinary Open/EnsureSchema never opt into this.
// Provisioning must run before granting runtime access. Existing raw rows must
// already belong to the configured tenant; legacy derived rows are retained.
func EnsureHostedTenant(ctx context.Context, database *sql.DB, schema, tenant string) error {
	if database == nil {
		return fmt.Errorf("hosted provisioning requires a database")
	}
	if err := validateHostedBinding(schema, tenant); err != nil {
		return err
	}

	tx, err := database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Transaction-scoped locking uses one pool connection, including concurrent
	// provisioners and pools bounded to a single connection.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "agentsview-hosted:"+schema); err != nil {
		return err
	}
	quoted, _ := quoteIdentifier(schema)
	if _, err = tx.ExecContext(ctx, `SELECT set_config('search_path',$1,true),set_config('agentsview.tenant_id',$2,true),set_config('standard_conforming_strings','on',true)`, quoted, tenant); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT to_regclass(format('%I.hosted_tenant_binding',$1::text)) IS NOT NULL`, schema).Scan(&exists); err != nil {
		return err
	}
	if exists {
		if err = checkHostedBinding(ctx, tx, schema, tenant); err != nil {
			return err
		}
		if err = installRawProjectionUpgrade(ctx, tx, schema, tenant); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, hostedLegacyRevisionDDL); err != nil {
			return err
		}
		if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err = preflightHostedAdoption(ctx, tx, schema, tenant); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+quoted); err != nil {
		return err
	}
	for _, ddl := range []string{coreDDL, rawIngestDDL, rawIngestAppendOnlyDDL, postgresUsageJSONHelperDDL, rawProjectionDDL, rawLinksDDL} {
		if _, err = tx.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating hosted application schema: %w", err)
		}
	}
	if err = ensureRawProjectionJobColumns(ctx, tx); err != nil {
		return err
	}
	migrations := schemaColumnMigrations()
	columns, err := loadExistingColumns(ctx, tx, migrations)
	if err != nil {
		return err
	}
	if _, err = ensureColumns(ctx, tx, columns, migrations); err != nil {
		return err
	}
	var dataVersion int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(data_version),0) FROM sessions`).Scan(&dataVersion); err != nil {
		return err
	}
	if dataVersion > archivedb.CurrentDataVersion() {
		return &archivedb.DataVersionTooNewError{DatabaseVersion: dataVersion, BinaryVersion: archivedb.CurrentDataVersion()}
	}
	if err = createPartialIndexesPG(ctx, tx); err != nil {
		return err
	}

	if _, err = tx.ExecContext(ctx, vectorBaseDDL); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE hosted_tenant_binding(singleton SMALLINT PRIMARY KEY CHECK(singleton=1),tenant_id TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO hosted_tenant_binding(singleton,tenant_id) VALUES (1,$1)`, tenant); err != nil {
		return err
	}
	if err = checkHostedInventory(ctx, tx, schema); err != nil {
		return err
	}
	if err = InstallHostedTables(ctx, tx, schema, tenant, hostedTables); err != nil {
		return fmt.Errorf("installing hosted tenant constraints: %w", err)
	}
	if _, err = tx.ExecContext(ctx, rawProjectionIndexesDDL); err != nil {
		return err
	}
	// Statement trigger also fences DELETE/TRUNCATE, which a literal CHECK cannot.
	if _, err = tx.ExecContext(ctx, `CREATE FUNCTION hosted_reject_binding_mutation() RETURNS trigger LANGUAGE plpgsql AS $binding$ BEGIN RAISE EXCEPTION 'hosted tenant binding is immutable' USING ERRCODE='55000'; END; $binding$;
 CREATE TRIGGER hosted_binding_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON hosted_tenant_binding FOR EACH STATEMENT EXECUTE FUNCTION hosted_reject_binding_mutation()`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, hostedLegacyRevisionDDL); err != nil {
		return err
	}
	if err = checkHostedCatalog(ctx, tx, schema, tenant); err != nil {
		return err
	}
	return tx.Commit()
}

type hostedQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func checkHostedBinding(ctx context.Context, q hostedQuerier, schema, tenant string) error {
	qs, _ := quoteIdentifier(schema)
	var got string
	if err := q.QueryRowContext(ctx, `SELECT tenant_id FROM `+qs+`.hosted_tenant_binding WHERE singleton=1`).Scan(&got); err != nil {
		return fmt.Errorf("reading hosted tenant binding: %w", err)
	}
	if got != tenant {
		return fmt.Errorf("configured tenant does not match permanent schema binding")
	}
	return nil
}
func checkHostedInventory(ctx context.Context, q hostedQuerier, schema string) error {
	extra, err := embeddingTables(ctx, q, schema)
	if err != nil {
		return err
	}
	expected := make(map[string]bool, len(hostedTables)+len(extra))
	for _, t := range append(append([]HostedTable(nil), hostedTables...), extra...) {
		expected[t.Name] = true
	}
	rows, err := q.QueryContext(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m','f')`, schema)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			return err
		}
		if !expected[name] && strings.HasPrefix(name, "vector_") {
			return fmt.Errorf("hosted vector schema is unsupported")
		}
		if !expected[name] {
			return fmt.Errorf("hosted schema contains uninventoried relation %s", name)
		}
		delete(expected, name)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if len(expected) > 0 {
		return fmt.Errorf("hosted schema is missing required tables")
	}
	return nil
}

func checkHostedCatalog(ctx context.Context, q hostedQuerier, schema, tenant string) error {
	if err := checkHostedInventory(ctx, q, schema); err != nil {
		return err
	}
	extra, err := embeddingTables(ctx, q, schema)
	if err != nil {
		return err
	}
	for _, table := range append(append([]HostedTable(nil), hostedTables...), extra...) {
		var good bool
		err := q.QueryRowContext(ctx, `SELECT c.relkind='r' AND c.relrowsecurity AND c.relforcerowsecurity
   AND EXISTS(SELECT 1 FROM pg_attribute a WHERE a.attrelid=c.oid AND a.attname='tenant_id' AND a.attnotnull AND a.atttypid='text'::regtype)
   AND EXISTS(SELECT 1 FROM pg_constraint k WHERE k.conrelid=c.oid AND k.contype='p' AND array_to_string(ARRAY(SELECT a.attname FROM unnest(k.conkey) WITH ORDINALITY x(n,i) JOIN pg_attribute a ON a.attrelid=c.oid AND a.attnum=x.n ORDER BY x.i),',')=$3)
   AND EXISTS(SELECT 1 FROM pg_constraint k WHERE k.conrelid=c.oid AND k.contype='c' AND k.convalidated AND k.conname='hosted_tenant_check' AND pg_get_expr(k.conbin,k.conrelid)=format('(tenant_id = %L::text)',$4::text))
   AND (SELECT count(*) FROM pg_policy p WHERE p.polrelid=c.oid)=1
   AND EXISTS(SELECT 1 FROM pg_policy p WHERE p.polrelid=c.oid AND p.polname='hosted_tenant_policy' AND p.polcmd='*' AND p.polroles=ARRAY[0::oid] AND pg_get_expr(p.polqual,p.polrelid)=$5 AND pg_get_expr(p.polwithcheck,p.polrelid)=$5)
   FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relname=$2`, schema, table.Name, "tenant_id,"+strings.Join(table.Key, ","), tenant, `(tenant_id = current_setting('agentsview.tenant_id'::text, true))`).Scan(&good)
		if err != nil {
			return err
		}
		if !good {
			return fmt.Errorf("hosted protections missing or altered on %s", table.Name)
		}
		var count int
		if err = q.QueryRowContext(ctx, `SELECT count(*) FROM pg_constraint WHERE conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND contype='f'`, schema, table.Name).Scan(&count); err != nil {
			return err
		}
		if count != len(table.ForeignKeys) {
			return fmt.Errorf("hosted foreign key inventory mismatch on %s", table.Name)
		}
		for _, fk := range table.ForeignKeys {
			del, _ := hostedAction(fk.Delete)
			upd, _ := hostedAction(fk.Update)
			err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint c WHERE c.conrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND c.confrelid=to_regclass(format('%I.%I',$1::text,$3::text)) AND c.contype='f' AND c.convalidated AND c.confdeltype::text=$4 AND c.confupdtype::text=$5 AND array_to_string(ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(n,i) JOIN pg_attribute a ON a.attrelid=c.conrelid AND a.attnum=k.n ORDER BY k.i),',')=$6 AND array_to_string(ARRAY(SELECT a.attname FROM unnest(c.confkey) WITH ORDINALITY k(n,i) JOIN pg_attribute a ON a.attrelid=c.confrelid AND a.attnum=k.n ORDER BY k.i),',')=$7)`, schema, table.Name, fk.Table, hostedActionCode(del), hostedActionCode(upd), "tenant_id,"+strings.Join(fk.Columns, ","), "tenant_id,"+strings.Join(fk.References, ",")).Scan(&good)
			if err != nil {
				return err
			}
			if !good {
				return fmt.Errorf("hosted foreign key protection missing on %s", table.Name)
			}
		}
	}
	var immutable bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid=to_regclass(format('%I.hosted_tenant_binding',$1::text)) AND tgname='hosted_binding_immutable' AND tgenabled='O')`, schema).Scan(&immutable); err != nil {
		return err
	}
	if !immutable {
		return fmt.Errorf("hosted binding immutability trigger missing")
	}
	return checkEmbeddingCatalog(ctx, q, schema, tenant, extra)
}

// CheckHostedTenant is a read-only startup gate for a permanently bound pool.
// Runtime ownership, role switching, schema creation and sibling access are
// rejected; PostgreSQL RLS support is mandatory in hosted mode.
func CheckHostedTenant(ctx context.Context, database *sql.DB, schema, tenant string) error {
	if database == nil {
		return fmt.Errorf("hosted check requires a database")
	}
	if err := validateHostedBinding(schema, tenant); err != nil {
		return err
	}
	conn, err := database.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var currentSchema, currentTenant, searchPath string
	if err = conn.QueryRowContext(ctx, `SELECT COALESCE(current_schema(),''),COALESCE(current_setting('agentsview.tenant_id',true),''),current_setting('search_path')`).Scan(&currentSchema, &currentTenant, &searchPath); err != nil {
		return err
	}
	quoted, _ := quoteIdentifier(schema)
	if currentSchema != schema || currentTenant != tenant || searchPath != quoted+", pg_temp" {
		return fmt.Errorf("hosted connection schema or tenant context mismatch")
	}
	var unsafe bool
	if err = conn.QueryRowContext(ctx, `SELECT r.rolsuper OR r.rolbypassrls OR r.rolcreaterole OR r.rolcreatedb OR r.rolreplication OR current_user<>session_user
  OR EXISTS(SELECT 1 FROM pg_auth_members WHERE member=r.oid)
  OR has_database_privilege(current_database(),'CREATE')
  OR EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace WHERE p.prosecdef AND n.nspname !~ '^pg_' AND n.nspname<>'information_schema' AND has_function_privilege(p.oid,'EXECUTE'))
  OR EXISTS(SELECT 1 FROM pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema' AND (n.nspowner=r.oid OR has_schema_privilege(n.oid,'CREATE') OR (n.nspname<>$1 AND n.nspname<>'public' AND has_schema_privilege(n.oid,'USAGE'))))
  OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p') AND (has_table_privilege(c.oid,'TRUNCATE,TRIGGER,REFERENCES') OR has_any_column_privilege(c.oid,'REFERENCES')))
  OR EXISTS(SELECT 1 FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname !~ '^pg_' AND n.nspname<>'information_schema' AND c.relkind IN ('r','p','v','m','f','S') AND (c.relowner=r.oid OR (n.nspname<>$1 AND CASE WHEN c.relkind='S' THEN has_sequence_privilege(c.oid,'USAGE,SELECT,UPDATE') ELSE (has_table_privilege(c.oid,'SELECT,INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER') OR has_any_column_privilege(c.oid,'SELECT,INSERT,UPDATE,REFERENCES')) END)))
  FROM pg_roles r WHERE r.rolname=current_user`, schema).Scan(&unsafe); err != nil {
		return err
	}
	if unsafe {
		return fmt.Errorf("hosted runtime requires a restricted non-owner role without sibling access or DDL privileges")
	}
	if err = checkHostedBinding(ctx, conn, schema, tenant); err != nil {
		return err
	}
	return checkHostedCatalog(ctx, conn, schema, tenant)
}

func preflightHostedAdoption(ctx context.Context, q hostedQuerier, schema, tenant string) error {
	known := make(map[string]bool)
	for _, table := range hostedTables {
		known[table.Name] = true
	}
	rows, err := q.QueryContext(ctx, `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname=$1 AND c.relkind IN ('r','p','v','m','f')`, schema)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	qs, _ := quoteIdentifier(schema)
	for _, name := range names {
		if strings.HasPrefix(name, "vector_") && !known[name] {
			return fmt.Errorf("hosted vector schema is unsupported")
		}
		if !known[name] {
			return fmt.Errorf("hosted schema contains uninventoried relation %s", name)
		}
		if strings.HasPrefix(name, "raw_") {
			qt, _ := quoteIdentifier(name)
			var mismatch bool
			if err = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+qs+`.`+qt+` WHERE tenant_id IS DISTINCT FROM $1)`, tenant).Scan(&mismatch); err != nil {
				return err
			}
			if mismatch {
				return fmt.Errorf("existing raw rows do not match configured hosted tenant")
			}
		}
	}
	return nil
}
