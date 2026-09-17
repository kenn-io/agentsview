//go:build pgtest

package postgres

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func loosenRawIngestJobStageCheck(t *testing.T, pg *sql.DB) {
	t.Helper()
	rows, err := pg.QueryContext(t.Context(), `
		SELECT format(
			'ALTER TABLE %I.raw_ingest_jobs DROP CONSTRAINT %I',
			$1::text, conname)
		FROM pg_catalog.pg_constraint
		WHERE conrelid = to_regclass(format('%I.raw_ingest_jobs', $1::text))
			AND contype = 'c'
			AND pg_get_constraintdef(oid) LIKE '%stage%'
			AND pg_get_constraintdef(oid) LIKE '%parse%'`,
		schemaTestSchema)
	require.NoError(t, err)
	defer rows.Close()
	var drops []string
	for rows.Next() {
		var ddl string
		require.NoError(t, rows.Scan(&ddl))
		drops = append(drops, ddl)
	}
	require.NoError(t, rows.Err())
	require.Len(t, drops, 1,
		"a fresh test schema must carry exactly one raw_ingest_jobs stage CHECK")
	_, err = pg.ExecContext(t.Context(), drops[0])
	require.NoError(t, err)
}

func TestRawJobHealthOrphans(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))

	first := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	firstCommit, err := store.CommitManifest(t.Context(), first, "parser-data-17")
	require.NoError(t, err)
	second := rawIngestManifest(
		t, identity, "capture-b", firstCommit.Receipt,
		rawIngestCapturedAt().Add(time.Minute), object,
	)
	secondCommit, err := store.CommitManifest(t.Context(), second, "parser-data-17")
	require.NoError(t, err)

	_, err = pg.ExecContext(t.Context(),
		"DELETE FROM raw_ingest_jobs WHERE tenant_id = $1", identity.TenantID,
	)
	require.NoError(t, err)
	report := rawHealthReport(t, store, identity, 5, 3600)
	assert.Equal(t, int64(1), report.OrphanedManifestCount)
	require.Len(t, report.OrphanedManifests, 1)
	assert.Equal(t, secondCommit.ManifestID, report.OrphanedManifests[0].ManifestID)
	assert.NotEqual(t, firstCommit.ManifestID, report.OrphanedManifests[0].ManifestID)

	tombstone := rawHealthCommit(
		t, store, identity, "capture-tombstone", secondCommit.Receipt,
		"sessions/demo.jsonl#main", rawsync.ManifestTombstone,
		rawIngestCapturedAt().Add(2*time.Minute), object,
	)
	_, err = pg.ExecContext(t.Context(),
		"DELETE FROM raw_ingest_jobs WHERE tenant_id = $1", identity.TenantID,
	)
	require.NoError(t, err)
	report = rawHealthReport(t, store, identity, 5, 3600)
	assert.Equal(t, int64(1), report.OrphanedManifestCount)
	require.Len(t, report.OrphanedManifests, 1)
	assert.Equal(t, tombstone.ManifestID, report.OrphanedManifests[0].ManifestID)
	assert.Equal(t, rawsync.ManifestTombstone, report.OrphanedManifests[0].Kind)

	loosenRawIngestJobStageCheck(t, pg)
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_ingest_jobs (
			tenant_id, manifest_id, stage, processing_version, state,
			attempt_count, lease_expires_at, last_error_class, last_error
		) VALUES ($1, $2, 'derive', 'derive-health', 'failed', 9,
			now() - interval '1 hour', 'hidden-class', 'hidden-message')`,
		identity.TenantID, tombstone.ManifestID,
	)
	require.NoError(t, err)
	report = rawHealthReport(t, store, identity, 5, 1)
	assert.Equal(t, int64(1), report.OrphanedManifestCount)
	assert.Zero(t, report.FailedJobCount)
	assert.Zero(t, report.ExpiredLeaseCount)
	assert.Zero(t, report.RetryingNearLimitCount)

	for i, state := range []string{
		"ready", "leased", "retrying", "complete", "failed", "superseded",
	} {
		_, err = pg.ExecContext(t.Context(), `
			DELETE FROM raw_ingest_jobs
			WHERE tenant_id = $1 AND manifest_id = $2 AND stage = 'parse'`,
			identity.TenantID, tombstone.ManifestID,
		)
		require.NoError(t, err)
		_, err = pg.ExecContext(t.Context(), `
			INSERT INTO raw_ingest_jobs (
				tenant_id, manifest_id, stage, processing_version, state
			) VALUES ($1, $2, 'parse', $3, $4)`,
			identity.TenantID, tombstone.ManifestID,
			fmt.Sprintf("health-state-%d", i), state,
		)
		require.NoError(t, err)
		report = rawHealthReport(t, store, identity, 5, 1)
		assert.Zero(t, report.OrphanedManifestCount, state)
	}
}

func TestRawJobHealthExpiredLeases(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawHealthCommit(
		t, store, identity, "capture-a", "", "sessions/leases",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)

	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET state = 'leased', attempt_count = 2, lease_owner = 'expired-owner',
			lease_expires_at = statement_timestamp() - interval '1 second'
		WHERE tenant_id = $1 AND manifest_id = $2 AND stage = 'parse'`,
		identity.TenantID, manifest.ManifestID,
	)
	require.NoError(t, err)
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-expired-equal", "leased", 3, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-active", "leased", 4, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-null", "leased", 5, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-ready", "ready", 6, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-retry", "retrying", 7, "", "")
	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET lease_expires_at = CASE processing_version
			WHEN 'health-expired-equal' THEN statement_timestamp()
			WHEN 'health-active' THEN statement_timestamp() + interval '1 hour'
			WHEN 'health-null' THEN NULL
			WHEN 'health-ready' THEN statement_timestamp() - interval '1 hour'
			WHEN 'health-retry' THEN statement_timestamp() - interval '1 hour'
			ELSE lease_expires_at
		END
		WHERE tenant_id = $1 AND manifest_id = $2`,
		identity.TenantID, manifest.ManifestID,
	)
	require.NoError(t, err)
	tx, err := pg.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(
		t.Context(),
		"UPDATE raw_ingest_jobs SET lease_expires_at = CURRENT_TIMESTAMP "+
			"WHERE tenant_id = $1 AND manifest_id = $2 "+
			"AND processing_version = 'health-expired-equal'",
		identity.TenantID, manifest.ManifestID,
	)
	require.NoError(t, err)
	exactReport, err := rawJobHealth(
		t.Context(), tx, identity,
		rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 3600},
	)
	require.NoError(t, err)
	require.Len(t, exactReport.ExpiredLeases, 2)
	assert.Equal(t, "health-expired-equal",
		exactReport.ExpiredLeases[1].ProcessingVersion)
	require.NoError(t, tx.Commit())
	report := rawHealthReport(t, store, identity, 5, 3600)
	assert.Equal(t, int64(2), report.ExpiredLeaseCount)
	require.Len(t, report.ExpiredLeases, 2)
	assert.Equal(t, "parser-data-17", report.ExpiredLeases[0].ProcessingVersion)
	assert.Equal(t, "health-expired-equal", report.ExpiredLeases[1].ProcessingVersion)
	assert.NotContains(t, string(mustMarshalJSON(t, report)), `"lease_owner"`)
}

func TestRawJobHealthFailureClasses(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	first := rawHealthCommit(
		t, store, identity, "capture-a", "", "sessions/failures",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	second := rawHealthCommit(
		t, store, identity, "capture-b", first.Receipt, "sessions/failures",
		rawsync.ManifestSnapshot, rawIngestCapturedAt().Add(time.Minute), object,
	)

	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET state = 'failed', last_error_class = 'historical',
			last_error = 'historical secret message'
		WHERE tenant_id = $1 AND manifest_id = $2 AND stage = 'parse'`,
		identity.TenantID, first.ManifestID,
	)
	require.NoError(t, err)
	insertRawHealthJob(t, pg, identity, second.ManifestID,
		"health-failure-parse", "failed", 1, "parse", "parse secret")
	insertRawHealthJob(t, pg, identity, second.ManifestID,
		"health-failure-empty", "failed", 1, "", "empty secret")
	insertRawHealthJob(t, pg, identity, second.ManifestID,
		"health-failure-parse-2", "failed", 1, "parse", "another secret")

	report := rawHealthReport(t, store, identity, 5, 3600)
	assert.Equal(t, int64(4), report.FailedJobCount)
	require.Len(t, report.FailedJobsByErrorClass, 3)
	classes := make(map[string]int64, len(report.FailedJobsByErrorClass))
	for _, row := range report.FailedJobsByErrorClass {
		classes[row.ErrorClass] = row.JobCount
		assert.NotZero(t, row.LatestFailureAt)
	}
	assert.Equal(t, int64(2), classes["parse"])
	assert.Equal(t, int64(1), classes[""])
	assert.Equal(t, int64(1), classes["historical"])
	assert.NotContains(t, string(mustMarshalJSON(t, report)), `"last_error"`+`:`)
}

