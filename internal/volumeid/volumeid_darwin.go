//go:build darwin

package volumeid

import (
	"hash/fnv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// On macOS each mounted volume's statfs carries its current st_dev in
// f_fsid.val[0], beside the path it is mounted on. A device number is mapped
// to the volume mounted under it, and the value is derived from that volume's
// UUID, which belongs to the volume itself: it is the same wherever and
// however often the volume is mounted, and a different volume, even one
// mounted later at the same path, normally has a different one. The exception
// is a copy that keeps its source's UUID, such as a block-level clone or a
// second attached copy of a disk image; mounted at the same path as the
// original, it shares the original's value.
//
// Only volumes flagged local and not automounted are asked for a UUID. Any
// other volume is identified by its mount point instead, as is a local volume
// that reports no UUID, such as devfs. A mount point is stable across reboots
// but names a place rather than a volume.
//
// Every read of the mount table asks each of those volumes again, so a drive
// swapped in under the same device number, path and disk name is seen on the
// next read whose query answers. When a query fails or does not answer within
// queryTimeout, the value the previous read found for that mount is kept, so
// a transient failure cannot make every file on a known volume look replaced.
// Two limits follow. A volume whose first query fails or does not answer in
// time is known by its mount point until a query succeeds. And while a query
// hangs, the mount is not asked again, so a drive swapped in under it keeps the
// previous drive's value until that query returns.
//
// A value can change from one read of the table to the next: a newly mounted
// device goes from its raw number to its volume's value, and a volume whose
// first query failed goes from its mount point to its UUID. A caller that
// takes two identities a moment apart and compares them, such as raw capture
// before and after a copy, can see that change once as a changed file.

const (
	// How long a mount table read is trusted. A device number can be handed
	// to a different volume after an unmount, so the table is re-read now and
	// then rather than once per process, as well as on a number it does not
	// know.
	refreshAfter = 30 * time.Second
	// The least time between two attempts to read the mount table, whatever
	// prompted them, counted from when the last one finished. It bounds the
	// cost of a device that is never in the table, and of a mount table that
	// cannot be read at all, to one read a second rather than one per file,
	// and callers queued behind a slow read find it just finished rather than
	// each starting another.
	retryAfter = time.Second
)

// uuidAnswer is what asking a volume for its UUID came to.
type uuidAnswer int

const (
	// uuidFailed: the call failed, so nothing is learned about the volume.
	uuidFailed uuidAnswer = iota
	// uuidNone: the volume has no UUID, so its mount point identifies it.
	uuidNone
	uuidFound
)

// mountKey is one mount of one volume.
type mountKey struct {
	dev  uint64
	path string
	from string // the disk it is mounted from, such as /dev/disk3s5
}

var (
	// mu guards the table and its times. It is held only to read or swap
	// them, never while the mount table is being read, so a volume that is
	// slow to answer cannot hold up a lookup the current table can serve.
	mu        sync.Mutex
	byDevice  map[uint64]uint64
	readAt    time.Time // the last successful read
	attemptAt time.Time // the last read's start while it runs, its end after

	// buildMu lets one read of the mount table run at a time. It also guards
	// previous, the value the last read found for each mount.
	buildMu  sync.Mutex
	previous map[mountKey]uint64
	// refreshing tracks refreshes running in the background.
	refreshing sync.WaitGroup

	// pendingMu guards the mounts whose last query has not returned, so a
	// hung volume is not asked again on every read.
	pendingMu sync.Mutex
	pending   map[mountKey]bool
	// querying tracks UUID queries, including ones that outlived their wait.
	querying sync.WaitGroup
	// How long a read waits for the volumes' UUIDs. They are asked at once, so
	// however many volumes hang, a read waits this long at most.
	queryTimeout = 2 * time.Second

	// Variables so a test can supply the mount table, the volume UUIDs and
	// the time.
	getfsstat  = unix.Getfsstat
	volumeUUID = mountedVolumeUUID
	now        = time.Now
)

// stable maps dev to its volume's value. A number the mount table does not
// hold is returned unchanged, which is what was recorded before this package
// existed. That includes a volume mounted after the last read until the next
// one, so a file on it can be seen first with its raw number and then with its
// volume's value: the cost is one re-parse of that file, which is the
// behaviour this package replaces.
func stable(dev uint64) uint64 {
	// dev_t is 32 bits here; callers widen it with sign extension, the mount
	// table does not, so both are compared as the same 32 bits.
	key := uint64(uint32(dev))
	v, ok, due := lookup(key)
	if ok {
		// A known volume never waits for a read. When a refresh is due, the
		// first caller to notice starts it in the background, and every
		// caller uses the current table until the new one is swapped in.
		if due && buildMu.TryLock() {
			refreshing.Add(1)
			go func() {
				defer refreshing.Done()
				defer buildMu.Unlock()
				rebuild()
			}()
		}
		return v
	}
	// Not in the table, or no table yet. Wait for any read in progress rather
	// than return a raw number that read is about to replace, then read again
	// if that one did not cover this device.
	buildMu.Lock()
	defer buildMu.Unlock()
	if v, ok, _ := lookup(key); ok {
		return v
	}
	rebuild()
	if v, ok, _ := lookup(key); ok {
		return v
	}
	return dev
}

// lookup reads key from the current table and says whether a refresh is due.
func lookup(key uint64) (v uint64, ok, due bool) {
	mu.Lock()
	defer mu.Unlock()
	v, ok = byDevice[key]
	t := now()
	// Due only when a read could run: not while one is held back by
	// retryAfter, which would start a refresh with nothing to do.
	due = (byDevice == nil || t.Sub(readAt) > refreshAfter) &&
		(attemptAt.IsZero() || t.Sub(attemptAt) >= retryAfter)
	return v, ok, due
}

// rebuild re-reads the mount table unless a read started or finished within
// retryAfter, and swaps the result in. The caller holds buildMu. A failed read
// keeps the previous table, so a transient error cannot turn every stable
// value back into a raw one.
func rebuild() {
	mu.Lock()
	t := now()
	if !attemptAt.IsZero() && t.Sub(attemptAt) < retryAfter {
		mu.Unlock()
		return
	}
	attemptAt = t
	mu.Unlock()

	table, ok := readMounts()
	mu.Lock()
	defer mu.Unlock()
	attemptAt = now()
	if ok {
		byDevice, readAt = table, t
	}
}

// readMounts reads the mount table and each volume's value. The caller holds
// buildMu, and not mu, so it may take as long as the slowest volume it asks,
// and no longer than queryTimeout.
func readMounts() (map[uint64]uint64, bool) {
	n, err := getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil || n <= 0 {
		return nil, false
	}
	buf := make([]unix.Statfs_t, n)
	n, err = getfsstat(buf, unix.MNT_NOWAIT)
	if err != nil {
		return nil, false
	}
	table := make(map[uint64]uint64, n)
	found := make(map[mountKey]uint64, n)
	var eligible []mountKey
	for _, fs := range buf[:n] {
		mount := unix.ByteSliceToString(fs.Mntonname[:])
		if mount == "" {
			continue
		}
		key := mountKey{
			dev:  uint64(uint32(fs.Fsid.Val[0])),
			path: mount,
			from: unix.ByteSliceToString(fs.Mntfromname[:]),
		}
		table[key.dev] = fromMountPoint(mount)
		// Only volumes flagged local and not automounted are asked. Asking a
		// network volume for an attribute can block for as long as its server
		// takes to answer, and touching an automount point can trigger a
		// mount.
		if fs.Flags&unix.MNT_LOCAL == 0 || fs.Flags&unix.MNT_AUTOMOUNTED != 0 {
			continue
		}
		eligible = append(eligible, key)
	}
	answers := askAll(eligible)
	for _, key := range eligible {
		v, ok := answers[key]
		if !ok {
			v, ok = previous[key]
		}
		if ok {
			table[key.dev], found[key] = v, v
		}
	}
	previous = found
	return table, true
}

// reply is a volume's answer to a UUID query.
type reply struct {
	uuid   [16]byte
	answer uuidAnswer
}

// askAll asks every volume in keys for its UUID at once and returns the value
// of each that answered within queryTimeout. A volume is left out when nothing
// was learned: the call failed, the last query for that mount has still not
// returned, or this one did not answer in time. An answer that arrives later
// is discarded, so a query that outlives a swap of the drive cannot give the
// replacement its identity.
func askAll(keys []mountKey) map[mountKey]uint64 {
	type asked struct {
		key      mountKey
		answered <-chan reply
	}
	var queries []asked
	for _, key := range keys {
		if answered, ok := start(key); ok {
			queries = append(queries, asked{key, answered})
		}
	}
	values := make(map[mountKey]uint64, len(queries))
	deadline := time.NewTimer(queryTimeout)
	defer deadline.Stop()
	expired := false
	for _, q := range queries {
		var r reply
		if !expired {
			select {
			case r = <-q.answered:
			case <-deadline.C:
				expired = true
			}
		}
		if expired {
			select {
			case r = <-q.answered:
			default:
				continue
			}
		}
		switch r.answer {
		case uuidFound:
			values[q.key] = fromUUID(r.uuid)
		case uuidNone:
			values[q.key] = fromMountPoint(q.key.path)
		}
	}
	return values
}

// start asks the volume mounted at key for its UUID in the background, unless
// its last query has not returned, and returns where the answer will arrive.
func start(key mountKey) (<-chan reply, bool) {
	pendingMu.Lock()
	if pending[key] {
		pendingMu.Unlock()
		return nil, false
	}
	if pending == nil {
		pending = map[mountKey]bool{}
	}
	pending[key] = true
	pendingMu.Unlock()

	answered := make(chan reply, 1) // buffered: a late answer never blocks
	querying.Add(1)
	go func() {
		defer querying.Done()
		uuid, answer := volumeUUID(key.path)
		pendingMu.Lock()
		delete(pending, key)
		pendingMu.Unlock()
		answered <- reply{uuid, answer}
	}()
	return answered, true
}

func fromUUID(uuid [16]byte) uint64 { return hashValue("uuid:", uuid[:]) }

func fromMountPoint(mount string) uint64 { return hashValue("path:", []byte(mount)) }

// hashValue hashes a tagged identifier to a positive, non-zero value: callers
// store it as a signed integer and read zero as "no identity". The tag keeps
// a UUID and a mount point from ever hashing to the same value by sharing
// bytes.
func hashValue(tag string, id []byte) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(tag))
	_, _ = h.Write(id)
	if v := h.Sum64() &^ (1 << 63); v != 0 {
		return v
	}
	return 1
}
