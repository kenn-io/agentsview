package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
)

// A report-only parse-diff run links a CodeBuddy image by the same address the
// archive saved, from the original or from the copy an earlier import
// archived, and leaves the asset store as it found it.
func TestParseDiffCodeBuddyImagesDoNotWriteAssets(t *testing.T) {
	tests := []struct {
		name string
		// prepare changes the image files after the import and returns the
		// asset store entries parse-diff must leave in place.
		prepare func(t *testing.T, blobPath, assetsDir, saved string) []string
	}{
		{
			name: "original present, archived copy missing",
			prepare: func(t *testing.T, _, _, saved string) []string {
				require.NoError(t, os.Remove(saved))
				return nil
			},
		},
		{
			name: "original deleted, archived copy present",
			prepare: func(t *testing.T, blobPath, _, saved string) []string {
				require.NoError(t, os.Remove(blobPath))
				return []string{filepath.Base(saved)}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database := openTestDB(t)
			home, blobPath, _, prompt := cbcImageSync(t, database)
			assetsDir := t.TempDir()
			database.SetAssetsDir(assetsDir)
			engine := newCBCEngine(t, database, home)
			engine.SyncAll(t.Context(), nil)
			link := cbcAssetLinkRe.FindStringSubmatch(prompt())
			require.Len(t, link, 2, "the import links the image")
			want := tc.prepare(t, blobPath, assetsDir, filepath.Join(assetsDir, link[1]))

			diff := NewDiffEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodeBuddy: {home}},
				Machine:   "test",
			})
			t.Cleanup(diff.Close)
			report, err := diff.ParseDiff(t.Context(), ParseDiffOptions{
				Agents: []parser.AgentType{parser.AgentCodeBuddy},
			})
			require.NoError(t, err)
			assert.Equal(t, ParseDiffTotals{Examined: 1, Identical: 1}, report.Totals,
				"the re-parse links the image the archive saved")

			entries, err := os.ReadDir(assetsDir)
			require.NoError(t, err)
			var got []string
			for _, entry := range entries {
				got = append(got, entry.Name())
			}
			assert.Equal(t, want, got, "parse-diff writes nothing to the asset store")
		})
	}
}
