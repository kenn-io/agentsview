// ABOUTME: Change-detection state for shared SQLite container files, built
// ABOUTME: on SQLite's own write markers rather than timestamp precision.
package parser

import (
	"encoding/binary"
	"os"
)

// SQLiteContainerState captures a shared SQLite container file's
// change-detection state. Two equal states mean the container provably has
// not changed between the two captures.
//
// "Provably" deliberately does not rest on timestamp equality. Filesystem
// mtime granularity varies (ns on APFS/ext4, 1s on HFS+, 2s on FAT) and
// timestamps round-trip through layers with different precisions, so
// comparisons finer than one second are not meaningful; mtimes here are
// truncated to whole seconds and act only as coarse extra signal. The real
// precision comes from SQLite's own write markers, which advance on every
// committed transaction regardless of any clock:
//
//   - the 32-bit file change counter at byte 24 of the database header
//     (bumped per transaction in rollback-journal mode, and on every
//     checkpoint in WAL mode), and
//   - the WAL header's checkpoint sequence number and random salts, plus
//     the WAL size: between WAL resets commits only append frames ("a WAL
//     always grows from beginning toward the end", so the size advances),
//     and frames are only ever overwritten after a reset, whose first
//     write transaction rewrites the header with an incremented salt-1
//     and re-randomized salt-2 — the salts are SQLite's own mechanism for
//     invalidating stale frames. Every committed frame therefore either
//     grows the WAL or lands behind changed salts; a state that compares
//     equal cannot hide new frames short of a fresh WAL's two random
//     salts colliding with the captured ones.
//
// A spurious mismatch merely costs one redundant re-read, while a wrong
// match is what the write markers rule out.
//
// Both headers are read per the documented on-disk format, which SQLite
// pledges to keep backwards compatible through 2050:
//
//	https://www.sqlite.org/fileformat2.html (database header
//	#the_database_header, WAL format #wal_file_format; layouts
//	unchanged since 3.0.0 and 3.7.0 respectively)
//	https://www.sqlite.org/lts.html (long-term support and format
//	stability pledge through 2050)
type SQLiteContainerState struct {
	DBSize          int64
	DBMtimeSec      int64
	DBChangeCounter uint32
	// DBInode and DBDevice distinguish a replaced or restored container
	// file (new inode) from in-place transaction writes, which the header
	// markers alone cannot: a byte-identical copy carries the same size,
	// counter, and salts. Zero on platforms without cheap file identity
	// (Windows), where replacement detection degrades to the other
	// markers. In-place byte surgery that preserves every marker and
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

// StatSQLiteContainerState captures the current change-detection state of a
// shared SQLite container. ok is false when the container is missing or its
// headers cannot be read, in which case the container must never be treated
// as unchanged.
func StatSQLiteContainerState(dbPath string) (SQLiteContainerState, bool) {
	info, err := os.Stat(dbPath)
	if err != nil || !info.Mode().IsRegular() {
		return SQLiteContainerState{}, false
	}
	state := SQLiteContainerState{
		DBSize:     info.Size(),
		DBMtimeSec: info.ModTime().Unix(),
	}
	state.DBInode, state.DBDevice = sourceFileIdentity(info)
	counter, ok := readSQLiteChangeCounter(dbPath)
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
	if !walInfo.Mode().IsRegular() ||
		walInfo.Size() <= sqliteWALHeaderSize {
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
