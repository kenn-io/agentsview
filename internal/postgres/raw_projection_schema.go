package postgres

import (
	"context"
	"database/sql"
)

// Projection metadata is durable independently of disposable physical payloads.
// Ordinary schema setup never creates these hosted-only relations.
const rawProjectionDDL = `
CREATE TABLE IF NOT EXISTS raw_projection_rollouts (
 run_id TEXT PRIMARY KEY, processing_version TEXT NOT NULL,
 device_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
 configured_root_id TEXT NOT NULL DEFAULT '', source_key_sha256 TEXT NOT NULL DEFAULT '',
 complete BOOLEAN NOT NULL DEFAULT false
);

CREATE TABLE IF NOT EXISTS raw_source_projections (
 source_id TEXT PRIMARY KEY, device_id TEXT NOT NULL, provider TEXT NOT NULL,
 configured_root_id TEXT NOT NULL, source_key_sha256 TEXT NOT NULL,
 selected_manifest_id TEXT NOT NULL, processing_version TEXT NOT NULL,
 projection_generation BIGINT NOT NULL CHECK(projection_generation>0),
 selected_job_id BIGINT NOT NULL, successful_manifest_id TEXT, last_attempt_manifest_id TEXT,
 membership_complete BOOLEAN NOT NULL DEFAULT false, diagnostics TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS raw_projection_generations (
 source_id TEXT NOT NULL, generation BIGINT NOT NULL, manifest_id TEXT NOT NULL,
 processing_version TEXT NOT NULL, PRIMARY KEY(source_id,generation)
);
CREATE TABLE IF NOT EXISTS raw_session_groups (
 group_id TEXT PRIMARY KEY, provider TEXT NOT NULL, logical_key TEXT NOT NULL,
 base_alias TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS raw_content_revisions (
 session_id TEXT PRIMARY KEY, group_id TEXT NOT NULL, content_revision TEXT NOT NULL,
 payload BYTEA NOT NULL, recency_state JSONB NOT NULL DEFAULT '{}', UNIQUE(group_id,content_revision)
);
CREATE TABLE IF NOT EXISTS raw_session_branches (
 branch_id TEXT PRIMARY KEY, source_id TEXT NOT NULL, group_id TEXT NOT NULL,
 member_id TEXT NOT NULL, session_id TEXT NOT NULL, content_revision TEXT NOT NULL,
 manifest_id TEXT NOT NULL, processing_version TEXT NOT NULL,
 projection_generation BIGINT NOT NULL, active BOOLEAN NOT NULL,
 prior_payload BYTEA NOT NULL, UNIQUE(source_id,group_id)
);
CREATE INDEX IF NOT EXISTS raw_branches_group ON raw_session_branches(group_id,active,session_id);
CREATE INDEX IF NOT EXISTS raw_branches_source ON raw_session_branches(source_id);
CREATE TABLE IF NOT EXISTS session_sources (
 branch_id TEXT PRIMARY KEY, group_id TEXT NOT NULL, source_id TEXT NOT NULL,
 session_id TEXT NOT NULL, physical_session_id TEXT, manifest_id TEXT NOT NULL, content_revision TEXT NOT NULL,
 processing_version TEXT NOT NULL, projection_generation BIGINT NOT NULL
);
CREATE TABLE IF NOT EXISTS raw_source_contributions (
 branch_id TEXT NOT NULL, manifest_id TEXT NOT NULL, projection_generation BIGINT NOT NULL, processing_version TEXT NOT NULL, prior_contributed BOOLEAN NOT NULL, payload BYTEA NOT NULL,
 PRIMARY KEY(branch_id,manifest_id,projection_generation)
);
CREATE TABLE IF NOT EXISTS raw_session_public_aliases (
 alias_id TEXT NOT NULL, group_id TEXT NOT NULL, anchor_branch TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(alias_id,group_id)
);
CREATE TABLE IF NOT EXISTS raw_curation (
 group_id TEXT NOT NULL, branch_id TEXT NOT NULL DEFAULT '', field TEXT NOT NULL,
 value JSONB NOT NULL, PRIMARY KEY(group_id,branch_id,field)
);
CREATE TABLE IF NOT EXISTS raw_pins (
 group_id TEXT NOT NULL, branch_id TEXT NOT NULL DEFAULT '', message_key TEXT NOT NULL,
 ordinal INTEGER NOT NULL, content_revision TEXT NOT NULL, pinned BOOLEAN NOT NULL,
 note TEXT NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(group_id,branch_id,message_key)
);
CREATE TABLE IF NOT EXISTS raw_corpus_state (
 singleton SMALLINT PRIMARY KEY CHECK(singleton=1), corpus_revision BIGINT NOT NULL DEFAULT 0,
 identity_revision BIGINT NOT NULL DEFAULT 0, selection_revision BIGINT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS raw_embedding_outbox (
 session_id TEXT NOT NULL, selection_revision BIGINT NOT NULL, corpus_revision BIGINT NOT NULL, content_revision TEXT NOT NULL,
 action TEXT NOT NULL CHECK(action IN ('reconcile','remove')),
 PRIMARY KEY(session_id,selection_revision,corpus_revision)
);
`

