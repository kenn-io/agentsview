package remotesync

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupRegistryRetriesBeforeReturningAndBeforeLaterWork(t *testing.T) {
	operationErr := errors.New("operation failed")
	cleanupErr := errors.New("cleanup failed")
	owner := &cleanupRetryTestError{
		cause: operationErr,
		results: []error{
			cleanupErr,
			cleanupErr,
			cleanupErr,
			nil,
		},
	}
	var registry CleanupRegistry
	runs := 0

	_, err := registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{}, owner
	})
	require.Same(t, owner, err)
	assert.ErrorIs(t, err, operationErr)
	assert.Equal(t, 1, owner.retryCount())
	assert.Equal(t, 1, runs)

	_, err = registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{}, nil
	})
	var pending *PendingCleanupError
	require.ErrorAs(t, err, &pending)
	assert.NotSame(t, owner, err)
	assert.Same(t, owner, pending.Err)
	assert.ErrorIs(t, err, owner)
	assert.ErrorIs(t, err, operationErr)
	assert.Equal(t, 2, owner.retryCount())
	assert.Equal(t, 1, runs, "retained cleanup blocks new work")

	_, err = registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{}, nil
	})
	require.ErrorAs(t, err, &pending)
	assert.ErrorIs(t, err, owner)
	assert.Equal(t, 3, owner.retryCount())
	assert.Equal(t, 1, runs, "later calls remain explicitly blocked")

	_, err = registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{SessionsSynced: 1}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 4, owner.retryCount())
	assert.Equal(t, 2, runs, "new work starts only after retained ownership releases")
}

func TestCleanupRegistryRetriesEveryDistinctJoinedOwner(t *testing.T) {
	firstFailure := errors.New("first cleanup still blocked")
	first := &cleanupRetryTestError{
		cause:   errors.New("first cleanup owner"),
		results: []error{firstFailure, nil},
	}
	secondFailure := errors.New("second cleanup still blocked")
	second := &cleanupRetryTestError{
		cause:   errors.New("second cleanup owner"),
		results: []error{secondFailure, secondFailure, nil},
	}
	joined := errors.Join(
		fmt.Errorf("wrapped first owner: %w", first),
		second,
		fmt.Errorf("duplicate first owner: %w", first),
	)
	var registry CleanupRegistry
	runs := 0

	_, err := registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{}, joined
	})
	require.Same(t, joined, err)
	assert.Equal(t, 1, first.retryCount(),
		"one owner appearing twice in the error tree is retried once")
	assert.Equal(t, 1, second.retryCount(),
		"a sibling owner is retried even when the first owner retains cleanup")

	_, err = registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{SessionsSynced: 1}, nil
	})
	var pending *PendingCleanupError
	require.ErrorAs(t, err, &pending)
	assert.ErrorIs(t, err, first.cause)
	assert.ErrorIs(t, err, second.cause)
	assert.Equal(t, 2, first.retryCount())
	assert.Equal(t, 2, second.retryCount())
	assert.Equal(t, 1, runs,
		"new work stays blocked while any retained owner still fails")

	_, err = registry.Run(func() (SyncStats, error) {
		runs++
		return SyncStats{SessionsSynced: 1}, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, first.retryCount(),
		"successful owners are not retried with the retained failures")
	assert.Equal(t, 3, second.retryCount())
	assert.Equal(t, 2, runs,
		"new work starts after every retained owner releases")
}

func TestCleanupRegistrySerializesConcurrentWork(t *testing.T) {
	var registry CleanupRegistry
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondAttempting := make(chan struct{})
	secondStarted := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = registry.Run(func() (SyncStats, error) {
			close(firstStarted)
			<-releaseFirst
			return SyncStats{}, nil
		})
	}()
	<-firstStarted
	go func() {
		defer wg.Done()
		close(secondAttempting)
		_, _ = registry.Run(func() (SyncStats, error) {
			close(secondStarted)
			return SyncStats{}, nil
		})
	}()
	<-secondAttempting

	select {
	case <-secondStarted:
		require.FailNow(t, "concurrent work entered before the active run completed")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseFirst)
	wg.Wait()
	assertClosed(t, secondStarted)
}

type cleanupRetryTestError struct {
	mu      sync.Mutex
	cause   error
	results []error
	retries int
}

func (e *cleanupRetryTestError) Error() string { return e.cause.Error() }

func (e *cleanupRetryTestError) Unwrap() error { return e.cause }

func (e *cleanupRetryTestError) RetryCleanup() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	result := e.results[e.retries]
	e.retries++
	return result
}

func (e *cleanupRetryTestError) retryCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.retries
}

func assertClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	default:
		assert.Fail(t, "channel remains open")
	}
}
