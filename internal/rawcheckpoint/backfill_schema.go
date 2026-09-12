package rawcheckpoint

var versionNineMigrationStatements = []string{
	`CREATE TABLE backfill_runs (
 run_id TEXT PRIMARY KEY, device_id TEXT NOT NULL, destination TEXT NOT NULL,
 selection TEXT NOT NULL, discovery TEXT NOT NULL DEFAULT 'open',
 captured INTEGER NOT NULL DEFAULT 0, acknowledged INTEGER NOT NULL DEFAULT 0,
 invalidated INTEGER NOT NULL DEFAULT 0, watermark INTEGER NOT NULL DEFAULT 0,
 complete INTEGER NOT NULL DEFAULT 0, error_class TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE backfill_providers (
 run_id TEXT NOT NULL REFERENCES backfill_runs(run_id), provider TEXT NOT NULL,
 complete INTEGER NOT NULL DEFAULT 0, changed INTEGER NOT NULL DEFAULT 0,
 unsupported INTEGER NOT NULL DEFAULT 0, degraded INTEGER NOT NULL DEFAULT 0,
 error_class TEXT NOT NULL DEFAULT '', PRIMARY KEY(run_id, provider))`,
	`CREATE TABLE backfill_roots (
 run_id TEXT NOT NULL, provider TEXT NOT NULL, configured_root_id TEXT NOT NULL,
 local_root TEXT NOT NULL, PRIMARY KEY(run_id, provider, configured_root_id),
 FOREIGN KEY(run_id,provider) REFERENCES backfill_providers(run_id,provider))`,
	// Members intentionally have no FK to mutable outbox or source-head rows.
	`CREATE TABLE backfill_members (
 run_id TEXT NOT NULL REFERENCES backfill_runs(run_id), provider TEXT NOT NULL,
 configured_root_id TEXT NOT NULL, source_key TEXT NOT NULL, ordinal INTEGER NOT NULL,
 capture_id TEXT NOT NULL, status TEXT NOT NULL,
 manifest_id TEXT NOT NULL DEFAULT '', receipt TEXT NOT NULL DEFAULT '',
 generation INTEGER NOT NULL DEFAULT 0, error_class TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(run_id,provider,configured_root_id,source_key), UNIQUE(run_id,ordinal))`,
	`CREATE INDEX backfill_members_capture ON backfill_members(capture_id,status)`,
	`CREATE INDEX backfill_members_pending ON backfill_members(run_id,status,capture_id)`,
	`CREATE TRIGGER backfill_member_insert AFTER INSERT ON backfill_members BEGIN
 UPDATE backfill_runs SET captured = captured + 1,
 acknowledged = acknowledged + (NEW.status = 'acknowledged') WHERE run_id = NEW.run_id;
 END`,
	`CREATE TRIGGER backfill_member_status AFTER UPDATE OF status ON backfill_members BEGIN
 UPDATE backfill_runs SET acknowledged = acknowledged +
 (NEW.status = 'acknowledged') - (OLD.status = 'acknowledged'),
 invalidated = invalidated + (NEW.status = 'invalidated') - (OLD.status = 'invalidated')
 WHERE run_id = NEW.run_id;
 END`,
	// Covers recovery, rejection, cascaded predecessor loss, and device reset.
	// Acknowledgement has already copied its receipt before this deletion fires.
	`CREATE TRIGGER backfill_generation_deleted BEFORE DELETE ON outbox_generations BEGIN
 UPDATE backfill_members SET status = 'invalidated', error_class = 'capture_lost'
 WHERE capture_id = OLD.capture_id AND status = 'pending';
 END`,
}
