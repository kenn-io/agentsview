package postgres

import (
	"context"
	cryptorand "crypto/rand"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

var migrationParityTables = []HostedTable{
	{Name: "migration_parity_identity", Key: []string{"singleton"}},
	{Name: "migration_parity_runs", Key: []string{"run_id"}},
	{Name: "migration_parity_sources", Key: []string{"run_id", "init_epoch", "source_id"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"run_id", "init_epoch"}, Table: "migration_parity_runs", References: []string{"run_id", "init_epoch"}, Delete: "RESTRICT"}}},
	{Name: "migration_parity_members", Key: []string{"run_id", "init_epoch", "source_id", "member_digest"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"run_id", "init_epoch", "source_id"}, Table: "migration_parity_sources", References: []string{"run_id", "init_epoch", "source_id"}, Delete: "RESTRICT"}}},
	{Name: "migration_parity_dependencies", Key: []string{"run_id", "init_epoch", "source_id", "kind", "key_digest"}, ForeignKeys: []HostedForeignKey{{Columns: []string{"run_id", "init_epoch"}, Table: "migration_parity_runs", References: []string{"run_id", "init_epoch"}, Delete: "RESTRICT"}}},
}

const migrationParityDDL = `
CREATE TABLE IF NOT EXISTS migration_parity_identity (
	 tenant_id TEXT NOT NULL DEFAULT current_setting('agentsview.tenant_id',true),
	 singleton SMALLINT CONSTRAINT migration_parity_identity_singleton_check CHECK(singleton=1),
	 target_id UUID NOT NULL
	,PRIMARY KEY(tenant_id,singleton)
);
CREATE TABLE IF NOT EXISTS migration_parity_runs (
	 tenant_id TEXT NOT NULL DEFAULT current_setting('agentsview.tenant_id',true),
	 run_id UUID NOT NULL,
 request BYTEA NOT NULL,
 request_digest BYTEA NOT NULL CONSTRAINT migration_parity_runs_request_digest_check CHECK(octet_length(request_digest)=32),
 binding BYTEA,
 binding_digest BYTEA CONSTRAINT migration_parity_runs_binding_digest_check CHECK(binding_digest IS NULL OR octet_length(binding_digest)=32),
 init_epoch BIGINT NOT NULL DEFAULT 1 CONSTRAINT migration_parity_runs_init_epoch_check CHECK(init_epoch>0),
 baseline_sealed BOOLEAN NOT NULL DEFAULT FALSE,
 inventory_digest BYTEA CONSTRAINT migration_parity_runs_inventory_digest_check CHECK(inventory_digest IS NULL OR octet_length(inventory_digest)=32),
 baseline_digest BYTEA CONSTRAINT migration_parity_runs_baseline_digest_check CHECK(baseline_digest IS NULL OR octet_length(baseline_digest)=32),
 state TEXT NOT NULL DEFAULT 'requested' CONSTRAINT migration_parity_runs_state_check CHECK(state IN ('requested','initializing','running','validating','complete')),
 batch_size INTEGER NOT NULL CONSTRAINT migration_parity_runs_batch_size_check CHECK(batch_size BETWEEN 1 AND 128),
 budget_sources INTEGER NOT NULL CONSTRAINT migration_parity_runs_budget_sources_check CHECK(budget_sources BETWEEN 0 AND 128),
 request_generation BIGINT NOT NULL DEFAULT 1 CONSTRAINT migration_parity_runs_request_generation_check CHECK(request_generation>0),
 completed_generation BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_runs_completed_generation_check CHECK(completed_generation>=0 AND completed_generation<=request_generation),
 validation_epoch BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_runs_validation_epoch_check CHECK(validation_epoch>=0),
 validation_state TEXT NOT NULL DEFAULT 'unchecked' CONSTRAINT migration_parity_runs_validation_state_check CHECK(validation_state IN ('unchecked','validating','checked','stale','incomplete')),
 validation_digest BYTEA CONSTRAINT migration_parity_runs_validation_digest_check CHECK(validation_digest IS NULL OR octet_length(validation_digest)=32),
 baseline_observed_at TIMESTAMPTZ,
 runtime_observed_at TIMESTAMPTZ,
 observed_at TIMESTAMPTZ,
 last_code TEXT NOT NULL DEFAULT 'pending' CONSTRAINT migration_parity_runs_code_check CHECK(last_code IN ('pending','profile_unavailable','baseline_unprovisioned','binding_conflict','snapshot_timeout','invalid','missing_history','missing_object','sandbox_unavailable','parse_failed','limit_exceeded','cleanup_failed','canceled','dependency_changed','historical_evidence','generation_not_available','exclusion_provenance_unavailable','internal')),
 lease_owner TEXT NOT NULL DEFAULT '',
 lease_token UUID,
	 lease_expires_at TIMESTAMPTZ,
	 PRIMARY KEY(tenant_id,run_id),
	 UNIQUE(tenant_id,run_id,init_epoch)
);
CREATE TABLE IF NOT EXISTS migration_parity_sources (
	 tenant_id TEXT NOT NULL DEFAULT current_setting('agentsview.tenant_id',true),
 run_id UUID NOT NULL,
 init_epoch BIGINT NOT NULL CONSTRAINT migration_parity_sources_init_epoch_check CHECK(init_epoch>0),
 source_id TEXT NOT NULL,
 source_kind TEXT NOT NULL CONSTRAINT migration_parity_sources_kind_check CHECK(source_kind IN ('raw','legacy')),
 device_id TEXT NOT NULL,
 provider TEXT NOT NULL,
 root_id TEXT NOT NULL,
 head_manifest TEXT NOT NULL,
 head_generation BIGINT NOT NULL CONSTRAINT migration_parity_sources_head_generation_check CHECK(head_generation>=0),
 head_receipt TEXT NOT NULL,
 source_key_sha256 TEXT NOT NULL DEFAULT '',
 dependency_digest BYTEA NOT NULL CONSTRAINT migration_parity_sources_dependency_digest_check CHECK(octet_length(dependency_digest)=32),
 captured_verdict TEXT CONSTRAINT migration_parity_sources_captured_verdict_check CHECK(captured_verdict IS NULL OR captured_verdict IN ('matched','mismatched','ambiguous','legacy_only','missing','partial_unsupported','stale')),
 captured_code TEXT NOT NULL DEFAULT 'pending' CONSTRAINT migration_parity_sources_captured_code_check CHECK(captured_code IN ('pending','profile_unavailable','baseline_unprovisioned','binding_conflict','snapshot_timeout','invalid','missing_history','missing_object','sandbox_unavailable','parse_failed','limit_exceeded','cleanup_failed','canceled','dependency_changed','historical_evidence','generation_not_available','exclusion_provenance_unavailable','internal')),
 verdict TEXT CONSTRAINT migration_parity_sources_verdict_check CHECK(verdict IS NULL OR verdict IN ('matched','mismatched','ambiguous','legacy_only','missing','partial_unsupported','stale')),
 code TEXT NOT NULL DEFAULT 'pending' CONSTRAINT migration_parity_sources_code_check CHECK(code IN ('pending','profile_unavailable','baseline_unprovisioned','binding_conflict','snapshot_timeout','invalid','missing_history','missing_object','sandbox_unavailable','parse_failed','limit_exceeded','cleanup_failed','canceled','dependency_changed','historical_evidence','generation_not_available','exclusion_provenance_unavailable','internal')),
 evidence_generation BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_sources_evidence_generation_check CHECK(evidence_generation>=0),
 validation_epoch BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_sources_validation_epoch_check CHECK(validation_epoch>=0),
 inventory_kind TEXT NOT NULL DEFAULT 'baseline' CONSTRAINT migration_parity_sources_inventory_kind_check CHECK(inventory_kind IN ('baseline','validation_delta')),
 candidate_complete BOOLEAN NOT NULL DEFAULT FALSE,
 required BOOLEAN NOT NULL DEFAULT TRUE,
	 PRIMARY KEY(tenant_id,run_id,init_epoch,source_id)
);
CREATE TABLE IF NOT EXISTS migration_parity_members (
	 tenant_id TEXT NOT NULL DEFAULT current_setting('agentsview.tenant_id',true),
 run_id UUID NOT NULL,
 init_epoch BIGINT NOT NULL CONSTRAINT migration_parity_members_init_epoch_check CHECK(init_epoch>0),
 source_id TEXT NOT NULL,
 member_key BYTEA NOT NULL,
 member_digest BYTEA NOT NULL CONSTRAINT migration_parity_members_member_digest_check CHECK(octet_length(member_digest)=32),
 kind TEXT NOT NULL CONSTRAINT migration_parity_members_kind_check CHECK(kind IN ('session','exclusion','legacy')),
 baseline_ref TEXT,
 required BOOLEAN NOT NULL DEFAULT TRUE,
 mapping_state TEXT NOT NULL CONSTRAINT migration_parity_members_mapping_state_check CHECK(mapping_state IN ('exact','ambiguous','legacy_only','missing','excluded')),
 baseline_fingerprint BYTEA,
 physical_fingerprint BYTEA CONSTRAINT migration_parity_members_physical_fingerprint_check CHECK(physical_fingerprint IS NULL OR octet_length(physical_fingerprint)=32),
 candidate_fingerprint BYTEA,
 captured_verdict TEXT CONSTRAINT migration_parity_members_captured_verdict_check CHECK(captured_verdict IS NULL OR captured_verdict IN ('matched','mismatched','ambiguous','legacy_only','missing','partial_unsupported','stale')),
 verdict TEXT CONSTRAINT migration_parity_members_verdict_check CHECK(verdict IS NULL OR verdict IN ('matched','mismatched','ambiguous','legacy_only','missing','partial_unsupported','stale')),
 different INTEGER NOT NULL DEFAULT 0 CONSTRAINT migration_parity_members_different_check CHECK(different BETWEEN 0 AND 255),
 validation_epoch BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_members_validation_epoch_check CHECK(validation_epoch>=0),
 inventory_kind TEXT NOT NULL DEFAULT 'baseline' CONSTRAINT migration_parity_members_inventory_kind_check CHECK(inventory_kind IN ('baseline','candidate_only','validation_delta')),
	 PRIMARY KEY(tenant_id,run_id,init_epoch,source_id,member_digest)
);
CREATE TABLE IF NOT EXISTS migration_parity_dependencies (
	 tenant_id TEXT NOT NULL DEFAULT current_setting('agentsview.tenant_id',true),
 run_id UUID NOT NULL,
 init_epoch BIGINT NOT NULL CONSTRAINT migration_parity_dependencies_init_epoch_check CHECK(init_epoch>0),
 source_id TEXT NOT NULL,
 kind TEXT NOT NULL CONSTRAINT migration_parity_dependencies_kind_check CHECK(kind IN ('head','manifest','projection','contribution','member','alias','relationship','overlay','census_delta')),
 dependency_key BYTEA NOT NULL,
 key_digest BYTEA NOT NULL CONSTRAINT migration_parity_dependencies_key_digest_check CHECK(octet_length(key_digest)=32),
 expected BYTEA NOT NULL CONSTRAINT migration_parity_dependencies_expected_check CHECK(octet_length(expected)=32),
 sequence BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_dependencies_sequence_check CHECK(sequence>=0),
 validation_epoch BIGINT NOT NULL DEFAULT 0 CONSTRAINT migration_parity_dependencies_validation_epoch_check CHECK(validation_epoch>=0),
 observed BYTEA CONSTRAINT migration_parity_dependencies_observed_check CHECK(observed IS NULL OR octet_length(observed)=32),
	 PRIMARY KEY(tenant_id,run_id,init_epoch,source_id,kind,key_digest)
);
CREATE INDEX IF NOT EXISTS migration_parity_runs_runnable ON migration_parity_runs(tenant_id,state,run_id) WHERE state IN ('requested','initializing','running','validating');
CREATE INDEX IF NOT EXISTS migration_parity_runs_expired ON migration_parity_runs(tenant_id,lease_expires_at,run_id) WHERE lease_token IS NOT NULL;
CREATE INDEX IF NOT EXISTS migration_parity_sources_pending ON migration_parity_sources(tenant_id,run_id,init_epoch,evidence_generation,source_id);
CREATE INDEX IF NOT EXISTS migration_parity_members_baseline_lookup ON migration_parity_members(tenant_id,run_id,init_epoch,baseline_ref);
CREATE INDEX IF NOT EXISTS migration_parity_dependencies_reverse ON migration_parity_dependencies(tenant_id,kind,key_digest,run_id,init_epoch,source_id);
CREATE INDEX IF NOT EXISTS migration_parity_dependencies_history ON migration_parity_dependencies(tenant_id,run_id,init_epoch,source_id,kind,sequence);
`

