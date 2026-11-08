package sync_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

const (
	claudeHookResumeUser     = `{"type":"user","uuid":"u1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`
	claudeHookResumeAnswer   = `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}}`
	claudeHookResumeNative   = `{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","stdout":"first output"}}`
	claudeHookResumeNextUser = `{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2024-01-01T00:00:04Z","message":{"role":"user","content":"next request"}}`
)

// A hook record can sit before the assistant response in the file and
// still sort after it, because it can carry a later timestamp than the
// response. The stored row then trails the response it precedes.
const (
	claudeHookRelocatedDated  = `{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","stdout":"first output"}}`
	claudeHookRelocatedFirst  = `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"Hel"}]}}`
	claudeHookRelocatedSecond = `{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2024-01-01T00:00:04Z","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn"}}`
)

func TestClaudeHookResumeKeepsOrdinaryDAGCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialHook bool
		hook        string
		id          string
	}{
		{name: "full import ending in hook", initialHook: true, hook: claudeHookResumeNative, id: "h1"},
		{name: "hook-only append", hook: claudeHookResumeNative, id: "h1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
			env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)
			path := filepath.Join(dir, "proj", "hook-resume.jsonl")
			lines := []string{claudeHookResumeUser, claudeHookResumeAnswer}
			if tc.initialHook {
				lines = append(lines, tc.hook)
			}
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
			require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
			env.engine.SyncAll(t.Context(), nil)
			before := fetchMessages(t, env.db, "hook-resume")
			require.GreaterOrEqual(t, len(before), 2)
			_, err := env.db.PinMessage(t.Context(), "hook-resume", before[1].ID, nil)
			require.NoError(t, err)
			counter.full.Store(0)
			if !tc.initialHook {
				appendClaudeSplitLines(t, path, tc.hook)
				env.engine.SyncPaths([]string{path})
			}
			info, ok := env.db.GetSessionForIncremental(t.Context(), path, "claude")
			require.True(t, ok)
			assert.Equal(t, 3, info.NextOrdinal)
			stat, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, stat.Size(), info.FileSize)
			rows := fetchMessages(t, env.db, "hook-resume")
			require.Len(t, rows, 3)
			assert.Equal(t, tc.id, rows[2].SourceUUID)
			appendClaudeSplitLines(t, path, claudeHookResumeNextUser)
			env.engine.SyncPaths([]string{path})
			assert.Zero(t, counter.full.Load(), "deferred hooks must not cause whole-source reads on the next ordinary append")
			rows = fetchMessages(t, env.db, "hook-resume")
			require.Len(t, rows, 4)
			for i := range 2 {
				assert.Equal(t, before[i].ID, rows[i].ID)
			}
			pins, err := env.db.ListPinnedMessages(t.Context(), "hook-resume", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, before[1].ID, pins[0].MessageID)
			info, ok = env.db.GetSessionForIncremental(t.Context(), path, "claude")
			require.True(t, ok)
			assert.Equal(t, 4, info.NextOrdinal)
		})
	}
}

func TestClaudeHookResumeFullIncrementalAgreement(t *testing.T) {
	for _, archive := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		for _, tc := range []struct {
			name    string
			lines   []string
			split   int
			pinHook bool
		}{
			{name: "hook-only append older than stored tail", lines: []string{
				claudeHookResumeUser,
				claudeHookResumeAnswer,
				`{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:00.500Z","attachment":{"type":"hook_success","content":"delayed output"}}`,
			}, split: 2},
			{name: "queued prompt older than stored hook", lines: []string{
				claudeHookResumeUser,
				claudeHookResumeAnswer,
				claudeHookResumeNative,
				`{"type":"attachment","timestamp":"2024-01-01T00:00:01.500Z","attachment":{"type":"queued_command","prompt":"queued request"}}`,
			}, split: 3},
			{name: "queued prompt after dropped trailing carrier", lines: []string{
				claudeHookResumeUser,
				`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"},{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"pwd"}}],"stop_reason":"tool_use"}}`,
				claudeHookResumeNative,
				`{"type":"user","uuid":"r1","parentUuid":"a1","timestamp":"2024-01-01T00:00:03Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"/tmp"}]}}`,
				`{"type":"attachment","timestamp":"2024-01-01T00:00:01.500Z","attachment":{"type":"queued_command","prompt":"queued request"}}`,
			}, split: 4},
			{name: "linear native parent chain", lines: []string{
				`{"type":"user","uuid":"u1","parentUuid":"missing-ancestor","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`,
				claudeHookResumeAnswer,
				`{"type":"attachment","uuid":"h1","parentUuid":"a1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","content":"first hook"}}`,
				`{"type":"attachment","uuid":"h2","parentUuid":"h1","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","content":"second hook"}}`,
				`{"type":"user","uuid":"u2","parentUuid":"h2","timestamp":"2024-01-01T00:00:04Z","message":{"role":"user","content":"next request"}}`,
			}, split: 2},
			// The hook sits before the response in the file but sorts
			// after it, so the stored hook row trails the assistant.
			// Finishing the response replaces the rows from the
			// assistant's ordinal onward, and the replaced byte range
			// cannot reread the earlier hook.
			{name: "relocated dated hook", lines: []string{
				claudeHookResumeUser,
				claudeHookRelocatedDated,
				claudeHookRelocatedFirst,
				claudeHookRelocatedSecond,
			}, split: 3, pinHook: true},
		} {
			t.Run(string(archive)+"/"+tc.name, func(t *testing.T) {
				incrementalEnv, path := testClaudeSplitMatchesFullParse(t, archive, tc.lines, tc.split, func(env *testEnv) {
					if tc.pinHook && archive == config.ArchiveContentFull {
						before := fetchMessages(t, env.db, "split")
						require.Len(t, before, 3)
						_, err := env.db.PinMessage(t.Context(), "split", before[2].ID, nil)
						require.NoError(t, err)
					}
				})
				if tc.pinHook && archive == config.ArchiveContentFull {
					after := fetchMessages(t, incrementalEnv.db, "split")
					require.Len(t, after, 3)
					hookIndex := slices.IndexFunc(after, func(row db.Message) bool { return row.SourceUUID == "h1" })
					require.GreaterOrEqual(t, hookIndex, 0)
					pins, err := incrementalEnv.db.ListPinnedMessages(t.Context(), "split", "")
					require.NoError(t, err)
					require.Len(t, pins, 1)
					assert.Equal(t, after[hookIndex].ID, pins[0].MessageID)
					info, ok := incrementalEnv.db.GetSessionForIncremental(t.Context(), path, "claude")
					require.True(t, ok)
					stat, err := os.Stat(path)
					require.NoError(t, err)
					assert.Equal(t, stat.Size(), info.FileSize)
					assert.Equal(t, 3, info.NextOrdinal)
					assert.Equal(t, "a2", info.LastEntryUUID)
				}
			})
		}
	}
}
