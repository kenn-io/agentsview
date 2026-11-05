package sync

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/ingest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// Imported artifact wire events omit raw digest and summary participation.
// Reparse the synthetic source only to establish a valid message/call shape;
// omit those two fields before the real artifact-import database boundary.
func TestCodexForkImportedResultOccurrences(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		for _, current := range []string{"absent", "fresh", "idless"} {
			t.Run(strconv.FormatInt(stage, 10)+"/"+current, func(t *testing.T) {
				root := t.TempDir()
				cp := filepath.Join(root, "rollout-2026-09-01T11-00-00-"+forkReviewChild+".jsonl")
				pp := filepath.Join(root, "rollout-2026-09-01T10-00-00-"+forkReviewParent+".jsonl")
				meta := testjsonl.JoinJSONL(testjsonl.CodexForkedSessionMetaJSON(forkReviewChild, forkReviewParent, "/work/project", "user", forkReviewTime))
				prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
				full := prefix + testjsonl.JoinJSONL(
					testjsonl.CodexFunctionCallOutputJSON("call_test", "alpha", "2026-09-01T10:01:00Z"),
					testjsonl.CodexFunctionCallOutputJSON("call_test", "second longer", "2026-09-01T10:02:00Z"),
				)
				require.NoError(t, os.WriteFile(cp, []byte(full), 0o600))
				provider, ok := parser.NewProvider(parser.AgentCodex, parser.ProviderConfig{Roots: []string{root}, Machine: "fixture"})
				require.True(t, ok)
				sources, err := provider.Discover(t.Context())
				require.NoError(t, err)
				require.Len(t, sources, 1)
				parsed, err := provider.Parse(t.Context(), parser.ParseRequest{Source: sources[0], Machine: "fixture", ForceParse: true})
				require.NoError(t, err)
				require.Len(t, parsed.Results, 1)
				candidate, err := ingest.PrepareCandidate(t.Context(), parsed.Results[0].Result, ingest.ContentOptions{})
				require.NoError(t, err)
				rows, err := ingest.FinalizeRows(t.Context(), candidate, ingest.ContentOptions{})
				require.NoError(t, err)
				require.Len(t, rows.Messages, 1)
				require.Len(t, rows.Messages[0].ToolCalls, 1)
				require.Len(t, rows.Messages[0].ToolCalls[0].ResultEvents, 2)
				id := "fixture~codex:" + forkReviewChild
				rows.Session.ID, rows.Session.Machine = id, "fixture"
				rows.Session.ParentSessionID = new("fixture~codex:" + forkReviewParent)
				for i := range rows.Messages {
					rows.Messages[i].SessionID = id
					for j := range rows.Messages[i].ToolCalls {
						rows.Messages[i].ToolCalls[j].SessionID = id
						if current == "idless" {
							rows.Messages[i].ToolCalls[j].ToolUseID = ""
						}
						for k := range rows.Messages[i].ToolCalls[j].ResultEvents {
							event := &rows.Messages[i].ToolCalls[j].ResultEvents[k]
							event.RawContentDigest, event.SummaryParticipates = nil, nil
							if current == "idless" {
								event.ToolUseID = ""
							}
						}
					}
				}
				d := openTestDB(t)
				imported := db.ArtifactImportedSession{Origin: "fixture", GID: id, ManifestHash: strings.Repeat("a", 64), ImportedSessionID: id}
				result, err := d.ApplyArtifactImportedSession(t.Context(), imported, db.SessionBatchWrite{Session: rows.Session, Messages: rows.Messages, ReplaceMessages: true, DataVersion: 17, SkipSignalUpdates: true})
				require.NoError(t, err)
				require.True(t, result.Written)
				initial, err := d.GetAllMessages(t.Context(), id)
				require.NoError(t, err)
				require.Len(t, initial, 1)
				require.Len(t, initial[0].ToolCalls[0].ResultEvents, 2)
				assert.Nil(t, initial[0].ToolCalls[0].ResultEvents[0].RawContentDigest)
				assert.Nil(t, initial[0].ToolCalls[0].ResultEvents[1].RawContentDigest)
				assert.Nil(t, initial[0].ToolCalls[0].ResultEvents[0].SummaryParticipates)
				t.Logf("accepted artifact import: events=%d lengths=[%d,%d] digests=[%x,%x]", len(initial[0].ToolCalls[0].ResultEvents), initial[0].ToolCalls[0].ResultEvents[0].ContentLength, initial[0].ToolCalls[0].ResultEvents[1].ContentLength, initial[0].ToolCalls[0].ResultEvents[0].RawContentDigest, initial[0].ToolCalls[0].ResultEvents[1].RawContentDigest)
				e := NewEngine(t.Context(), d, EngineConfig{AgentDirs: map[parser.AgentType][]string{parser.AgentCodex: {root}}, Machine: "fixture", IDPrefix: "fixture~", DisableFilesystemProjectDiscovery: true, StagedCodexParseMinBytes: stage, ToolResultImages: config.ToolResultImagesDrop, BlockedResultCategories: []string{"Bash"}})
				t.Cleanup(e.Close)
				if current == "idless" {
					e.blockedResultCategories = nil
					prefix = meta
				}
				require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
				for retry := range 4 {
					want, wantLength := 2, 13
					if retry >= 1 && current == "fresh" {
						require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "fresh source result", "2026-09-01T10:03:00Z"))), 0o600))
						want, wantLength = 3, 19
					}
					if retry == 3 {
						require.NoError(t, os.WriteFile(pp, []byte(testjsonl.JoinJSONL(testjsonl.CodexSessionMetaJSON(forkReviewParent, "/work/project", "user", forkReviewTime))), 0o600))
					}
					stats := e.SyncAllForceParse(t.Context(), nil)
					require.Zero(t, stats.Failed)
					stored, err := d.GetAllMessages(t.Context(), id)
					require.NoError(t, err)
					require.Len(t, stored, 1)
					require.Len(t, stored[0].ToolCalls, 1)
					call := stored[0].ToolCalls[0]
					var timestamps []string
					var lengths []int
					known := 0
					for _, event := range call.ResultEvents {
						timestamps = append(timestamps, event.Timestamp)
						lengths = append(lengths, event.ContentLength)
						if event.RawContentDigest != nil {
							known++
						}
						if current == "idless" {
							assert.NotEmpty(t, event.Content)
						} else {
							assert.Empty(t, event.Content)
						}
						if event.Timestamp != "2026-09-01T10:03:00Z" {
							assert.Nil(t, event.RawContentDigest)
							assert.Nil(t, event.SummaryParticipates)
						}
					}
					t.Logf("retry=%d parent=%v events=%d known=%d lengths=%v times=%v summaryLength=%d", retry, retry == 3, len(call.ResultEvents), known, lengths, timestamps, call.ResultContentLength)
					if current == "idless" {
						assert.Equal(t, "second longer", call.ResultContent)
					}
					assert.Equal(t, wantLength, call.ResultContentLength, "retry %d", retry)
					assert.Len(t, call.ResultEvents, want, "accepted historical occurrences must survive and fresh source event must appear once")
					assert.Contains(t, timestamps, "2026-09-01T10:01:00Z")
					assert.Contains(t, timestamps, "2026-09-01T10:02:00Z")
					if current == "fresh" && retry >= 1 {
						assert.Equal(t, 1, known, "fresh source event must be deduplicated across retries")
					}
				}
			})
		}
	}
}

