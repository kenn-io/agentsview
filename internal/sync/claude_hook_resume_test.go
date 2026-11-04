package sync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
)

const (
	claudeHookResumeUser     = `{"type":"user","uuid":"u1","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`
	claudeHookResumeAnswer   = `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"answer-1","role":"assistant","content":[{"type":"text","text":"answer"}],"stop_reason":"end_turn"}}`
	claudeHookResumeNative   = `{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","stdout":"first output"}}`
	claudeHookResumeContext  = `{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_additional_context","content":["extra context"]}}`
	claudeHookResumeNextUser = `{"type":"user","uuid":"u2","parentUuid":"a1","timestamp":"2024-01-01T00:00:04Z","message":{"role":"user","content":"next request"}}`
)

func TestClaudeHookResumeKeepsOrdinaryDAGCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialHook bool
		hooks       []string
		ids         []string
		subtype     string
	}{
		{name: "full import ending in hook", initialHook: true, hooks: []string{claudeHookResumeNative}, ids: []string{"h1"}},
		{name: "hook-only append", hooks: []string{claudeHookResumeNative}, ids: []string{"h1"}},
		{name: "full import ending in additional context", initialHook: true, hooks: []string{claudeHookResumeContext}, ids: []string{"h1"}, subtype: "hook_additional_context"},
		{name: "additional context append", hooks: []string{claudeHookResumeContext}, ids: []string{"h1"}, subtype: "hook_additional_context"},
		{name: "multiple out-of-band hooks", hooks: []string{claudeHookResumeNative,
			`{"type":"attachment","uuid":"h2","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","stderr":"second output"}}`}, ids: []string{"h1", "h2"}},
		{name: "hook without native ID", hooks: []string{
			`{"type":"attachment","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","content":"no native ID"}}`}, ids: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.subtype == "" {
				tc.subtype = "hook_success"
			}
			counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
			env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)
			path := filepath.Join(dir, "proj", "hook-resume.jsonl")
			lines := []string{claudeHookResumeUser, claudeHookResumeAnswer}
			if tc.initialHook {
				lines = append(lines, tc.hooks...)
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
				appendClaudeSplitLines(t, path, tc.hooks...)
				env.engine.SyncPaths([]string{path})
			}
			info, ok := env.db.GetSessionForIncremental(t.Context(), path, "claude")
			require.True(t, ok)
			assert.Equal(t, "a1", info.LastEntryUUID)
			assert.Equal(t, 2+len(tc.hooks), info.NextOrdinal)
			stat, err := os.Stat(path)
			require.NoError(t, err)
			assert.Equal(t, stat.Size(), info.FileSize)
			rows := fetchMessages(t, env.db, "hook-resume")
			require.Len(t, rows, 2+len(tc.hooks))
			for i, wantID := range tc.ids {
				row := rows[i+2]
				assert.Equal(t, wantID, row.SourceUUID)
				assert.Equal(t, "attachment", row.SourceType)
				assert.Equal(t, tc.subtype, row.SourceSubtype)
				assert.True(t, row.IsSystem)
				require.NotNil(t, row.DialogueText)
				assert.Empty(t, *row.DialogueText)
			}
			appendClaudeSplitLines(t, path, claudeHookResumeNextUser)
			env.engine.SyncPaths([]string{path})
			assert.Zero(t, counter.full.Load(), "deferred hooks must not cause whole-source reads on the next ordinary append")
			rows = fetchMessages(t, env.db, "hook-resume")
			require.Len(t, rows, 3+len(tc.hooks))
			for i := range 2 {
				assert.Equal(t, before[i].ID, rows[i].ID)
			}
			pins, err := env.db.ListPinnedMessages(t.Context(), "hook-resume", "")
			require.NoError(t, err)
			require.Len(t, pins, 1)
			assert.Equal(t, before[1].ID, pins[0].MessageID)
			info, ok = env.db.GetSessionForIncremental(t.Context(), path, "claude")
			require.True(t, ok)
			assert.Equal(t, "u2", info.LastEntryUUID)
			assert.Equal(t, 3+len(tc.hooks), info.NextOrdinal)
			assert.Equal(t, 2, snapshotSplitSession(t, env.db, "hook-resume").UserMessageCount)
		})
	}
}

