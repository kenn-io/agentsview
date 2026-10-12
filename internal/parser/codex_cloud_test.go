package parser

import (
	"encoding/json/jsontext"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
)

var codexCloudFetched = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ccRecord(kind mirror.Kind, data string) mirror.Record {
	return mirror.Record{Kind: kind, FetchedAt: codexCloudFetched, Data: jsontext.Value(data)}
}

// ccItem wraps an item object the way thread/items/list returns it.
func ccItem(turnID, item string, startedMs int64) mirror.Record {
	return ccRecord(mirror.KindItem,
		`{"turnId":"`+turnID+`","startedAtMs":`+strconv.FormatInt(startedMs, 10)+`,"completedAtMs":`+strconv.FormatInt(startedMs+500, 10)+`,"item":`+item+`}`)
}

const ccListing = `{"id":"thread-a","name":"Fix the build","preview":"fix it","createdAt":1759320000,"updatedAt":1759323600,` +
	`"model":"gpt-6","reasoningEffort":"high","cwd":"/fallback","environments":[{"environmentId":"env-1","cwd":"/workspace/project-a"}],` +
	`"originator":"codex_work_cca","threadSource":"aeon_child","parentThreadId":null,"forkedFromId":null,` +
	`"source":{"custom":"codex-cloud-agent"},"status":{"type":"idle"}}`

func writeCodexCloudMirror(t *testing.T, threadID string, records ...mirror.Record) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "account-key", threadID+".jsonl.zstd")
	_, err := mirror.Append(path, 0, records)
	require.NoError(t, err)
	return path
}

func parseCodexCloud(t *testing.T, path string) ParseResult {
	t.Helper()
	result, ok, err := ParseCodexCloudFile(t.Context(), path, "machine-a")
	require.NoError(t, err)
	require.True(t, ok)
	return result
}

func toolCallsByID(messages []ParsedMessage) map[string][]ParsedToolCall {
	calls := map[string][]ParsedToolCall{}
	for _, msg := range messages {
		for _, call := range msg.ToolCalls {
			calls[call.ToolUseID] = append(calls[call.ToolUseID], call)
		}
	}
	return calls
}

func TestCodexCloudMappingEveryItemType(t *testing.T) {
	const turn = `{"id":"turn-1","status":"completed","startedAt":1759320000,"completedAt":1759320600}`
	items := []string{
		`{"type":"userMessage","id":"u1","content":[{"type":"text","text":"Fix the build","text_elements":[]}]}`,
		`{"type":"reasoning","id":"r1","summary":["Look at the failing test"],"content":[]}`,
		`{"type":"commandExecution","id":"c1","command":"go test ./...","cwd":"/workspace/project-a","aggregatedOutput":"ok","exitCode":0.0,"durationMs":1200,"status":"completed"}`,
		`{"type":"mcpToolCall","id":"m1","server":"github","tool":"search","arguments":{"q":"x"},"result":{"content":[]},"error":null,"durationMs":50,"status":"completed"}`,
		`{"type":"fileChange","id":"f1","changes":[{"path":"/workspace/project-a/a.go"},{"path":"/workspace/project-a/b.go"}],"status":"completed"}`,
		`{"type":"webSearch","id":"w1","query":"zstd frames","action":{},"results":[]}`,
		`{"type":"dynamicToolCall","id":"d1","tool":"lookup","arguments":{},"status":"completed"}`,
		`{"type":"collabAgentToolCall","id":"k1","tool":"spawnAgent","status":"completed"}`,
		`{"type":"functionCallOutput","id":"o1","name":"exec","namespace":null,"output":"done"}`,
		`{"type":"imageView","id":"i1","path":"/workspace/project-a/shot.png"}`,
		`{"type":"imageGeneration","id":"g1","status":"completed"}`,
		`{"type":"contextCompaction","id":"cc1"}`,
		`{"type":"plan","id":"p1","text":"1. Fix the test"}`,
		`{"type":"hookPrompt","id":"h1","text":"hook ran"}`,
		`{"type":"subAgentActivity","id":"s1"}`,
		`{"type":"enteredReviewMode","id":"rv1"}`,
		`{"type":"sleep","id":"z1"}`,
		`{"type":"agentMessage","id":"a1","text":"Fixed.","phase":"final_answer"}`,
		`{"type":"unknownType","id":"x1","detail":"kept"}`,
	}
	records := []mirror.Record{ccRecord(mirror.KindThread, ccListing), ccRecord(mirror.KindTurn, turn)}
	for i, item := range items {
		records = append(records, ccItem("turn-1", item, 1759320000000+int64(i)*1000))
	}
	result := parseCodexCloud(t, writeCodexCloudMirror(t, "thread-a", records...))
	msgs := result.Messages

	assert.Equal(t, RoleUser, msgs[0].Role)
	assert.Equal(t, "Fix the build", msgs[0].Content)
	assert.True(t, msgs[1].HasThinking)
	assert.Equal(t, "Look at the failing test", msgs[1].ThinkingText)
	assert.Equal(t, "gpt-6", msgs[1].Model)
	assert.Equal(t, "high", msgs[1].ReasoningEffort)

	calls := toolCallsByID(msgs)
	for id, want := range map[string][2]string{
		"c1": {"shell_command", "Bash"},
		"m1": {"mcp__github__search", NormalizeToolCategory("mcp__github__search")},
		"f1": {"apply_patch", "Edit"},
		"w1": {"web_search", "Tool"},
		"d1": {"lookup", NormalizeToolCategory("lookup")},
		"k1": {"spawnAgent", "Task"},
		"o1": {"exec", NormalizeToolCategory("exec")},
		"i1": {"view_image", "Read"},
		"g1": {"image_generation", "Tool"},
		"x1": {"unknownType", "Other"},
	} {
		require.NotEmpty(t, calls[id], id)
		assert.Equal(t, want[0], calls[id][0].ToolName, id)
		assert.Equal(t, want[1], calls[id][0].Category, id)
	}
	assert.Len(t, calls["f1"], 2, "one edit call per changed file")
	assert.Equal(t, "/workspace/project-a/b.go", calls["f1"][1].FilePath)
	assert.Contains(t, calls["x1"][0].InputJSON, `"detail":"kept"`)

	var compaction, notes int
	var final string
	for _, msg := range msgs {
		if msg.IsCompactBoundary {
			compaction++
		}
		if msg.IsSystem && !msg.IsCompactBoundary {
			notes++
		}
		if msg.Role == RoleAssistant && msg.Content == "Fixed." {
			final = msg.Model
		}
	}
	assert.Equal(t, 1, compaction)
	assert.Equal(t, 4, notes, "hookPrompt, subAgentActivity, review marker, and sleep are kept")
	assert.Equal(t, "gpt-6", final)

	var commandResult string
	for _, msg := range msgs {
		for _, res := range msg.ToolResults {
			if res.ToolUseID == "c1" {
				commandResult = res.ContentRaw
			}
		}
	}
	assert.JSONEq(t, `"ok\n[exit code 0, 1200 ms]"`, commandResult)
	for i, msg := range msgs {
		assert.Equal(t, i, msg.Ordinal)
	}
}