const rawProjectionIndexesDDL = `
CREATE INDEX IF NOT EXISTS raw_selected_job ON raw_source_projections(tenant_id,selected_job_id);
CREATE INDEX IF NOT EXISTS raw_pending_signals ON sessions(tenant_id,ended_at,id) WHERE provenance_kind='raw' AND signals_pending_since IS NOT NULL;
CREATE INDEX IF NOT EXISTS raw_hosted_jobs_due ON raw_ingest_jobs(tenant_id,processing_version,available_at,id) WHERE stage='parse' AND projection_selected AND projection_generation>0 AND state IN ('ready','retrying');
CREATE INDEX IF NOT EXISTS raw_hosted_jobs_expired ON raw_ingest_jobs(tenant_id,processing_version,lease_expires_at,id) WHERE stage='parse' AND projection_selected AND projection_generation>0 AND state='leased';

CREATE INDEX IF NOT EXISTS raw_branches_session ON raw_session_branches(tenant_id,session_id,active);
CREATE INDEX IF NOT EXISTS raw_links_target ON raw_session_links(tenant_id,target_alias);
CREATE INDEX IF NOT EXISTS raw_alias_group ON raw_session_public_aliases(tenant_id,group_id);
CREATE INDEX IF NOT EXISTS raw_session_sources_source ON session_sources(tenant_id,source_id);
CREATE INDEX IF NOT EXISTS raw_session_sources_content ON session_sources(tenant_id,session_id);
CREATE INDEX IF NOT EXISTS raw_session_sources_physical ON session_sources(tenant_id,physical_session_id);
`

