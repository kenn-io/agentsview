package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunPeriodicPricingRefreshContinuesAfterFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 2)
	done := make(chan struct{})
	var attempts atomic.Int32

	go func() {
		runPeriodicPricingRefresh(ctx, ticks, func(context.Context) error {
			if attempts.Add(1) == 1 {
				return errors.New("temporary pricing failure")
			}
			return nil
		})
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		require.Eventually(t, func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		}, time.Second, time.Millisecond)
	})

	ticks <- time.Time{}
	require.Eventually(t, func() bool {
		return attempts.Load() == 1
	}, time.Second, time.Millisecond)

	ticks <- time.Time{}
	require.Eventually(t, func() bool {
		return attempts.Load() == 2
	}, time.Second, time.Millisecond)
}
