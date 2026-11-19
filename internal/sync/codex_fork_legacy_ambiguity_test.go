package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Ambiguous legacy usage must remain withheld after parent recovery.
// Controls prove unambiguous and current-source usage can still recover.
func TestCodexForkLegacyDuplicateUsageOwnership(t *testing.T) {
	const parent = "11111111-1111-4111-8111-111111111111"
	const child = "22222222-2222-4222-8222-222222222222"
	const id = "codex:" + child
	const ts = "2026-09-01T10:00:00Z"
	const later = "2026-09-01T11:00:00Z"
	for _, stage := range []int64{1, 1 << 30} {
		for _, scenario := range []string{"collision", "distinct-key", "complete-occurrences", "current-usage"} {
			t.Run(fmt.Sprintf("stage=%d/%s", stage, scenario), func(t *testing.T) {
				root := t.TempDir()
				cp := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+child+".jsonl")
				pp := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+parent+".jsonl")
				childTS := ts
				if scenario == "distinct-key" {
					childTS = later
				}
				meta := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(child, parent, "/work/project", "user", ts))
				inherited := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", ts),
					testjsonl.CodexMsgJSON("assistant", "same answer", ts),
				)
				childTurn := testjsonl.JoinJSONL(
					testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", childTS),
					testjsonl.CodexMsgJSON("assistant", "same answer", childTS),
				)
				full := meta + inherited + testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(ts, 100, 40, 0)) + childTurn + testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(childTS, 20, 5, 0))
				require.NoError(t, os.WriteFile(cp, []byte(full), 0o600))

				// Establish the pre-provenance shape through the real parser
				// and row preparation used by the importer before this PR.
				provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}, Machine: "local"})
				require.True(t, ok)
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				require.Len(t, sources, 1)
				parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0], Machine: "local", ForceParse: true})
				require.NoError(t, err)
				require.Len(t, parsed.Results, 1)
				require.Equal(t, parser.DataVersionNeedsRetry, parsed.Results[0].DataVersion)
				candidate, err := ingest.PrepareCandidate(t.Context(), parsed.Results[0].Result, ingest.ContentOptions{})
				require.NoError(t, err)
				rows, err := ingest.FinalizeRows(t.Context(), candidate, ingest.ContentOptions{})
				require.NoError(t, err)
				require.Len(t, rows.Messages, 2)
				require.Equal(t, 40, rows.Messages[0].OutputTokens)
				require.Equal(t, 5, rows.Messages[1].OutputTokens)
				key0, err := db.CodexForkMessageKey(rows.Messages[0])
				require.NoError(t, err)
				key1, err := db.CodexForkMessageKey(rows.Messages[1])
				require.NoError(t, err)
				if scenario == "distinct-key" {
					require.NotEqual(t, key0, key1)
				} else {
					require.Equal(t, key0, key1)
				}
				d := openTestDB(t)
				_, err = d.WriteSessionBatchAtomic(t.Context(), []db.SessionBatchWrite{{Session: rows.Session, Messages: rows.Messages, UsageEvents: rows.UsageEvents, DataVersion: db.CurrentDataVersion() - 1, ReplaceMessages: true, SkipSignalUpdates: true}})
				require.NoError(t, err)
				state, err := d.GetCodexForkState(t.Context(), id)
				require.NoError(t, err)
				require.Nil(t, state)

				short := meta + childTurn
				if scenario == "complete-occurrences" {
					short = meta + inherited + childTurn
				}
				if scenario == "current-usage" {
					short += testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(childTS, 20, 5, 0))
				}
				require.NoError(t, os.WriteFile(cp, []byte(short), 0o600))
				e := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "local", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage})
				t.Cleanup(e.Close)
				for retry := range 2 {
					require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
					usage, err := d.GetSessionUsage(t.Context(), id, true)
					require.NoError(t, err)
					require.NotNil(t, usage)
					require.Zero(t, usage.TotalOutputTokens)
					state, err = d.GetCodexForkState(t.Context(), id)
					require.NoError(t, err)
					require.NotNil(t, state)
					if scenario == "collision" {
						require.Len(t, state.Messages, 3, "unchanged retries must not duplicate occurrences")
						var unknownUsage []int
						for _, saved := range state.Messages {
							if saved.Origin.Line == 0 && saved.PendingUsage != nil {
								unknownUsage = append(unknownUsage, saved.PendingUsage.OutputTokens)
							}
							if saved.Origin.TurnID == "child-turn" && saved.PendingUsage != nil {
								assert.Zero(t, saved.PendingUsage.OutputTokens, "legacy ambiguity cannot supply the current child's usage")
							}
						}
						assert.Equal(t, []int{40, 5}, unknownUsage, "ambiguous archived usage remains stored without claiming a source owner")
					}
					t.Logf("unresolved retry=%d messages=%d first_origin=%+v first_pending=%+v", retry, len(state.Messages), state.Messages[0].Origin, state.Messages[0].PendingUsage)
				}
				parentBody := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(parent, "/work/project", "user", ts)) + inherited + testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(ts, 100, 40, 0))
				require.NoError(t, os.WriteFile(pp, []byte(parentBody), 0o600))
				want := 5
				if scenario == "collision" {
					want = 0
				}
				for retry := range 2 {
					require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
					usage, err := d.GetSessionUsage(t.Context(), id, true)
					require.NoError(t, err)
					require.NotNil(t, usage)
					assert.Equal(t, want, usage.TotalOutputTokens)
					if scenario == "collision" {
						state, err = d.GetCodexForkState(t.Context(), id)
						require.NoError(t, err)
						require.NotNil(t, state)
						require.Len(t, state.Messages, 3, "unchanged retries must not duplicate occurrences")
						var unknownUsage []int
						for _, saved := range state.Messages {
							if saved.Origin.Line == 0 && saved.PendingUsage != nil {
								unknownUsage = append(unknownUsage, saved.PendingUsage.OutputTokens)
							}
						}
						assert.Equal(t, []int{40, 5}, unknownUsage, "knowing parent membership alone does not disambiguate originless legacy duplicates")
					}
					t.Logf("resolved retry=%d child_output=%d source_child_usage=5 parent_usage=40", retry, usage.TotalOutputTokens)
				}
				// Explicit source usage counts only the live child while unknown
				// archived usage remains retained and uncounted.
				require.NoError(t, os.WriteFile(cp, []byte(meta+childTurn+testjsonl.JoinJSONL(testjsonl.CodexTokenCountJSON(childTS, 20, 5, 0))), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), id))
				usage, err := d.GetSessionUsage(t.Context(), id, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 5, usage.TotalOutputTokens)
				t.Logf("returned source usage child_output=%d", usage.TotalOutputTokens)
			})
		}
	}
}
