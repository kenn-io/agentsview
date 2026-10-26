package review_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/friction/review"
)

func TestJob(t *testing.T) {
	t.Run("exclusive", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		seed(t, d, subject{id: "s", ended: "2026-09-21T10:00:00Z"})
		r := newRunner(d)
		r.BackfillDays = 1
		calls := 0
		job := review.Job(r, func(work func() error) error { calls++; return work() })
		require.NoError(t, job.Run(t.Context()))
		assert.Equal(t, 1, calls, "one date is built under the exclusive section")
		got, err := d.GetFrictionDigest(t.Context(), "2026-09-21")
		require.NoError(t, err)
		assert.NotNil(t, got)
	})

	t.Run("direct", func(t *testing.T) {
		d := dbtest.OpenTestDB(t)
		seed(t, d, subject{id: "s", ended: "2026-09-21T10:00:00Z"})
		direct := review.Job(newRunner(d), nil)
		require.NoError(t, direct.Run(t.Context()), "nil exclusive runs the work directly")
		got, err := d.GetFrictionDigest(t.Context(), "2026-09-21")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, 1, got.SessionsScanned)
	})
}

func TestJobYieldsBetweenDates(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	seed(t, d, subject{id: "first", ended: "2026-09-20T10:00:00Z"})
	released := false
	r := newRunner(d)
	r.BackfillDays = 2
	job := review.Job(r, func(work func() error) error {
		err := work()
		if !released {
			released = true
			require.NotNil(t, stored(t, d, "2026-09-20"))
			assert.Nil(t, stored(t, d, "2026-09-21"), "the first day releases the barrier before the second is built")
			// A sync between dates supplies a session that the next build must see.
			seed(t, d, subject{id: "arrived", ended: "2026-09-21T10:00:00Z"})
		}
		return err
	})
	require.NoError(t, job.Run(t.Context()))
	got := stored(t, d, "2026-09-21")
	require.NotNil(t, got)
	assert.Equal(t, 1, got.SessionsScanned)
}