func TestRawJobHealthRetryThreshold(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawHealthCommit(
		t, store, identity, "capture-a", "", "sessions/retries",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)

	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET state = 'retrying', attempt_count = 3,
			available_at = statement_timestamp() + interval '1 day'
		WHERE tenant_id = $1 AND manifest_id = $2 AND stage = 'parse'`,
		identity.TenantID, manifest.ManifestID,
	)
	require.NoError(t, err)
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-retry-at", "retrying", 4, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-retry-over", "retrying", 5, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-retry-zero", "retrying", 0, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-failed-high", "failed", 99, "failed", "hidden")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-leased-high", "leased", 99, "", "")
	insertRawHealthJob(t, pg, identity, manifest.ManifestID,
		"health-ready-high", "ready", 99, "", "")

	report := rawHealthReport(t, store, identity, 5, 3600)
	assert.Equal(t, int64(2), report.RetryingNearLimitCount)
	require.Len(t, report.RetryingNearLimit, 2)
	assert.Equal(t, 5, report.RetryingNearLimit[0].AttemptCount)
	assert.Equal(t, 4, report.RetryingNearLimit[1].AttemptCount)
	assert.Greater(t, report.RetryingNearLimit[0].AvailableAt, time.Now().UTC())

	report = rawHealthReport(t, store, identity, 1, 3600)
	assert.Equal(t, int64(3), report.RetryingNearLimitCount)
	require.Len(t, report.RetryingNearLimit, 3)
	assert.Equal(t, 5, report.RetryingNearLimit[0].AttemptCount)
	assert.Equal(t, 4, report.RetryingNearLimit[1].AttemptCount)
	assert.Equal(t, 3, report.RetryingNearLimit[2].AttemptCount)
}

func TestRawJobHealthStaleHeads(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	old := rawHealthCommit(
		t, store, identity, "capture-old", "", "sessions/old",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	boundary := rawHealthCommit(
		t, store, identity, "capture-boundary", "", "sessions/boundary",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	recent := rawHealthCommit(
		t, store, identity, "capture-recent", "", "sessions/recent",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	tombstone := rawHealthCommit(
		t, store, identity, "capture-tombstone", "", "sessions/removed",
		rawsync.ManifestTombstone, rawIngestCapturedAt(), object,
	)

	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_source_heads
		SET updated_at = CASE source_key
			WHEN 'sessions/old' THEN statement_timestamp() - interval '10 seconds'
			WHEN 'sessions/boundary' THEN statement_timestamp() - interval '1 second'
			WHEN 'sessions/recent' THEN statement_timestamp()
			WHEN 'sessions/removed' THEN statement_timestamp() - interval '10 seconds'
			ELSE updated_at
		END
		WHERE tenant_id = $1`, identity.TenantID,
	)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_source_heads (
			tenant_id, device_id, provider, configured_root_id, source_key,
			source_key_sha256, generation, updated_at
		) VALUES ($1, 'device-a', 'codex', 'root-a', 'sessions/zero', $2,
			0, statement_timestamp() - interval '1 day')`,
		identity.TenantID, rawIngestKeyDigest("sessions/zero"),
	)
	require.NoError(t, err)

	tx, err := pg.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(
		t.Context(),
		"UPDATE raw_source_heads "+
			"SET updated_at = CURRENT_TIMESTAMP - interval '1 second' "+
			"WHERE tenant_id = $1 AND manifest_id = $2",
		identity.TenantID, boundary.ManifestID,
	)
	require.NoError(t, err)
	exactReport, err := rawJobHealth(
		t.Context(), tx, identity,
		rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 1},
	)
	require.NoError(t, err)
	require.NotEmpty(t, exactReport.StaleSourceHeads)
	var boundaryFound bool
	for _, row := range exactReport.StaleSourceHeads {
		boundaryFound = boundaryFound || row.ManifestID == boundary.ManifestID
	}
	assert.True(t, boundaryFound)
	require.NoError(t, tx.Commit())

	report := rawHealthReport(t, store, identity, 5, 1)
	assert.Equal(t, int64(3), report.StaleSourceHeadCount)
	require.Len(t, report.StaleSourceHeads, 3)
	ids := make(map[string]rawsync.StaleSourceHead, len(report.StaleSourceHeads))
	for _, row := range report.StaleSourceHeads {
		ids[row.ManifestID] = row
	}
	assert.Contains(t, ids, old.ManifestID)
	assert.Contains(t, ids, boundary.ManifestID)
	assert.Contains(t, ids, tombstone.ManifestID)
	assert.NotContains(t, ids, recent.ManifestID)
	assert.Equal(t, rawsync.ManifestTombstone, ids[tombstone.ManifestID].Kind)
}

func TestRawJobHealthIsolation(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identityA := rawIngestIdentity(t, "tenant-a")
	identityB, err := rawsync.NewAuthIdentity("tenant-b", "device-b")
	require.NoError(t, err)
	identityAOther, err := rawsync.NewAuthIdentity("tenant-a", "device-b")
	require.NoError(t, err)
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identityA, object))
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identityB, object))
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identityAOther, object))
	mainA := rawHealthCommit(
		t, store, identityA, "capture-main", "", "sessions/main",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	mainB := rawHealthCommit(
		t, store, identityB, "capture-main", "", "sessions/main",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	nonParseA := rawHealthCommit(
		t, store, identityA, "capture-nonparse", "", "sessions/nonparse",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	nonParseB := rawHealthCommit(
		t, store, identityB, "capture-nonparse", "", "sessions/nonparse",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	staleAOther := rawHealthCommit(
		t, store, identityAOther, "capture-stale", "", "sessions/stale",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	staleB := rawHealthCommit(
		t, store, identityB, "capture-stale", "", "sessions/stale",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)

	setHealthJobState(t, pg, identityA, mainA.ManifestID, "leased", 1,
		"health-a", "", "")
	insertRawHealthJob(t, pg, identityA, mainA.ManifestID,
		"health-a-retry", "retrying", 4, "", "")
	insertRawHealthJob(t, pg, identityA, mainA.ManifestID,
		"health-a-failed", "failed", 1, "a-class", "hidden")
	setHealthJobState(t, pg, identityB, mainB.ManifestID, "leased", 1,
		"health-b", "", "")
	insertRawHealthJob(t, pg, identityB, mainB.ManifestID,
		"health-b-retry", "retrying", 4, "", "")
	insertRawHealthJob(t, pg, identityB, mainB.ManifestID,
		"health-b-failed", "failed", 1, "b-class", "hidden")

	loosenRawIngestJobStageCheck(t, pg)
	for _, identity := range []rawsync.AuthIdentity{identityA, identityB} {
		manifestID := nonParseA.ManifestID
		if identity == identityB {
			manifestID = nonParseB.ManifestID
		}
		_, err = pg.ExecContext(t.Context(), `
			DELETE FROM raw_ingest_jobs
			WHERE tenant_id = $1 AND manifest_id = $2 AND stage = 'parse'`,
			identity.TenantID, manifestID,
		)
		require.NoError(t, err)
		_, err = pg.ExecContext(t.Context(), `
			INSERT INTO raw_ingest_jobs (
				tenant_id, manifest_id, stage, processing_version, state,
				attempt_count, lease_expires_at, last_error_class, last_error
			) VALUES ($1, $2, 'derive', 'derive-health', 'failed', 99,
				now() - interval '1 hour', 'derive-class', 'hidden')`,
			identity.TenantID, manifestID,
		)
		require.NoError(t, err)
	}
	for _, fixture := range []struct {
		identity rawsync.AuthIdentity
		manifest string
	}{
		{identityAOther, staleAOther.ManifestID},
		{identityB, staleB.ManifestID},
	} {
		_, err = pg.ExecContext(
			t.Context(),
			"UPDATE raw_source_heads "+
				"SET updated_at = CURRENT_TIMESTAMP - interval '1 hour' "+
				"WHERE tenant_id = $1 AND manifest_id = $2",
			fixture.identity.TenantID, fixture.manifest,
		)
		require.NoError(t, err)
	}

	reportA := rawHealthReport(t, store, identityA, 5, 1)
	assert.Equal(t, int64(1), reportA.OrphanedManifestCount)
	assert.Equal(t, int64(1), reportA.ExpiredLeaseCount)
	assert.Equal(t, int64(1), reportA.RetryingNearLimitCount)
	assert.Equal(t, int64(1), reportA.FailedJobCount)
	assert.Equal(t, int64(1), reportA.StaleSourceHeadCount)
	for _, row := range reportA.ExpiredLeases {
		assert.Equal(t, "device-a", row.DeviceID)
	}
	for _, row := range reportA.RetryingNearLimit {
		assert.Equal(t, "device-a", row.DeviceID)
	}
	require.Len(t, reportA.StaleSourceHeads, 1)
	assert.Equal(t, staleAOther.ManifestID, reportA.StaleSourceHeads[0].ManifestID)
	assert.Equal(t, "device-b", reportA.StaleSourceHeads[0].DeviceID)
	reportB := rawHealthReport(t, store, identityB, 5, 1)
	assert.Equal(t, int64(1), reportB.OrphanedManifestCount)
	assert.Equal(t, int64(1), reportB.ExpiredLeaseCount)
	assert.Equal(t, int64(1), reportB.RetryingNearLimitCount)
	assert.Equal(t, int64(1), reportB.FailedJobCount)
	assert.Equal(t, int64(1), reportB.StaleSourceHeadCount)
	require.Len(t, reportB.StaleSourceHeads, 1)
	assert.Equal(t, staleB.ManifestID, reportB.StaleSourceHeads[0].ManifestID)
	assert.Equal(t, "device-b", reportB.StaleSourceHeads[0].DeviceID)
}

func TestRawJobHealthReadOnly(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawHealthCommit(
		t, store, identity, "capture-a", "", "sessions/read-only",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	setHealthJobState(t, pg, identity, manifest.ManifestID, "leased", 2,
		"read-only-owner", "", "")
	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET lease_expires_at = statement_timestamp() - interval '1 hour'
		WHERE tenant_id = $1 AND manifest_id = $2`,
		identity.TenantID, manifest.ManifestID,
	)
	require.NoError(t, err)

	before := rawHealthMutationFingerprint(t, pg)
	first := rawHealthReport(t, store, identity, 5, 1)
	second := rawHealthReport(t, store, identity, 5, 1)
	after := rawHealthMutationFingerprint(t, pg)
	assert.Equal(t, first.MaxAttempts, second.MaxAttempts)
	assert.Equal(t, first.StaleAfterSeconds, second.StaleAfterSeconds)
	assert.Equal(t, first.OrphanedManifests, second.OrphanedManifests)
	assert.Equal(t, first.OrphanedManifestCount, second.OrphanedManifestCount)
	assert.Equal(t, first.ExpiredLeases, second.ExpiredLeases)
	assert.Equal(t, first.ExpiredLeaseCount, second.ExpiredLeaseCount)
	assert.Equal(t, first.FailedJobsByErrorClass, second.FailedJobsByErrorClass)
	assert.Equal(t, first.FailedJobCount, second.FailedJobCount)
	assert.Equal(t, first.RetryingNearLimit, second.RetryingNearLimit)
	assert.Equal(t, first.RetryingNearLimitCount, second.RetryingNearLimitCount)
	assert.Equal(t, first.StaleSourceHeads, second.StaleSourceHeads)
	assert.Equal(t, first.StaleSourceHeadCount, second.StaleSourceHeadCount)
	assert.Equal(t, before, after)

	pgURL := testPGURL(t)
	const role = "agentsview_health_reader"
	const password = "agentsview_health_reader_pw"
	_, _ = pg.Exec("DROP OWNED BY " + role)
	_, _ = pg.Exec("DROP ROLE IF EXISTS " + role)
	_, err = pg.Exec("CREATE ROLE " + role + " LOGIN PASSWORD '" + password + "'")
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = pg.Exec("DROP OWNED BY " + role)
		_, _ = pg.Exec("DROP ROLE IF EXISTS " + role)
	})
	for _, grant := range []string{
		"GRANT USAGE ON SCHEMA " + schemaTestSchema + " TO " + role,
		"GRANT SELECT ON " + schemaTestSchema + ".raw_manifests TO " + role,
		"GRANT SELECT ON " + schemaTestSchema + ".raw_source_heads TO " + role,
		"GRANT SELECT ON " + schemaTestSchema + ".raw_ingest_jobs TO " + role,
	} {
		_, err = pg.Exec(grant)
		require.NoError(t, err, grant)
	}
	readerURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	readerURL.User = url.UserPassword(role, password)
	readerDB, err := Open(readerURL.String(), schemaTestSchema, true)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, readerDB.Close()) })
	reader, err := NewRawIngestStore(readerDB)
	require.NoError(t, err)
	readerReport, err := reader.RawJobHealth(
		t.Context(), identity, rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 1},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), readerReport.ExpiredLeaseCount)
}

