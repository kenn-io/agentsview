package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A merge stays incremental only when every source part already carried
// the merged part's mutation. A source that took the mutation after the
// tracker read it changed rows the tracker counted, so the table is read
// again. A later insert never needed the mutation, so merging it keeps the
// check incremental.
func TestFinalRowsPlanMergeAfterMutation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		before, now []partInfo
		incremental bool
	}{
		{
			name: "mutation finished on a part read before it, then merged",
			before: []partInfo{
				{name: "all_1_5_1_10", partition: "all", minBlock: 1, maxBlock: 5, mutation: 10, rows: 5},
				{name: "all_6_8_1", partition: "all", minBlock: 6, maxBlock: 8, rows: 3},
			},
			now: []partInfo{
				{name: "all_1_8_2_10", partition: "all", minBlock: 1, maxBlock: 8, mutation: 10, rows: 7},
			},
			incremental: false,
		},
		{
			name: "mutated part merged with a later insert",
			before: []partInfo{
				{name: "all_1_9_1_10", partition: "all", minBlock: 1, maxBlock: 9, mutation: 10, rows: 9},
				{name: "all_11_11_0", partition: "all", minBlock: 11, maxBlock: 11, rows: 1},
			},
			now: []partInfo{
				{name: "all_1_11_2_10", partition: "all", minBlock: 1, maxBlock: 11, mutation: 10, rows: 10},
			},
			incremental: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			byName := func(parts []partInfo) map[string]partInfo {
				m := map[string]partInfo{}
				for _, p := range parts {
					m[p.name] = p
				}
				return m
			}
			f := &finalRows{spec: sessionsFinalTable, parts: byName(tc.before)}
			_, ok := f.plan(byName(tc.now))
			require.Equal(t, tc.incremental, ok)
		})
	}
}
