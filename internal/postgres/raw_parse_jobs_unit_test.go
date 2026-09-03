package postgres

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/rawderive"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestValidateRawParseLeaseRequestBoundsClaimBatch(t *testing.T) {
	t.Parallel()

	require.NoError(t, validateRawParseLeaseRequest(
		"worker-a", rawderive.MaxClaimBatchSize, time.Minute,
	))

	err := validateRawParseLeaseRequest("worker-a", 0, time.Minute)
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
	err = validateRawParseLeaseRequest("worker-a", -1, time.Minute)
	assert.ErrorIs(t, err, rawsync.ErrInvalid)
	err = validateRawParseLeaseRequest("worker-a", rawderive.MaxClaimBatchSize+1, time.Minute)
	assert.ErrorIs(t, err, rawsync.ErrInvalid,
		"the queue and worker must share one claim cap from the rawderive contract")
}
