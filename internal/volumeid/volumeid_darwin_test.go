//go:build darwin

package volumeid

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

type mount struct {
	dev  int32
	path string
	uuid string // empty for a volume that reports no UUID
}

// world is a fake mount table, volume UUIDs and clock. A background refresh
// reads it from another goroutine, so every field is behind mu.
type world struct {
	mu          sync.Mutex
	mounts      []mount
	remote      map[string]bool // mount points that are not local
	automounted map[string]bool // mount points that are automounted
	asked       []string        // mount points asked for a UUID, in order
	uuidFails   map[string]bool // mount points whose UUID query fails
	fail        bool
	reads       int // mount table reads attempted
	clock       time.Time
	// gate, when set, is called before a mount point is asked for its UUID.
	gate func(path string)
}

func (w *world) advance(d time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.clock = w.clock.Add(d)
}

func (w *world) set(f func(w *world)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

func (w *world) get(f func(w *world)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	f(w)
}

// resetPackage clears everything a test left in the package. It takes buildMu
// before clearing, so a lookup still reading the mount table finishes first
// and cannot write its fake table back afterwards.
func resetPackage() {
	refreshing.Wait()
	buildMu.Lock()
	defer buildMu.Unlock()
	querying.Wait()
	mu.Lock()
	byDevice, readAt, attemptAt = nil, time.Time{}, time.Time{}
	mu.Unlock()
	previous = nil
	pendingMu.Lock()
	pending = nil
	pendingMu.Unlock()
}

func newWorld(t *testing.T, mounts ...mount) *world {
	t.Helper()
	w := &world{
		mounts:      mounts,
		remote:      map[string]bool{},
		automounted: map[string]bool{},
		uuidFails:   map[string]bool{},
		clock:       time.Unix(1_000_000, 0),
	}
	origGet, origUUID, origNow, origTimeout := getfsstat, volumeUUID, now, queryTimeout
	now = func() time.Time {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.clock
	}
	getfsstat = func(buf []unix.Statfs_t, _ int) (int, error) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if buf == nil {
			w.reads++
		}
		if w.fail {
			return 0, errors.New("getfsstat failed")
		}
		if buf == nil {
			return len(w.mounts), nil
		}
		for i, m := range w.mounts {
			buf[i] = unix.Statfs_t{}
			buf[i].Fsid.Val[0] = m.dev
			copy(buf[i].Mntonname[:], m.path)
			// As on a real system, the disk name follows the device, not the
			// volume: a replacement drive can reuse all three.
			copy(buf[i].Mntfromname[:], fmt.Sprintf("/dev/disk%d", m.dev))
			if !w.remote[m.path] {
				buf[i].Flags |= unix.MNT_LOCAL
			}
			if w.automounted[m.path] {
				buf[i].Flags |= unix.MNT_AUTOMOUNTED
			}
		}
		return len(w.mounts), nil
	}
	volumeUUID = func(path string) ([16]byte, uuidAnswer) {
		// The answer is about the volume mounted when the query starts, as
		// with a real call, even if it is held up and returned later.
		w.mu.Lock()
		gate := w.gate
		w.asked = append(w.asked, path) // recorded as asked, answered or not
		uuid, answer := [16]byte{}, uuidNone
		if w.uuidFails[path] {
			answer = uuidFailed
		}
		for _, m := range w.mounts {
			if answer == uuidNone && m.path == path && m.uuid != "" {
				copy(uuid[:], m.uuid)
				answer = uuidFound
			}
		}
		w.mu.Unlock()
		if gate != nil {
			gate(path)
		}
		return uuid, answer
	}
	resetPackage()
	t.Cleanup(func() {
		resetPackage()
		getfsstat, volumeUUID, now, queryTimeout = origGet, origUUID, origNow, origTimeout
	})
	return w
}

// reboot discards everything a running process had read, as a new boot does.
func (w *world) reboot(mounts ...mount) {
	resetPackage()
	w.set(func(w *world) { w.mounts = mounts })
}

