package rawcheckpoint

// currentSchema creates fresh stores without replaying historical migrations.
const currentSchema = `
CREATE TABLE device_config (
		id INTEGER PRIMARY KEY,
		device_id TEXT NOT NULL,
		created_at TEXT NOT NULL
	);

CREATE TABLE raw_sources (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		head_manifest_id TEXT NOT NULL DEFAULT '',
		head_receipt TEXT NOT NULL DEFAULT '',
		head_generation INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		latest_capture_id TEXT NOT NULL DEFAULT '',
		head_capture_id TEXT NOT NULL DEFAULT '',
		observation_revision INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, configured_root_id, source_key)
	);

CREATE TABLE outbox_config (
		id INTEGER PRIMARY KEY,
		spool_path TEXT NOT NULL,
		max_outbox_bytes INTEGER NOT NULL DEFAULT 1073741824,
		destination TEXT NOT NULL DEFAULT ''
	);

CREATE TABLE configured_roots (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		local_root TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE (provider, local_root)
	);

CREATE TABLE outbox_reservations (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		reserved_bytes INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id) ON DELETE CASCADE
	);

CREATE TABLE outbox_objects (
		sha256 TEXT NOT NULL,
		length INTEGER NOT NULL,
		spool_name TEXT NOT NULL,
		ref_count INTEGER NOT NULL,
		state TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (sha256, length)
	);

CREATE TABLE outbox_generations (
		capture_id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		predecessor_capture_id TEXT,
		captured_at TEXT NOT NULL,
		kind TEXT NOT NULL,
		state TEXT NOT NULL,
		expected_parent_receipt TEXT NOT NULL DEFAULT '',
		manifest_id TEXT NOT NULL DEFAULT '',
		ack_receipt TEXT NOT NULL DEFAULT '',
		ack_generation INTEGER NOT NULL DEFAULT 0,
		metadata_bytes INTEGER NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		retry_at TEXT NOT NULL DEFAULT '',
		error_class TEXT NOT NULL DEFAULT '',
		blocked INTEGER NOT NULL DEFAULT 0,
		attempt_count INTEGER NOT NULL DEFAULT 0,
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id),
		FOREIGN KEY (predecessor_capture_id) REFERENCES outbox_generations(capture_id) ON DELETE CASCADE,
		UNIQUE (provider, configured_root_id, source_key, capture_id)
	);

CREATE TABLE outbox_entries (
		capture_id TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL,
		path TEXT NOT NULL,
		length INTEGER NOT NULL,
		mod_time_ns INTEGER NOT NULL,
		file_identity TEXT NOT NULL,
		prefix_sha256 TEXT NOT NULL,
		appendable INTEGER NOT NULL,
		PRIMARY KEY (capture_id, entry_ordinal),
		UNIQUE (capture_id, path),
		FOREIGN KEY (capture_id) REFERENCES outbox_generations(capture_id) ON DELETE CASCADE
	);

CREATE TABLE outbox_entry_objects (
		capture_id TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL,
		object_ordinal INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		length INTEGER NOT NULL,
		PRIMARY KEY (capture_id, entry_ordinal, object_ordinal),
		FOREIGN KEY (capture_id, entry_ordinal)
			REFERENCES outbox_entries(capture_id, entry_ordinal) ON DELETE CASCADE,
		FOREIGN KEY (sha256, length)
			REFERENCES outbox_objects(sha256, length)
	);

CREATE TABLE raw_source_base_entries (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL,
		path TEXT NOT NULL,
		length INTEGER NOT NULL,
		mod_time_ns INTEGER NOT NULL,
		file_identity TEXT NOT NULL,
		prefix_sha256 TEXT NOT NULL,
		appendable INTEGER NOT NULL,
		PRIMARY KEY (provider, configured_root_id, source_key, entry_ordinal),
		UNIQUE (provider, configured_root_id, source_key, path),
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id)
	);

CREATE TABLE raw_source_base_objects (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		entry_ordinal INTEGER NOT NULL,
		object_ordinal INTEGER NOT NULL,
		sha256 TEXT NOT NULL,
		length INTEGER NOT NULL,
		PRIMARY KEY (
			provider, configured_root_id, source_key, entry_ordinal, object_ordinal
		),
		FOREIGN KEY (provider, configured_root_id, source_key, entry_ordinal)
			REFERENCES raw_source_base_entries(
				provider, configured_root_id, source_key, entry_ordinal
			) ON DELETE CASCADE
	);

CREATE TABLE raw_coverage (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		state TEXT NOT NULL,
		reason TEXT NOT NULL DEFAULT '',
		degraded_at TEXT,
		recovered_at TEXT,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (provider, configured_root_id),
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id)
	);

CREATE TABLE raw_coverage_failures (
		provider TEXT NOT NULL,
		configured_root_id TEXT NOT NULL,
		source_key TEXT NOT NULL,
		reason TEXT NOT NULL,
		degraded_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (provider, configured_root_id, source_key),
		FOREIGN KEY (configured_root_id) REFERENCES configured_roots(id) ON DELETE CASCADE
	);

CREATE INDEX outbox_generations_ready_idx
		ON outbox_generations(state, captured_at, capture_id);

CREATE INDEX outbox_generations_source_idx
		ON outbox_generations(provider, configured_root_id, source_key, captured_at);

CREATE INDEX outbox_objects_state_idx ON outbox_objects(state);

CREATE INDEX outbox_generations_due_idx
		ON outbox_generations(blocked,
		retry_at, state, captured_at, capture_id);

CREATE TABLE backfill_runs (
 run_id TEXT PRIMARY KEY, device_id TEXT NOT NULL, destination TEXT NOT NULL,
 selection TEXT NOT NULL, discovery TEXT NOT NULL DEFAULT 'open',
 captured INTEGER NOT NULL DEFAULT 0, acknowledged INTEGER NOT NULL DEFAULT 0,
 invalidated INTEGER NOT NULL DEFAULT 0, watermark INTEGER NOT NULL DEFAULT 0,
 complete INTEGER NOT NULL DEFAULT 0, error_class TEXT NOT NULL DEFAULT '');

CREATE TABLE backfill_providers (
 run_id TEXT NOT NULL REFERENCES backfill_runs(run_id), provider TEXT NOT NULL,
 complete INTEGER NOT NULL DEFAULT 0, changed INTEGER NOT NULL DEFAULT 0,
 unsupported INTEGER NOT NULL DEFAULT 0, degraded INTEGER NOT NULL DEFAULT 0,
 error_class TEXT NOT NULL DEFAULT '', PRIMARY KEY(run_id, provider));

CREATE TABLE backfill_roots (
 run_id TEXT NOT NULL, provider TEXT NOT NULL, configured_root_id TEXT NOT NULL,
 local_root TEXT NOT NULL, project_path TEXT NOT NULL DEFAULT '', ordinal INTEGER NOT NULL, PRIMARY KEY(run_id, provider, configured_root_id),
 FOREIGN KEY(run_id,provider) REFERENCES backfill_providers(run_id,provider));

CREATE TABLE backfill_members (
 run_id TEXT NOT NULL REFERENCES backfill_runs(run_id), provider TEXT NOT NULL,
 configured_root_id TEXT NOT NULL, source_key TEXT NOT NULL, ordinal INTEGER NOT NULL,
 capture_id TEXT NOT NULL, status TEXT NOT NULL,
 manifest_id TEXT NOT NULL DEFAULT '', receipt TEXT NOT NULL DEFAULT '',
 generation INTEGER NOT NULL DEFAULT 0, error_class TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(run_id,provider,configured_root_id,source_key), UNIQUE(run_id,ordinal));

CREATE INDEX backfill_members_capture ON backfill_members(capture_id,status);

CREATE INDEX backfill_members_pending ON backfill_members(run_id,status,capture_id);

CREATE TRIGGER backfill_member_insert AFTER INSERT ON backfill_members BEGIN
 UPDATE backfill_runs SET captured = captured + 1,
 acknowledged = acknowledged + (NEW.status = 'acknowledged') WHERE run_id = NEW.run_id;
 END;

CREATE TRIGGER backfill_member_status AFTER UPDATE OF status ON backfill_members BEGIN
 UPDATE backfill_runs SET acknowledged = acknowledged +
 (NEW.status = 'acknowledged') - (OLD.status = 'acknowledged'),
 invalidated = invalidated + (NEW.status = 'invalidated') - (OLD.status = 'invalidated')
 WHERE run_id = NEW.run_id;
 END;

CREATE TRIGGER backfill_generation_deleted BEFORE DELETE ON outbox_generations BEGIN
 UPDATE backfill_members SET status = 'invalidated', error_class = 'capture_lost'
 WHERE capture_id = OLD.capture_id AND status = 'pending';
 END;
`
