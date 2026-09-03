//go:build pgtest

package postgres

import (
	"database/sql"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
)

func TestRawParseJobsLeaseExclusivelyAndFenceExpiredAttempts(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	_, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.NoError(t, err)

	first, err := store.ClaimRawParseJobs(t.Context(), "worker-a", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, first, 1)
	assert.Equal(t, manifest.ManifestID, first[0].ManifestID)
	assert.Equal(t, 1, first[0].Attempt)
	assert.Equal(t, identity, first[0].Identity)

	other, err := store.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, other, "an active lease must be exclusive")

	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET lease_expires_at = now() - interval '1 second'
		WHERE id = $1`, first[0].ID)
	require.NoError(t, err)

	second, err := store.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, second, 1)
	assert.Equal(t, first[0].ID, second[0].ID)
	assert.Equal(t, 2, second[0].Attempt)

	err = store.HeartbeatRawParseJob(t.Context(), first[0], time.Minute)
	assert.ErrorIs(t, err, rawderive.ErrLeaseLost)
	err = store.CompleteRawParseJob(t.Context(), first[0])
	assert.ErrorIs(t, err, rawderive.ErrLeaseLost)
	require.NoError(t, store.HeartbeatRawParseJob(t.Context(), second[0], time.Minute))
	require.NoError(t, store.CompleteRawParseJob(t.Context(), second[0]))

	var state string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT state FROM raw_ingest_jobs WHERE id = $1`, second[0].ID,
	).Scan(&state))
	assert.Equal(t, "complete", state)
}

func TestRawParseJobsSupersedeNonHeadManifestsOnShortClaim(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	firstManifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	firstCommit, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
	require.NoError(t, err)
	secondManifest := rawIngestManifest(
		t, identity, "capture-b", firstCommit.Receipt,
		rawIngestCapturedAt().Add(time.Minute), object,
	)
	_, err = store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
	require.NoError(t, err)
	// Commit-time supersession already retired the prior-head job, so reopen
	// it as a legacy row the commit path never reached.
	reOpenSupersededRawParseJobs(t, pg)

	leases, err := store.ClaimRawParseJobs(t.Context(), "worker-a", 2, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, secondManifest.ManifestID, leases[0].ManifestID)

	var state string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT state
		FROM raw_ingest_jobs
		WHERE tenant_id = $1 AND manifest_id = $2`,
		identity.TenantID, firstManifest.ManifestID,
	).Scan(&state))
	assert.Equal(t, "superseded", state,
		"a short claim must still retire legacy non-head rows the commit path missed")
}

func TestRawParseJobsRetryOnlyWhenAvailable(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	_, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.NoError(t, err)
	lease := claimOneRawParseJob(t, store)

	retryAt := time.Now().Add(time.Hour)
	require.NoError(t, store.RetryRawParseJob(
		t.Context(), lease, retryAt, "object_store", "temporary failure",
	))
	leasing, err := store.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, leasing)

	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs SET available_at = now() - interval '1 second'
		WHERE id = $1`, lease.ID)
	require.NoError(t, err)
	leasing, err = store.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leasing, 1)
	assert.Equal(t, lease.Attempt+1, leasing[0].Attempt)
}

func TestRawParseJobsFenceRetryAfterSourceHeadAdvances(t *testing.T) {
	_, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	firstManifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	firstCommit, err := store.CommitManifest(
		t.Context(), firstManifest, "parser-data-17",
	)
	require.NoError(t, err)
	lease := claimOneRawParseJob(t, store)

	secondManifest := rawIngestManifest(
		t, identity, "capture-b", firstCommit.Receipt,
		rawIngestCapturedAt().Add(time.Minute), object,
	)
	_, err = store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
	require.NoError(t, err)

	err = store.RetryRawParseJob(
		t.Context(), lease, time.Now().Add(time.Minute),
		"object_store", "temporary failure",
	)
	assert.ErrorIs(t, err, rawderive.ErrLeaseLost)
}