// holdSlowVolume blocks the UUID query for path until the test ends or
// release is called, and returns a channel that is closed once a query is
// blocked. The release is registered as a cleanup after newWorld's, so a test
// that fails first unblocks the query before the package is reset.
func holdSlowVolume(t *testing.T, w *world, path string) (entered <-chan struct{}, release func()) {
	t.Helper()
	in, out := make(chan struct{}), make(chan struct{})
	var inOnce, outOnce sync.Once
	release = func() { outOnce.Do(func() { close(out) }) }
	t.Cleanup(release)
	w.set(func(w *world) {
		earlier := w.gate // so several volumes can be held at once
		w.gate = func(p string) {
			if p == path {
				inOnce.Do(func() { close(in) })
				<-out
			} else if earlier != nil {
				earlier(p)
			}
		}
	})
	return in, release
}

// within calls f and fails the test if it has not returned in five seconds.
func within(t *testing.T, what string, f func() uint64) uint64 {
	t.Helper()
	got := make(chan uint64, 1)
	go func() { got <- f() }()
	select {
	case v := <-got:
		return v
	case <-time.After(5 * time.Second):
		require.Fail(t, what+" did not return")
		return 0
	}
}

func TestStableFollowsTheVolumeNotItsDeviceNumberOrPath(t *testing.T) {
	tests := []struct {
		name   string
		before mount
		after  mount
		same   bool
	}{
		{
			// The failure this package exists for: after a system update the
			// data volume came back under a different st_dev.
			name:   "same volume, new device number after a reboot",
			before: mount{16777231, "/System/Volumes/Data", "data-volume-uuid"},
			after:  mount{16777233, "/System/Volumes/Data", "data-volume-uuid"},
			same:   true,
		},
		{
			name:   "same volume mounted at a different path",
			before: mount{40, "/Volumes/Work", "external-uuid-1"},
			after:  mount{41, "/Volumes/Work 1", "external-uuid-1"},
			same:   true,
		},
		{
			// Two drives that both mount as Untitled: a mount point names a
			// place, so it must not decide that two volumes are one.
			name:   "different volume later mounted at the same path",
			before: mount{40, "/Volumes/Untitled", "external-uuid-1"},
			after:  mount{40, "/Volumes/Untitled", "external-uuid-2"},
			same:   false,
		},
		{
			name:   "a volume with no UUID keeps its mount point across a reboot",
			before: mount{50, "/Volumes/share", ""},
			after:  mount{51, "/Volumes/share", ""},
			same:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t, tt.before)
			first := Stable(uint64(tt.before.dev))
			w.reboot(tt.after)
			second := Stable(uint64(tt.after.dev))

			assert.NotEqual(t, uint64(tt.before.dev), first, "mapped, not passed through")
			if tt.same {
				assert.Equal(t, first, second)
			} else {
				assert.NotEqual(t, first, second)
			}
		})
	}
}

func TestStableKeepsMountedVolumesApartAndIsPositive(t *testing.T) {
	newWorld(t,
		mount{1, "/System/Volumes/Data", "data-volume-uuid"},
		mount{2, "/Volumes/External", "external-uuid-1"},
		mount{3, "/dev", ""},
		mount{4, "/Volumes/share", ""},
	)
	values := map[uint64]bool{}
	for _, dev := range []uint64{1, 2, 3, 4} {
		v := Stable(dev)
		assert.NotZero(t, v)
		assert.Less(t, v, uint64(1)<<63, "stored as a signed integer, so kept positive")
		values[v] = true
	}
	assert.Len(t, values, 4)
}

func TestStableMatchesASignExtendedDeviceNumber(t *testing.T) {
	// st_dev is a signed 32-bit number on macOS, and devfs is negative. Callers
	// widen it with sign extension; the mount table does not.
	newWorld(t, mount{-1711263034, "/dev", ""})
	var dev int32 = -1711263034
	signExtended, zeroExtended := uint64(dev), uint64(uint32(dev))

	got := Stable(signExtended)
	assert.NotEqual(t, signExtended, got)
	assert.Equal(t, Stable(zeroExtended), got)
	assert.Equal(t, got, Stable(dev), "st_dev passed as the int32 macOS declares")
}