func TestCodexCloudNewestRecordWins(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-a",
		ccRecord(mirror.KindThread, ccListing),
		ccItem("turn-1", `{"type":"commandExecution","id":"c1","command":"make","aggregatedOutput":"partial","status":"inProgress"}`, 1759320000000),
		ccItem("turn-1", `{"type":"agentMessage","id":"a1","text":"Working"}`, 1759320001000),
		ccItem("turn-1", `{"type":"commandExecution","id":"c1","command":"make","aggregatedOutput":"all done","exitCode":0,"status":"completed"}`, 1759320000000),
	)
	result := parseCodexCloud(t, path)
	var results []string
	for _, msg := range result.Messages {
		for _, res := range msg.ToolResults {
			results = append(results, res.ContentRaw)
		}
	}
	assert.Equal(t, []string{`"all done\n[exit code 0]"`}, results)
	require.NotEmpty(t, result.Messages[0].ToolCalls, "a re-fetched item keeps its first position")
	assert.Equal(t, "c1", result.Messages[0].ToolCalls[0].ToolUseID)
}

func TestCodexCloudSessionFields(t *testing.T) {
	result := parseCodexCloud(t, writeCodexCloudMirror(t, "thread-a", ccRecord(mirror.KindThread, ccListing)))
	s := result.Session
	assert.Equal(t, "codex-cloud:thread-a", s.ID)
	assert.Equal(t, AgentCodexCloud, s.Agent)
	assert.Equal(t, "Fix the build", s.SessionName)
	assert.Equal(t, "dot", s.Entrypoint)
	assert.Empty(t, s.SessionKind)
	assert.Equal(t, "/workspace/project-a", s.Cwd)
	assert.Equal(t, time.Unix(1759320000, 0), s.StartedAt)
	assert.Equal(t, time.Unix(1759323600, 0), s.EndedAt)

	for originator, want := range map[string]string{
		"codex_work_web": "codex-web", "codex_cloud": "codex-cloud", "other_client": "other_client",
	} {
		listing := `{"id":"thread-b","preview":"from preview","createdAt":1,"updatedAt":2,"originator":"` + originator +
			`","environments":null,"cwd":"/fallback/dir"}`
		s := parseCodexCloud(t, writeCodexCloudMirror(t, "thread-b", ccRecord(mirror.KindThread, listing))).Session
		assert.Equal(t, want, s.Entrypoint, originator)
		assert.Equal(t, "from preview", s.SessionName)
		assert.Equal(t, "/fallback/dir", s.Cwd)
	}
}

