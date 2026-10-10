package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func codexParentTurnsFixture(tb testing.TB, pages, turns int) (ProviderFactory, ProviderConfig, SourceRef) {
	tb.Helper()
	factory, cfg, source := codexForkInventoryFixture(tb, 0)
	for page := range pages {
		var content strings.Builder
		for turn := range turns {
			fmt.Fprintf(&content, "{\"type\":\"turn_context\",\"payload\":{\"turn_id\":\"page-%d-turn-%d\"}}\n", page, turn)
		}
		name := fmt.Sprintf("rollout-2026-09-01T12-00-00-%s_00000000-0000-4000-8000-%012d.jsonl", revertThread, page)
		require.NoError(tb, os.WriteFile(filepath.Join(cfg.Roots[0], name), []byte(content.String()), 0o600))
	}
	return factory, cfg, source
}

func TestCodexForkParentTurnsReuseCombinedSet(t *testing.T) {
	for _, pages := range []int{2, 80} {
		t.Run(strconv.Itoa(pages), func(t *testing.T) {
			var allocations []float64
			for _, turns := range []int{1, 2000} {
				factory, cfg, source := codexParentTurnsFixture(t, pages, turns)
				allocations = append(allocations, testing.AllocsPerRun(3, func() {
					p := factory.NewProvider(cfg).(*codexProvider)
					ids, ok := p.parentTurnResolver(t.Context(), source.Opaque.(codexSource).Path)(revertThread)
					require.True(t, ok)
					require.Len(t, ids, pages*turns+1)
					_, hasHead := ids["head-turn"]
					_, hasPage := ids["page-0-turn-0"]
					require.True(t, hasHead)
					require.True(t, hasPage)
				}))
			}
			t.Logf("warm allocations for 1 and 2000 turns per page: %v", allocations)
			assert.Less(t, allocations[1], 2*allocations[0], "unchanged parent turns must not be read or copied per fork")
		})
	}
}

func BenchmarkCodexForkParentTurns(b *testing.B) {
	for _, pages := range []int{2, 80} {
		b.Run(strconv.Itoa(pages), func(b *testing.B) {
			factory, cfg, source := codexParentTurnsFixture(b, pages, 2000)
			out, err := factory.NewProvider(cfg).Parse(b.Context(), ParseRequest{Source: source})
			require.NoError(b, err)
			require.Len(b, out.Results, 1)
			require.Len(b, out.Results[0].Result.Messages, 1)
			require.Equal(b, "new answer", out.Results[0].Result.Messages[0].Content)
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

func TestCodexParentTurnCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "parent.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("one\n"), 0o600))
	info, err := os.Stat(path)
	require.NoError(t, err)
	key := codexParentTurnCacheKeyFor(path, info)
	turns := map[string]struct{}{"opaque-turn": {}}

	cache := newCodexParentTurnCache(1)
	cache.Put([]codexParentTurnCacheKey{key}, turns)
	got, ok := cache.Get([]codexParentTurnCacheKey{key})
	require.True(t, ok)
	assert.Equal(t, turns, got)

	other := key
	other.path = filepath.Join(filepath.Dir(path), "other.jsonl")
	cache.Put([]codexParentTurnCacheKey{other}, map[string]struct{}{"other": {}})
	_, kept := cache.Get([]codexParentTurnCacheKey{key})
	assert.False(t, kept)
}