func TestStableReturnsAnUnknownDeviceUnchanged(t *testing.T) {
	newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	assert.Equal(t, uint64(99), Stable(uint64(99)))
}

func TestStablePicksUpANewMountAfterTheRetryWindow(t *testing.T) {
	w := newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	assert.Equal(t, uint64(2), Stable(uint64(2)), "not mounted yet")

	w.set(func(w *world) {
		w.mounts = append(w.mounts, mount{2, "/Volumes/External", "external-uuid-1"})
	})
	assert.Equal(t, uint64(2), Stable(uint64(2)), "within a second of the last read")

	w.advance(retryAfter + time.Millisecond)
	assert.NotEqual(t, uint64(2), Stable(uint64(2)), "read again once the window passed")
}

func TestStableRereadsAReassignedDeviceNumberAfterTheRefreshWindow(t *testing.T) {
	w := newWorld(t, mount{5, "/Volumes/Untitled", "external-uuid-1"})
	first := Stable(uint64(5))

	// The drive is swapped without the process restarting, and the new one
	// is handed the same device number.
	w.set(func(w *world) { w.mounts = []mount{{5, "/Volumes/Untitled", "external-uuid-2"}} })
	w.advance(refreshAfter / 2)
	assert.Equal(t, first, Stable(uint64(5)), "the table is trusted inside the window")

	w.advance(refreshAfter)
	assert.Equal(t, first, Stable(uint64(5)), "a due refresh starts in the background")
	refreshing.Wait()
	assert.NotEqual(t, first, Stable(uint64(5)), "and its table is used once it is in")
}

func TestStableReadsAFailingMountTableAtMostOnceASecond(t *testing.T) {
	w := newWorld(t)
	w.set(func(w *world) { w.fail = true })
	for range 100 {
		assert.Equal(t, uint64(7), Stable(uint64(7)))
	}
	w.get(func(w *world) { assert.Equal(t, 1, w.reads) })

	w.advance(retryAfter + time.Millisecond)
	Stable(uint64(7))
	w.get(func(w *world) { assert.Equal(t, 2, w.reads) })
}

func TestStableKeepsTheLastTableWhenAReadFails(t *testing.T) {
	w := newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	want := Stable(uint64(1))

	w.set(func(w *world) { w.fail = true })
	w.advance(refreshAfter + time.Second)
	Stable(uint64(1))
	refreshing.Wait()
	assert.Equal(t, want, Stable(uint64(1)))
}

func TestStableNeverAsksANetworkOrAutomountVolumeForItsUUID(t *testing.T) {
	// A hung share would hold up the mount table read, and touching an
	// automount point can trigger a mount, so neither is asked; each is known
	// by its mount point.
	w := newWorld(t,
		mount{1, "/System/Volumes/Data", "data-volume-uuid"},
		mount{2, "/Volumes/share", "share-uuid"},
		mount{3, "/Volumes/auto", "auto-uuid"},
	)
	w.set(func(w *world) {
		w.remote["/Volumes/share"] = true
		w.automounted["/Volumes/auto"] = true
	})

	share, auto := Stable(uint64(2)), Stable(uint64(3))
	Stable(uint64(1))
	w.get(func(w *world) { assert.Equal(t, []string{"/System/Volumes/Data"}, w.asked) })

	// Identified by mount point, not UUID: after a reboot under new numbers,
	// and with different UUIDs that are never read, each keeps its value.
	w.reboot(
		mount{4, "/Volumes/share", "other-share-uuid"},
		mount{5, "/Volumes/auto", "other-auto-uuid"},
	)
	assert.Equal(t, share, Stable(uint64(4)))
	assert.Equal(t, auto, Stable(uint64(5)))
}