func TestRawJobHealthValidation(t *testing.T) {
	_, store := newRawIngestTestStore(t)
	validIdentity := rawIngestIdentity(t, "tenant-a")
	for _, identity := range []rawsync.AuthIdentity{
		{}, {TenantID: "tenant-a"}, {DeviceID: "device-a"},
	} {
		_, err := store.RawJobHealth(
			t.Context(), identity,
			rawsync.JobHealthQuery{MaxAttempts: 1, StaleAfterSeconds: 1},
		)
		assert.ErrorIs(t, err, rawsync.ErrInvalid)
	}
	for _, query := range []rawsync.JobHealthQuery{
		{MaxAttempts: 0, StaleAfterSeconds: 1},
		{MaxAttempts: -1, StaleAfterSeconds: 1},
		{MaxAttempts: 1, StaleAfterSeconds: 0},
		{MaxAttempts: 1, StaleAfterSeconds: -1},
	} {
		_, err := store.RawJobHealth(t.Context(), validIdentity, query)
		assert.ErrorIs(t, err, rawsync.ErrInvalid)
	}
	report, err := store.RawJobHealth(
		t.Context(), validIdentity,
		rawsync.JobHealthQuery{MaxAttempts: 1, StaleAfterSeconds: 1},
	)
	require.NoError(t, err)
	assert.NotNil(t, report.OrphanedManifests)
	assert.NotNil(t, report.ExpiredLeases)
	assert.NotNil(t, report.FailedJobsByErrorClass)
	assert.NotNil(t, report.RetryingNearLimit)
	assert.NotNil(t, report.StaleSourceHeads)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.RawJobHealth(
		ctx, validIdentity,
		rawsync.JobHealthQuery{MaxAttempts: 1, StaleAfterSeconds: 1},
	)
	assert.Error(t, err)
}