func TestCodexCloudResumeRecordNeverSetsTimesOrCwd(t *testing.T) {
	resume := `{"id":"thread-c","createdAt":1759320000,"updatedAt":1760000000,"cwd":"~/project-a",` +
		`"environments":[{"environmentId":"env-1","cwd":"~/project-a"}],"model":"gpt-6"}`
	s := parseCodexCloud(t, writeCodexCloudMirror(t, "thread-c",
		ccRecord(mirror.KindThreadResume, resume),
		ccItem("turn-1", `{"type":"agentMessage","id":"a1","text":"hi"}`, 1759320100000),
	)).Session
	assert.Empty(t, s.Cwd)
	assert.Equal(t, time.UnixMilli(1759320100500), s.EndedAt, "ends at the newest item, not the resume time")
}

func TestCodexCloudResolvedParent(t *testing.T) {
	spawned := `{"id":"child","createdAt":1,"updatedAt":2,"parentThreadId":null,"forkedFromId":"other",` +
		`"source":{"subAgent":{"thread_spawn":{"parent_thread_id":"parent-1","depth":1}}}}`
	s := parseCodexCloud(t, writeCodexCloudMirror(t, "child", ccRecord(mirror.KindThread, spawned))).Session
	assert.Equal(t, "codex-cloud:parent-1", s.ParentSessionID)
	assert.Equal(t, RelSubagent, s.RelationshipType)

	direct := `{"id":"child2","createdAt":1,"updatedAt":2,"parentThreadId":"parent-2","forkedFromId":"other"}`
	s = parseCodexCloud(t, writeCodexCloudMirror(t, "child2", ccRecord(mirror.KindThread, direct))).Session
	assert.Equal(t, "codex-cloud:parent-2", s.ParentSessionID)
	assert.Equal(t, RelSubagent, s.RelationshipType)

	fork := `{"id":"fork","createdAt":1,"updatedAt":2,"forkedFromId":"origin"}`
	s = parseCodexCloud(t, writeCodexCloudMirror(t, "fork", ccRecord(mirror.KindThread, fork))).Session
	assert.Equal(t, "codex-cloud:origin", s.ParentSessionID)
	assert.Equal(t, RelFork, s.RelationshipType)
}

func TestCodexCloudSkipsFileWithoutMetadata(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-d",
		ccItem("turn-1", `{"type":"agentMessage","id":"a1","text":"hi"}`, 1759320100000))
	_, ok, err := ParseCodexCloudFile(t.Context(), path, "machine-a")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestCodexCloudProviderDiscoversMirrorFilesOnly(t *testing.T) {
	path := writeCodexCloudMirror(t, "thread-e", ccRecord(mirror.KindThread, ccListing))
	root := filepath.Dir(filepath.Dir(path))
	_, err := mirror.Append(filepath.Join(filepath.Dir(path), "thread-e.state.json.zstd"), 0,
		[]mirror.Record{ccRecord(mirror.KindThread, ccListing)})
	require.NoError(t, err)

	factory, ok := ProviderFactoryByType(AgentCodexCloud)
	require.True(t, ok)
	sources, err := factory.NewProvider(ProviderConfig{Roots: []string{root}}).Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	assert.Equal(t, path, sources[0].DisplayPath)
}

// BenchmarkCodexCloudParseLargeThread shows the cost of re-parsing a long
// thread in full, which each changed pass does.
func BenchmarkCodexCloudParseLargeThread(b *testing.B) {
	records := []mirror.Record{ccRecord(mirror.KindThread, ccListing)}
	for turn := range 30 {
		turnID := "turn-" + strconv.Itoa(turn)
		records = append(records, ccRecord(mirror.KindTurn,
			`{"id":"`+turnID+`","status":"completed","startedAt":`+strconv.Itoa(1759320000+turn*600)+`}`))
		for item := range 30 {
			id := turnID + "-" + strconv.Itoa(item)
			records = append(records, ccItem(turnID,
				`{"type":"commandExecution","id":"`+id+`","command":"go test ./...","aggregatedOutput":"`+
					strings.Repeat("output line\\n", 40)+`","exitCode":0,"status":"completed"}`,
				int64(1759320000000+turn*600000+item*1000)))
		}
	}
	path := filepath.Join(b.TempDir(), "account-key", "thread-a.jsonl.zstd")
	_, err := mirror.Append(path, 0, records)
	require.NoError(b, err)
	b.ResetTimer()
	for b.Loop() {
		_, _, err := ParseCodexCloudFile(b.Context(), path, "machine-a")
		require.NoError(b, err)
	}
}