func TestStableNeverWaitsForARefreshOfAKnownVolume(t *testing.T) {
	// A newly attached disk that is slow to answer, such as one still
	// spinning up, is asked for its UUID by the next refresh. Neither the
	// caller that notices the refresh is due nor any caller after it waits
	// for that read while the current table can serve them.
	w := newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	data := Stable(uint64(1))
	w.set(func(w *world) {
		w.mounts = append(w.mounts, mount{2, "/Volumes/Slow", "slow-uuid"})
	})
	w.advance(refreshAfter + time.Second)
	entered, _ := holdSlowVolume(t, w, "/Volumes/Slow")

	assert.Equal(t, data, within(t, "the lookup that starts the refresh", func() uint64 { return Stable(uint64(1)) }))
	<-entered
	assert.Equal(t, data, within(t, "a lookup during the refresh", func() uint64 { return Stable(uint64(1)) }))
}

func TestStableNeverReturnsARawNumberWhileTheFirstReadRuns(t *testing.T) {
	// Callers that arrive while the first mount table read is still running
	// wait for it, rather than record a raw number it is about to replace.
	w := newWorld(t,
		mount{1, "/System/Volumes/Data", "data-volume-uuid"},
		mount{2, "/Volumes/Slow", "slow-uuid"},
	)
	entered, release := holdSlowVolume(t, w, "/Volumes/Slow")
	results := make(chan uint64, 8)
	for range 8 {
		go func() { results <- Stable(uint64(1)) }()
	}
	<-entered
	time.AfterFunc(50*time.Millisecond, release)
	for range 8 {
		assert.NotEqual(t, uint64(1), <-results)
	}
}

func TestStableKeepsAKnownValueWhenAQueryWouldFail(t *testing.T) {
	// A call that fails, say with EINTR, must not switch a volume from its
	// UUID value to its mount point and back: every file on it would look
	// replaced twice.
	w := newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	want := Stable(uint64(1))

	w.set(func(w *world) { w.uuidFails["/System/Volumes/Data"] = true })
	w.advance(refreshAfter + time.Second)
	Stable(uint64(1))
	refreshing.Wait()
	assert.Equal(t, want, Stable(uint64(1)))
}

func TestStableAsksAgainAfterAFirstQueryFails(t *testing.T) {
	// Nothing is learned from a failed call, so the volume is known by its
	// mount point for now and asked again on the next read.
	w := newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	w.set(func(w *world) { w.uuidFails["/System/Volumes/Data"] = true })
	first := Stable(uint64(1))

	w.set(func(w *world) { w.uuidFails["/System/Volumes/Data"] = false })
	w.advance(refreshAfter + time.Second)
	Stable(uint64(1))
	refreshing.Wait()
	second := Stable(uint64(1))

	assert.NotEqual(t, first, second, "the UUID replaced the mount point")
	w.reboot(mount{9, "/System/Volumes/Data", "data-volume-uuid"})
	assert.Equal(t, second, Stable(uint64(9)), "and it is the volume's UUID value")
}

func TestStableDoesNotWaitLongerThanTheQueryTimeoutForAVolume(t *testing.T) {
	// A hung local volume, such as a disk image on a dropped share, must not
	// stall the first read, which every caller waits for. Past the timeout it
	// is known by its mount point; its UUID is used once it answers.
	w := newWorld(t,
		mount{1, "/System/Volumes/Data", "data-volume-uuid"},
		mount{2, "/Volumes/Hung", "hung-uuid"},
	)
	queryTimeout = 50 * time.Millisecond
	entered, release := holdSlowVolume(t, w, "/Volumes/Hung")

	hung := within(t, "the first read with a hung volume", func() uint64 { return Stable(uint64(2)) })
	<-entered
	assert.NotEqual(t, uint64(2), hung, "known by its mount point, not raw")
	assert.NotEqual(t, uint64(1), within(t, "a lookup on another volume", func() uint64 { return Stable(uint64(1)) }))

	release()
	querying.Wait()
	w.advance(refreshAfter + time.Second)
	Stable(uint64(2))
	refreshing.Wait()
	assert.NotEqual(t, hung, Stable(uint64(2)), "asked again on the next read, it answers")
}

