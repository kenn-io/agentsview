// ABOUTME: Change-detection state for shared SQLite container files, built
// ABOUTME: on SQLite's own write markers rather than timestamp precision.
package parser

import (
	"encoding/binary"
	"errors"
	"os"
)

// SQLiteContainerState captures SQLite write markers for bounded freshness reuse.
// Checkpointed WAL writes can preserve every marker within one recorded second,
// so matching states still require periodic full digest verification.
// Header counters detect rollback-journal transactions; WAL size and salts detect
// writes while the WAL retains frames. File identity distinguishes replacements.
// Header layouts follow https://www.sqlite.org/fileformat2.html.
type SQLiteContainerState struct {
	DBSize          int64
	DBMtimeSec      int64
	DBChangeCounter uint32
	// DBInode and DBDevice distinguish a replaced or restored container
	// file (new inode) from in-place transaction writes, which the header
	// markers alone cannot: a byte-identical copy carries the same size,
	// counter, and salts. Zero when native file identity is unavailable.
	// In-place byte surgery that preserves every marker and
	// lands within the trusted mtime's second is explicitly out of
	// scope — the same exposure class every mtime-based sync tool
	// accepts.
	DBInode  uint64
	DBDevice uint64

	WALSize     int64
	WALMtimeSec int64
	WALCkptSeq  uint32
	WALSalt1    uint32
	WALSalt2    uint32
}

// sqliteHeaderProbeSize covers the 100-byte SQLite database header; the
// file change counter lives at bytes 24-27 (big-endian), per
// https://www.sqlite.org/fileformat2.html#the_database_header.
const sqliteHeaderProbeSize = 100

// The documented WAL header magic values (byte order of the frame
// checksums) and the only WAL format version ever published (stable since
// SQLite 3.7.0), per https://www.sqlite.org/fileformat2.html#wal_file_format.
// The salts and checkpoint sequence are only meaningful under this exact
// format, so anything else fails closed to "never trusted".
const (
	sqliteWALMagicBE = 0x377f0683
	sqliteWALMagicLE = 0x377f0682
	sqliteWALVersion = 3007000
)

var sqliteHeaderMagic = []byte("SQLite format 3\x00")

// A SQLite WAL begins with a 32-byte header. Only bytes beyond the header
// can contain transaction frames; read-only clients can create an empty WAL.
const sqliteWALHeaderSize = int64(32)

// sqliteDBJournalSuffixes is the sibling-file suffix list for
// sqliteDBCompositeMtime: the database file itself plus its WAL. The "-shm"
// index is left out on purpose. Every committed write lands in the main file
// or the WAL, while readers, including this process's own scan connections,
// rewrite the shared-memory file, so folding its mtime in would make every
// scan report the container as changed and schedule the next scan.
var sqliteDBJournalSuffixes = []string{"", "-wal"}

// sqliteDBCompositeMtime returns the freshest mtime across a SQLite database
// file and the listed sibling suffixes (e.g. "-wal", "-shm").
func sqliteDBCompositeMtime(dbPath string, suffixes []string) (int64, error) {
	var maxMtime int64
	for _, suffix := range suffixes {
		info, err := os.Stat(dbPath + suffix)
		if err != nil {
			continue
		}
		if suffix == "-wal" && !sqliteWALInfoHasFrames(info) {
			// Read-only connections create and delete an empty WAL on
			// open and close; its mtime says nothing about content.
			continue
		}
		if mtime := info.ModTime().UnixNano(); mtime > maxMtime {
			maxMtime = mtime
		}
	}
	if maxMtime == 0 {
		return 0, &os.PathError{Op: "stat", Path: dbPath, Err: os.ErrNotExist}
	}
	return maxMtime, nil
}

// sqliteWALInfoHasFrames reports whether a WAL sibling can hold committed
// transaction frames. A WAL at or under header size is equivalent to an
// absent one: read-only SQLite clients, including this process's own scan
// connections, create an empty WAL when they open a WAL-mode database and
// remove it again on close, so its presence, size, and mtime must not count
// as a content change.
func sqliteWALInfoHasFrames(info os.FileInfo) bool {
	return info.Mode().IsRegular() && info.Size() > sqliteWALHeaderSize
}

// sqliteWALHasFrames is sqliteWALInfoHasFrames for a path. Only a WAL that
// is confirmed missing (typically already deleted) holds no frames; any other
// stat error fails open so a real update still syncs, at worst once more.
func sqliteWALHasFrames(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return sqliteWALInfoHasFrames(info)
}

// StatSQLiteContainerState captures the current change-detection state of a
// shared SQLite container. ok is false when the container is missing or its
// headers cannot be read, in which case the container must never be treated
// as unchanged.
func StatSQLiteContainerState(dbPath string) (SQLiteContainerState, bool) {
	file, err := os.Open(dbPath)
	if err != nil {
		return SQLiteContainerState{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return SQLiteContainerState{}, false
	}
	state := SQLiteContainerState{
		DBSize:     info.Size(),
		DBMtimeSec: info.ModTime().Unix(),
	}
	state.DBInode, state.DBDevice = sourceFileIdentityForFile(file, info)
	counter, ok := readSQLiteChangeCounterFromFile(file)
	if !ok {
		return SQLiteContainerState{}, false
	}
	state.DBChangeCounter = counter

	walPath := dbPath + "-wal"
	walInfo, err := os.Stat(walPath)
	if err != nil {
		if os.IsNotExist(err) {
			return state, true
		}
		return SQLiteContainerState{}, false
	}
	if !sqliteWALInfoHasFrames(walInfo) {
		// A WAL at or under header size carries no transaction frames, so
		// it is equivalent to an absent WAL: read-only SQLite clients can
		// leave an empty WAL behind without implying any content change.
		return state, true
	}
	header := make([]byte, sqliteWALHeaderSize)
	f, err := os.Open(walPath)
	if err != nil {
		return SQLiteContainerState{}, false
	}
	defer f.Close()
	if _, err := f.ReadAt(header, 0); err != nil {
		return SQLiteContainerState{}, false
	}
	magic := binary.BigEndian.Uint32(header[0:4])
	if magic != sqliteWALMagicBE && magic != sqliteWALMagicLE {
		return SQLiteContainerState{}, false
	}
	if binary.BigEndian.Uint32(header[4:8]) != sqliteWALVersion {
		return SQLiteContainerState{}, false
	}
	state.WALSize = walInfo.Size()
	state.WALMtimeSec = walInfo.ModTime().Unix()
	state.WALCkptSeq = binary.BigEndian.Uint32(header[12:16])
	state.WALSalt1 = binary.BigEndian.Uint32(header[16:20])
	state.WALSalt2 = binary.BigEndian.Uint32(header[20:24])
	return state, true
}

func readSQLiteChangeCounter(dbPath string) (uint32, bool) {
	f, err := os.Open(dbPath)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	return readSQLiteChangeCounterFromFile(f)
}

func readSQLiteChangeCounterFromFile(f *os.File) (uint32, bool) {
	header := make([]byte, sqliteHeaderProbeSize)
	if _, err := f.ReadAt(header, 0); err != nil {
		return 0, false
	}
	for i, b := range sqliteHeaderMagic {
		if header[i] != b {
			return 0, false
		}
	}
	return binary.BigEndian.Uint32(header[24:28]), true
}
