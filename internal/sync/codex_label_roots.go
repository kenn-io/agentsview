package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/parser"
)

const codexLabelRootsKey = "codex_label_roots"

type codexLabelRootsPendingKey struct{}

func (e *Engine) prepareCodexLabelRoots(ctx context.Context) (context.Context, func(context.Context, *SyncStats)) {
	roots := []string{"v1"}
	for _, root := range e.sources().agentDirs[parser.AgentCodex] {
		if !strings.HasPrefix(root, "s3://") {
			roots = append(roots, filepath.Clean(root))
		}
	}
	encoded, _ := json.Marshal(roots)
	value := string(encoded)
	var stored string
	err := e.db.Reader().QueryRow(ctx, `SELECT value FROM archive_metadata WHERE key = ?`, codexLabelRootsKey).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Printf("read Codex root marker: %v", err)
		return context.WithValue(ctx, codexLabelRootsPendingKey{}, true), func(_ context.Context, stats *SyncStats) { stats.RecordFailed() }
	}
	if stored == value {
		return ctx, func(context.Context, *SyncStats) {}
	}
	ctx = context.WithValue(ctx, codexLabelRootsPendingKey{}, true)
	return ctx, func(ctx context.Context, stats *SyncStats) {
		if ctx.Err() != nil || firstImportFromContext(ctx) != nil || !stats.ProcessingComplete() {
			return
		}
		for _, root := range roots[1:] {
			if info, err := os.Stat(root); err != nil || !info.IsDir() {
				return
			}
		}
		if err := e.db.Update(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO archive_metadata(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, codexLabelRootsKey, value)
			return err
		}); err != nil {
			log.Printf("write Codex root marker: %v", err)
			stats.RecordFailed()
		}
	}
}