func TestRawParseJobsRecordTerminalFailureWithLeaseFence(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	manifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	_, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
	require.NoError(t, err)
	lease := claimOneRawParseJob(t, store)

	require.NoError(t, store.FailRawParseJob(
		t.Context(), lease, "parse", "unsupported provider format",
	))
	err = store.FailRawParseJob(t.Context(), lease, "parse", "duplicate completion")
	assert.ErrorIs(t, err, rawderive.ErrLeaseLost)

	var state, errorClass, message string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT state, last_error_class, last_error
		FROM raw_ingest_jobs WHERE id = $1`, lease.ID,
	).Scan(&state, &errorClass, &message))
	assert.Equal(t, "failed", state)
	assert.Equal(t, "parse", errorClass)
	assert.Equal(t, "unsupported provider format", message)

	leasing, err := store.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)
	require.NoError(t, err)
	assert.Empty(t, leasing)
}

func TestClaimRawParseJobsSupersedeBatchIsBounded(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	// Chain six generations for one source: every commit moves the head, so
	// five obsolete jobs pile up behind the current one.
	receipt := ""
	capturedAt := rawIngestCapturedAt()
	for i := range 6 {
		manifest := rawIngestManifest(t, identity, fmt.Sprintf("capture-%d", i), receipt, capturedAt.Add(time.Duration(i)*time.Minute), object)
		commit, err := store.CommitManifest(t.Context(), manifest, "parser-data-17")
		require.NoError(t, err)
		receipt = commit.Receipt
	}
	countState := func(state string) int {
		var count int
		require.NoError(t, pg.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM raw_ingest_jobs WHERE state = $1`, state,
		).Scan(&count))
		return count
	}
	assert.Equal(t, 1, countState("ready"),
		"commit-time supersession keeps only the current head's job ready")
	assert.Equal(t, 5, countState("superseded"))
	// Reopen the retired jobs as a legacy backlog no commit path can reach,
	// so the bounded claim-path fallback stays provably bounded.
	reOpenSupersededRawParseJobs(t, pg)
	require.Equal(t, 6, countState("ready"))

	tx, err := pg.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	require.NoError(t, supersedeObsoleteRawParseJobs(t.Context(), tx, 2))
	require.NoError(t, tx.Commit())
	assert.Equal(t, 2, countState("superseded"),
		"obsolete-job supersession must be bounded per call")
	assert.Equal(t, 4, countState("ready"))

	for range 2 {
		tx, err = pg.BeginTx(t.Context(), nil)
		require.NoError(t, err)
		require.NoError(t, supersedeObsoleteRawParseJobs(t.Context(), tx, 2))
		require.NoError(t, tx.Commit())
	}
	assert.Equal(t, 5, countState("superseded"),
		"repeated bounded calls must drain the obsolete backlog")
	assert.Equal(t, 1, countState("ready"))
}

// reOpenSupersededRawParseJobs simulates legacy non-head rows that the
// commit-time supersession never reached, so claim-path cleanup stays
// exercised against rows only its bounded fallback can retire.
func reOpenSupersededRawParseJobs(t *testing.T, pg *sql.DB) {
	t.Helper()
	_, err := pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET state = 'ready', lease_owner = '', lease_expires_at = NULL,
			available_at = now() - interval '1 second', updated_at = now()
		WHERE state = 'superseded'`)
	require.NoError(t, err)
}

func TestClaimRawParseJobsSkipsSupersedeWhenBatchFillsWithCurrentHeads(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	// Two sources, each with one obsolete ready job behind a current-head
	// ready job: four claimable-looking rows, two of which may never lease.
	headManifestIDs := make([]string, 0, 2)
	for i, tenant := range []string{"tenant-a", "tenant-b"} {
		identity := rawIngestIdentity(t, tenant)
		object := rawIngestObject(t, string(rune('a'+i)), 7)
		require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
		firstManifest := rawIngestManifest(
			t, identity, "capture-a", "", rawIngestCapturedAt(), object,
		)
		firstCommit, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
		require.NoError(t, err)
		secondManifest := rawIngestManifest(
			t, identity, "capture-b", firstCommit.Receipt,
			rawIngestCapturedAt().Add(time.Minute), object,
		)
		commit, err := store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
		require.NoError(t, err)
		headManifestIDs = append(headManifestIDs, commit.ManifestID)
	}
	countJobs := func(state string) int {
		var count int
		require.NoError(t, pg.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM raw_ingest_jobs WHERE state = $1`, state,
		).Scan(&count))
		return count
	}
	// The second commit of each source already superseded the prior job, so
	// reopen those rows as legacy backlog the full claim must still skip.
	reOpenSupersededRawParseJobs(t, pg)
	require.Equal(t, 4, countJobs("ready"),
		"setup must leave two legacy obsolete and two current-head ready jobs")

	leases, err := store.ClaimRawParseJobs(t.Context(), "worker-a", 2, time.Minute)

	require.NoError(t, err)
	require.Len(t, leases, 2)
	leasedManifestIDs := []string{leases[0].ManifestID, leases[1].ManifestID}
	assert.ElementsMatch(t, headManifestIDs, leasedManifestIDs,
		"the claim itself must only ever lease current source heads")
	assert.Equal(t, 2, countJobs("leased"))
	assert.Equal(t, 2, countJobs("ready"),
		"a full current-head claim must not pay the obsolete-job supersede scan")
	assert.Zero(t, countJobs("superseded"))

	starved, err := store.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)

	require.NoError(t, err)
	assert.Empty(t, starved,
		"the leased heads and the head-probed obsolete rows leave nothing to claim")
	assert.Equal(t, 2, countJobs("superseded"),
		"a claim that cannot be filled must still drain obsolete jobs")
}

