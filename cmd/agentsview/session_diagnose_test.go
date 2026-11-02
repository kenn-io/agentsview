package main

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionDiagnosePGRawRemoteID(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://example.test/agentsview")
	store := dbtest.OpenTestDBAt(t, filepath.Join(dataDir, "pg.db"))
	const raw = "019da6a6-8c67-7c23-b102-ef48502852d0"
	const canonical = "host-a~codex:" + raw
	seedUsageSession(t, store, canonical, "project", "codex", 42)
	parentID := canonical
	require.NoError(t, store.UpsertSession(t.Context(), db.Session{
		ID: "child", Agent: "codex", Project: "project", Machine: "host-a",
		ParentSessionID:   &parentID,
		RelationshipType:  "subagent",
		TotalOutputTokens: 5, HasTotalOutputTokens: true,
	}))
	for ordinal := range 3 {
		require.NoError(t, store.InsertMessages(t.Context(), []db.Message{{
			SessionID: canonical, Ordinal: ordinal, Role: "assistant",
			HasToolUse: true,
			ToolCalls:  []db.ToolCall{{SessionID: canonical, ToolUseID: strconv.Itoa(ordinal), ToolName: "read", InputJSON: "{}", ResultContentLength: 20}},
		}}))
	}
	stub := stubPGReadStore(t, store)
	out, err := executeCommand(newRootCommand(), "session", "diagnose", raw, "--pg", "--format", "json")
	require.NoError(t, err)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, canonical, report.SessionID)
	assert.Equal(t, 47, report.Usage.TotalOutputTokens)
	assert.Equal(t, 1, report.Usage.SubagentCount)
	assert.Contains(t, report.ChildLinks, diagnosisChildLink{SessionID: "child"})
	assert.Equal(t, []diagnosisRetry{{Tool: "read", Count: 3, FirstOrdinal: 0, LastOrdinal: 2}}, report.RetryRuns)
	require.Len(t, report.LargeResults, 3)
	assert.Equal(t, 20, report.LargeResults[0].ResultLength)
	assert.Equal(t, 1, stub.OpenCount)
	out, err = executeCommand(newRootCommand(), "session", "diagnose", canonical, "--pg", "--own-only", "--format", "json")
	require.NoError(t, err)
	report = sessionDiagnosis{}
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, []diagnosisChildLink{{SessionID: "child"}}, report.ChildLinks)
	assert.Contains(t, report.Scope, "child links remain listed")
	assert.Equal(t, 2, stub.OpenCount)
}

func TestSessionDiagnoseMissingSession(t *testing.T) {
	seedEmptyArchive(t, newAgentDataDir(t))
	_, err := executeCommand(newRootCommand(), "session", "diagnose", "missing-session", "--format", "json")
	require.ErrorIs(t, err, errSessionNotFound)
}

func TestSessionDiagnoseRefreshFailure(t *testing.T) {
	testDataDir(t)
	refreshes := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":999}`))
		case "/api/v1/sessions/remote":
			_, _ = w.Write([]byte(`{"id":"remote","agent":"codex"}`))
		case "/api/v1/sessions/sync":
			assert.Equal(t, http.MethodPost, r.Method)
			refreshes++
			http.Error(w, `{"error":"refresh failed"}`, http.StatusServiceUnavailable)
		case "/api/v1/sessions/remote/usage":
			_, _ = w.Write([]byte(`{"session_id":"remote","has_token_data":true,"total_output_tokens":42}`))
		case "/api/v1/sessions/remote/messages":
			_, _ = w.Write([]byte(`{"messages":[]}`))
		case "/api/v1/sessions/remote/children":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	out, err := executeCommand(newRootCommand(), "session", "diagnose", "remote", "--server", ts.URL, "--format", "json")
	require.NoError(t, err)
	assert.Equal(t, 1, refreshes)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 42, report.Usage.TotalOutputTokens)
	assert.True(t, report.Usage.RefreshIncomplete)
}

