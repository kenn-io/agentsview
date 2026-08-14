package sync

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// TestCodexStagedParityPerturbations runs the full dual-path DB comparison
// over structurally perturbed transcripts: reordered lines, truncation,
// duplicated and interleaved events, junk and empty lines, and a forked
// session-meta replay. Every variant must produce byte-identical stored
// projections from both paths.
func TestCodexStagedParityPerturbations(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	base := strings.Split(
		strings.TrimSuffix(codexParityTranscript(uuid), "\n"), "\n",
	)
	cases := map[string]func() string{
		"reversed lines": func() string {
			rev := slices.Clone(base)
			slices.Reverse(rev)
			return strings.Join(rev, "\n")
		},
		"truncated tail": func() string {
			// Drop the last output line: call_c has no result event.
			return strings.Join(base[:len(base)-1], "\n")
		},
		"duplicated output": func() string {
			out := slices.Clone(base)
			out = slices.Insert(out, 7, base[6])
			return strings.Join(out, "\n")
		},
		"interleaved outputs": func() string {
			out := slices.Clone(base)
			out[6], out[12] = out[12], out[6]
			return strings.Join(out, "\n")
		},
		"junk lines": func() string {
			out := slices.Clone(base)
			out = slices.Insert(out, 3, "not json at all", `{"type":"unknown"}`)
			return strings.Join(out, "\n")
		},
		"empty lines": func() string {
			out := slices.Clone(base)
			out = slices.Insert(out, 2, "", "   ")
			return strings.Join(out, "\n")
		},
		"forked meta replay": func() string {
			meta := testjsonl.CodexSessionMetaJSON(
				uuid, "/workspace/project-a", "codex_cli_rs",
				"2024-01-01T10:00:00Z",
			)
			out := slices.Clone(base)
			out = slices.Insert(out, 1, meta)
			return strings.Join(out, "\n")
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			assertCodexStagedParity(t, uuid, build())
		})
	}
}

// failingStagedResults wraps a real staging sink and fails a chosen
// publish operation, simulating a mid-publish abort.
type failingStagedResults struct {
	*codexStagingSink
	failResolve  bool
	failEvents   bool
	resolveCalls int
	eventsCalls  int
}

func (f *failingStagedResults) ResolveSummary(
	ctx context.Context, toolUseID string,
) (string, int, error) {
	f.resolveCalls++
	if f.failResolve {
		return "", 0, fmt.Errorf("injected summary resolution failure")
	}
	return f.codexStagingSink.ResolveSummary(ctx, toolUseID)
}

func (f *failingStagedResults) InsertEventsTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
	positions map[string]db.StagedToolCallPosition,
) error {
	f.eventsCalls++
	if f.failEvents {
		return fmt.Errorf("injected event insert failure")
	}
	return f.codexStagingSink.InsertEventsTx(
		ctx, tx, sessionID, positions,
	)
}

