package sync

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const codexNoteHead = `{"timestamp":"2024-01-01T10:00:05Z","type":"event_msg","payload":{"type":"agentsview_test_note",`

func TestCodexCheckpointRestrictedArchives(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122d01"
	const marker = "SOURCE-MARKER-7f3a"
	sessionID := "codex:" + uuid
	for _, content := range []config.ArchiveContent{
		config.ArchiveContentFull,
		config.ArchiveContentTranscripts,
		config.ArchiveContentUsage,
	} {
		t.Run(string(content), func(t *testing.T) {
			root := t.TempDir()
			day := filepath.Join(root, "2024", "01", "01")
			require.NoError(t, os.MkdirAll(day, 0o755))
			path := filepath.Join(day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl")
			require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(
				testjsonl.CodexSessionMetaJSON(uuid, "/workspace/project", "codex_cli_rs", "2024-01-01T10:00:00Z"),
				testjsonl.CodexTurnContextJSON("gpt-5.4", "2024-01-01T10:00:01Z"),
				testjsonl.CodexMsgJSON("user", strings.Repeat("x", 2<<20), "2024-01-01T10:00:02Z"),
			)), 0o644))
			database := openTestDB(t)
			database.SetArchiveContent(content)
			engine := NewEngine(t.Context(), database, EngineConfig{
				AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}},
				Machine:   "local",
			})
			t.Cleanup(engine.Close)
			require.Equal(t, 1, engine.SyncAll(t.Context(), nil).Synced)
			if content == config.ArchiveContentUsage {
				// Usage archives skip the Codex checkpoint gate, not just its rows.
				provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{
					Roots: []string{root}, Machine: "local",
				})
				require.True(t, ok)
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				require.Len(t, sources, 1)
				result, err := engine.codexCheckpointFingerprint(t.Context(), sources[0], parser.DiscoveredFile{
					Agent: parser.AgentCodex, Path: path, ProviderSource: &sources[0],
				})
				require.NoError(t, err)
				assert.Equal(t, codexCheckpointFallback, result.decision)
			}

			original := codexResumeHashFn
			t.Cleanup(func() { codexResumeHashFn = original })
			var hashed int64
			codexResumeHashFn = func(path string, offset, size int64, state []byte) ([]byte, string, error) {
				hashed += size - offset
				return original(path, offset, size, state)
			}
			for i, appendLine := range []func(){
				func() { appendPaddedLine(t, path, codexNoteHead, marker, 63) },
				func() {
					appendFile(t, path, testjsonl.JoinJSONL(
						testjsonl.CodexMsgJSON("assistant", "resumed", "2024-01-01T10:00:06Z"),
					))
				},
			} {
				info, err := os.Stat(path)
				require.NoError(t, err)
				before := engine.contentCheckBytes.Load()
				hashed = 0
				appendLine()
				engine.SyncPaths([]string{path})
				grown, err := os.Stat(path)
				require.NoError(t, err)
				actual, err := ComputeFileHash(path)
				require.NoError(t, err)
				stored, ok := database.GetFileHashByAgentPath(t.Context(), path, "codex")
				require.True(t, ok)
				assert.Equal(t, actual, stored, "append %d", i)

				cp, ok, err := database.GetParserCheckpoint(t.Context(), sessionID)
				require.NoError(t, err)
				if content == config.ArchiveContentUsage {
					assert.False(t, ok, "usage archives keep no Codex checkpoint")
					continue
				}
				require.True(t, ok)
				assert.Equal(t, grown.Size(), cp.Offset, "append %d", i)
				assert.Equal(t, actual, cp.Hash, "append %d", i)
				assert.Equal(t, before, engine.contentCheckBytes.Load(), "append %d", i)
				assert.LessOrEqual(t, hashed, grown.Size()-info.Size()+sha256.BlockSize, "append %d", i)
				if i > 0 {
					continue
				}
				require.Equal(t, int64(sha256.BlockSize-1), grown.Size()%sha256.BlockSize)
				blobs, ok, err := database.GetParserCheckpointBlobs(t.Context(), sessionID)
				require.NoError(t, err)
				require.True(t, ok)
				assert.NotEmpty(t, blobs.Cursor)
				assert.NotContains(t, string(blobs.Cursor), marker)
				if content == config.ArchiveContentFull {
					assert.Contains(t, string(blobs.HashState), marker)
				} else {
					assert.NotContains(t, string(blobs.HashState), marker)
					requireBlockAlignedState(t, path, blobs.HashState)
				}
			}
			if content != config.ArchiveContentTranscripts {
				return
			}
			// Usage archives drop tool call names and IDs, which the cursor
			// keeps, so they refuse even a valid Codex cursor.
			cp, ok, err := database.GetParserCheckpoint(t.Context(), sessionID)
			require.NoError(t, err)
			require.True(t, ok)
			blobs, ok, err := database.GetParserCheckpointBlobs(t.Context(), sessionID)
			require.NoError(t, err)
			require.True(t, ok)
			database.SetArchiveContent(config.ArchiveContentUsage)
			require.NoError(t, database.UpsertParserCheckpoint(t.Context(), *cp, blobs))
			_, ok, err = database.GetParserCheckpoint(t.Context(), sessionID)
			require.NoError(t, err)
			assert.False(t, ok)
		})
	}
}