func TestStableKeepsAKnownValueWhileAQueryHangs(t *testing.T) {
	// A known volume that stops answering keeps the value it had, and is not
	// asked again while its last query is still out.
	w := newWorld(t, mount{1, "/System/Volumes/Data", "data-volume-uuid"})
	want := Stable(uint64(1))

	queryTimeout = 50 * time.Millisecond
	entered, _ := holdSlowVolume(t, w, "/System/Volumes/Data")
	w.advance(refreshAfter + time.Second)
	Stable(uint64(1))
	refreshing.Wait()
	<-entered
	assert.Equal(t, want, Stable(uint64(1)))

	w.advance(refreshAfter + time.Second)
	Stable(uint64(1))
	refreshing.Wait()
	assert.Equal(t, want, Stable(uint64(1)))
	w.get(func(w *world) {
		assert.Equal(t, []string{"/System/Volumes/Data", "/System/Volumes/Data"}, w.asked,
			"asked once to learn it and once more, then not while that query hangs")
	})
}

func TestStableNeverLetsALateAnswerIdentifyAReplacementDrive(t *testing.T) {
	// A query that times out may answer after its drive has been swapped for
	// another under the same device number, path and disk name. That answer
	// belongs to the old drive and must not become the new one's identity.
	w := newWorld(t, mount{5, "/Volumes/Untitled", "old-drive-uuid"})
	queryTimeout = 50 * time.Millisecond
	_, release := holdSlowVolume(t, w, "/Volumes/Untitled")
	Stable(uint64(5)) // times out; the old drive's answer is still on its way

	w.set(func(w *world) {
		w.gate = nil
		w.mounts = []mount{{5, "/Volumes/Untitled", "new-drive-uuid"}}
	})
	release()
	querying.Wait()
	w.advance(refreshAfter + time.Second)
	Stable(uint64(5))
	refreshing.Wait()
	got := Stable(uint64(5))

	w.reboot(mount{5, "/Volumes/Untitled", "new-drive-uuid"})
	assert.Equal(t, Stable(uint64(5)), got, "the replacement has its own identity")
}

func TestStableDoesNotRereadForCallersQueuedBehindASlowRead(t *testing.T) {
	// Callers for a device the table does not hold queue behind a read in
	// progress. The retry window counts from when that read finishes, so even
	// a read that took longer than the window is not repeated by each of them.
	w := newWorld(t,
		mount{1, "/System/Volumes/Data", "data-volume-uuid"},
		mount{2, "/Volumes/Slow", "slow-uuid"},
	)
	entered, release := holdSlowVolume(t, w, "/Volumes/Slow")
	first := make(chan uint64, 1)
	go func() { first <- Stable(uint64(1)) }()
	<-entered

	queued := make(chan uint64, 4)
	for range 4 {
		go func() { queued <- Stable(uint64(99)) }()
	}
	w.advance(5 * retryAfter) // the read outlasts the retry window
	release()

	assert.NotEqual(t, uint64(1), <-first)
	for range 4 {
		assert.Equal(t, uint64(99), <-queued)
	}
	w.get(func(w *world) { assert.Equal(t, 1, w.reads) })
}

func TestStableAsksEveryVolumeAtOnce(t *testing.T) {
	// Several hung volumes, such as disk images on a share that dropped, must
	// cost one timeout, not one each: every caller waits for the first read.
	w := newWorld(t,
		mount{1, "/System/Volumes/Data", "data-volume-uuid"},
		mount{2, "/Volumes/ImageA", "image-a-uuid"},
		mount{3, "/Volumes/ImageB", "image-b-uuid"},
	)
	queryTimeout = 10 * time.Second // asked one by one, B would wait for A
	enteredA, _ := holdSlowVolume(t, w, "/Volumes/ImageA")
	enteredB, _ := holdSlowVolume(t, w, "/Volumes/ImageB")
	go Stable(uint64(1))

	for _, entered := range []<-chan struct{}{enteredA, enteredB} {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			require.Fail(t, "both hung volumes should be asked before either answers")
		}
	}
}
