package duckdb

import (
	"context"
	"database/sql"
	"fmt"
)

const duckCheckpointMinFreeBytes int64 = 16 << 20

type duckDBMaintenance interface {
	checkpointAfterPush(context.Context, *sql.DB) error
}

type duckDBCheckpointMaintenance struct{}

type duckDBSize struct {
	blockSize  int64
	freeBlocks int64
}

func (duckDBCheckpointMaintenance) checkpointAfterPush(
	ctx context.Context, duck *sql.DB,
) error {
	size, err := readDuckDBSize(ctx, duck)
	if err != nil {
		return err
	}
	if !shouldCheckpointDuckDB(size) {
		return nil
	}
	if _, err := duck.ExecContext(ctx, `CHECKPOINT`); err != nil {
		return fmt.Errorf("checkpointing duckdb mirror: %w", err)
	}
	return nil
}

func readDuckDBSize(ctx context.Context, duck *sql.DB) (duckDBSize, error) {
	var size duckDBSize
	if err := duck.QueryRowContext(ctx, `
		SELECT block_size, free_blocks
		FROM pragma_database_size()
		WHERE database_name = current_database()
	`).Scan(&size.blockSize, &size.freeBlocks); err != nil {
		return duckDBSize{}, fmt.Errorf("reading duckdb database size: %w", err)
	}
	return size, nil
}

func shouldCheckpointDuckDB(size duckDBSize) bool {
	if size.blockSize <= 0 || size.freeBlocks <= 0 {
		return false
	}
	blocksNeeded := (duckCheckpointMinFreeBytes + size.blockSize - 1) / size.blockSize
	return size.freeBlocks >= blocksNeeded
}
