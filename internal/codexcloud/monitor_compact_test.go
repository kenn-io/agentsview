package codexcloud

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// TestCompactionKeepsThePeakCallRequestPriced parses a turn's token
// records with and without compaction. The call with the largest context
// must stay one request event with the same tokens, because only request
// events get long-context price bands.
func TestCompactionKeepsThePeakCallRequestPriced(t *testing.T) {
	env := newPassEnv(t)
	env.threads.Set(finishedThread("a", ago(time.Hour)))
	env.run(t)
	synced, err := mirror.ReadAll(t.Context(), env.mirrorPath(t, "a"))
	require.NoError(t, err)

	type call struct{ total, last int64 }
	cases := []struct {
		name  string
		calls []call
		peak  int64 // the peak call's total
	}{
		{
			// Context grows through the turn, so the last record is the peak.
			name:  "peak at the end",
			calls: []call{{100, 100}, {300, 200}, {600, 300}, {1000, 400}},
			peak:  1000,
		},
		{
			name:  "peak in the middle",
			calls: []call{{100, 100}, {200, 100}, {1100, 900}, {1200, 100}, {1300, 100}},
			peak:  1100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			at := fetchNow
			var records []mirror.Record
			for _, c := range tc.calls {
				at = at.Add(time.Second)
				data := mirror.TokenUsageData{
					JoinID: "join-1", TurnID: "a-turn-1", Model: "gpt-a", ObservedAt: at,
					Total: mirror.TokenCounts{Input: c.total, Total: c.total},
					Last:  mirror.TokenCounts{Input: c.last, Total: c.last},
				}
				require.NoError(t, appendRecord(&records, mirror.KindTokenUsage, data, at))
			}
			peakEvent := func(records []mirror.Record) parser.ParsedUsageEvent {
				path := filepath.Join(t.TempDir(), "a.jsonl")
				_, err := mirror.Append(path, 0, slices.Concat(synced, records))
				require.NoError(t, err)
				result, ok, err := parser.ParseCodexCloudFile(t.Context(), path, "m")
				require.NoError(t, err)
				require.True(t, ok)
				// The peak call's event is the one observed when the call was.
				idx := slices.IndexFunc(tc.calls, func(c call) bool { return c.total == tc.peak })
				observed := fetchNow.Add(time.Duration(idx+1) * time.Second).UTC().Format(time.RFC3339Nano)
				i := slices.IndexFunc(result.UsageEvents, func(ev parser.ParsedUsageEvent) bool {
					return ev.OccurredAt == observed
				})
				require.GreaterOrEqual(t, i, 0, "a usage event for the peak call")
				return result.UsageEvents[i]
			}
			want := peakEvent(records)
			require.Equal(t, usagefacts.CodexCloudRequestSource, want.Source)

			compacted := compactPending(slices.Clone(records))
			require.Less(t, len(compacted), len(records), "compaction drops a record")
			got := peakEvent(compacted)

			assert.Equal(t, usagefacts.CodexCloudRequestSource, got.Source,
				"the peak call is still priced as one request")
			assert.Equal(t, want.InputTokens, got.InputTokens)
			assert.Equal(t, want.OutputTokens, got.OutputTokens)
			assert.Equal(t, want.Model, got.Model)
		})
	}
}