func TestSessionDiagnoseNoSync(t *testing.T) {
	testDataDir(t)
	refreshes := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":999}`))
		case "/api/v1/sessions/remote":
			_, _ = w.Write([]byte(`{"id":"remote","agent":"codex"}`))
		case "/api/v1/sessions/sync":
			refreshes++
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/sessions/remote/usage":
			_, _ = w.Write([]byte(`{"session_id":"remote","has_token_data":true,"total_output_tokens":42}`))
		case "/api/v1/sessions/remote/messages":
			_, _ = w.Write([]byte(`{"messages":[]}`))
		case "/api/v1/sessions/remote/children":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	_, err := executeCommand(newRootCommand(), "session", "diagnose", "remote", "--server", ts.URL, "--no-sync", "--format", "json")
	require.NoError(t, err)
	assert.Zero(t, refreshes)
}

func TestSessionDiagnosisHumanBoundsContext(t *testing.T) {
	messages := make([]db.Message, 100)
	for i := range messages {
		messages[i] = db.Message{Ordinal: i, HasContextTokens: true, ContextTokens: i * 100}
	}
	report := testSessionDiagnosis(t, "root", &service.SessionUsageReport{}, messages)
	var out strings.Builder
	require.NoError(t, renderSessionDiagnosis(&out, report))
	assert.Equal(t, 10, strings.Count(out.String(), "  Context #"))
	assert.Contains(t, out.String(), "Context #99: 9900 tokens")
	assert.Len(t, report.Context, 100)
}

func TestSessionDiagnosisEvidenceAndPrivacy(t *testing.T) {
	ordinal := 3
	usage := &service.SessionUsageReport{
		SessionID: "root", Project: "private-project", HasTokenData: true, SubagentCount: 1,
		Breakdown: []db.SessionUsageBreakdownEntry{{Source: "message", MessageOrdinal: &ordinal, InputTokens: 40, CacheReadInputTokens: 60, CacheCreationInputTokens: 20, OutputTokens: 10}, {Source: "message", SubagentSessionID: "child", InputTokens: 5, OutputTokens: 2}},
	}
	messages := []db.Message{
		{Ordinal: 3, Content: "secret prompt", HasContextTokens: true, ContextTokens: 120, Model: "model-a", ToolCalls: []db.ToolCall{{ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultContentLength: 20}}},
		{Ordinal: 4, ToolCalls: []db.ToolCall{{ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultContentLength: 900}}},
		{Ordinal: 5, ToolCalls: []db.ToolCall{{ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultContentLength: 50}}},
		{Ordinal: 6, ToolCalls: []db.ToolCall{{ToolName: "read", ResultContentLength: 100, SubagentSessionID: "child"}}},
		{Ordinal: 7, IsCompactBoundary: true},
	}
	got := testSessionDiagnosis(t, "root", usage, messages)
	assert.Equal(t, diagnosisTokens{UncachedInput: 45, CacheRead: 60, CacheWrite: 20, Output: 12}, got.ObservedRows)
	assert.Equal(t, []int{7}, got.Compactions)
	require.Len(t, got.RetryRuns, 1)
	assert.Equal(t, diagnosisRetry{Tool: "shell", Count: 3, FirstOrdinal: 3, LastOrdinal: 5}, got.RetryRuns[0])
	require.Len(t, got.LargeResults, 4)
	assert.Equal(t, 900, got.LargeResults[0].ResultLength)
	assert.Equal(t, 1, got.Usage.SubagentCount)
	assert.Equal(t, []diagnosisChildLink{{Ordinal: new(6), Tool: "read", SessionID: "child"}}, got.ChildLinks)
	assert.Nil(t, got.Coverage.RequestCount)
	one, err := json.Marshal(got)
	require.NoError(t, err)
	two, err := json.Marshal(testSessionDiagnosis(t, "root", usage, messages))
	require.NoError(t, err)
	assert.Equal(t, string(one), string(two))
	assert.NotContains(t, string(one), "secret")
	assert.NotContains(t, string(one), "private-project")
	var human strings.Builder
	require.NoError(t, renderSessionDiagnosis(&human, got))
	assert.Contains(t, human.String(), "cache read 60")
	assert.Contains(t, human.String(), "  Child: child via read (#6)\n")
	assert.NotContains(t, human.String(), "secret")
}

