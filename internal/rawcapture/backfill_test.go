package rawcapture

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/rawcheckpoint"
	"go.kenn.io/agentsview/internal/rawsync"
)

func TestBackfillCaptureReusesDurableBindingAfterAppend(t *testing.T) {
	store, _ := openCapturerTestStore(t, 1<<20)
	require.NoError(t, store.SetDevice(t.Context(), "device-a"))
	provider, source, path := captureFileProvider(t, "first\n")
	root, err := store.ResolveConfiguredRoot(t.Context(), parser.AgentClaude, provider.plan.ConfiguredRoot)
	require.NoError(t, err)
	_, err = store.BeginBackfill(t.Context(), rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: root.ID}}})
	require.NoError(t, err)
	first, err := New(store).CaptureForBackfill(t.Context(), provider, source, "run-a")
	require.NoError(t, err)
	member, found, err := store.BackfillSource(t.Context(), "run-a", first.Source)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, first.CaptureID, member.CaptureID)
	require.NoError(t, os.WriteFile(path, []byte("first\nsecond\n"), 0o600))
	retry, err := New(store).CaptureForBackfill(t.Context(), provider, source, "run-a")
	require.NoError(t, err)
	require.Equal(t, first.CaptureID, retry.CaptureID)
	gen, found, err := store.NextGeneration(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, int64(6), gen.Entries[0].Length)
}

func TestBackfillUnchangedBindsPendingAndAcknowledgedBase(t *testing.T) {
	for _, ack := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "acknowledged"}[ack], func(t *testing.T) {
			store, _ := openCapturerTestStore(t, 1<<20)
			require.NoError(t, store.SetDevice(t.Context(), "device-a"))
			provider, source, _ := captureFileProvider(t, "first\n")
			c := New(store)
			base, err := c.Capture(t.Context(), provider, source)
			require.NoError(t, err)
			commit := rawsync.CommitResult{ManifestID: strings.Repeat("a", 64), Receipt: strings.Repeat("b", 64), Generation: 1}
			if ack {
				_, found, err := store.FinalizeNextManifest(t.Context(), "device-a")
				require.NoError(t, err)
				require.True(t, found)
				require.NoError(t, store.BindFinalizedCommit(t.Context(), "device-a", base.CaptureID, commit))
				_, err = store.AcknowledgeGeneration(t.Context(), "device-a", base.CaptureID, commit)
				require.NoError(t, err)
			}
			_, err = store.BeginBackfill(t.Context(), rawcheckpoint.BackfillRunSpec{RunID: "run-a", DeviceID: "device-a", Destination: "https://ingest.example", Providers: []parser.AgentType{parser.AgentClaude}, Roots: []rawcheckpoint.BackfillSelection{{Provider: parser.AgentClaude, ConfiguredRootID: base.Source.ConfiguredRootID}}})
			require.NoError(t, err)
			got, err := c.CaptureForBackfill(t.Context(), provider, source, "run-a")
			require.NoError(t, err)
			require.Equal(t, StatusUnchanged, got.Status)
			require.Equal(t, base.CaptureID, got.CaptureID)
			member, found, err := store.BackfillSource(t.Context(), "run-a", base.Source)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, base.CaptureID, member.CaptureID)
			if ack {
				require.Equal(t, commit.Receipt, member.Receipt)
				require.Equal(t, "acknowledged", member.Status)
			} else {
				require.Empty(t, member.Receipt)
				require.Equal(t, "pending", member.Status)
			}
		})
	}
}