// TestCodexStagedPublishFailureKeepsPriorContent pins the atomicity
// guarantee: when the staged publish aborts mid-transaction (summary
// resolution or event insert), the archive keeps the complete prior
// content — never a partial rewrite.
func TestCodexStagedPublishFailureKeepsPriorContent(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(
		path, []byte(codexParityTranscript(uuid)), 0o644,
	))
	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(
		context.Background(), parser.FindSourceRequest{
			FullSessionID: "codex:" + uuid,
		},
	)
	require.NoError(t, err)
	require.True(t, found)

	// Prior content via the collecting path (the complete old version).
	database := openTestDB(t)
	legacySink := parser.NewCodexCollectingSink(0)
	legacySess, legacyMsgs, _, _, _, _, err :=
		parser.ParseCodexSessionStreaming(cfg, source, legacySink)
	require.NoError(t, err)
	require.NotNil(t, legacySess)
	row := db.Session{
		ID:               legacySess.ID,
		Project:          legacySess.Project,
		Machine:          legacySess.Machine,
		Agent:            string(legacySess.Agent),
		MessageCount:     legacySess.MessageCount,
		UserMessageCount: legacySess.UserMessageCount,
	}
	require.NoError(t, database.UpsertSession(row))
	dbMsgs := toDBMessages(pendingWrite{
		sess: *legacySess, msgs: legacyMsgs,
	}, nil)
	update, findings := computeSignalsAndSecrets(row, dbMsgs)
	require.NoError(t, database.ReplaceSessionContent(
		row.ID, dbMsgs, update, findings,
	))
	before, err := database.GetAllMessages(context.Background(), row.ID)
	require.NoError(t, err)
	require.NotEmpty(t, before)

	// A failed staged publish must leave the prior rows untouched. The
	// summary-resolution failure aborts while the transaction resolves
	// per-call summaries; the event-insert failure aborts after messages
	// and tool calls were staged into the same transaction. Both must
	// roll back to the complete prior content.
	for _, tc := range []struct {
		name    string
		resolve bool
		events  bool
	}{
		{name: "summary resolution failure", resolve: true},
		{name: "event insert failure", events: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			staged, err := newCodexStagingSink(map[string]bool{})
			require.NoError(t, err)
			failing := &failingStagedResults{
				codexStagingSink: staged,
				failResolve:      tc.resolve,
				failEvents:       tc.events,
			}
			t.Cleanup(func() { require.NoError(t, failing.Close()) })
			stagedSess, stagedMsgs, _, _, _, _, err :=
				parser.ParseCodexSessionStreaming(cfg, source, failing)
			require.NoError(t, err)
			require.NotNil(t, stagedSess)
			stagedDBMsgs := toDBMessages(pendingWrite{
				sess: *stagedSess, msgs: stagedMsgs,
			}, nil)
			positions := make(map[string]db.StagedToolCallPosition)
			for _, m := range stagedDBMsgs {
				for callIdx, tc := range m.ToolCalls {
					if tc.ToolUseID == "" {
						continue
					}
					positions[tc.ToolUseID] =
						db.StagedToolCallPosition{
							ToolUseID: tc.ToolUseID,
							Ordinal:   m.Ordinal,
							CallIndex: callIdx,
						}
				}
			}
			err = database.ReplaceSessionContentStaged(
				context.Background(), row.ID, stagedDBMsgs, failing,
				map[string]bool{},
				func(verdicts map[string]bool) (
					db.SessionSignalUpdate, []db.SecretFinding, error,
				) {
					update, findings :=
						computeSignalsAndSecretsWithContentFailures(
							row, stagedDBMsgs, verdicts,
						)
					combined := append(
						append([]db.SecretFinding(nil), findings...),
						failing.Findings(row.ID, positions)...,
					)
					update.SecretLeakCount =
						definiteFindingCount(combined)
					return update, combined, nil
				},
			)
			require.Error(t, err,
				"the injected failure must abort the publish")
			after, err := database.GetAllMessages(
				context.Background(), row.ID,
			)
			require.NoError(t, err)
			require.Equal(t, before, after,
				"aborted staged publish must keep the prior content")
		})
	}

	// After two aborted publishes on the same single-connection writer
	// pool, a successful staged publish must still work: the ATTACH is
	// torn down after every transaction, so no stale codex_staging schema
	// can collide with the next publish.
	staged, err := newCodexStagingSink(map[string]bool{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, staged.Close()) })
	stagedSess, stagedMsgs, _, _, _, _, err :=
		parser.ParseCodexSessionStreaming(cfg, source, staged)
	require.NoError(t, err)
	require.NotNil(t, stagedSess)
	stagedDBMsgs := toDBMessages(pendingWrite{
		sess: *stagedSess, msgs: stagedMsgs,
	}, nil)
	positions := make(map[string]db.StagedToolCallPosition)
	for _, m := range stagedDBMsgs {
		for callIdx, tc := range m.ToolCalls {
			if tc.ToolUseID == "" {
				continue
			}
			positions[tc.ToolUseID] = db.StagedToolCallPosition{
				ToolUseID: tc.ToolUseID,
				Ordinal:   m.Ordinal,
				CallIndex: callIdx,
			}
		}
	}
	require.NoError(t, database.ReplaceSessionContentStaged(
		context.Background(), row.ID, stagedDBMsgs, staged,
		map[string]bool{},
		func(verdicts map[string]bool) (
			db.SessionSignalUpdate, []db.SecretFinding, error,
		) {
			update, findings :=
				computeSignalsAndSecretsWithContentFailures(
					row, stagedDBMsgs, verdicts,
				)
			combined := append(
				append([]db.SecretFinding(nil), findings...),
				staged.Findings(row.ID, positions)...,
			)
			update.SecretLeakCount = definiteFindingCount(combined)
			return update, combined, nil
		},
	))
	after, err := database.GetAllMessages(context.Background(), row.ID)
	require.NoError(t, err)
	require.Equal(t, before, after,
		"successful staged publish after aborts must match the legacy projection")
}

// FuzzCodexStagedParityWithCollecting asserts parser-level parity between
// the collecting and staging paths over arbitrary transcript bodies. The
// fuzzed bytes follow a fixed valid session meta so both paths always
// face the same session shape.
// TestCodexStagedScratchFailureIsSticky pins the failure contract: a
// scratch write failure must stick to the sink, fail the parse outcome
// and the publish, and never silently commit an archive missing tool
// outputs. The scratch connection is poisoned by closing it under the
// sink, which is the same failure shape a disk-full or I/O error takes.
func TestCodexStagedScratchFailureIsSticky(t *testing.T) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	root := t.TempDir()
	day := filepath.Join(root, "2024", "01", "01")
	require.NoError(t, os.MkdirAll(day, 0o755))
	path := filepath.Join(
		day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
	)
	require.NoError(t, os.WriteFile(
		path, []byte(codexParityTranscript(uuid)), 0o644,
	))
	cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
	provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
	require.True(t, ok)
	source, found, err := provider.FindSource(
		context.Background(), parser.FindSourceRequest{
			FullSessionID: "codex:" + uuid,
		},
	)
	require.NoError(t, err)
	require.True(t, found)

	staged, err := newCodexStagingSink(map[string]bool{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = staged.Close() })
	// Poison the scratch so every staged write fails.
	require.NoError(t, staged.scratch.Close())

	_, _, _, _, _, _, err = parser.ParseCodexSessionStreaming(
		cfg, source, staged,
	)
	require.NoError(t, err, "the parser itself completes; the sink fails")
	require.Error(t, staged.Err())

	// The outcome wrapper must surface the sticky error so the engine
	// treats the parse as failed and keeps prior archive content.
	_, err = stagedCodexParseOutcome(cfg, source, staged)
	require.Error(t, err)
	require.ErrorContains(t, err, "codex staging")
}

