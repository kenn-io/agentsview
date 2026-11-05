package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestCodexForkMessageFreeBoundarySurvivesShortening(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(fmt.Sprintf("stage=%d", stage), func(t *testing.T) {
			e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			replay := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "parent replay", forkReviewTime))
			child := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "message-free-child", forkReviewTime), testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "retained idless child", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))
			require.NoError(t, os.WriteFile(cp, []byte(meta+replay+child), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			require.NoError(t, os.WriteFile(cp, []byte(meta+replay), 0o600))
			parent := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime)) + replay + testjsonl.JoinJSONL(testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime))
			require.NoError(t, os.WriteFile(pp, []byte(parent), 0o600))
			for _, rebuild := range []bool{false, true} {
				if rebuild {
					stats := e.ResyncAll(t.Context(), nil)
					require.True(t, stats.ArchiveRebuilt)
					require.Zero(t, stats.Failed)
				} else {
					require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				}
				assert.Equal(t, []string{"retained idless child"}, messageContents(t, d, "codex:"+forkReviewChild))
				usage, err := d.GetSessionUsage(t.Context(), "codex:"+forkReviewChild, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				assert.Equal(t, 5, usage.TotalOutputTokens)
			}
		})
	}
}

func TestCodexForkMessageFreeChildBoundary(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, tc := range []struct {
			name                               string
			parentIdless, childBoundaryMessage bool
		}{
			{"named_parent_control", false, false},
			{"message_bearing_boundary_control", true, true},
			{"message_free_boundary", true, false},
		} {
			t.Run(fmt.Sprintf("stage=%d/%s", stage, tc.name), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				replay := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "parent replay", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 100, 40, 0))
				boundary := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime))
				if tc.childBoundaryMessage {
					boundary += testjsonl.JoinJSONL(testjsonl.CodexMsgJSON("user", "child boundary message", forkReviewTime))
				}
				child := testjsonl.JoinJSONL(testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "idless child answer", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))
				require.NoError(t, os.WriteFile(cp, []byte(meta+replay+boundary+child), 0o600))
				require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
				t.Logf("UNRESOLVED_TRANSCRIPT: %q", messageContents(t, d, "codex:"+forkReviewChild))
				parent := testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime)) + replay
				if tc.parentIdless {
					parent += testjsonl.JoinJSONL(testjsonl.CodexTurnContextJSON("gpt-5", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "different old idless parent answer", forkReviewTime))
				}
				require.NoError(t, os.WriteFile(pp, []byte(parent), 0o600))
				provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{filepath.Dir(cp)}, Machine: "local"})
				require.True(t, ok)
				source, ok, err := provider.FindSource(t.Context(), parser.FindSourceRequest{FullSessionID: "codex:" + forkReviewChild})
				require.NoError(t, err)
				require.True(t, ok)
				fingerprint, err := provider.Fingerprint(t.Context(), source)
				require.NoError(t, err)
				parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: source, Fingerprint: fingerprint})
				require.NoError(t, err)
				require.Len(t, parsed.Results, 1)
				var parsedContents []string
				for _, m := range parsed.Results[0].Result.Messages {
					parsedContents = append(parsedContents, m.Content)
					t.Logf("PARSER_MESSAGE: %q; ORIGIN: %+v", m.Content, m.CodexOrigin)
				}
				parserWant := []string{"idless child answer"}
				if tc.childBoundaryMessage {
					parserWant = []string{"child boundary message", "idless child answer"}
				}
				require.Equal(t, parserWant, parsedContents)
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				got := messageContents(t, d, "codex:"+forkReviewChild)
				usage, err := d.GetSessionUsage(t.Context(), "codex:"+forkReviewChild, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				t.Logf("RECOVERED_TRANSCRIPT: %q; OUTPUT_TOKENS: %d", got, usage.TotalOutputTokens)
				want := []string{"idless child answer"}
				if tc.childBoundaryMessage {
					want = []string{"child boundary message", "idless child answer"}
				}
				assert.Equal(t, want, got)
				assert.Equal(t, 5, usage.TotalOutputTokens)
			})
		}
	}
}

func TestCodexForkReorderedArchivedReplay(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, withChild := range []bool{true, false} {
			t.Run(fmt.Sprintf("stage=%d/child_in_second_snapshot=%v", stage, withChild), func(t *testing.T) {
				e, d, cp, pp, meta := forkReviewEngine(t, stage, parser.AgentCodex)
				child := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "child answer", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 20, 5, 0))
				replay := testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "parent-turn", forkReviewTime), testjsonl.CodexMsgJSON("assistant", "parent replay", forkReviewTime), testjsonl.CodexTokenCountJSON(forkReviewTime, 100, 40, 0))
				require.NoError(t, os.WriteFile(cp, []byte(meta+child), 0o600))
				require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
				second := meta + replay
				if withChild {
					second += child
				}
				require.NoError(t, os.WriteFile(cp, []byte(second), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				t.Logf("AFTER_SECOND_SNAPSHOT: %q", messageContents(t, d, "codex:"+forkReviewChild))
				saved, err := d.GetCodexForkState(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.NotNil(t, saved)
				for i, m := range saved.Messages {
					t.Logf("SAVED_ORIGIN ordinal=%d line=%d turn=%q in_turn=%v provisional=%v", i, m.Origin.Line, m.Origin.TurnID, m.Origin.InTurn, m.Provisional)
				}
				require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))+replay), 0o600))
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				got := messageContents(t, d, "codex:"+forkReviewChild)
				usage, err := d.GetSessionUsage(t.Context(), "codex:"+forkReviewChild, true)
				require.NoError(t, err)
				require.NotNil(t, usage)
				t.Logf("RECOVERED_TRANSCRIPT: %q; OUTPUT_TOKENS: %d", got, usage.TotalOutputTokens)
				assert.Equal(t, []string{"child answer"}, got)
				assert.Equal(t, 5, usage.TotalOutputTokens)
			})
		}
	}
}
