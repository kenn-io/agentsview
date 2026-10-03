package parser

import (
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/testjsonl"
)

const (
	revertThread   = "11111111-1111-4111-8111-111111111111"
	revertRollout  = "22222222-2222-4222-8222-222222222222"
	revertRollout1 = "33333333-3333-4333-8333-333333333333"
	revertOther    = "44444444-4444-4444-8444-444444444444"
	revertSpawner  = "55555555-5555-4555-8555-555555555555"
)

// writeRevertRollout writes one rollout into root's 2026/09/01 dated folder.
func writeRevertRollout(t *testing.T, root, key string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "09", "01")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "rollout-2026-09-01T10-00-00-"+key+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(testjsonl.JoinJSONL(lines...)), 0o600))
	return path
}

// revertPageMeta returns a page session_meta; an empty base omits history_base.
func revertPageMeta(id, base string, extra map[string]any) string {
	fields := map[string]any{"history_mode": "paginated"}
	if base != "" {
		fields["history_base"] = map[string]any{
			"thread_id":             base,
			"end_ordinal_exclusive": 3,
		}
	}
	maps.Copy(fields, extra)
	return testjsonl.CodexSessionMetaWithFieldsJSON(
		id, "/work/project", "codex_cli_rs", "2026-09-01T11:00:00Z", fields,
	)
}

func parseRevertSource(t *testing.T, agent AgentType, root, rawID string) *ParsedSession {
	t.Helper()
	provider, ok := NewProvider(agent, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{RawSessionID: rawID})
	require.NoError(t, err)
	require.True(t, found, "source for %s", rawID)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	return &outcome.Results[0].Result.Session
}

func TestCodexRevertPageParentFollowsHistoryBase(t *testing.T) {
	pageKey := revertThread + "_" + revertRollout
	tests := []struct {
		name       string
		agent      AgentType
		headerID   string
		base       string
		wantID     string
		wantParent string
		wantRel    RelationshipType
	}{
		{"base_is_thread", AgentCodex, revertThread, revertThread, "codex:" + pageKey, "codex:" + revertThread, RelContinuation},
		{"base_is_rollout", AgentCodex, revertThread, revertRollout1, "codex:" + pageKey, "codex:" + revertThread + "_" + revertRollout1, RelContinuation},
		{"no_base", AgentCodex, revertThread, "", "codex:" + pageKey, "codex:" + revertThread, RelContinuation},
		{"header_disagrees", AgentCodex, revertOther, revertThread, "codex:" + revertOther, "", ""},
		{"traex", AgentTraeX, revertThread, revertThread, "traex:" + pageKey, "traex:" + revertThread, RelContinuation},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeRevertRollout(t, root, pageKey,
				revertPageMeta(tt.headerID, tt.base, nil),
				testjsonl.CodexMsgJSON("user", "page prompt", "2026-09-01T11:00:01Z"),
			)
			sess := parseRevertSource(t, tt.agent, root, pageKey)
			assert.Equal(t, tt.wantID, sess.ID)
			assert.Equal(t, tt.wantParent, sess.ParentSessionID)
			assert.Equal(t, tt.wantRel, sess.RelationshipType)
			assert.Equal(t, 1, sess.MessageCount)
		})
	}
}

// A forked thread's revert page carries the fork cutoff Codex copies from the
// thread (forked_from_ordinal_exclusive). A base ending at that cutoff lies in
// history the thread inherited, so the page continues the thread it was forked
// from; a base ending past it is one of the thread's own rollouts.
func TestCodexRevertPageOfForkedThreadFollowsInheritedBase(t *testing.T) {
	pageKey := revertThread + "_" + revertRollout
	tests := []struct {
		name       string
		base       string
		baseEnd    int
		cutoff     any
		wantParent string
	}{
		{"base_is_fork_source", revertOther, 5, 5, "codex:" + revertOther},
		{"inherited_source_page", revertRollout1, 5, 5, "codex:" + revertOther},
		{"own_rollout", revertRollout1, 9, 5, "codex:" + revertThread + "_" + revertRollout1},
		{"no_base", "", 0, 0, "codex:" + revertThread},
		{"no_recorded_cutoff", revertRollout1, 5, nil, "codex:" + revertThread + "_" + revertRollout1},
		{"no_recorded_cutoff_base_is_fork_source", revertOther, 5, nil, "codex:" + revertOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := map[string]any{
				"history_mode":   "paginated",
				"forked_from_id": revertOther,
			}
			if tt.base != "" {
				fields["history_base"] = map[string]any{
					"thread_id":             tt.base,
					"end_ordinal_exclusive": tt.baseEnd,
				}
			}
			if tt.cutoff != nil {
				fields["forked_from_ordinal_exclusive"] = tt.cutoff
			}
			root := t.TempDir()
			writeRevertRollout(t, root, pageKey,
				testjsonl.CodexSessionMetaWithFieldsJSON(
					revertThread, "/work/project", "codex_cli_rs", "2026-09-01T11:00:00Z", fields,
				),
				testjsonl.CodexMsgJSON("user", "page prompt", "2026-09-01T11:00:01Z"),
			)
			sess := parseRevertSource(t, AgentCodex, root, pageKey)
			assert.Equal(t, "codex:"+pageKey, sess.ID)
			assert.Equal(t, tt.wantParent, sess.ParentSessionID)
			assert.Equal(t, RelContinuation, sess.RelationshipType)
			assert.Equal(t, 1, sess.MessageCount)
		})
	}
}