func FuzzCodexStagedParityWithCollecting(f *testing.F) {
	const uuid = "019eb791-cf7d-75c1-8439-9ed74c122b05"
	f.Add([]byte(testjsonl.JoinJSONL(
		testjsonl.CodexMsgJSON(
			"user", "hi", "2024-01-01T10:00:02Z",
		),
		testjsonl.CodexFunctionCallWithCallIDJSON(
			"exec_command", "call_x", nil, "2024-01-01T10:00:03Z",
		),
		testjsonl.CodexFunctionCallOutputJSON(
			"call_x", "ok", "2024-01-01T10:00:04Z",
		),
	)))
	f.Add([]byte(`{"timestamp":"2024-01-01T10:00:01Z","type":"event_msg","payload":{"type":"task_started"}}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 256<<10 {
			t.Skip()
		}
		// Cap the line count: both paths share the collecting sink's
		// O(n) insertion per orphan message, so tens of thousands of
		// tiny lines stall iterations without distinguishing the two
		// paths (the slowdown is identical on both).
		if strings.Count(string(data), "\n") > 2000 {
			t.Skip()
		}
		content := testjsonl.JoinJSONL(
			testjsonl.CodexSessionMetaJSON(
				uuid, "/w", "codex_cli_rs", "2024-01-01T10:00:00Z",
			),
			string(data),
		)
		root := t.TempDir()
		day := filepath.Join(root, "2024", "01", "01")
		require.NoError(t, os.MkdirAll(day, 0o755))
		path := filepath.Join(
			day, "rollout-2024-01-01T10-00-00-"+uuid+".jsonl",
		)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		cfg := parser.ProviderConfig{Roots: []string{root}, Machine: "local"}
		provider, ok := parser.NewProvider(parser.AgentCodex, cfg)
		if !ok {
			t.Skip()
		}
		source, found, err := provider.FindSource(
			context.Background(), parser.FindSourceRequest{
				FullSessionID: "codex:" + uuid,
			},
		)
		if err != nil || !found {
			t.Skip()
		}

		legacy := parser.NewCodexCollectingSink(0)
		sessL, msgsL, curL, hashL, anchorL, retryL, errL :=
			parser.ParseCodexSessionStreaming(cfg, source, legacy)
		staged, err := newCodexStagingSink(map[string]bool{})
		require.NoError(t, err)
		sessS, msgsS, curS, hashS, anchorS, retryS, errS :=
			parser.ParseCodexSessionStreaming(cfg, source, staged)
		require.NoError(t, staged.Close())

		if (errL == nil) != (errS == nil) {
			t.Fatalf("error parity: legacy=%v staged=%v", errL, errS)
		}
		if errL != nil {
			return
		}
		require.Equal(t, retryL, retryS)
		if sessL == nil || sessS == nil {
			require.True(t, sessL == nil && sessS == nil)
			return
		}
		require.Equal(t, sessL.ID, sessS.ID)
		require.Equal(t, sessL.MessageCount, sessS.MessageCount)
		require.Equal(t, curL, curS)
		require.Equal(t, hashL, hashS)
		require.Equal(t, anchorL, anchorS)
		require.Len(t, msgsS, len(msgsL))
		for i := range msgsL {
			require.Equal(t, msgsL[i].Role, msgsS[i].Role)
			require.Equal(t, msgsL[i].Content, msgsS[i].Content)
			require.Len(t, msgsS[i].ToolCalls, len(msgsL[i].ToolCalls))
			for j := range msgsL[i].ToolCalls {
				lc, sc := msgsL[i].ToolCalls[j], msgsS[i].ToolCalls[j]
				require.Equal(t, lc.ToolUseID, sc.ToolUseID)
				require.Equal(t, lc.Category, sc.Category)
				require.Len(t, sc.ResultEvents, len(lc.ResultEvents))
				for k := range lc.ResultEvents {
					le, se := lc.ResultEvents[k], sc.ResultEvents[k]
					require.Equal(t, le.Status, se.Status)
					require.Equal(t, le.Source, se.Source)
					require.Equal(t, le.AgentID, se.AgentID)
					if le.Content != "" {
						require.NotEqual(t, le.Content, se.Content,
							"staged events carry placeholders")
					}
				}
			}
		}
	})
}
