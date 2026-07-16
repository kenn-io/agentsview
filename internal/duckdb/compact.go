package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"
)

// compactSrcCatalog and compactDstCatalog are the ATTACH aliases used while
// copying the mirror into a fresh file. They are fixed identifiers so the
// COPY FROM DATABASE statement never has to interpolate the auto-derived
// catalog name of the mirror file.
const (
	compactSrcCatalog = "compact_src"
	compactDstCatalog = "compact_dst"
)

// compactTempSuffix is appended to the mirror path to form the fresh file that
// compaction builds alongside the original before the atomic swap.
const compactTempSuffix = ".compact"

// DuckDBSizeStats summarizes the block accounting for a DuckDB database file.
type DuckDBSizeStats struct {
	BlockSize   int64
	TotalBlocks int64
	UsedBlocks  int64
	FreeBlocks  int64
}

// FreeBytes returns the allocated-but-unused space implied by the free block
// count. DuckDB never shrinks its file on its own, so this is the space a
// compaction pass can reclaim.
func (s DuckDBSizeStats) FreeBytes() int64 {
	return s.FreeBlocks * s.BlockSize
}

// CompactResult reports what a Compact call reclaimed.
type CompactResult struct {
	Path            string
	BeforeFileBytes int64
	AfterFileBytes  int64
	Before          DuckDBSizeStats
	After           DuckDBSizeStats
	RowCounts       map[string]int64
	Duration        time.Duration
}

// Compact rewrites the local DuckDB mirror at path into a fresh file to reclaim
// the allocated-but-free blocks that accumulate from push-time churn, then
// atomically swaps the new file over the original.
//
// The original is never deleted or truncated before a verified swap: Compact
// copies the entire database (schema plus data) into a temp file alongside the
// original, checks that every mirror table has the same row count in both
// files, and only then renames the temp file over the original. On any failure
// the temp file is removed and the original is left untouched.
//
// Compact takes DuckDB's single-writer file lock on the original for the
// duration of the copy. If another process (for example `duckdb serve` or
// `duckdb push --watch`) holds the file, Compact fails with a clear error
// rather than risking corruption.
func Compact(ctx context.Context, path string) (CompactResult, error) {
	if strings.TrimSpace(path) == "" {
		return CompactResult{}, fmt.Errorf("duckdb path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return CompactResult{}, fmt.Errorf(
				"duckdb mirror file does not exist: %s", path,
			)
		}
		return CompactResult{}, fmt.Errorf("stat duckdb mirror: %w", err)
	}
	if info.IsDir() {
		return CompactResult{}, fmt.Errorf(
			"duckdb mirror path is a directory: %s", path,
		)
	}

	start := time.Now()
	tempPath := path + compactTempSuffix
	// Clear any leftover temp file (and its WAL sidecar) from a previous
	// crashed run so ATTACH creates a fresh, empty destination.
	if err := removeDuckDBFile(tempPath); err != nil {
		return CompactResult{}, err
	}

	result, err := compactInto(ctx, path, tempPath)
	if err != nil {
		// Best-effort cleanup; never touch the original on failure.
		_ = removeDuckDBFile(tempPath)
		return CompactResult{}, err
	}

	if statErr := statFileBytes(tempPath, &result.AfterFileBytes); statErr != nil {
		_ = removeDuckDBFile(tempPath)
		return CompactResult{}, statErr
	}
	result.BeforeFileBytes = info.Size()

	// Atomic swap: rename the compacted file over the original. On POSIX this
	// replaces the inode atomically; a running reader keeps the old inode.
	if err := os.Rename(tempPath, path); err != nil {
		_ = removeDuckDBFile(tempPath)
		return CompactResult{}, fmt.Errorf(
			"swapping compacted duckdb mirror into place: %w", err,
		)
	}
	// The compacted file is fully checkpointed and self-contained. Remove any
	// stale WAL that belonged to the old file so a later open does not replay
	// it against the new file.
	if err := os.Remove(path + ".wal"); err != nil && !os.IsNotExist(err) {
		return CompactResult{}, fmt.Errorf(
			"removing stale duckdb WAL after compaction: %w", err,
		)
	}

	result.Path = path
	result.Duration = time.Since(start)
	return result, nil
}

