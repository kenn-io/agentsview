package review_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction/review"
)

func TestJob(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "s", ended: "2026-09-21T10:00:00Z"})
	r := newRunner(d)
	calls := 0
	job := review.Job(r, func(work func() error) error { calls++; return work() })
	assert.Equal(t, "friction-review", job.Name)
	assert.Equal(t, review.JobName, job.Name)
	assert.Equal(t, time.Hour, job.Interval)
	assert.Equal(t, 5*time.Minute, job.Jitter)
	assert.Equal(t, 10*time.Minute, job.Cooldown)
	assert.True(t, job.RunAtStart)
	require.NoError(t, job.Run(t.Context()))
	assert.Equal(t, 1, calls, "the whole catch-up runs under one exclusive section")
	got, err := d.GetFrictionDigest(t.Context(), "2026-09-21")
	require.NoError(t, err)
	assert.NotNil(t, got)

	direct := review.Job(r, nil)
	require.NoError(t, direct.Run(t.Context()), "nil exclusive runs the work directly")
}