func TestSessionDiagnosisMissingEvidence(t *testing.T) {
	messages := []db.Message{
		{Ordinal: 0, ToolCalls: []db.ToolCall{{ToolName: "read"}}},
		{Ordinal: 1, ToolCalls: []db.ToolCall{{ToolName: "read"}}},
		{Ordinal: 2, ToolCalls: []db.ToolCall{{ToolName: "read"}}},
	}
	got := testSessionDiagnosis(t, "root", &service.SessionUsageReport{}, messages)
	assert.Equal(t, []diagnosisRetry{{Tool: "read", Count: 3, FirstOrdinal: 0, LastOrdinal: 2}}, got.RetryRuns)
	assert.Equal(t, "no token data reported", got.Coverage.UsageStatus)
	assert.Nil(t, got.Coverage.RequestCount)
}

func TestSessionDiagnosisInferredCompactions(t *testing.T) {
	messages := make([]db.Message, db.MaxMessageLimit+1)
	for i := range messages {
		messages[i].Ordinal = i
	}
	messages[db.MaxMessageLimit-1] = db.Message{Ordinal: db.MaxMessageLimit - 1, Role: "assistant", HasContextTokens: true, ContextTokens: 100000}
	messages[db.MaxMessageLimit] = db.Message{Ordinal: db.MaxMessageLimit, Role: "assistant", HasContextTokens: true, ContextTokens: 20000}
	for _, tt := range []struct {
		name     string
		boundary bool
		role     string
		want     int
	}{
		{name: "context drop across pages", role: "assistant", want: 1},
		{name: "explicit boundary takes precedence", boundary: true, role: "assistant", want: 0},
		{name: "user context ignored", role: "user", want: 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			messages[0].IsCompactBoundary = tt.boundary
			messages[db.MaxMessageLimit].Role = tt.role
			got := testSessionDiagnosis(t, "root", &service.SessionUsageReport{}, messages)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			var report struct {
				InferredCompactions int `json:"inferred_compaction_count"`
			}
			require.NoError(t, json.Unmarshal(encoded, &report))
			assert.Equal(t, tt.want, report.InferredCompactions)
			if tt.boundary {
				assert.Equal(t, []int{0}, got.Compactions)
			} else {
				assert.Empty(t, got.Compactions)
			}
			var human strings.Builder
			require.NoError(t, renderSessionDiagnosis(&human, got))
			assert.Contains(t, human.String(), "inferred compactions: "+strconv.Itoa(tt.want))
		})
	}
}