func TestClaimRawParseJobsRequiresCurrentHeadEvenWhenSupersedeMissed(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	firstManifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	firstCommit, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
	require.NoError(t, err)
	secondManifest := rawIngestManifest(
		t, identity, "capture-b", firstCommit.Receipt,
		rawIngestCapturedAt().Add(time.Minute), object,
	)
	_, err = store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
	require.NoError(t, err)
	// Leave more obsolete jobs than one bounded supersede pass can retire. The
	// claim statement itself must refuse to lease the stale rows that remain.
	_, err = pg.ExecContext(t.Context(), `
		UPDATE raw_ingest_jobs
		SET state = 'ready', lease_owner = '', lease_expires_at = NULL,
			available_at = now() - interval '1 second', updated_at = now()
		WHERE tenant_id = $1 AND manifest_id = $2`,
		identity.TenantID, firstManifest.ManifestID)
	require.NoError(t, err)
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_ingest_jobs (
			tenant_id, manifest_id, stage, processing_version, state, available_at
		)
		SELECT $1, $2, 'parse', 'stale-backlog-' || sequence, 'ready',
			now() - interval '1 second'
		FROM generate_series(1, $3) AS sequence`,
		identity.TenantID, firstManifest.ManifestID, maxRawParseSupersedeBatch+1)
	require.NoError(t, err)

	leases, err := store.ClaimRawParseJobs(t.Context(), "worker-a", 10, time.Minute)

	require.NoError(t, err)
	require.Len(t, leases, 1)
	assert.Equal(t, secondManifest.ManifestID, leases[0].ManifestID,
		"the claim itself must only ever lease current source heads")
	var state string
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT state FROM raw_ingest_jobs
		WHERE tenant_id = $1 AND manifest_id = $2
			AND processing_version = 'parser-data-17'`,
		identity.TenantID, firstManifest.ManifestID).Scan(&state))
	assert.Equal(t, "superseded", state)
	var staleReady, staleLeased int
	require.NoError(t, pg.QueryRowContext(t.Context(), `
		SELECT
			COUNT(*) FILTER (WHERE state = 'ready'),
			COUNT(*) FILTER (WHERE state = 'leased')
		FROM raw_ingest_jobs
		WHERE tenant_id = $1 AND manifest_id = $2`,
		identity.TenantID, firstManifest.ManifestID,
	).Scan(&staleReady, &staleLeased))
	assert.Greater(t, staleReady, 0,
		"the bounded supersede pass must leave stale claimable rows behind")
	assert.Zero(t, staleLeased,
		"the current-head predicate must prevent every remaining stale row from leasing")
}

// loosenRawIngestJobStageCheck removes the raw_ingest_jobs stage CHECK
// from this test's isolated per-test schema only, so tests can model job rows
// from stages the production schema does not admit yet. The constraint is
// discovered by definition rather than assumed name, and the schema itself
// is dropped again when the test ends.
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