func installMigrationParityUpgrade(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	missing := make([]HostedTable, 0, len(migrationParityTables))
	for _, table := range migrationParityTables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass(format('%I.%I',$1::text,$2::text)) IS NOT NULL`, schema, table.Name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if _, err := tx.ExecContext(ctx, migrationParityDDL); err != nil {
		return fmt.Errorf("creating migration parity evidence schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE migration_parity_sources
		ADD COLUMN IF NOT EXISTS validation_epoch BIGINT NOT NULL DEFAULT 0,
		ADD COLUMN IF NOT EXISTS inventory_kind TEXT NOT NULL DEFAULT 'baseline',
		ADD COLUMN IF NOT EXISTS source_key_sha256 TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS captured_verdict TEXT DEFAULT 'stale',
		ADD COLUMN IF NOT EXISTS captured_code TEXT NOT NULL DEFAULT 'dependency_changed';
		ALTER TABLE migration_parity_sources ALTER COLUMN captured_verdict DROP DEFAULT,
		 ALTER COLUMN captured_code SET DEFAULT 'pending';
		ALTER TABLE migration_parity_members ADD COLUMN IF NOT EXISTS captured_verdict TEXT DEFAULT 'stale';
		ALTER TABLE migration_parity_members ALTER COLUMN captured_verdict DROP DEFAULT;
		UPDATE migration_parity_sources SET verdict='stale',code='dependency_changed' WHERE captured_verdict='stale';
		UPDATE migration_parity_members SET verdict='stale',different=0 WHERE captured_verdict='stale';
		DO $parity_upgrade$ BEGIN
		 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='migration_parity_sources'::regclass AND conname='migration_parity_sources_validation_epoch_check') THEN
		  ALTER TABLE migration_parity_sources ADD CONSTRAINT migration_parity_sources_validation_epoch_check CHECK(validation_epoch>=0);
		 END IF;
		 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='migration_parity_sources'::regclass AND conname='migration_parity_sources_inventory_kind_check') THEN
		  ALTER TABLE migration_parity_sources ADD CONSTRAINT migration_parity_sources_inventory_kind_check CHECK(inventory_kind IN ('baseline','validation_delta'));
		 END IF;
		 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='migration_parity_sources'::regclass AND conname='migration_parity_sources_captured_verdict_check') THEN
		  ALTER TABLE migration_parity_sources ADD CONSTRAINT migration_parity_sources_captured_verdict_check CHECK(captured_verdict IS NULL OR captured_verdict IN ('matched','mismatched','ambiguous','legacy_only','missing','partial_unsupported','stale'));
		 END IF;
		 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='migration_parity_sources'::regclass AND conname='migration_parity_sources_captured_code_check') THEN
		  ALTER TABLE migration_parity_sources ADD CONSTRAINT migration_parity_sources_captured_code_check CHECK(captured_code IN ('pending','profile_unavailable','baseline_unprovisioned','binding_conflict','snapshot_timeout','invalid','missing_history','missing_object','sandbox_unavailable','parse_failed','limit_exceeded','cleanup_failed','canceled','dependency_changed','historical_evidence','generation_not_available','exclusion_provenance_unavailable','internal'));
		 END IF;
		 IF NOT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='migration_parity_members'::regclass AND conname='migration_parity_members_captured_verdict_check') THEN
		  ALTER TABLE migration_parity_members ADD CONSTRAINT migration_parity_members_captured_verdict_check CHECK(captured_verdict IS NULL OR captured_verdict IN ('matched','mismatched','ambiguous','legacy_only','missing','partial_unsupported','stale'));
		 END IF;
		END $parity_upgrade$`); err != nil {
		return fmt.Errorf("upgrading migration parity source staging: %w", err)
	}
	if len(missing) > 0 {
		if err := InstallHostedTables(ctx, tx, schema, tenant, missing); err != nil {
			return err
		}
	}
	if err := ensureMigrationParityIdentity(ctx, tx); err != nil {
		return err
	}
	return ensureMigrationParityIdentityTrigger(ctx, tx)
}

func ensureMigrationParityIdentity(ctx context.Context, tx *sql.Tx) error {
	id, err := uuid.NewRandomFromReader(cryptorand.Reader)
	if err != nil {
		return fmt.Errorf("generating migration parity target identity: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO migration_parity_identity(singleton,target_id) VALUES(1,$1) ON CONFLICT(tenant_id,singleton) DO NOTHING`, id.String())
	return err
}

func ensureMigrationParityIdentityTrigger(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE OR REPLACE FUNCTION migration_parity_reject_identity_mutation() RETURNS trigger LANGUAGE plpgsql AS $identity$ BEGIN RAISE EXCEPTION 'migration parity identity is immutable' USING ERRCODE='55000'; END; $identity$;
DROP TRIGGER IF EXISTS migration_parity_identity_immutable ON migration_parity_identity;
CREATE TRIGGER migration_parity_identity_immutable BEFORE UPDATE OR DELETE OR TRUNCATE ON migration_parity_identity FOR EACH STATEMENT EXECUTE FUNCTION migration_parity_reject_identity_mutation()`)
	return err
}