func TestSessionDiagnoseRemoteUsesUsageTransport(t *testing.T) {
	for _, discovered := range []bool{false, true} {
		t.Run(strconv.FormatBool(discovered), func(t *testing.T) {
			dataDir := testDataDir(t)
			var requests []string
			ts := sessionUsageRuntimeServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/sessions/sync":
					assert.Equal(t, http.MethodPost, r.Method)
					_, _ = w.Write([]byte(`{}`))
				case "/api/v1/version":
					_, _ = w.Write([]byte(`{"api_version":999}`))
				case "/api/v1/sessions/remote":
					_, _ = w.Write([]byte(`{"id":"remote","agent":"codex"}`))
				case "/api/v1/sessions/remote/usage":
					assert.Equal(t, "true", r.URL.Query().Get("subagents"))
					assert.Equal(t, "true", r.URL.Query().Get("breakdown"))
					_, _ = w.Write([]byte(`{"session_id":"remote","agent":"codex","has_token_data":true,"total_output_tokens":2,"peak_context_tokens":10,"cost":{"microdollars":0},"has_cost":false,"models":[],"breakdown":[{"ordinal":1,"source":"message","label":"Prompt 1","timestamp":"","model":"x","input_tokens":7,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":0,"cost":{"microdollars":0},"has_cost":false}],"breakdown_count":1}`))
				case "/api/v1/sessions/remote/children":
					_, _ = w.Write([]byte(`[{"id":"child"},{"id":"unlinked-child"}]`))
				case "/api/v1/sessions/remote/messages":
					_, _ = w.Write([]byte(`{"messages":[{"ordinal":1,"role":"assistant","content":"private text","has_context_tokens":true,"context_tokens":10,"tool_calls":[{"tool_name":"read","input_json":"private args","result_content_length":12,"subagent_session_id":"child"},{"tool_name":"task","subagent_session_id":"child"}]}],"count":1}`))
				case "/api/v1/sessions/remote/tool-calls":
					_, _ = w.Write([]byte(`{"tool_calls":[{"ordinal":1,"tool_name":"read","input_json":"private args","result_length":12}],"count":1}`))
				default:
					http.NotFound(w, r)
				}
			})
			defer ts.Close()
			args := []string{"session", "diagnose", "remote", "--json"}
			if discovered {
				t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
				registerTestRuntime(t, dataDir, ts.URL, true)
			} else {
				args = append(args, "--server", ts.URL)
			}
			out, err := executeCommand(newRootCommand(), args...)
			require.NoError(t, err)
			var report sessionDiagnosis
			require.NoError(t, json.Unmarshal([]byte(out), &report))
			assert.Equal(t, 7, report.ObservedRows.UncachedInput)
			assert.Equal(t, 3, report.ObservedRows.CacheRead)
			require.Len(t, report.LargeResults, 1)
			assert.Equal(t, 12, report.LargeResults[0].ResultLength)
			assert.NotContains(t, out, "private")
			assert.Equal(t, []diagnosisChildLink{{Ordinal: new(1), Tool: "read", SessionID: "child"}, {SessionID: "unlinked-child"}}, report.ChildLinks)
			assert.Contains(t, requests, "POST /api/v1/sessions/sync")
			for _, request := range requests {
				if request != "POST /api/v1/sessions/sync" {
					assert.True(t, strings.HasPrefix(request, "GET "), request)
				}
				assert.NotContains(t, request, "/tool-calls")
			}
		})
	}
}

func FuzzDiagnosisLargestResults(f *testing.F) {
	f.Add([]byte{1, 200, 9, 4, 3, 8, 7, 6, 5, 2, 11, 10})
	f.Add([]byte{})
	f.Add([]byte{0, 7, 7, 0, 1})
	f.Add([]byte{8, 8, 8, 8, 8, 8, 8, 8, 8, 8, 8})
	f.Fuzz(func(t *testing.T, sizes []byte) {
		if len(sizes) > 200 {
			sizes = sizes[:200]
		}
		messages := make([]db.Message, 0, len(sizes))
		for i, n := range sizes {
			messages = append(messages, db.Message{Ordinal: i, ToolCalls: []db.ToolCall{{ToolName: "read", ResultContentLength: int(n)}}})
		}
		result := testSessionDiagnosis(t, "session", &service.SessionUsageReport{}, messages)
		var counts [256]int
		for _, n := range sizes {
			counts[n]++
		}
		expected := []int{}
		for n := 255; n > 0 && len(expected) < 10; n-- {
			for range min(counts[n], 10-len(expected)) {
				expected = append(expected, n)
			}
		}
		actual := []int{}
		seen := make(map[int]bool)
		for _, c := range result.LargeResults {
			require.GreaterOrEqual(t, c.Ordinal, 0)
			require.Less(t, c.Ordinal, len(sizes))
			assert.False(t, seen[c.Ordinal], "a result must appear only once")
			seen[c.Ordinal] = true
			assert.Equal(t, int(sizes[c.Ordinal]), c.ResultLength)
			actual = append(actual, c.ResultLength)
		}
		assert.Equal(t, expected, actual)
	})
}