// TestClaimRawParseJobsLeasesAndSupersedesOnlyParseStageJobs proves the
// claim and claim-path supersession queries stay scoped to parse jobs even
// when the schema admits other stages: a future stage must never be leased
// by parse workers nor retired by their cleanup, while obsolete parse jobs
// keep draining.
func TestClaimRawParseJobsLeasesAndSupersedesOnlyParseStageJobs(t *testing.T) {
	pg, store := newRawIngestTestStore(t)
	identity := rawIngestIdentity(t, "tenant-a")
	object := rawIngestObject(t, "a", 7)
	require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
	firstManifest := rawIngestManifest(
		t, identity, "capture-a", "", rawIngestCapturedAt(), object,
	)
	firstCommit, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
	require.NoError(t, err)
	secondManifest := rawIngestManifest(
		t, identity, "capture-b", firstCommit.Receipt,
		rawIngestCapturedAt().Add(time.Minute), object,
	)
	_, err = store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
	require.NoError(t, err)
	// The production schema only admits the parse stage today. Drop exactly
	// that CHECK inside this test's isolated schema to model a future schema
	// that also carries other stages, then seed eligible-looking derive jobs:
	// one on the current head (claim bait) and one behind it (supersede
	// bait).
	loosenRawIngestJobStageCheck(t, pg)
	_, err = pg.ExecContext(t.Context(), `
		INSERT INTO raw_ingest_jobs (
			tenant_id, manifest_id, stage, processing_version, state, available_at
		) VALUES
			($1, $2, 'derive', 'derive-1', 'ready', now() - interval '5 seconds'),
			($1, $3, 'derive', 'derive-1', 'ready', now() - interval '5 seconds')`,
		identity.TenantID, secondManifest.ManifestID, firstManifest.ManifestID)
	require.NoError(t, err)
	// Commit-time supersession retired the prior head's parse job. Reopen it
	// as legacy backlog so the claim-path cleanup still has a parse row it
	// must keep retiring.
	reOpenSupersededRawParseJobs(t, pg)

	leases, err := store.ClaimRawParseJobs(t.Context(), "worker-a", 3, time.Minute)

	require.NoError(t, err)
	require.Len(t, leases, 1,
		"the parse claim must lease only the current-head parse job")
	assert.Equal(t, secondManifest.ManifestID, leases[0].ManifestID)

	readJob := func(manifestID, stage string) (state, owner string, attempts int) {
		require.NoError(t, pg.QueryRowContext(t.Context(), `
			SELECT state, lease_owner, attempt_count
			FROM raw_ingest_jobs
			WHERE tenant_id = $1 AND manifest_id = $2 AND stage = $3`,
			identity.TenantID, manifestID, stage,
		).Scan(&state, &owner, &attempts))
		return state, owner, attempts
	}

	state, owner, attempts := readJob(secondManifest.ManifestID, "derive")
	assert.Equal(t, "ready", state,
		"a non-parse job on the current head must never be leased by parse workers")
	assert.Empty(t, owner)
	assert.Zero(t, attempts)

	state, owner, _ = readJob(firstManifest.ManifestID, "derive")
	assert.Equal(t, "ready", state,
		"an obsolete non-parse job must never be superseded by the parse claim path")
	assert.Empty(t, owner)

	state, _, _ = readJob(firstManifest.ManifestID, "parse")
	assert.Equal(t, "superseded", state,
		"the bounded claim-path cleanup must still retire obsolete parse jobs")
}