func TestCodexForkNativeResultIdentity(t *testing.T) {
	for _, stage := range []int64{1, 1 << 30} {
		t.Run(strconv.FormatInt(stage, 10), func(t *testing.T) {
			e, d, cp, _, meta := forkReviewEngine(t, stage, parser.AgentCodex)
			prefix := meta + testjsonl.JoinJSONL(testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", forkReviewTime), testjsonl.CodexFunctionCallJSON("exec_command", "", forkReviewTime))
			require.NoError(t, os.WriteFile(cp, []byte(prefix+testjsonl.JoinJSONL(testjsonl.CodexFunctionCallOutputJSON("call_test", "alpha", "2026-09-01T10:01:00Z"), testjsonl.CodexFunctionCallOutputJSON("call_test", "second longer", "2026-09-01T10:02:00Z"))), 0o600))
			require.Equal(t, 1, e.SyncAll(t.Context(), nil).Deferred)
			e.blockedResultCategories = map[string]bool{"Bash": true}
			require.NoError(t, os.WriteFile(cp, []byte(prefix), 0o600))
			for retry := range 3 {
				require.NoError(t, e.TrySyncSessionWithSubagentsContext(t.Context(), "codex:"+forkReviewChild))
				stored, err := d.GetAllMessages(t.Context(), "codex:"+forkReviewChild)
				require.NoError(t, err)
				require.Len(t, stored, 1)
				call := stored[0].ToolCalls[0]
				require.Len(t, call.ResultEvents, 2)
				assert.NotEmpty(t, call.ResultEvents[0].RawContentDigest)
				assert.NotEmpty(t, call.ResultEvents[1].RawContentDigest)
				assert.NotEqual(t, call.ResultEvents[0].RawContentDigest, call.ResultEvents[1].RawContentDigest)
				assert.Equal(t, 13, call.ResultContentLength)
				t.Logf("native retry=%d events=%d summaryLength=%d", retry, len(call.ResultEvents), call.ResultContentLength)
			}
		})
	}
}
