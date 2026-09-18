package rawsync

import (
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobHealthQueryRequiresPositiveThresholds(t *testing.T) {
	t.Parallel()

	valid := JobHealthQuery{MaxAttempts: 5, StaleAfterSeconds: 3600}
	assert.NoError(t, valid.Validate())

	for _, query := range []JobHealthQuery{
		{MaxAttempts: 0, StaleAfterSeconds: 1},
		{MaxAttempts: -1, StaleAfterSeconds: 1},
		{MaxAttempts: 1, StaleAfterSeconds: 0},
		{MaxAttempts: 1, StaleAfterSeconds: -1},
	} {
		assert.ErrorIs(t, query.Validate(), ErrInvalid, "%+v", query)
	}
}

func TestJobHealthReportUsesSafeSnakeCaseWireFields(t *testing.T) {
	t.Parallel()

	report := JobHealthReport{
		ObservedAt:             time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC),
		MaxAttempts:            5,
		StaleAfterSeconds:      3600,
		OrphanedManifests:      []OrphanedManifest{},
		ExpiredLeases:          []ExpiredLease{},
		FailedJobsByErrorClass: []JobFailureClass{},
		RetryingNearLimit:      []JobAttemptWarning{},
		StaleSourceHeads:       []StaleSourceHead{},
	}
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	body := string(raw)
	for _, field := range []string{
		`"observed_at"`,
		`"max_attempts"`,
		`"stale_after_seconds"`,
		`"orphaned_manifests":[]`,
		`"expired_leases":[]`,
		`"failed_jobs_by_error_class":[]`,
		`"retrying_near_limit":[]`,
		`"stale_source_heads":[]`,
	} {
		assert.Contains(t, body, field)
	}
	assert.NotContains(t, body, "last_error")
}

func TestJobHealthRowsDoNotExposeStoredErrorMessages(t *testing.T) {
	t.Parallel()

	row := JobAttemptWarning{LastErrorClass: "parse"}
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"last_error_class":"parse"`)
	assert.NotContains(t, string(raw), `"last_error":`)
}
