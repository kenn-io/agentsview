package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// A new provider for each parse matches the sync engine's factory lifetime.
func codexForkInventoryFixture(tb testing.TB, files int) (ProviderFactory, ProviderConfig, SourceRef) {
	tb.Helper()
	root := tb.TempDir()
	for i := range files {
		name := fmt.Sprintf("rollout-2026-09-01T10-00-00-00000000-0000-4000-8000-%012d.jsonl", i)
		require.NoError(tb, os.WriteFile(filepath.Join(root, name), nil, 0o600))
	}
	parent := testjsonl.JoinJSONL(
		testjsonl.CodexSessionMetaJSON(revertThread, "/work/project", "user", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", tsEarly),
	)
	require.NoError(tb, os.WriteFile(filepath.Join(root, "rollout-2026-09-01T10-00-00-"+revertThread+".jsonl"), []byte(parent), 0o600))
	fork := testjsonl.JoinJSONL(
		testjsonl.CodexForkedSessionMetaJSON(revertOther, revertThread, "/work/project", "user", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", tsEarly),
		testjsonl.CodexMsgJSON("assistant", "copied answer", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", tsEarlyS1),
		testjsonl.CodexMsgJSON("assistant", "new answer", tsEarlyS1),
	)
	path := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+revertOther+".jsonl")
	require.NoError(tb, os.WriteFile(path, []byte(fork), 0o600))
	registered, ok := ProviderFactoryByType(AgentCodex)
	require.True(tb, ok)
	factory := newCodexProviderFactory(registered.Definition())
	cfg := ProviderConfig{Roots: []string{root}}
	source, found, err := factory.NewProvider(cfg).FindSource(tb.Context(), FindSourceRequest{RawSessionID: revertOther})
	require.NoError(tb, err)
	require.True(tb, found)
	return factory, cfg, source
}

func TestCodexForkInventoryDoesNotAllocatePerUnchangedFile(t *testing.T) {
	var allocations []float64
	for _, files := range []int{10, 1000} {
		factory, cfg, source := codexForkInventoryFixture(t, files)
		allocations = append(allocations, testing.AllocsPerRun(5, func() {
			out, err := factory.NewProvider(cfg).Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, out.Results, 1)
			msgs := out.Results[0].Result.Messages
			require.Len(t, msgs, 1)
			require.Equal(t, "new answer", msgs[0].Content)
		}))
	}
	assert.Less(t, allocations[1], 2*allocations[0], "unchanged filenames must not be enumerated per fork")
}

func TestCodexForkInventoryConcurrentProviders(t *testing.T) {
	t.Parallel()
	factory, cfg, source := codexForkInventoryFixture(t, 100)
	for i := range 8 {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Parallel()
			out, err := factory.NewProvider(cfg).Parse(t.Context(), ParseRequest{Source: source})
			require.NoError(t, err)
			require.Len(t, out.Results, 1)
			require.Len(t, out.Results[0].Result.Messages, 1)
			assert.Equal(t, "new answer", out.Results[0].Result.Messages[0].Content)
		})
	}
}

func TestCodexForkInventoryFindsNewAndMovedPages(t *testing.T) {
	for _, dir := range []string{".", "2026/09/01", "2026/09/02", "2027/01/01"} {
		t.Run(dir, func(t *testing.T) {
			factory, cfg, source := codexForkInventoryFixture(t, 0)
			root := cfg.Roots[0]
			archive := t.TempDir()
			cfg.Roots = append(cfg.Roots, archive)
			require.NoError(t, os.MkdirAll(filepath.Join(root, "2026", "09", "01"), 0o700))
			fork := testjsonl.JoinJSONL(
				testjsonl.CodexForkedSessionMetaJSON(revertOther, revertThread, "/work/project", "user", tsEarly),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", tsEarly),
				testjsonl.CodexMsgJSON("assistant", "copied head", tsEarly),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly),
				testjsonl.CodexMsgJSON("assistant", "copied page", tsEarly),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", tsEarlyS1),
				testjsonl.CodexMsgJSON("assistant", "new answer", tsEarlyS1),
			)
			require.NoError(t, os.WriteFile(source.Opaque.(codexSource).Path, []byte(fork), 0o600))
			parse := func() ParseResultOutcome {
				t.Helper()
				out, err := factory.NewProvider(cfg).Parse(t.Context(), ParseRequest{Source: source})
				require.NoError(t, err)
				require.Len(t, out.Results, 1)
				return out.Results[0]
			}
			require.Len(t, parse().Result.Messages, 2, "warm the inventory before the page exists")
			pageDir := filepath.Join(root, filepath.FromSlash(dir))
			require.NoError(t, os.MkdirAll(pageDir, 0o700))
			pagePath := filepath.Join(pageDir, "rollout-2026-09-01T12-00-00-"+revertThread+"_"+revertRollout+".jsonl")
			page := testjsonl.JoinJSONL(
				revertPageMeta(revertThread, revertThread, nil),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly),
			)
			require.NoError(t, os.WriteFile(pagePath, []byte(page), 0o600))
			got := parse()
			require.Len(t, got.Result.Messages, 1)
			assert.Equal(t, "new answer", got.Result.Messages[0].Content)
			// Content changes do not change the directory inventory. The
			// per-file turn cache must still re-read the changed rollout.
			require.NoError(t, os.WriteFile(pagePath, []byte(testjsonl.JoinJSONL(
				revertPageMeta(revertThread, revertThread, nil),
				testjsonl.CodexTurnContextWithIDJSON("gpt-5", "replacement-turn", tsEarly),
			)), 0o600))
			require.Len(t, parse().Result.Messages, 2)
			require.NoError(t, os.WriteFile(pagePath, []byte(page), 0o600))
			require.Len(t, parse().Result.Messages, 1)

			// Archiving moves every rollout. Both inventories must refresh.
			require.NoError(t, os.Rename(pagePath, filepath.Join(archive, filepath.Base(pagePath))))
			headName := "rollout-2026-09-01T10-00-00-" + revertThread + ".jsonl"
			require.NoError(t, os.Rename(filepath.Join(root, headName), filepath.Join(archive, headName)))
			got = parse()
			require.Len(t, got.Result.Messages, 1)
			assert.Equal(t, "new answer", got.Result.Messages[0].Content)
			assert.Equal(t, DataVersionCurrent, got.DataVersion)
			require.NoError(t, os.Remove(filepath.Join(archive, headName)))
			assert.Equal(t, DataVersionNeedsRetry, parse().DataVersion, "a cached head cannot resolve a missing original")
		})
	}
}

func BenchmarkCodexForkParentInventory(b *testing.B) {
	for _, files := range []int{10, 1000} {
		b.Run(strconv.Itoa(files), func(b *testing.B) {
			factory, cfg, source := codexForkInventoryFixture(b, files)
			out, err := factory.NewProvider(cfg).Parse(b.Context(), ParseRequest{Source: source})
			require.NoError(b, err)
			require.Len(b, out.Results, 1)
			require.Len(b, out.Results[0].Result.Messages, 1)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				out, err := factory.NewProvider(cfg).Parse(b.Context(), ParseRequest{Source: source})
				require.NoError(b, err)
				require.Len(b, out.Results, 1)
				require.Len(b, out.Results[0].Result.Messages, 1)
			}
		})
	}
}
