package postgres

const rawRolloutDDL = `CREATE TABLE IF NOT EXISTS raw_rollout_scopes (
 tenant_id TEXT NOT NULL,
 device_id TEXT NOT NULL DEFAULT '', provider TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL,
 parity JSONB, activated_corpus_revision BIGINT,
 updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
 updated_by TEXT NOT NULL DEFAULT current_user,
 PRIMARY KEY (tenant_id,device_id,provider)
)`