func TestRawJobHealthCaps(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	for i := range rawJobHealthMaxRows + 1 {
		rawHealthCommit(
			t, store, identity, fmt.Sprintf("capture-cap-%03d", i), "",
			fmt.Sprintf("sessions/cap-%03d", i), rawsync.ManifestSnapshot,
			rawIngestCapturedAt(), object,
		)
	}
	capManifest := rawHealthCommit(
		t, store, identity, "capture-job-cap", "", "sessions/job-cap",
		rawsync.ManifestSnapshot, rawIngestCapturedAt(), object,
	)
	_, err := pg.ExecContext(t.Context(), `
		DELETE FROM raw_ingest_jobs
		WHERE tenant_id = $1 AND manifest_id <> $2`,
		identity.TenantID, capManifest.ManifestID,
	)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_source_heads
		SET updated_at = statement_timestamp() - interval '1 hour'
		WHERE tenant_id = $1 AND source_key LIKE 'sessions/cap-%'`,
		identity.TenantID,
	)
	require.NoError(t, err)
	for i := range 60 {
		insertRawHealthJob(t, pg, identity, capManifest.ManifestID,
			fmt.Sprintf("cap-expired-%03d", i), "leased", 1, "", "")
	}
	for i := range 60 {
		insertRawHealthJob(t, pg, identity, capManifest.ManifestID,
			fmt.Sprintf("cap-retry-%03d", i), "retrying", 4, "", "")
	}
	for i := range 25 {
		insertRawHealthJob(t, pg, identity, capManifest.ManifestID,
			fmt.Sprintf("cap-failed-%03d", i), "failed", 1,
			fmt.Sprintf("class-%02d", i), "hidden")
	}
	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET lease_expires_at = TIMESTAMPTZ '2020-01-01'
		WHERE tenant_id = $1 AND processing_version LIKE 'cap-expired-%'`,
		identity.TenantID,
	)
	require.NoError(t, err)

	query := rawsync.JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 1}
	first := rawHealthReport(t, store, identity, query.MaxAttempts, query.StaleAfterSeconds)
	second := rawHealthReport(t, store, identity, query.MaxAttempts, query.StaleAfterSeconds)
	assert.Equal(t, int64(51), first.OrphanedManifestCount)
	assert.Equal(t, int64(51), first.StaleSourceHeadCount)
	assert.Equal(t, int64(60), first.ExpiredLeaseCount)
	assert.Equal(t, int64(60), first.RetryingNearLimitCount)
	assert.Equal(t, int64(25), first.FailedJobCount)
	assert.Len(t, first.OrphanedManifests, rawJobHealthMaxRows)
	assert.Len(t, first.StaleSourceHeads, rawJobHealthMaxRows)
	assert.Len(t, first.ExpiredLeases, rawJobHealthMaxRows)
	assert.Len(t, first.RetryingNearLimit, rawJobHealthMaxRows)
	assert.Len(t, first.FailedJobsByErrorClass, rawJobHealthMaxErrorClasses)
	assert.Equal(t, first.OrphanedManifests, second.OrphanedManifests)
	assert.Equal(t, first.StaleSourceHeads, second.StaleSourceHeads)
	assert.Equal(t, first.ExpiredLeases, second.ExpiredLeases)
	assert.Equal(t, first.RetryingNearLimit, second.RetryingNearLimit)
	assert.Equal(t, first.FailedJobsByErrorClass, second.FailedJobsByErrorClass)
	assert.Equal(t, "class-00", first.FailedJobsByErrorClass[0].ErrorClass)
	assert.Equal(t, "class-19", first.FailedJobsByErrorClass[19].ErrorClass)
	assert.Less(t,
		first.ExpiredLeases[0].JobID,
		first.ExpiredLeases[len(first.ExpiredLeases)-1].JobID,
	)
	assert.Less(t,
		first.RetryingNearLimit[0].JobID,
		first.RetryingNearLimit[len(first.RetryingNearLimit)-1].JobID,
	)
}

