package sync

import (
	"database/sql"
	"encoding/base64"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// backdateCBCTrees sets every file and directory under roots a month back and
// returns a quick-sync cutoff just after the newest modification or change
// time left in them. Change times cannot be set, so the cutoff sits after the
// ones the backdating itself recorded; nothing under roots is recent unless a
// test changes it afterwards.
func backdateCBCTrees(t *testing.T, roots ...string) time.Time {
	t.Helper()
	old := time.Now().Add(-30 * 24 * time.Hour)
	for _, root := range roots {
		require.NoError(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			return os.Chtimes(path, old, old)
		}))
	}
	var newest int64
	for _, root := range roots {
		require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			newest = max(newest, info.ModTime().UnixNano())
			if changed, ok := fileChangeTime(path, info); ok {
				newest = max(newest, changed)
			}
			return nil
		}))
	}
	return time.Unix(0, newest+1)
}

// requireQuickSyncSkipsUnchanged checks that a quick sync from cutoff looks
// at no source while nothing changed, so a later pass that refreshes a
// conversation does so because of the edit it follows.
func requireQuickSyncSkipsUnchanged(t *testing.T, engine *Engine, cutoff time.Time) {
	t.Helper()
	stats := engine.SyncAllSince(t.Context(), cutoff, nil)
	require.Zero(t, stats.TotalSessions, "an unchanged home has nothing newer than the cutoff")
}

// requireChangedAfter keeps path's modification time at mtime and waits until
// its change time, which cannot be set, is at or after cutoff. It skips the
// test where the platform exposes no change time.
func requireChangedAfter(t *testing.T, path string, mtime, cutoff time.Time) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	if _, ok := fileChangeTime(path, info); !ok {
		t.Skip("platform does not expose file change time")
	}
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, os.Chtimes(path, mtime, mtime))
		info, err := os.Stat(path)
		require.NoError(c, err)
		changed, ok := fileChangeTime(path, info)
		require.True(c, ok)
		assert.GreaterOrEqual(c, changed, cutoff.UnixNano())
	}, 2*time.Second, time.Millisecond)
}

// A quick sync refreshes an old conversation when something its parse reads
// outside the transcript changes: an IDE rename in the catalog, an image
// restored from a backup with its old time, or a saved tool output rewritten
// with its old time.
func TestSyncAllSinceCodeBuddyFollowsInputsBesideTranscript(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString(codebuddyBlobPNG)
	require.NoError(t, err)
	blob := strings.Repeat("cd", 32)
	tests := []struct {
		name  string
		lines func(project string) []string
		setup func(t *testing.T, home, project string)
		edit  func(t *testing.T, home, project string, old, cutoff time.Time)
		read  func(t *testing.T, database *db.DB) string
		want  string
	}{
		{
			name: "catalog rename",
			lines: func(string) []string {
				return []string{
					`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-a","content":[{"type":"input_text","text":"hello"}]}`,
					`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"hi"}]}`,
				}
			},
			setup: func(t *testing.T, home, _ string) {
				conn := openCBCCatalog(t, home)
				_, err := conn.ExecContext(t.Context(), `CREATE TABLE conversations (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', custom_title TEXT, runtime_session_id TEXT);
					INSERT INTO conversations VALUES ('conv-a', 'Before', NULL, 'run-a')`)
				require.NoError(t, err)
			},
			edit: func(t *testing.T, home, _ string, _, cutoff time.Time) {
				conn := openCBCCatalog(t, home)
				_, err := conn.ExecContext(t.Context(), `UPDATE conversations SET custom_title = 'Renamed in IDE' WHERE id = 'conv-a'`)
				require.NoError(t, err)
				later := cutoff.Add(time.Second)
				require.NoError(t, os.Chtimes(filepath.Join(home, parser.CodeBuddyCatalogDBName), later, later))
			},
			read: func(t *testing.T, database *db.DB) string {
				sess, err := database.GetSessionFull(t.Context(), "codebuddy:run-a")
				require.NoError(t, err)
				require.NotNil(t, sess)
				require.NotNil(t, sess.SessionName)
				return *sess.SessionName
			},
			want: "Renamed in IDE",
		},
		{
			name: "image restored with its old time",
			lines: func(string) []string {
				return []string{
					`{"id":"u1","type":"message","role":"user","timestamp":1767225600000,"sessionId":"run-a","content":[{"type":"input_text","text":"what is this"},{"type":"image_blob_ref","blob_id":"` + blob + `","mime":"image/png","original_filename":"shot.png"}]}`,
					`{"id":"a1","parentId":"u1","type":"message","role":"assistant","timestamp":1767225601000,"content":[{"type":"output_text","text":"a pixel"}]}`,
				}
			},
			edit: func(t *testing.T, home, _ string, old, cutoff time.Time) {
				stored := filepath.Join(home, "blobs", blob[:2], blob+".png")
				require.NoError(t, os.MkdirAll(filepath.Dir(stored), 0o755))
				require.NoError(t, os.WriteFile(stored, png, 0o600))
				requireChangedAfter(t, stored, old, cutoff)
			},
			read: func(t *testing.T, database *db.DB) string {
				msgs, err := database.GetMessages(t.Context(), "codebuddy:run-a", 0, 100, true)
				require.NoError(t, err)
				require.NotEmpty(t, msgs)
				if strings.Contains(msgs[0].Content, "![image](asset://") {
					return "image"
				}
				return msgs[0].Content
			},
			want: "image",
		},
		{
			name: "saved output rewritten with its old time",
			lines: func(project string) []string {
				return cbcPersistedTurn(filepath.Join(project, "run-a", "tool-results", "c1.txt"))
			},
			setup: func(t *testing.T, _, project string) {
				side := filepath.Join(project, "run-a", "tool-results", "c1.txt")
				require.NoError(t, os.MkdirAll(filepath.Dir(side), 0o755))
				require.NoError(t, os.WriteFile(side, []byte("first output"), 0o600))
			},
			edit: func(t *testing.T, _, project string, old, cutoff time.Time) {
				side := filepath.Join(project, "run-a", "tool-results", "c1.txt")
				require.NoError(t, os.WriteFile(side, []byte("other output"), 0o600))
				requireChangedAfter(t, side, old, cutoff)
			},
			read: func(t *testing.T, database *db.DB) string {
				return cbcToolResult(t, database, "codebuddy:run-a", "c1")
			},
			want: "other output",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, project := cbcSyncHome(t)
			writeCBCSync(t, filepath.Join(project, "run-a.jsonl"), tc.lines(project)...)
			if tc.setup != nil {
				tc.setup(t, home, project)
			}
			cutoff := backdateCBCTrees(t, home)
			info, err := os.Stat(filepath.Join(project, "run-a.jsonl"))
			require.NoError(t, err)
			old := info.ModTime()

			database := openTestDB(t)
			database.SetAssetsDir(t.TempDir())
			engine := newCBCEngine(t, database, home)
			engine.SyncAll(t.Context(), nil)
			require.NotEqual(t, tc.want, tc.read(t, database))
			requireQuickSyncSkipsUnchanged(t, engine, cutoff)

			tc.edit(t, home, project, old, cutoff)
			engine.SyncAllSince(t.Context(), cutoff, nil)
			assert.Equal(t, tc.want, tc.read(t, database))
		})
	}
}

// openCBCCatalog opens the IDE catalog in home for writing.
func openCBCCatalog(t *testing.T, home string) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite3", filepath.Join(home, parser.CodeBuddyCatalogDBName))
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
