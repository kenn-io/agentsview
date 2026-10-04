package sync

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

// Rebuilds with no source roots still copy archived sessions, but need no
// collision ownership snapshot. Compare allocations as the archive grows.
func BenchmarkResyncWithoutSourceRoots(b *testing.B) {
	routeBenchLogs(b)
	for _, count := range []int{1000, 10000} {
		b.Run(strconv.Itoa(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				ctx := b.Context()
				root := b.TempDir()
				archive, err := db.OpenIsolated(ctx, filepath.Join(root, "archive.db"))
				require.NoError(b, err)
				// Metadata-only orphans allow an empty discovery to rebuild.
				require.NoError(b, archive.Update(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `WITH RECURSIVE fixture(n) AS (
						SELECT 1 UNION ALL SELECT n + 1 FROM fixture WHERE n < ?
					) INSERT INTO sessions (id, project, machine, agent, file_path, data_version)
					SELECT CASE WHEN n % 2 = 0 THEN 'cursor:' ELSE 'gemini:' END || n,
						'fixture', 'fixture', CASE WHEN n % 2 = 0 THEN 'cursor' ELSE 'gemini' END,
						? || n, ? FROM fixture`, count, filepath.Join(root, "absent-"), db.CurrentDataVersion())
					return err
				}))
				engine := NewEngine(ctx, archive, EngineConfig{
					Machine: "fixture", Ephemeral: true,
					DisableFilesystemProjectDiscovery: true,
				})
				b.StartTimer()
				stats := engine.ResyncAll(ctx, nil)
				b.StopTimer()
				require.False(b, stats.Aborted, "%+v", stats)
				require.Zero(b, stats.Failed)
				require.Equal(b, count, stats.OrphanedCopied)
				var stored int
				require.NoError(b, archive.Reader().QueryRow(ctx, "SELECT COUNT(*) FROM sessions").Scan(&stored))
				require.Equal(b, count, stored)
				engine.Close()
				require.NoError(b, archive.Close())
				b.StartTimer()
			}
		})
	}
}