func rawHealthReport(
	t *testing.T,
	store *RawIngestStore,
	identity rawsync.AuthIdentity,
	maxAttempts, staleAfterSeconds int32,
) rawsync.JobHealthReport {
	t.Helper()
	report, err := store.RawJobHealth(t.Context(), identity, rawsync.JobHealthQuery{
		MaxAttempts: maxAttempts, StaleAfterSeconds: staleAfterSeconds,
	})
	require.NoError(t, err)
	return report
}

func rawHealthCommit(
	t *testing.T,
	store *RawIngestStore,
	identity rawsync.AuthIdentity,
	captureID, parentReceipt, sourceKey string,
	kind rawsync.ManifestKind,
	capturedAt time.Time,
	object rawsync.ObjectRef,
) rawsync.CommitResult {
	t.Helper()
	manifest := rawHealthManifest(
		t, identity, captureID, parentReceipt, sourceKey, kind, capturedAt, object,
	)
	if kind == rawsync.ManifestSnapshot {
		require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	}
	result, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.NoError(t, err)
	return result
}

func rawHealthManifest(
	t *testing.T,
	identity rawsync.AuthIdentity,
	captureID, parentReceipt, sourceKey string,
	kind rawsync.ManifestKind,
	capturedAt time.Time,
	object rawsync.ObjectRef,
) rawsync.CanonicalManifest {
	t.Helper()
	manifest := rawsync.Manifest{
		SchemaVersion:         rawsync.ManifestSchemaVersion,
		Provider:              parser.AgentCodex,
		ConfiguredRootID:      "root-a",
		SourceKey:             sourceKey,
		ExpectedParentReceipt: parentReceipt,
		CaptureID:             captureID,
		CapturedAt:            capturedAt,
		Kind:                  kind,
	}
	if kind == rawsync.ManifestSnapshot {
		manifest.Entries = []rawsync.Entry{{
			Path: "session.jsonl", Type: "file", Length: object.Length,
			Objects: []rawsync.ObjectRef{object},
		}}
	}
	canonical, err := rawsync.ValidateAndCanonicalize(
		identity, manifest, rawsync.DefaultManifestLimits(),
	)
	require.NoError(t, err)
	return canonical
}