func TestCodexRevertPageHelperRejectsNonPages(t *testing.T) {
	plain := "rollout-2026-09-01T10-00-00-" + revertThread + ".jsonl"
	page := "rollout-2026-09-01T10-00-00-" + revertThread + "_" + revertRollout + ".jsonl"
	for _, tc := range []struct {
		name, path, threadID string
	}{
		{"plain_rollout", plain, revertThread},
		{"empty_header_id", page, ""},
		{"other_thread", page, revertOther},
		{"not_a_rollout", "notes.jsonl", revertThread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, parent, ok := codexRevertPage(tc.path, tc.threadID, revertThread, "", false)
			assert.False(t, ok)
			assert.Empty(t, id)
			assert.Empty(t, parent)
		})
	}
}

func TestCodexSubagentRevertPageLinksToItsBase(t *testing.T) {
	root := t.TempDir()
	pageKey := revertThread + "_" + revertRollout
	subagentFields := map[string]any{
		"agent_nickname":   "worker",
		"agent_path":       "/root/worker",
		"parent_thread_id": revertSpawner,
		"session_id":       revertSpawner,
		"thread_source":    "subagent",
		"source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{
			"parent_thread_id": revertSpawner,
			"depth":            1,
			"agent_nickname":   "worker",
			"agent_path":       "/root/worker",
		}}},
	}
	writeRevertRollout(t, root, revertThread,
		testjsonl.CodexSubagentSessionMetaJSON(
			revertThread, revertSpawner, "/work/project", "codex_cli_rs", "2026-09-01T10:00:00Z"),
		testjsonl.CodexMsgJSON("assistant", "head work", "2026-09-01T10:00:01Z"),
	)
	writeRevertRollout(t, root, pageKey,
		revertPageMeta(revertThread, revertThread, subagentFields),
		testjsonl.CodexMsgJSON("assistant", "page work", "2026-09-01T11:00:01Z"),
	)

	head := parseRevertSource(t, AgentCodex, root, revertThread)
	assert.Equal(t, "codex:"+revertThread, head.ID)
	assert.Equal(t, "codex:"+revertSpawner, head.ParentSessionID)
	assert.Equal(t, RelSubagent, head.RelationshipType)

	page := parseRevertSource(t, AgentCodex, root, pageKey)
	assert.Equal(t, "codex:"+pageKey, page.ID)
	assert.Equal(t, "codex:"+revertThread, page.ParentSessionID)
	assert.Equal(t, RelContinuation, page.RelationshipType)
	assert.Equal(t, "worker", head.SessionName)
	assert.Equal(t, head.SessionName, page.SessionName, "the page keeps the head's agent-path title")
}

func TestCodexFindSourceResolvesRevertPageID(t *testing.T) {
	base := t.TempDir()
	dated := filepath.Join(base, "sessions")
	archived := filepath.Join(base, "archived_sessions")
	pageKey := revertThread + "_" + revertRollout
	datedPath := writeRevertRollout(t, dated, pageKey,
		revertPageMeta(revertThread, revertThread, nil),
		testjsonl.CodexMsgJSON("user", "page prompt", "2026-09-01T11:00:01Z"),
	)
	content, err := os.ReadFile(datedPath)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(archived, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(archived, filepath.Base(datedPath)), content, 0o600))

	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{dated, archived}})
	require.True(t, ok)
	source, found, err := provider.FindSource(t.Context(), FindSourceRequest{FullSessionID: "codex:" + pageKey})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, datedPath, source.DisplayPath, "the dated copy wins over the archived one")

	// The head's id must not resolve to the page.
	_, found, err = provider.FindSource(t.Context(), FindSourceRequest{FullSessionID: "codex:" + revertThread})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestCodexReadThreadNameMapsRevertPageKeyToThread(t *testing.T) {
	home := t.TempDir()
	pageKey := revertThread + "_" + revertRollout
	session := filepath.Join(home, "sessions", "2026", "09", "01",
		"rollout-2026-09-01T10-00-00-"+pageKey+".jsonl")
	writeIndex(t, home, `{"id":"`+revertThread+`","thread_name":"Thread title"}`+"\n", time.Now())

	name, ok, err := CodexMetadata{}.ReadThreadName(session, pageKey)
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "Thread title", name, "a page takes its thread's index title")
}

