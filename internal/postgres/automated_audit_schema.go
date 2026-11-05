package postgres

import (
	"context"
	"database/sql"
)

const automationAuditDDL = `
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS automation_audit_hash TEXT;
CREATE INDEX IF NOT EXISTS idx_sessions_automation_audit ON sessions(automation_audit_hash, id);
CREATE OR REPLACE FUNCTION mark_session_automation_audit() RETURNS trigger LANGUAGE plpgsql AS $audit$
BEGIN
    NEW.automation_audit_hash := NULL;
    RETURN NEW;
END;
$audit$;
DROP TRIGGER IF EXISTS trg_sessions_automation_audit ON sessions;
CREATE TRIGGER trg_sessions_automation_audit
BEFORE UPDATE OF session_kind, first_message, user_message_count, is_automated, transcript_revision, prompt_evidence_discarded, updated_at ON sessions
FOR EACH ROW WHEN (
    NEW.automation_audit_hash IS NOT DISTINCT FROM OLD.automation_audit_hash
    AND ROW(NEW.session_kind, NEW.first_message, NEW.user_message_count, NEW.is_automated, NEW.transcript_revision, NEW.prompt_evidence_discarded, NEW.updated_at)
        IS DISTINCT FROM ROW(OLD.session_kind, OLD.first_message, OLD.user_message_count, OLD.is_automated, OLD.transcript_revision, OLD.prompt_evidence_discarded, OLD.updated_at)
) EXECUTE FUNCTION mark_session_automation_audit();
`

func automationAuditSchemaCurrentPG(ctx context.Context, q hostedQuerier) (bool, error) {
	var current bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (
        SELECT 1 FROM pg_attribute WHERE attrelid = 'sessions'::regclass
        AND attname = 'automation_audit_hash' AND NOT attisdropped
    ) AND EXISTS (
        SELECT 1 FROM pg_trigger WHERE tgrelid = 'sessions'::regclass
        AND tgname = 'trg_sessions_automation_audit' AND tgenabled = 'O'
    ) AND to_regclass('idx_sessions_automation_audit') IS NOT NULL`).Scan(&current)
	return current, err
}

func installAutomationAuditSchemaPG(ctx context.Context, tx *sql.Tx) error {
	if err := lockSyncMetadataRow(ctx, tx, "automation_audit_schema_lock"); err != nil {
		return err
	}
	current, err := automationAuditSchemaCurrentPG(ctx, tx)
	if err != nil || current {
		return err
	}
	_, err = tx.ExecContext(ctx, automationAuditDDL)
	return err
}

func prepareAutomationAuditPG(ctx context.Context, pg *sql.DB, column string) (bool, error) {
	if column != "xmin" {
		return false, nil
	}
	current, err := automationAuditSchemaCurrentPG(ctx, pg)
	if err != nil || current {
		return current, err
	}
	tx, err := pg.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := installAutomationAuditSchemaPG(ctx, tx); err != nil {
		if isInsufficientPrivilege(err) {
			return false, nil
		}
		return false, err
	}
	err = tx.Commit()
	return err == nil, err
}