// compactInto opens the mirror at srcPath with the exclusive file lock, copies
// its full contents into a fresh file at dstPath, and verifies that every
// mirror table survived the copy with an identical row count. It does not swap
// files; the caller owns the atomic rename.
func compactInto(
	ctx context.Context, srcPath, dstPath string,
) (CompactResult, error) {
	orchestrator, err := openDuckDB("")
	if err != nil {
		return CompactResult{}, fmt.Errorf(
			"opening duckdb compaction connection: %w", err,
		)
	}
	orchestrator.SetMaxOpenConns(1)
	orchestrator.SetMaxIdleConns(1)
	defer orchestrator.Close()
	if err := configureDuckDBThreads(orchestrator); err != nil {
		return CompactResult{}, err
	}

	// Pin a single connection so the ATTACH state persists across statements.
	conn, err := orchestrator.Conn(ctx)
	if err != nil {
		return CompactResult{}, fmt.Errorf(
			"pinning duckdb compaction connection: %w", err,
		)
	}
	defer conn.Close()

	// A read-write ATTACH takes DuckDB's single-writer lock on the mirror.
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("ATTACH %s AS %s", duckLiteral(srcPath), compactSrcCatalog),
	); err != nil {
		return CompactResult{}, wrapCompactAttachError(srcPath, err)
	}
	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("ATTACH %s AS %s", duckLiteral(dstPath), compactDstCatalog),
	); err != nil {
		return CompactResult{}, fmt.Errorf(
			"attaching compacted duckdb destination: %w", err,
		)
	}

	before, err := readAttachedDuckDBSize(ctx, conn, compactSrcCatalog)
	if err != nil {
		return CompactResult{}, err
	}

	if _, err := conn.ExecContext(ctx, fmt.Sprintf(
		"COPY FROM DATABASE %s TO %s", compactSrcCatalog, compactDstCatalog,
	)); err != nil {
		return CompactResult{}, fmt.Errorf("copying duckdb mirror: %w", err)
	}

	rowCounts, err := verifyCompactRowCounts(ctx, conn)
	if err != nil {
		return CompactResult{}, err
	}

	// Fold the destination WAL into the file so its block accounting and the
	// on-disk size reflect the final compacted state before the swap.
	if _, err := conn.ExecContext(ctx,
		"CHECKPOINT "+compactDstCatalog,
	); err != nil {
		return CompactResult{}, fmt.Errorf(
			"checkpointing compacted duckdb mirror: %w", err,
		)
	}

	after, err := readAttachedDuckDBSize(ctx, conn, compactDstCatalog)
	if err != nil {
		return CompactResult{}, err
	}

	// Detach both catalogs and release the source lock before the caller
	// renames the file. Detaching the destination flushes it to disk.
	if _, err := conn.ExecContext(ctx, "DETACH "+compactDstCatalog); err != nil {
		return CompactResult{}, fmt.Errorf(
			"detaching compacted duckdb destination: %w", err,
		)
	}
	if _, err := conn.ExecContext(ctx, "DETACH "+compactSrcCatalog); err != nil {
		return CompactResult{}, fmt.Errorf(
			"detaching duckdb source: %w", err,
		)
	}

	return CompactResult{
		Before:    before,
		After:     after,
		RowCounts: rowCounts,
	}, nil
}

// verifyCompactRowCounts asserts that every mirror table has the same row count
// in the source and destination catalogs. It returns the per-table counts from
// the freshly compacted destination.
func verifyCompactRowCounts(
	ctx context.Context, conn *sql.Conn,
) (map[string]int64, error) {
	srcCounts := make(map[string]int64, len(mirrorTables))
	dstCounts := make(map[string]int64, len(mirrorTables))
	for _, table := range mirrorTables {
		var srcCount, dstCount int64
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+compactSrcCatalog+"."+table.name,
		).Scan(&srcCount); err != nil {
			return nil, fmt.Errorf(
				"counting source rows in %s: %w", table.name, err,
			)
		}
		if err := conn.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+compactDstCatalog+"."+table.name,
		).Scan(&dstCount); err != nil {
			return nil, fmt.Errorf(
				"counting compacted rows in %s: %w", table.name, err,
			)
		}
		srcCounts[table.name] = srcCount
		dstCounts[table.name] = dstCount
	}
	if err := compareCompactRowCounts(srcCounts, dstCounts); err != nil {
		return nil, err
	}
	return dstCounts, nil
}

// compareCompactRowCounts reports the first table whose original and compacted
// row counts disagree. A mismatch means the copy dropped or duplicated rows and
// the compacted file must not be swapped in.
func compareCompactRowCounts(src, dst map[string]int64) error {
	for _, table := range mirrorTables {
		srcCount, dstCount := src[table.name], dst[table.name]
		if srcCount != dstCount {
			return fmt.Errorf(
				"compaction verification failed for %s: original has %d rows, compacted has %d",
				table.name, srcCount, dstCount,
			)
		}
	}
	return nil
}

func readAttachedDuckDBSize(
	ctx context.Context, conn *sql.Conn, catalog string,
) (DuckDBSizeStats, error) {
	var stats DuckDBSizeStats
	// pragma_database_size() returns wal_size/database_size as human-formatted
	// strings, so only the plain integer block columns are read here.
	if err := conn.QueryRowContext(ctx, `
		SELECT block_size, total_blocks, used_blocks, free_blocks
		FROM pragma_database_size()
		WHERE database_name = ?`,
		catalog,
	).Scan(
		&stats.BlockSize, &stats.TotalBlocks,
		&stats.UsedBlocks, &stats.FreeBlocks,
	); err != nil {
		return DuckDBSizeStats{}, fmt.Errorf(
			"reading duckdb size for %s: %w", catalog, err,
		)
	}
	return stats, nil
}

func wrapCompactAttachError(path string, err error) error {
	if isDuckDBFileLockError(err) {
		return fmt.Errorf(
			"duckdb mirror %s is in use by another process; stop 'duckdb serve' "+
				"or 'duckdb push --watch' before compacting: %w",
			path, err,
		)
	}
	return fmt.Errorf("attaching duckdb source %s: %w", path, err)
}

func isDuckDBFileLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "could not set lock") ||
		strings.Contains(msg, "conflicting lock") ||
		strings.Contains(msg, "file is already open") ||
		strings.Contains(msg, "already open in")
}

// removeDuckDBFile removes a DuckDB database file and its WAL sidecar, treating
// a missing file as success.
func removeDuckDBFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing duckdb file %s: %w", path, err)
	}
	if err := os.Remove(path + ".wal"); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing duckdb WAL %s.wal: %w", path, err)
	}
	return nil
}

func statFileBytes(path string, dst *int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat compacted duckdb mirror: %w", err)
	}
	*dst = info.Size()
	return nil
}
