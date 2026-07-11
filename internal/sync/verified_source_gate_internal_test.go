package sync

import (
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func verifiedSourceSignatureForTest(seed int64) verifiedSourceSignature {
	return verifiedSourceSignature{
		size: seed + 10, mtime: seed + 20,
		inode: seed + 30, device: seed + 40,
		changeTime: seed + 50, sidecarSize: seed + 60,
		sidecarMtime: seed + 70, sidecarInode: seed + 80,
		sidecarDevice: seed + 90, sidecarChangeTime: seed + 100,
	}
}

func TestVerifiedSourceGatePromotionAndInvalidation(t *testing.T) {
	const path = "archive/session-a.jsonl"
	signature := verifiedSourceSignatureForTest(1)

	t.Run("promotion trusts the captured signature", func(t *testing.T) {
		e := &Engine{}
		capture, fresh := e.captureVerifiedSource(path, signature)
		assert.False(t, fresh)
		e.promoteVerifiedSource(capture)

		_, fresh = e.captureVerifiedSource(path, signature)
		assert.True(t, fresh)
		_, fresh = e.captureVerifiedSource(
			path, verifiedSourceSignatureForTest(2),
		)
		assert.False(t, fresh, "a changed signature must re-verify")
	})

	t.Run("path invalidation vetoes stale promotion", func(t *testing.T) {
		e := &Engine{}
		capture, _ := e.captureVerifiedSource(path, signature)
		e.invalidateVerifiedSource(path)
		e.promoteVerifiedSource(capture)

		_, fresh := e.captureVerifiedSource(path, signature)
		assert.False(t, fresh)
	})

	t.Run("global clear vetoes stale promotion", func(t *testing.T) {
		e := &Engine{}
		capture, _ := e.captureVerifiedSource(path, signature)
		e.clearVerifiedSources()
		e.promoteVerifiedSource(capture)

		_, fresh := e.captureVerifiedSource(path, signature)
		assert.False(t, fresh)
	})

	t.Run("unrelated invalidation does not veto promotion", func(t *testing.T) {
		e := &Engine{}
		capture, _ := e.captureVerifiedSource(path, signature)
		e.invalidateVerifiedSource("archive/session-b.jsonl")
		e.promoteVerifiedSource(capture)

		_, fresh := e.captureVerifiedSource(path, signature)
		assert.True(t, fresh)
	})
}

func TestVerifiedSourceGateFullPassPruning(t *testing.T) {
	e := &Engine{}
	keepSignature := verifiedSourceSignatureForTest(1)
	dropSignature := verifiedSourceSignatureForTest(2)

	firstPass := e.beginVerifiedSourcePass()
	keepCapture, keepFresh := e.captureVerifiedSource(
		"archive/keep.jsonl", keepSignature,
	)
	dropCapture, dropFresh := e.captureVerifiedSource(
		"archive/drop.jsonl", dropSignature,
	)
	require.False(t, keepFresh)
	require.False(t, dropFresh)
	e.promoteVerifiedSource(keepCapture)
	e.promoteVerifiedSource(dropCapture)
	e.finishVerifiedSourcePass(firstPass, true)
	require.Len(t, e.verifiedSources, 2)

	secondPass := e.beginVerifiedSourcePass()
	_, keepFresh = e.captureVerifiedSource("archive/keep.jsonl", keepSignature)
	require.True(t, keepFresh)
	e.finishVerifiedSourcePass(secondPass, true)

	require.Len(t, e.verifiedSources, 1)
	_, ok := e.verifiedSources["archive/keep.jsonl"]
	assert.True(t, ok)

	incompletePass := e.beginVerifiedSourcePass()
	e.finishVerifiedSourcePass(incompletePass, false)
	assert.Len(t, e.verifiedSources, 1,
		"an incomplete pass must not prune trusted sources")
}

func TestVerifiedSourceGateInvalidationSurvivesActivePassPruning(t *testing.T) {
	const path = "archive/session.jsonl"
	e := &Engine{}
	signature := verifiedSourceSignatureForTest(1)

	pass := e.beginVerifiedSourcePass()
	capture, _ := e.captureVerifiedSource(path, signature)
	e.invalidateVerifiedSource(path)
	e.finishVerifiedSourcePass(pass, true)

	require.Len(t, e.verifiedSources, 1,
		"the invalidation record must survive its active pass")
	e.promoteVerifiedSource(capture)
	_, fresh := e.captureVerifiedSource(path, signature)
	assert.False(t, fresh,
		"pruning must not erase the generation that vetoes stale promotion")
}

func TestVerifiedSourceGateRetainedBudget(t *testing.T) {
	const (
		smallCount = 100
		largeCount = 47_000
		budget     = 16 << 20
	)

	retainedBytes := func(count int) uint64 {
		t.Helper()
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		e := &Engine{}
		pass := e.beginVerifiedSourcePass()
		for i := range count {
			path := "archive/provider/session-" + strconv.Itoa(i) + ".jsonl"
			capture, _ := e.captureVerifiedSource(
				path, verifiedSourceSignatureForTest(int64(i)),
			)
			e.promoteVerifiedSource(capture)
		}
		e.finishVerifiedSourcePass(pass, true)
		require.Len(t, e.verifiedSources, count)

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(e)
		if after.HeapAlloc <= before.HeapAlloc {
			return 0
		}
		return after.HeapAlloc - before.HeapAlloc
	}

	smallBytes := retainedBytes(smallCount)
	largeBytes := retainedBytes(largeCount)
	t.Logf("verified-source retained bytes: %d entries=%d, %d entries=%d",
		smallCount, smallBytes, largeCount, largeBytes)
	assert.Greater(t, largeBytes, smallBytes)
	assert.Less(t, largeBytes, uint64(budget),
		"47k source records exceed the retained-memory budget")

	e := &Engine{}
	firstPass := e.beginVerifiedSourcePass()
	for i := range largeCount {
		path := "archive/provider/session-" + strconv.Itoa(i) + ".jsonl"
		capture, _ := e.captureVerifiedSource(
			path, verifiedSourceSignatureForTest(int64(i)),
		)
		e.promoteVerifiedSource(capture)
	}
	e.finishVerifiedSourcePass(firstPass, true)
	secondPass := e.beginVerifiedSourcePass()
	for i := range smallCount {
		path := "archive/provider/session-" + strconv.Itoa(i) + ".jsonl"
		e.captureVerifiedSource(path, verifiedSourceSignatureForTest(int64(i)))
	}
	e.finishVerifiedSourcePass(secondPass, true)
	assert.Len(t, e.verifiedSources, smallCount,
		"pruning must return retained state to the active set")
}
