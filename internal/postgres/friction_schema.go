package postgres

import (
	"context"
	"database/sql"
)

const frictionDDL = `
CREATE TABLE IF NOT EXISTS friction_findings (
    id               BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    session_id       TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    kind             TEXT NOT NULL,
    detector         TEXT NOT NULL,
    message_ordinal  INTEGER,
    call_index       INTEGER,
    tool_name        TEXT NOT NULL DEFAULT '',
    label            TEXT NOT NULL DEFAULT '',
    text             TEXT NOT NULL DEFAULT '',
    evidence         TEXT NOT NULL DEFAULT '',
    title            TEXT NOT NULL,
    fingerprint      TEXT NOT NULL,
    occurred_at      TIMESTAMPTZ,
    seq              INTEGER NOT NULL,
    rules_version    TEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_friction_findings_session
    ON friction_findings (session_id);
`

var hostedFrictionTables = []HostedTable{
	{Name: "friction_findings", Key: []string{"id"}, ForeignKeys: sessionHostedFK()},
}

// installHostedFrictionUpgrade adds missing derived tables and repairs their
// ordinary indexes before the hosted catalog is checked.
func installHostedFrictionUpgrade(ctx context.Context, tx *sql.Tx, schema, tenant string) error {
	return installHostedDerivedUpgrade(ctx, tx, schema, tenant, frictionDDL, hostedFrictionTables)
}

// installHostedDerivedUpgrade runs a derived table's idempotent DDL and
// inventories any table the hosted schema did not have yet.
func installHostedDerivedUpgrade(ctx context.Context, tx *sql.Tx, schema, tenant, ddl string, tables []HostedTable) error {
	missing := make([]HostedTable, 0, len(tables))
	for _, table := range tables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT to_regclass(format('%I.%I',$1::text,$2::text)) IS NOT NULL`, schema, table.Name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			missing = append(missing, table)
		}
	}
	if _, err := tx.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if len(missing) > 0 {
		if err := InstallHostedTables(ctx, tx, schema, tenant, missing); err != nil {
			return err
		}
	}
	return nil
}