var rawProjectionTables = []HostedTable{
	{Name: "raw_projection_rollouts", Key: []string{"run_id"}},
	{Name: "raw_session_links", Key: []string{"branch_id", "kind", "ordinal", "call_index", "event_index"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"branch_id"}, Table: "raw_session_branches", References: []string{"branch_id"}, Delete: "RESTRICT"}}},
	{Name: "raw_source_projections", Key: []string{"source_id"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"device_id", "provider", "configured_root_id", "source_key_sha256"}, Table: "raw_source_heads", References: []string{"device_id", "provider", "configured_root_id", "source_key_sha256"}, Delete: "RESTRICT"},
		{Columns: []string{"selected_manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"},
		{Columns: []string{"selected_job_id"}, Table: "raw_ingest_jobs", References: []string{"id"}, Delete: "RESTRICT"},
		{Columns: []string{"successful_manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"},
		{Columns: []string{"last_attempt_manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"},
	}},
	{Name: "raw_projection_generations", Key: []string{"source_id", "generation"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"source_id"}, Table: "raw_source_projections", References: []string{"source_id"}, Delete: "RESTRICT"},
		{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"},
	}},
	{Name: "raw_session_groups", Key: []string{"group_id"}},
	{Name: "raw_content_revisions", Key: []string{"session_id"}, ForeignKeys: rawGroupFK()},
	{Name: "raw_session_branches", Key: []string{"branch_id"}, ForeignKeys: append(rawGroupFK(),
		HostedForeignKey{Columns: []string{"source_id"}, Table: "raw_source_projections", References: []string{"source_id"}, Delete: "RESTRICT"},
		HostedForeignKey{Columns: []string{"session_id"}, Table: "raw_content_revisions", References: []string{"session_id"}, Delete: "RESTRICT"},
		HostedForeignKey{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"})},
	{Name: "session_sources", Key: []string{"branch_id"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"physical_session_id"}, Table: "sessions", References: []string{"id"}, Delete: "RESTRICT"},
		{Columns: []string{"session_id"}, Table: "raw_content_revisions", References: []string{"session_id"}, Delete: "RESTRICT"},
		HostedForeignKey{Columns: []string{"branch_id"}, Table: "raw_session_branches", References: []string{"branch_id"}, Delete: "RESTRICT"},
		HostedForeignKey{Columns: []string{"group_id"}, Table: "raw_session_groups", References: []string{"group_id"}, Delete: "RESTRICT"},
		HostedForeignKey{Columns: []string{"source_id"}, Table: "raw_source_projections", References: []string{"source_id"}, Delete: "RESTRICT"},
		HostedForeignKey{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"}}},
	{Name: "raw_source_contributions", Key: []string{"branch_id", "manifest_id", "projection_generation"}, ForeignKeys: []HostedForeignKey{
		{Columns: []string{"branch_id"}, Table: "raw_session_branches", References: []string{"branch_id"}, Delete: "RESTRICT"},
		{Columns: []string{"manifest_id"}, Table: "raw_manifests", References: []string{"manifest_id"}, Delete: "RESTRICT"},
	}},
	{Name: "raw_session_public_aliases", Key: []string{"alias_id", "group_id"}, ForeignKeys: rawGroupFK()},
	{Name: "raw_curation", Key: []string{"group_id", "branch_id", "field"}, ForeignKeys: rawGroupFK()},
	{Name: "raw_pins", Key: []string{"group_id", "branch_id", "message_key"}, ForeignKeys: rawGroupFK()},
	{Name: "raw_corpus_state", Key: []string{"singleton"}},
	{Name: "raw_embedding_outbox", Key: []string{"session_id", "selection_revision", "corpus_revision"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"session_id"}, Table: "raw_content_revisions", References: []string{"session_id"}, Delete: "RESTRICT"}}},
}

func rawGroupFK() []HostedForeignKey {
	return []HostedForeignKey{{Columns: []string{"group_id"}, Table: "raw_session_groups", References: []string{"group_id"}, Delete: "RESTRICT"}}
}

// installRawProjectionUpgrade adds only missing projection tables to an already
// bound tenant schema. The complete catalog check that follows still rejects
// tampered protections or unknown tables; failures roll back the entire upgrade.
func installRawProjectionUpgrade(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	var missing []HostedTable
	for _, table := range rawProjectionTables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass(format('%I.%I',$1::text,$2::text)) IS NOT NULL`, schema, table.Name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if len(missing) > 0 {
		if _, err := tx.ExecContext(ctx, rawProjectionDDL+rawLinksDDL); err != nil {
			return err
		}
		if err := InstallHostedTables(ctx, tx, schema, tenant, missing); err != nil {
			return err
		}
	}
	// Job generation is a shared raw-schema migration; projection does not change
	// custody data or assign a generation to legacy jobs implicitly.
	if err := ensureRawProjectionJobColumns(ctx, tx); err != nil {
		return err
	}
	migrations := append(schemaColumnMigrations(), columnMigration{"raw_pins", "created_at", `created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()`, "adding durable raw pin creation time"}, columnMigration{"raw_content_revisions", "recency_state", `recency_state JSONB NOT NULL DEFAULT '{}'`, "adding raw content recency state"})
	columns, err := loadExistingColumns(ctx, tx, migrations)
	if err != nil {
		return err
	}
	_, err = ensureColumns(ctx, tx, columns, migrations)
	if err != nil {
		return err
	}
	if err := createPartialIndexesPG(ctx, tx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, rawProjectionIndexesDDL)
	return err
}