func TestSessionDiagnoseColdStartPreservesSourceSync(t *testing.T) {
	testDataDir(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/sync":
			assert.Equal(t, http.MethodPost, r.Method)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":999}`))
		case "/api/v1/sessions/remote":
			_, _ = w.Write([]byte(`{"id":"remote","agent":"codex"}`))
		case "/api/v1/sessions/remote/usage":
			_, _ = w.Write([]byte(`{"session_id":"remote","agent":"codex","has_token_data":true,"cost":{"microdollars":0},"has_cost":false,"models":[],"breakdown":[],"breakdown_count":0}`))
		case "/api/v1/sessions/remote/children":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/sessions/remote/messages":
			_, _ = w.Write([]byte(`{"messages":[],"count":0}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	u, err := url.Parse(ts.URL)
	require.NoError(t, err)
	host, portText, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	port, err := strconv.Atoi(portText)
	require.NoError(t, err)
	var noSync []bool
	stubStartBackgroundServeForTransport(t, func(_ context.Context, cfg *config.Config, _ time.Duration, _ bool) (*DaemonRuntime, error) {
		noSync = append(noSync, cfg.NoSync)
		return &DaemonRuntime{Host: host, Port: port}, nil
	})
	_, err = executeCommand(newRootCommand(), "session", "diagnose", "remote", "--json")
	require.NoError(t, err)
	require.NotEmpty(t, noSync)
	for _, value := range noSync {
		assert.False(t, value, "diagnosis must preserve background ingestion for later commands")
	}
}

func TestSessionDiagnoseDirectArchive(t *testing.T) {
	dataDir := newAgentDataDir(t)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	store := dbtest.OpenTestDBAt(t, sessionsDBPath(dataDir))
	seedUsageSession(t, store, "codex:direct", "project", "codex", 42)
	out, err := executeCommand(newRootCommand(), "session", "diagnose", "codex:direct", "--json")
	require.NoError(t, err)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 42, report.Usage.TotalOutputTokens)
	assert.Equal(t, 1, report.SchemaVersion)
	assert.Nil(t, report.Coverage.RequestCount)
}

func TestSessionDiagnoseLongGrowingArchive(t *testing.T) {
	testDataDir(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/sync":
			assert.Equal(t, http.MethodPost, r.Method)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":999}`))
		case "/api/v1/sessions/long":
			_, _ = w.Write([]byte(`{"id":"long","agent":"codex"}`))
		case "/api/v1/sessions/long/usage":
			_, _ = w.Write([]byte(`{"session_id":"long","has_token_data":true}`))
		case "/api/v1/sessions/long/children":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v1/sessions/long/messages":
			from, _ := strconv.Atoi(r.URL.Query().Get("from"))
			assert.Empty(t, r.URL.Query().Get("expected_revision"))
			messages := []db.Message{}
			for i := from; i < min(from+1000, 10003); i++ {
				m := db.Message{Ordinal: i, Content: "private transcript"}
				if i >= 9999 {
					m.ToolCalls = []db.ToolCall{{ToolName: "read", InputJSON: "private arguments", ResultContentLength: 12345}}
				}
				if i == 10002 {
					m.HasContextTokens, m.ContextTokens, m.IsCompactBoundary = true, 90000, true
				}
				messages = append(messages, m)
			}
			if !assert.NoError(t, json.MarshalWrite(w, service.MessageList{Messages: messages, TranscriptRevision: strconv.Itoa(from)})) {
				return
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	out, err := executeCommand(newRootCommand(), "session", "diagnose", "long", "--server", ts.URL, "--json")
	require.NoError(t, err)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, []int{10002}, report.Compactions)
	assert.Equal(t, []diagnosisContext{{Ordinal: 10002, ContextTokens: 90000}}, report.Context)
	assert.Equal(t, []diagnosisRetry{{Tool: "read", Count: 4, FirstOrdinal: 9999, LastOrdinal: 10002}}, report.RetryRuns)
	require.Len(t, report.LargeResults, 4)
	assert.Equal(t, 12345, report.LargeResults[0].ResultLength)
	assert.NotContains(t, out, "private")
}

type diagnosisMessageService struct {
	service.SessionService
	messages []db.Message
}

func (s diagnosisMessageService) Messages(_ context.Context, _ string, filter service.MessageFilter) (*service.MessageList, error) {
	from := min(*filter.From, len(s.messages))
	return &service.MessageList{Messages: s.messages[from:min(from+filter.Limit, len(s.messages))]}, nil
}

func testSessionDiagnosis(t *testing.T, id string, usage *service.SessionUsageReport, messages []db.Message) *sessionDiagnosis {
	t.Helper()
	out := buildSessionDiagnosis(id, usage)
	require.NoError(t, out.readMessages(t.Context(), diagnosisMessageService{messages: messages}))
	return out
}
