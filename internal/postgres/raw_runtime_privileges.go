package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/agentsview/internal/config"
)

// These are the grants used by Project, SettlePendingSignals, generation
// selection/rollout and supported raw/legacy curation. Cascading session deletes
// do not require DELETE on child payload tables. Row locks on raw_session_groups
// require UPDATE even though its immutable group identity is never rewritten.
var hostedProjectionPrivileges = []struct{ table, privileges string }{
	{"sessions", "SELECT,INSERT,UPDATE,DELETE"},
	{"messages", "SELECT,INSERT"}, {"tool_calls", "SELECT,INSERT"},
	{"tool_result_events", "SELECT,INSERT"}, {"usage_events", "SELECT,INSERT"},
	{"secret_findings", "SELECT,INSERT"}, {"excluded_sessions", "SELECT,INSERT"},
	{"session_aliases", "SELECT"}, {"starred_sessions", "SELECT,INSERT,DELETE"},
	{"pinned_messages", "SELECT,INSERT,UPDATE,DELETE"},
	{"raw_source_projections", "SELECT,INSERT,UPDATE"},
	{"raw_projection_generations", "SELECT,INSERT"},
	{"raw_session_groups", "SELECT,INSERT,UPDATE"},
	{"raw_content_revisions", "SELECT,INSERT,UPDATE"},
	{"raw_session_branches", "SELECT,INSERT,UPDATE"},
	{"session_sources", "SELECT,INSERT,UPDATE,DELETE"},
	{"raw_source_contributions", "SELECT,INSERT"},
	{"raw_session_public_aliases", "SELECT,INSERT"},
	{"raw_curation", "SELECT,INSERT,UPDATE,DELETE"},
	{"raw_pins", "SELECT,INSERT,UPDATE,DELETE"},
	{"raw_corpus_state", "SELECT,INSERT,UPDATE"},
	{"raw_embedding_outbox", "SELECT,INSERT"},
	{"raw_session_links", "SELECT,INSERT,DELETE"},
	{"raw_projection_rollouts", "SELECT,INSERT,UPDATE"},
}

// CheckHostedRuntimeWritable is a read-only preflight, never provisioning.
// The configured content policy determines whether the shared usage-only writer
// also needs to remove previously stored vector content.
func CheckHostedRuntimeWritable(ctx context.Context, database *sql.DB, schema string, archive config.ArchiveContent) error {
	writable, err := CanWriteRawSyncSchema(ctx, database, schema)
	if err != nil {
		return err
	}
	if !writable {
		return errors.New("hosted runtime requires writable tenant storage")
	}
	var readOnly string
	if err = database.QueryRowContext(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
		return err
	}
	if readOnly != "off" {
		return errors.New("hosted runtime requires writable tenant storage")
	}
	for _, grant := range hostedProjectionPrivileges {
		if err = checkHostedTablePrivileges(ctx, database, schema, grant.table, grant.privileges); err != nil {
			return err
		}
	}
	// nextval accepts USAGE or UPDATE. GENERATED AS IDENTITY does not require
	// sequence privileges; neither does a provisioned sequence-free ID default.
	for _, table := range []string{"tool_calls", "tool_result_events", "usage_events", "pinned_messages"} {
		var allowed bool
		err = database.QueryRowContext(ctx, `SELECT a.attidentity<>'' OR pg_get_serial_sequence(format('%I.%I',$1::text,$2::text),'id') IS NULL OR has_sequence_privilege(pg_get_serial_sequence(format('%I.%I',$1::text,$2::text),'id'),'USAGE,UPDATE') FROM pg_attribute a WHERE a.attrelid=to_regclass(format('%I.%I',$1::text,$2::text)) AND a.attname='id' AND NOT a.attisdropped`, schema, table).Scan(&allowed)
		if err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("hosted runtime sequence privileges for %s are incomplete; owner provisioning required", table)
		}
	}
	if archive.UsageOnly() {
		return checkHostedUsageVectorPrivileges(ctx, database, schema)
	}
	return nil
}

func checkHostedTablePrivileges(ctx context.Context, database *sql.DB, schema, table, privileges string) error {
	var allowed bool
	err := database.QueryRowContext(ctx, `SELECT bool_and(has_table_privilege(format('%I.%I',$1::text,$2::text),p)) FROM unnest(string_to_array($3::text,',')) p`, schema, table, privileges).Scan(&allowed)
	if err != nil {
		return err
	}
	if !allowed {
		return fmt.Errorf("hosted runtime projection privileges for %s are incomplete; owner provisioning required", table)
	}
	return nil
}

func checkHostedUsageVectorPrivileges(ctx context.Context, database *sql.DB, schema string) error {
	var exists bool
	if err := database.QueryRowContext(ctx, `SELECT to_regclass(format('%I.vector_documents',$1::text)) IS NOT NULL`, schema).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	for _, grant := range []struct{ table, privileges string }{{"vector_generations", "SELECT"}, {"vector_documents", "SELECT,DELETE"}, {"vector_push_state", "SELECT,DELETE"}} {
		if err := checkHostedTablePrivileges(ctx, database, schema, grant.table, grant.privileges); err != nil {
			return err
		}
	}
	// Match clearSessionVectorsTx's current-generation table selection exactly.
	rows, err := database.QueryContext(ctx, `SELECT 'vector_chunks_g'||id FROM vector_generations WHERE to_regclass(format('%I.%I',$1::text,'vector_chunks_g'||id)) IS NOT NULL`, schema)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var table string
		if err = rows.Scan(&table); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, table := range tables {
		if err = checkHostedTablePrivileges(ctx, database, schema, table, "SELECT,DELETE"); err != nil {
			return err
		}
	}
	return nil
}
