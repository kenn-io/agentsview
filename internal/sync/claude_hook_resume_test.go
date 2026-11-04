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

// A hook record can sit before the assistant response in the file and
// still sort after it, because an undated attachment has no timestamp of
// its own and a dated one can carry a later stamp than the response. The
// stored row then trails the response it precedes.
const (
	claudeHookRelocatedUndated = `{"type":"attachment","uuid":"h1","attachment":{"type":"hook_success","stdout":"first output"}}`
	claudeHookRelocatedDated   = `{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","stdout":"first output"}}`
	claudeHookRelocatedContext = `{"type":"attachment","uuid":"h1","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_additional_context","content":["extra context"]}}`
	claudeHookRelocatedFirst   = `{"type":"assistant","uuid":"a1","parentUuid":"u1","timestamp":"2024-01-01T00:00:01Z","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"Hel"}]}}`
	claudeHookRelocatedSecond  = `{"type":"assistant","uuid":"a2","parentUuid":"a1","timestamp":"2024-01-01T00:00:04Z","message":{"id":"m","role":"assistant","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn"}}`
)

func TestClaudeHookResumeKeepsOrdinaryDAGCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		initialHook bool
		wantSubtype string
		hooks       []string
		ids         []string
	}{
		{name: "full import ending in hook", initialHook: true, hooks: []string{claudeHookResumeNative}, ids: []string{"h1"}},
		{name: "full import ending in stop summary", wantSubtype: "stop_hook_summary", initialHook: true, hooks: []string{`{"type":"system","subtype":"stop_hook_summary","uuid":"h1","timestamp":"2024-01-01T00:00:02Z","hookCount":1,"hookInfos":[{"command":"echo hook"}],"hookErrors":[],"preventedContinuation":false}`}, ids: []string{"h1"}},
		{name: "stop summary append", wantSubtype: "stop_hook_summary", hooks: []string{`{"type":"system","subtype":"stop_hook_summary","uuid":"h1","timestamp":"2024-01-01T00:00:02Z","hookCount":1,"hookInfos":[{"command":"echo hook"}],"hookErrors":[],"preventedContinuation":false}`}, ids: []string{"h1"}},
		{name: "hook-only append", hooks: []string{claudeHookResumeNative}, ids: []string{"h1"}},
		{name: "multiple out-of-band hooks", hooks: []string{
			claudeHookResumeNative,
			`{"type":"attachment","uuid":"h2","timestamp":"2024-01-01T00:00:03Z","attachment":{"type":"hook_success","stderr":"second output"}}`,
		}, ids: []string{"h1", "h2"}},
		{name: "hook without native ID", hooks: []string{
			`{"type":"attachment","timestamp":"2024-01-01T00:00:02Z","attachment":{"type":"hook_success","content":"no native ID"}}`,
		}, ids: []string{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			counter := &claudeFullParseCounter{inner: claudeSplitFactory(t)}
			env, dir := newClaudeSplitTestEnv(t, counter, config.ArchiveContentFull)
			path := filepath.Join(dir, "proj", "hook-resume.jsonl")
			lines := []string{claudeHookResumeUser, claudeHookResumeAnswer}
			if tc.name == "multiple out-of-band hooks" {
				lines[0] = `{"type":"user","uuid":"u1","parentUuid":"missing-ancestor","timestamp":"2024-01-01T00:00:00Z","message":{"role":"user","content":"request"}}`
			}
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
				sourceType := "attachment"
				if tc.wantSubtype == "stop_hook_summary" {
					sourceType = "system"
				}
				assert.Equal(t, sourceType, row.SourceType)
				wantSubtype := tc.wantSubtype
				if wantSubtype == "" {
					wantSubtype = "hook_success"
				}
				assert.Equal(t, wantSubtype, row.SourceSubtype)
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

func TestClaudeHookResumeFullIncrementalAgreement(t *testing.T) {
	for _, archive := range []config.ArchiveContent{config.ArchiveContentFull, config.ArchiveContentUsage} {
		for _, tc := range []struct {
			name  string
			lines []string
			split int
		}{
			{name: "trailing native hook", lines: []string{claudeHookResumeUser, claudeHookResumeAnswer, claudeHookResumeNative, claudeHookResumeNextUser}, split: 3},
			{name: "trailing additional context", lines: []string{claudeHookResumeUser, claudeHookResumeAnswer, claudeHookResumeContext, claudeHookResumeNextUser}, split: 3},
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
			{name: "relocated undated hook", lines: []string{
				claudeHookResumeUser,
				claudeHookRelocatedUndated,
				claudeHookRelocatedFirst,
				claudeHookRelocatedSecond,
			}, split: 3},
			{name: "relocated dated hook", lines: []string{
				claudeHookResumeUser,
				claudeHookRelocatedDated,
				claudeHookRelocatedFirst,
				claudeHookRelocatedSecond,
			}, split: 3},
			{name: "relocated additional context", lines: []string{
				claudeHookResumeUser,
				claudeHookRelocatedContext,
				claudeHookRelocatedFirst,
				claudeHookRelocatedSecond,
			}, split: 3},
		} {
			t.Run(string(archive)+"/"+tc.name, func(t *testing.T) {
				testClaudeSplitMatchesFullParse(t, archive, tc.lines, tc.split)
			})
		}
	}
}

// TestClaudeHookRelocatedSurvivesSameIDAppend pins the hook row that
// trails the assistant it precedes. An appended record that continues the
// same response replaces the stored rows from the assistant's ordinal
// onward, so the hook row has to survive that replacement, keep its
// native identity and pin, and leave the stored checkpoint correct.
func TestClaudeHookRelocatedSurvivesSameIDAppend(t *testing.T) {
	env, dir := newClaudeSplitTestEnv(
		t, claudeSplitFactory(t), config.ArchiveContentFull,
	)
	path := filepath.Join(dir, "proj", "relocated-hook.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.Join([]string{
		claudeHookResumeUser,
		claudeHookRelocatedDated,
		claudeHookRelocatedFirst,
	}, "\n")+"\n"), 0o600))
	env.engine.SyncAll(t.Context(), nil)

	before := fetchMessages(t, env.db, "relocated-hook")
	require.Len(t, before, 3)
	var hookID int64
	hooks := 0
	for _, row := range before {
		if row.SourceUUID != "h1" {
			continue
		}
		hooks++
		hookID = row.ID
		assert.Equal(t, "attachment", row.SourceType)
		assert.Equal(t, "hook_success", row.SourceSubtype)
		assert.True(t, row.IsSystem)
		assert.Contains(t, row.Content, "first output")
		require.NotNil(t, row.DialogueText)
		assert.Empty(t, *row.DialogueText)
	}
	require.Equal(t, 1, hooks, "the hook keeps its own stored row")

	_, err := env.db.PinMessage(t.Context(), "relocated-hook", hookID, nil)
	require.NoError(t, err)

	appendClaudeSplitLines(t, path, claudeHookRelocatedSecond)
	env.engine.SyncPaths([]string{path})

	after := fetchMessages(t, env.db, "relocated-hook")
	require.Len(t, after, 3)
	hooks = 0
	var afterHookID int64
	for _, row := range after {
		if row.SourceUUID != "h1" {
			continue
		}
		hooks++
		afterHookID = row.ID
		assert.Equal(t, "attachment", row.SourceType)
		assert.Equal(t, "hook_success", row.SourceSubtype)
		assert.True(t, row.IsSystem)
		assert.Contains(t, row.Content, "first output")
		require.NotNil(t, row.DialogueText)
		assert.Empty(t, *row.DialogueText)
	}
	require.Equal(t, 1, hooks, "the hook row survives the same-id append")

	pins, err := env.db.ListPinnedMessages(t.Context(), "relocated-hook", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, afterHookID, pins[0].MessageID)

	info, ok := env.db.GetSessionForIncremental(t.Context(), path, "claude")
	require.True(t, ok)
	stat, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, stat.Size(), info.FileSize)
	assert.Equal(t, 3, info.NextOrdinal)
	assert.Equal(t, "a2", info.LastEntryUUID)

	// A repeat sync must not change the stored rows, the pin, or the
	// checkpoint.
	env.engine.SyncPaths([]string{path})
	repeat := fetchMessages(t, env.db, "relocated-hook")
	require.Len(t, repeat, 3)
	assert.Equal(t, after, repeat)
	pins, err = env.db.ListPinnedMessages(t.Context(), "relocated-hook", "")
	require.NoError(t, err)
	require.Len(t, pins, 1)
	assert.Equal(t, afterHookID, pins[0].MessageID)
	info, ok = env.db.GetSessionForIncremental(t.Context(), path, "claude")
	require.True(t, ok)
	assert.Equal(t, stat.Size(), info.FileSize)
	assert.Equal(t, 3, info.NextOrdinal)
	assert.Equal(t, "a2", info.LastEntryUUID)
}
