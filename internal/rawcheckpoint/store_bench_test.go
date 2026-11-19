package rawcheckpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawsync"
)

func BenchmarkCheckpointSourceHead(b *testing.B) {
	store, err := Open(b.Context(), filepath.Join(b.TempDir(), "checkpoint.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	require.NoError(b, store.SetDevice(b.Context(), "device-a"))
	commit := testCommitResult(1, 1)
	require.NoError(b, store.AdvanceHead(b.Context(), "device-a", parser.AgentClaude,
		"root-a", "source-a", "", commit))
	_, _, err = store.SourceHead(b.Context(), parser.AgentClaude, "root-a", "source-a")
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		head, found, err := store.SourceHead(b.Context(), parser.AgentClaude, "root-a", "source-a")
		require.NoError(b, err)
		require.True(b, found)
		require.Equal(b, commit.Receipt, head.Receipt)
	}
}

func BenchmarkCheckpointAdvanceHead(b *testing.B) {
	store, err := Open(b.Context(), filepath.Join(b.TempDir(), "checkpoint.db"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })
	require.NoError(b, store.SetDevice(b.Context(), "device-a"))
	parent := ""
	for i := range 2 {
		commit := testCommitResult(i+1, int64(i+1))
		require.NoError(b, store.AdvanceHead(b.Context(), "device-a", parser.AgentClaude,
			"root-a", "source-a", parent, commit))
		parent = commit.Receipt
	}
	commits := make([]rawsync.CommitResult, b.N)
	for i := range commits {
		commits[i] = testCommitResult(i+3, int64(i+3))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for _, commit := range commits {
		require.NoError(b, store.AdvanceHead(b.Context(), "device-a", parser.AgentClaude,
			"root-a", "source-a", parent, commit))
		parent = commit.Receipt
	}
	b.StopTimer()
	head, found, err := store.SourceHead(b.Context(), parser.AgentClaude, "root-a", "source-a")
	require.NoError(b, err)
	require.True(b, found)
	require.Equal(b, int64(b.N+2), head.Generation)
	require.Equal(b, parent, head.Receipt)
}

// Each iteration queues another generation. Compare fixed iteration counts
// because the fixture grows during the benchmark.
func BenchmarkCheckpointCapture(b *testing.B) {
	for _, entryCount := range []int{1, 100} {
		b.Run(fmt.Sprintf("entries=%d", entryCount), func(b *testing.B) {
			dir := b.TempDir()
			store, err := Open(b.Context(), filepath.Join(dir, "checkpoint.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, store.Close()) })
			sourceDir := filepath.Join(dir, "sources")
			require.NoError(b, os.MkdirAll(sourceDir, 0o700))
			root, err := store.ResolveConfiguredRoot(b.Context(), parser.AgentClaude, sourceDir)
			require.NoError(b, err)
			ref := rawsync.ObjectRef{SHA256: abcObjectSHA256, Length: 3}
			objectPath := store.ObjectPath(ref)
			require.NoError(b, os.MkdirAll(filepath.Dir(objectPath), 0o700))
			require.NoError(b, os.WriteFile(objectPath, []byte("abc"), 0o600))
			base := testCapturedGeneration(1, root, "", ref)
			entries := make([]CapturedEntry, entryCount)
			for i := range entries {
				entries[i] = base.Entries[0]
				entries[i].Path = fmt.Sprintf("project/session-%03d.jsonl", i)
			}
			base.Entries = entries
			charge := CaptureMetadataCharge(entryCount, entryCount) + ref.Length
			reservation, err := store.ReserveSourceCapture(b.Context(), base.Source, charge)
			require.NoError(b, err)
			require.NoError(b, store.CommitCapture(b.Context(), reservation.ID, base))
			generations := make([]CapturedGeneration, b.N)
			parent := base.CaptureID
			for i := range generations {
				generations[i] = testCapturedGeneration(i+2, root, parent, ref)
				generations[i].Entries = entries
				parent = generations[i].CaptureID
			}
			b.ReportAllocs()
			b.ResetTimer()
			for _, generation := range generations {
				reservation, err := store.ReserveSourceCapture(b.Context(), generation.Source, charge)
				require.NoError(b, err)
				require.NoError(b, store.CommitCapture(b.Context(), reservation.ID, generation))
			}
			b.StopTimer()
			latest, found, err := store.CaptureBase(b.Context(), base.Source)
			require.NoError(b, err)
			require.True(b, found)
			require.Equal(b, parent, latest.CaptureID)
			require.Equal(b, entries, latest.Entries)
		})
	}
}
