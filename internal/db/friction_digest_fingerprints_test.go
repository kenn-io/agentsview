package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSaveFrictionDigestIndexesFrozenFingerprints(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	var err error
	firstFingerprint := "fl1:b6f2290da39465fa9590ce7b40a7624a797b4b041af17c867f80bb86160be012"
	secondFingerprint := "fl1:a61b5444f8c05fd85d97ef438a49ec26a65f6cdfd342eeba239f2f4a95d6f89c"
	firstSnapshot := []byte(`{"signals":[{"kind":"error","subject_id":"claude:first","tool_name":"Bash","text":"first"},{"kind":"error","subject_id":"claude:second","tool_name":"Bash","text":"second"},{"kind":"error","subject_id":"claude:first","tool_name":"Bash","text":"first"}]}`)
	digest := FrictionDigest{
		Date: "2026-09-15", Timezone: "UTC", RulesVersion: "friction-v1",
		BuiltAt: time.Date(2026, 9, 22, 1, 0, 0, 0, time.UTC), Revision: 1,
		SnapshotJSON: firstSnapshot, SummaryJSON: []byte("{}\n"), Markdown: []byte("# digest\n"),
		MarkdownSHA256: "sha", RunID: "run-1",
	}
	require.NoError(t, d.SaveFrictionDigest(ctx, digest, nil, nil))
	assert.Equal(t, []string{secondFingerprint, firstFingerprint}, frictionDigestFingerprintRows(t, d, "2026-09-15"))
	emptyDigest := digest
	emptyDigest.Date = "2026-09-17"
	emptyDigest.SnapshotJSON = []byte(`{"signals":[]}`)
	emptyDigest.RunID = "run-empty"
	require.NoError(t, d.SaveFrictionDigest(ctx, emptyDigest, nil, nil))
	emptyDates, err := d.DigestDatesForFingerprints(ctx, []string{""})
	require.NoError(t, err)
	assert.Empty(t, emptyDates)
	_, err = d.getWriter().ExecContext(ctx,
		`DELETE FROM friction_digest_fingerprints WHERE date = ?`, digest.Date)
	require.NoError(t, err)
	d.mu.Lock()
	err = d.backfillFrictionDigestFingerprintIndex(ctx, d.getWriter())
	d.mu.Unlock()
	require.NoError(t, err)

	// Link refreshes use the saved index and must not reparse each full snapshot.
	_, err = d.getWriter().ExecContext(ctx,
		`UPDATE friction_digests SET snapshot_json = '{"signals":[]}' WHERE date = ?`, digest.Date)
	require.NoError(t, err)
	got, err := d.DigestDatesForFingerprints(ctx, []string{firstFingerprint, secondFingerprint, ""})
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{
		firstFingerprint:  {"2026-09-15"},
		secondFingerprint: {"2026-09-15"},
	}, got)

	digest.Revision = 2
	digest.SnapshotJSON = []byte(`{"signals":[{"kind":"error","subject_id":"claude:second","tool_name":"Bash","text":"second"}]}`)
	digest.RunID = "run-2"
	require.NoError(t, d.SaveFrictionDigest(ctx, digest, nil, nil))
	assert.Equal(t, []string{secondFingerprint}, frictionDigestFingerprintRows(t, d, digest.Date))

	got, err = d.DigestDatesForFingerprints(ctx, []string{firstFingerprint, secondFingerprint, ""})
	require.NoError(t, err)
	assert.Equal(t, map[string][]string{secondFingerprint: {digest.Date}}, got)
}

func frictionDigestFingerprintRows(t *testing.T, d *DB, date string) []string {
	t.Helper()
	rows, err := d.getReader().QueryContext(t.Context(), `
		SELECT fingerprint FROM friction_digest_fingerprints WHERE date = ? ORDER BY fingerprint`, date)
	require.NoError(t, err)
	defer rows.Close()
	var fingerprints []string
	for rows.Next() {
		var fingerprint string
		require.NoError(t, rows.Scan(&fingerprint))
		fingerprints = append(fingerprints, fingerprint)
	}
	require.NoError(t, rows.Err())
	return fingerprints
}
