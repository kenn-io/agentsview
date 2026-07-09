package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestOpenCodeStorageInvalidationVetoesStalePromotion pins the gate's
// snapshot invariant: an invalidation landing between a pass's pre-parse
// capture and its post-write promotion must win. Watcher changed-path
// classification runs outside syncMu, so it can invalidate a session while
// a full sync is mid-pass; without the veto the pass's promotion would
// restore the pre-event signature and a same-size, same-mtime rewrite —
// the exact case the event invalidation exists to catch — would be skipped
// on every later pass.
func TestOpenCodeStorageInvalidationVetoesStalePromotion(t *testing.T) {
	const path = "/data/storage/session/proj/ses_1.json"
	const state = "sig-before-event"

	t.Run("invalidation after capture blocks promotion", func(t *testing.T) {
		e := &Engine{}
		snap := e.storageTrustSnapshotFor(path)
		e.invalidateOpenCodeStorageSession(path)
		e.promoteOpenCodeStorageSession(path, state, snap)
		assert.False(t, e.openCodeStorageSessionFresh(path, state),
			"a promotion captured before the invalidation must be discarded")
	})

	t.Run("clear after capture blocks promotion", func(t *testing.T) {
		e := &Engine{}
		snap := e.storageTrustSnapshotFor(path)
		e.clearTrustedOpenCodeStorageSessions()
		e.promoteOpenCodeStorageSession(path, state, snap)
		assert.False(t, e.openCodeStorageSessionFresh(path, state),
			"a promotion captured before a resync clear must be discarded")
	})

	t.Run("undisturbed capture promotes", func(t *testing.T) {
		e := &Engine{}
		snap := e.storageTrustSnapshotFor(path)
		e.promoteOpenCodeStorageSession(path, state, snap)
		assert.True(t, e.openCodeStorageSessionFresh(path, state),
			"a promotion with no intervening invalidation must land")
	})

	t.Run("own pass's in-flight drop does not veto", func(t *testing.T) {
		e := &Engine{}
		snap := e.storageTrustSnapshotFor(path)
		// stageOpenCodeStorageTrust drops trust while staged results are
		// in flight to the write batch; that drop belongs to the same
		// pass and must not block the pass's own promotion.
		e.dropOpenCodeStorageTrust(path)
		e.promoteOpenCodeStorageSession(path, state, snap)
		assert.True(t, e.openCodeStorageSessionFresh(path, state),
			"the staging drop must not veto the same pass's promotion")
	})

	t.Run("invalidation bumps only its own session", func(t *testing.T) {
		e := &Engine{}
		const other = "/data/storage/session/proj/ses_2.json"
		snap := e.storageTrustSnapshotFor(path)
		e.invalidateOpenCodeStorageSession(other)
		e.promoteOpenCodeStorageSession(path, state, snap)
		assert.True(t, e.openCodeStorageSessionFresh(path, state),
			"an unrelated session's invalidation must not veto this promotion")
	})
}