func insertRawHealthJob(
	t *testing.T,
	pg *sql.DB,
	identity rawsync.AuthIdentity,
	manifestID, processingVersion, state string,
	attemptCount int,
	errorClass, errorMessage string,
) int64 {
	t.Helper()
	var id int64
	err := pg.QueryRowContext(t.Context(), `
		INSERT INTO raw_ingest_jobs (
			tenant_id, manifest_id, stage, processing_version, state,
			attempt_count, available_at, lease_expires_at,
			last_error_class, last_error
		) VALUES ($1, $2, 'parse', $3, $4, $5,
			statement_timestamp() + interval '1 day',
			CASE WHEN $4 = 'leased'
				THEN statement_timestamp() - interval '1 second' END,
			$6, $7)
		RETURNING id`,
		identity.TenantID, manifestID, processingVersion, state,
		attemptCount, errorClass, errorMessage,
	).Scan(&id)
	require.NoError(t, err)
	return id
}

func setHealthJobState(
	t *testing.T,
	pg *sql.DB,
	identity rawsync.AuthIdentity,
	manifestID, state string,
	attemptCount int,
	leaseOwner, errorClass, errorMessage string,
) {
	t.Helper()
	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET state = $3, attempt_count = $4, lease_owner = $5,
			last_error_class = $6, last_error = $7,
			lease_expires_at = CASE WHEN $3 = 'leased'
				THEN statement_timestamp() - interval '1 second' END
		WHERE tenant_id = $1 AND manifest_id = $2 AND stage = 'parse'`,
		identity.TenantID, manifestID, state, attemptCount,
		leaseOwner, errorClass, errorMessage,
	)
	require.NoError(t, err)
}

func mustMarshalJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	return raw
}

func rawHealthMutationFingerprint(t *testing.T, pg *sql.DB) string {
	t.Helper()
	var fingerprint string
	err := pg.QueryRowContext(t.Context(), `
		SELECT md5(
			COALESCE((
				SELECT string_agg(
					id::text || ':' || tenant_id || ':' || manifest_id || ':' ||
					stage || ':' || processing_version || ':' || state || ':' ||
					attempt_count::text || ':' || lease_owner || ':' ||
					COALESCE(lease_expires_at::text, '') || ':' || last_error_class ||
					':' || last_error || ':' || updated_at::text, '|' ORDER BY id)
				FROM raw_ingest_jobs), '') ||
			COALESCE((
				SELECT string_agg(
					tenant_id || ':' || device_id || ':' || provider || ':' ||
					configured_root_id || ':' || source_key_sha256 || ':' ||
					COALESCE(manifest_id, '') || ':' || generation::text || ':' ||
					updated_at::text, '|' ORDER BY tenant_id, device_id, provider,
					configured_root_id, source_key_sha256)
				FROM raw_source_heads), '')
		)`).Scan(&fingerprint)
	require.NoError(t, err)
	return fingerprint
}