// TestClaimRawParseJobsRunsUnderRestrictedParseWorkerPrivileges pins the
// worker-role contract for both claim paths from the least-privileged seat:
// a role that may read custody rows but only update raw_ingest_jobs must be
// able to fill a full claim batch and to run the short-claim obsolete
// cleanup. PostgreSQL demands UPDATE privilege on every table a locking
// clause touches, so a regression that locks joined manifest rows fails
// here with permission denied even though the privileged seeding role
// would never notice, and the documented clause order (LIMIT before the
// locking clause) is exercised by both statements end to end.
func TestClaimRawParseJobsRunsUnderRestrictedParseWorkerPrivileges(t *testing.T) {
	pgURL := testPGURL(t)
	pg, store := newRawIngestTestStore(t)
	const role = "agentsview_parse_worker"
	const rolePassword = "agentsview_parse_worker_pw"

	_, _ = pg.Exec(`DROP OWNED BY ` + role)
	_, _ = pg.Exec(`DROP ROLE IF EXISTS ` + role)
	_, err := pg.Exec(`CREATE ROLE ` + role + ` LOGIN PASSWORD '` + rolePassword + `'`)
	require.NoError(t, err, "create parse worker role")
	t.Cleanup(func() {
		_, _ = pg.Exec(`DROP OWNED BY ` + role)
		_, _ = pg.Exec(`DROP ROLE IF EXISTS ` + role)
	})
	for _, grant := range []string{
		`GRANT USAGE ON SCHEMA ` + schemaTestSchema + ` TO ` + role,
		`GRANT SELECT ON ` + schemaTestSchema + `.raw_manifests TO ` + role,
		`GRANT SELECT ON ` + schemaTestSchema + `.raw_source_heads TO ` + role,
		`GRANT SELECT, UPDATE ON ` + schemaTestSchema + `.raw_ingest_jobs TO ` + role,
	} {
		_, err = pg.Exec(grant)
		require.NoError(t, err, grant)
	}
	workerURL, err := url.Parse(pgURL)
	require.NoError(t, err)
	workerURL.User = url.UserPassword(role, rolePassword)
	workerDB, err := Open(workerURL.String(), schemaTestSchema, true)
	require.NoError(t, err, "Open parse worker")
	t.Cleanup(func() { require.NoError(t, workerDB.Close()) })
	workerStore, err := NewRawIngestStore(workerDB)
	require.NoError(t, err)

	// Two sources, each with one legacy obsolete ready job behind a
	// current-head ready job, so a full claim skips cleanup while a starved
	// claim must run it.
	headManifestIDs := make([]string, 0, 2)
	for i, tenant := range []string{"tenant-a", "tenant-b"} {
		identity := rawIngestIdentity(t, tenant)
		object := rawIngestObject(t, string(rune('a'+i)), 7)
		require.NoError(t, store.RecordVerifiedObject(t.Context(), identity, object))
		firstManifest := rawIngestManifest(
			t, identity, "capture-a", "", rawIngestCapturedAt(), object,
		)
		firstCommit, err := store.CommitManifest(t.Context(), firstManifest, "parser-data-17")
		require.NoError(t, err)
		secondManifest := rawIngestManifest(
			t, identity, "capture-b", firstCommit.Receipt,
			rawIngestCapturedAt().Add(time.Minute), object,
		)
		commit, err := store.CommitManifest(t.Context(), secondManifest, "parser-data-17")
		require.NoError(t, err)
		headManifestIDs = append(headManifestIDs, commit.ManifestID)
	}
	reOpenSupersededRawParseJobs(t, pg)
	countJobs := func(state string) int {
		var count int
		require.NoError(t, pg.QueryRowContext(t.Context(), `
			SELECT COUNT(*) FROM raw_ingest_jobs WHERE state = $1`, state,
		).Scan(&count))
		return count
	}

	leases, err := workerStore.ClaimRawParseJobs(t.Context(), "worker-a", 2, time.Minute)

	require.NoError(t, err,
		"a restricted parse worker must fill a full claim batch")
	require.Len(t, leases, 2)
	assert.ElementsMatch(t, headManifestIDs,
		[]string{leases[0].ManifestID, leases[1].ManifestID},
		"the restricted claim must lease only current source heads")
	assert.Equal(t, 2, countJobs("leased"))
	assert.Equal(t, 2, countJobs("ready"),
		"a full restricted claim must not disturb the legacy obsolete rows")

	starved, err := workerStore.ClaimRawParseJobs(t.Context(), "worker-b", 1, time.Minute)

	require.NoError(t, err,
		"a restricted parse worker must run the obsolete-job cleanup")
	assert.Empty(t, starved)
	assert.Equal(t, 2, countJobs("superseded"),
		"the restricted short claim must retire legacy obsolete jobs")
}

func claimOneRawParseJob(t *testing.T, store *RawIngestStore) rawderive.JobLease {
	t.Helper()
	leases, err := store.ClaimRawParseJobs(t.Context(), "worker-a", 1, time.Minute)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	return leases[0]
}