func TestClaudeHookResumeLinearParentChainStaysIncremental(t *testing.T) {
	counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
	env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)
	path := filepath.Join(dir, "proj", "hook-chain.jsonl")
	initial := strings.Join([]string{
		`{"type":"user","uuid":"u1","parentUuid":"missing-ancestor","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`,
		claudeHookResumeAnswer,
	}, "\n") + "\n"
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(initial), 0o600))
	env.engine.SyncAll(t.Context(), nil)
	counter.full.Store(0)
	appendClaudeSplitLines(t, path,
		`{"type":"attachment","uuid":"h1","parentUuid":"a1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","content":"first hook"}}`,
		`{"type":"attachment","uuid":"h2","parentUuid":"h1","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","content":"second hook"}}`,
	)
	env.engine.SyncPaths([]string{path})
	info, ok := env.db.GetSessionForIncremental(t.Context(), path, "claude")
	require.True(t, ok)
	assert.Equal(t, "a1", info.LastEntryUUID)
	appendClaudeSplitLines(t, path, `{"type":"user","uuid":"u2","parentUuid":"h2","timestamp":"2024-01-01T00:00:04Z","message":{"role":"user","content":"next request"}}`)
	env.engine.SyncPaths([]string{path})
	assert.Zero(t, counter.full.Load())
	rows := fetchMessages(t, env.db, "hook-chain")
	require.Len(t, rows, 5)
	assert.Equal(t, "h1", rows[2].SourceUUID)
	assert.Equal(t, "h2", rows[3].SourceUUID)
	assert.Equal(t, "h1", rows[3].SourceParentUUID)
	assert.Equal(t, "h2", rows[4].SourceParentUUID)
	assert.Equal(t, 2, snapshotSplitSession(t, env.db, "hook-chain").UserMessageCount)
}

func TestClaudeHookResumeFullIncrementalAgreement(t *testing.T) {
	for _, archive := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		for _, tc := range []struct {
			name  string
			lines []string
			split int
		}{
			{name: "trailing native hook", lines: []string{claudeHookResumeUser, claudeHookResumeAnswer, claudeHookResumeNative, claudeHookResumeNextUser}, split: 3},
			{name: "trailing additional context", lines: []string{claudeHookResumeUser, claudeHookResumeAnswer, claudeHookResumeContext, claudeHookResumeNextUser}, split: 3},
			{name: "same response across hook", lines: []string{
				`{"type":"user","uuid":"u1","parentUuid":"missing-ancestor","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`,
				`{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"response","content":[{"type":"text","text":"Hel"}],"usage":{"input_tokens":10,"output_tokens":1},"stop_reason":"tool_use"}}`,
				claudeHookResumeNative,
				`{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2024-01-01T00:00:03Z","message":{"id":"response","content":[{"type":"text","text":"Hello"}],"usage":{"input_tokens":10,"output_tokens":2},"stop_reason":"end_turn"}}`,
			}, split: 2},
			{name: "linear native parent chain", lines: []string{
				`{"type":"user","uuid":"u1","parentUuid":"missing-ancestor","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`,
				claudeHookResumeAnswer,
				`{"type":"attachment","uuid":"h1","parentUuid":"a1","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","content":"first hook"}}`,
				`{"type":"attachment","uuid":"h2","parentUuid":"h1","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","content":"second hook"}}`,
				`{"type":"user","uuid":"u2","parentUuid":"h2","timestamp":"2024-01-01T00:00:04Z","message":{"role":"user","content":"next request"}}`,
			}, split: 2},
		} {
			t.Run(string(archive)+"/"+tc.name, func(t *testing.T) {
				testClaudeSplitMatchesFullParse(t, archive, tc.lines, tc.split)
			})
		}
	}
}
