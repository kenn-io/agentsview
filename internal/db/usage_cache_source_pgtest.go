//go:build pgtest

package db

import "time"

// SetWarmRetryDelayForTest sets the retry delay before the reader starts work.
func (reader *UsageCacheReader) SetWarmRetryDelayForTest(delay time.Duration) {
	reader.warmRetryDelay = delay
}
