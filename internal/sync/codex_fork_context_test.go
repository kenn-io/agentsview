package sync

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
)

func BenchmarkCodexForkDivergentContextPrefixes(b *testing.B) {
	for _, size := range []int{1000, 2000, 4000, 8000, 16000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			state := &db.CodexForkState{}
			full := make([]string, size+1)
			full[0] = "omitted-in-current-source"
			for i := range size {
				full[i+1] = fmt.Sprintf("turn-%d", i)
			}
			old, _ := prepareCodexForkContexts(state, full)
			current, _ := prepareCodexForkContexts(state, full[1:])
			b.ResetTimer()
			for b.Loop() {
				memo := make(map[[2]int]bool)
				for i := 1; i <= size; i++ {
					require.True(b, codexForkContextContains(state.TurnContexts, old[i+1], current[i], memo), "shortened snapshot must be contained")
				}
			}
		})
	}
}

func TestCodexForkContextContainsMemoizedSubsequences(t *testing.T) {
	turns := []db.CodexForkTurnContext{
		{Parent: 0, TurnID: "boundary"}, // 1
		{Parent: 1, TurnID: "a"},        // 2
		{Parent: 2, TurnID: "b"},        // 3
		{Parent: 0, TurnID: "a"},        // 4
		{Parent: 4, TurnID: "b"},        // 5
		{Parent: 5, TurnID: "b"},        // 6
		{Parent: 4, TurnID: "c"},        // 7
		{Parent: 3, TurnID: "b"},        // 8
	}
	memo := make(map[[2]int]bool)
	for _, tc := range []struct {
		full, part int
		want       bool
	}{
		{2, 4, true},
		{3, 5, true},
		{8, 6, true},
		{3, 7, false},
		{8, 7, false},
		{4, 2, false},
		{0, 0, true},
		{8, 0, true},
		{0, 8, false},
		{8, 8, true},
	} {
		assert.Equal(t, tc.want, codexForkContextContains(turns, tc.full, tc.part, memo), "full=%d part=%d", tc.full, tc.part)
	}
}