// A fork copies the thread's live history, which spans the head rollout and
// its revert pages, so turns replayed from a page must be recognized too.
func TestCodexForkOfRevertedThreadDropsReplayedPageTurns(t *testing.T) {
	const (
		forkID      = "66666666-6666-4666-8666-666666666666"
		headTurn    = "head-turn"
		pageTurn    = "page-turn"
		genuineTurn = "fork-turn"
	)
	root := t.TempDir()
	writeRevertRollout(t, root, revertThread,
		revertPageMeta(revertThread, "", nil),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", headTurn, "2026-09-01T10:00:01Z"),
	)
	pageDir := filepath.Join(root, "2026", "09", "02")
	require.NoError(t, os.MkdirAll(pageDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(pageDir, "rollout-2026-09-02T10-00-00-"+revertThread+"_"+revertRollout+".jsonl"),
		[]byte(testjsonl.JoinJSONL(
			revertPageMeta(revertThread, revertThread, nil),
			testjsonl.CodexTurnContextWithIDJSON("gpt-5", pageTurn, "2026-09-02T10:00:01Z"),
		)), 0o600))
	writeRevertRollout(t, root, forkID,
		testjsonl.CodexForkedSessionMetaJSON(forkID, revertThread, "/work/project", "user",
			"2026-09-03T10:00:00Z"),
		testjsonl.CodexSessionMetaJSON(revertThread, "/work/project", "user", "2026-09-03T10:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", headTurn, "2026-09-03T10:00:00Z"),
		testjsonl.CodexMsgJSON("user", "replayed head question", "2026-09-03T10:00:00Z"),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", pageTurn, "2026-09-03T10:00:00Z"),
		testjsonl.CodexMsgJSON("user", "replayed page question", "2026-09-03T10:00:00Z"),
		testjsonl.CodexMsgJSON("assistant", "replayed page answer", "2026-09-03T10:00:00Z"),
		testjsonl.CodexTokenCountJSON("2026-09-03T10:00:00Z", 50_000, 9_000, 0),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", genuineTurn, "2026-09-03T10:00:01Z"),
		testjsonl.CodexMsgJSON("user", "fork question", "2026-09-03T10:00:01Z"),
		testjsonl.CodexMsgJSON("assistant", "fork answer", "2026-09-03T10:00:02Z"),
		testjsonl.CodexTokenCountJSON("2026-09-03T10:00:02Z", 10_000, 500, 0),
	)

	sess := parseRevertSource(t, AgentCodex, root, forkID)

	assert.Equal(t, 2, sess.MessageCount, "only the fork's own turn is kept")
	assert.Equal(t, 500, sess.TotalOutputTokens, "replayed page usage is not billed to the fork")
}

func TestCodexForkWithOnlyParentPageNeedsRetry(t *testing.T) {
	root := t.TempDir()
	writeRevertRollout(t, root, revertThread+"_"+revertRollout,
		revertPageMeta(revertThread, revertThread, nil),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly),
	)
	writeRevertRollout(t, root, revertOther,
		testjsonl.CodexForkedSessionMetaJSON(revertOther, revertThread, "/work/project", "user", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", tsEarly),
		testjsonl.CodexMsgJSON("user", "copied head question", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "page-turn", tsEarly),
		testjsonl.CodexMsgJSON("user", "copied page question", tsEarly),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "child-turn", tsEarlyS1),
		testjsonl.CodexMsgJSON("user", "child question", tsEarlyS1),
	)
	provider, ok := NewProvider(AgentCodex, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	source := requireCodexProviderSource(t, provider, revertOther)
	outcome, err := provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionNeedsRetry, outcome.Results[0].DataVersion)
	assert.Contains(t, outcome.Results[0].RetryReason, "parent turns")
	assert.Len(t, outcome.Results[0].Result.Messages, 3, "keep the history until its parent can be resolved")

	writeRevertRollout(t, root, revertThread,
		revertPageMeta(revertThread, "", nil),
		testjsonl.CodexTurnContextWithIDJSON("gpt-5", "head-turn", tsEarly),
	)
	outcome, err = provider.Parse(t.Context(), ParseRequest{Source: source})
	require.NoError(t, err)
	require.Len(t, outcome.Results, 1)
	assert.Equal(t, DataVersionCurrent, outcome.Results[0].DataVersion)
	assert.Empty(t, outcome.Results[0].RetryReason)
	require.Len(t, outcome.Results[0].Result.Messages, 1)
	assert.Equal(t, "child question", outcome.Results[0].Result.Messages[0].Content)
}
