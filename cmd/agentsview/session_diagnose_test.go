package main

import (
	"context"
	"encoding/json/jsontext"
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
	"go.kenn.io/agentsview/internal/codexcloud/mirror"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
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
	assert.Equal(t, 42, report.Usage.TotalOutputTokens)
	assert.Zero(t, report.Usage.SubagentCount)
	assert.Equal(t, []diagnosisChildLink{{SessionID: "child"}}, report.ChildLinks)
	assert.Contains(t, report.Scope, "child links remain listed")
	assert.Equal(t, 2, stub.OpenCount)
	out, err = executeCommand(newRootCommand(), "session", "diagnose", canonical, "--pg")
	require.NoError(t, err)
	assert.Contains(t, out, "  Child: child\n")
}

func TestSessionDiagnoseMissingSession(t *testing.T) {
	seedEmptyArchive(t, newAgentDataDir(t))
	_, err := executeCommand(newRootCommand(), "session", "diagnose", "missing-session", "--format", "json")
	require.ErrorIs(t, err, errSessionNotFound)
	var stderr strings.Builder
	assert.Equal(t, 1, reportCLIError(err, &stderr))
	assert.Contains(t, stderr.String(), "session not found: missing-session")
}

func TestSessionDiagnoseRefreshFailure(t *testing.T) {
	for _, format := range []string{"json", "human"} {
		t.Run(format, func(t *testing.T) {
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
			args := []string{"session", "diagnose", "remote", "--server", ts.URL}
			if format == "json" {
				args = append(args, "--format", "json")
			}
			out, err := executeCommand(newRootCommand(), args...)
			require.NoError(t, err)
			assert.Equal(t, 1, refreshes)
			if format == "json" {
				var report struct {
					Usage sessionUsageOutput `json:"usage"`
				}
				require.NoError(t, json.Unmarshal([]byte(out), &report))
				assert.Equal(t, 42, report.Usage.TotalOutputTokens)
				assert.True(t, report.Usage.RefreshIncomplete)
			} else {
				assert.Contains(t, out, "Usage refresh: incomplete; showing archived totals")
			}
		})
	}
}

func TestSessionDiagnoseNoSync(t *testing.T) {
	testDataDir(t)
	refreshes := 0
	usageReads := 0
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
			usageReads++
			assert.Equal(t, "true", r.URL.Query().Get("subagents"))
			_, _ = w.Write([]byte(`{"session_id":"remote","has_token_data":true,"total_output_tokens":42,"subagent_count":1}`))
		case "/api/v1/sessions/remote/messages":
			_, _ = w.Write([]byte(`{"messages":[{"ordinal":7,"is_compact_boundary":true}]}`))
		case "/api/v1/sessions/remote/children":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	out, err := executeCommand(newRootCommand(), "session", "diagnose", "remote", "--server", ts.URL, "--no-sync", "--format", "json")
	require.NoError(t, err)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 42, report.Usage.TotalOutputTokens)
	assert.Equal(t, 1, report.Usage.SubagentCount)
	assert.Equal(t, []int{7}, report.Compactions)
	assert.Equal(t, 1, usageReads)
	assert.Zero(t, refreshes)
}

func TestSessionDiagnosisHumanBoundsContext(t *testing.T) {
	messages := make([]db.Message, 100)
	for i := range messages {
		messages[i] = db.Message{Ordinal: i, HasContextTokens: true, ContextTokens: i * 100}
	}
	report := testSessionDiagnosis(t, "root", &service.SessionUsageReport{}, messages, nil)
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
	messages := []db.Message{{Ordinal: 3, Content: "secret prompt", HasContextTokens: true, ContextTokens: 120, Model: "model-a"}, {Ordinal: 7, IsCompactBoundary: true}}
	calls := []service.ToolCall{{Ordinal: 3, ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultLength: 20}, {Ordinal: 4, ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultLength: 900}, {Ordinal: 5, ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultLength: 50}, {Ordinal: 6, ToolName: "read", ResultLength: 100, SubagentSessionID: "child"}}
	got := testSessionDiagnosis(t, "root", usage, messages, calls)
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
	two, err := json.Marshal(testSessionDiagnosis(t, "root", usage, messages, calls))
	require.NoError(t, err)
	assert.Equal(t, string(one), string(two))
	assert.NotContains(t, string(one), "secret")
	assert.NotContains(t, string(one), "private-project")
	var human strings.Builder
	require.NoError(t, renderSessionDiagnosis(&human, got))
	assert.Contains(t, human.String(), "cache read 60")
	assert.NotContains(t, human.String(), "secret")
}

func TestSessionDiagnosisMissingEvidence(t *testing.T) {
	calls := []service.ToolCall{{ToolName: "read"}, {ToolName: "read"}, {ToolName: "read"}}
	got := testSessionDiagnosis(t, "root", &service.SessionUsageReport{}, nil, calls)
	assert.Equal(t, []diagnosisRetry{{Tool: "read", Count: 3}}, got.RetryRuns)
	assert.Equal(t, "no token data reported", got.Coverage.UsageStatus)
	assert.Nil(t, got.Coverage.RequestCount)
}

type diagnosisPageService struct {
	service.SessionService
	filters []service.MessageFilter
}

func (s *diagnosisPageService) Messages(_ context.Context, _ string, f service.MessageFilter) (*service.MessageList, error) {
	s.filters = append(s.filters, f)
	if f.ExpectedRevision != "" || f.EvidenceSource != "" {
		return nil, service.ErrSourceChanged
	}
	if len(s.filters) == 1 {
		m := make([]db.Message, db.MaxMessageLimit)
		for i := range m {
			m[i].Ordinal = i
		}
		return &service.MessageList{Messages: m, TranscriptRevision: "rev", EvidenceSource: "archive"}, nil
	}
	return &service.MessageList{Messages: []db.Message{{Ordinal: 1000, IsCompactBoundary: true}}}, nil
}

func TestDiagnosisAndRecallReadGrowingSession(t *testing.T) {
	for _, diagnosis := range []bool{false, true} {
		t.Run(strconv.FormatBool(diagnosis), func(t *testing.T) {
			svc := &diagnosisPageService{}
			if diagnosis {
				report := buildSessionDiagnosis("session", &service.SessionUsageReport{})
				require.NoError(t, report.readMessages(t.Context(), svc))
				assert.Equal(t, []int{1000}, report.Compactions)
			} else {
				messages, err := loadRecallExtractionMessages(t.Context(), svc, "session")
				require.NoError(t, err)
				assert.Len(t, messages, 1001)
			}
			require.Len(t, svc.filters, 2)
			assert.Equal(t, 1000, *svc.filters[1].From)
		})
	}
}

func TestSessionDiagnoseReadsSQLiteWithoutContent(t *testing.T) {
	dataDir := newAgentDataDir(t)
	seedSession(t, dataDir, "diagnose-test", "project")
	seedMessages(t, dataDir, "diagnose-test", 3)
	out, err := executeCommand(newRootCommand(), "session", "diagnose", "diagnose-test", "--json")
	require.NoError(t, err)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 1, report.SchemaVersion)
	assert.Nil(t, report.Coverage.RequestCount)
	assert.NotContains(t, out, "msg-1")
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
		calls := make([]service.ToolCall, 0, len(sizes))
		for i, n := range sizes {
			calls = append(calls, service.ToolCall{Ordinal: i, ToolName: "read", ResultLength: int(n)})
		}
		result := testSessionDiagnosis(t, "session", &service.SessionUsageReport{}, nil, calls)
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
		case "/api/v1/sessions/remote/tool-calls":
			_, _ = w.Write([]byte(`{"tool_calls":[],"count":0}`))
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

func TestSessionUsageNoSyncRejectsVersionSkewWhenAutostartDisabled(t *testing.T) {
	dataDir := testDataDir(t)
	host, port := testPingServer(t)
	writeDaemonRuntimeForTest(t, dataDir, host, port, "1.0.0", false)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	setTestVersion(t, "1.1.0")
	forbidStartBackgroundServeForTransport(t, "autostart is disabled")

	cmd := sessionUsageCommand(t, "session", "usage", "session", "--no-sync", "--json")
	_, _, err := sessionUsageDataForCommand(cmd, "session")
	require.ErrorContains(t, err, "daemon restart required")
}

func TestSessionDiagnosisRetryCandidatesIncludeRecordedPlaceholders(t *testing.T) {
	fetched := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	records := []mirror.Record{{Kind: mirror.KindThread, FetchedAt: fetched, Data: jsontext.Value(`{"id":"thread-a","createdAt":1,"updatedAt":2}`)}}
	for _, id := range []string{"a", "b", "c"} {
		records = append(records, mirror.Record{Kind: mirror.KindItem, FetchedAt: fetched, Data: jsontext.Value(`{"turnId":"turn-1","startedAtMs":1759320000000,"item":{"type":"functionCallOutput","id":"` + id + `","name":"exec","output":"done"}}`)})
	}
	path := filepath.Join(t.TempDir(), "account", "thread-a.jsonl.zstd")
	_, err := mirror.Append(path, 0, records)
	require.NoError(t, err)
	parsed, ok, err := parser.ParseCodexCloudFile(t.Context(), path, "machine")
	require.NoError(t, err)
	require.True(t, ok)
	var calls []service.ToolCall
	for _, m := range parsed.Messages {
		for _, c := range m.ToolCalls {
			calls = append(calls, service.ToolCall{Ordinal: m.Ordinal, ToolName: c.ToolName, InputJSON: c.InputJSON})
		}
	}
	require.Len(t, calls, 3)
	for _, c := range calls {
		assert.Equal(t, "{}", c.InputJSON)
	}
	report := testSessionDiagnosis(t, parsed.Session.ID, &service.SessionUsageReport{}, nil, calls)
	require.Len(t, report.RetryRuns, 1)
	assert.Equal(t, 3, report.RetryRuns[0].Count)
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
	cmd := sessionUsageCommand(t, "session", "usage", "codex:direct")
	usage, _, err := sessionUsageDataForCommand(cmd, "codex:direct")
	require.NoError(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 42, usage.TotalOutputTokens)
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
			// Ingestion changes the revision between pages. Unpinned readers can
			// still read the newly appended evidence.
			if r.URL.Query().Get("expected_revision") != "" {
				http.Error(w, `{"error":"source_changed"}`, http.StatusConflict)
				return
			}
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
			require.NoError(t, json.MarshalWrite(w, service.MessageList{Messages: messages, TranscriptRevision: strconv.Itoa(from)}))
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

func (s diagnosisMessageService) Messages(context.Context, string, service.MessageFilter) (*service.MessageList, error) {
	return &service.MessageList{Messages: s.messages}, nil
}
func testSessionDiagnosis(t *testing.T, id string, usage *service.SessionUsageReport, messages []db.Message, calls []service.ToolCall) *sessionDiagnosis {
	t.Helper()
	messages = append([]db.Message{}, messages...)
	for _, c := range calls {
		messages = append(messages, db.Message{Ordinal: c.Ordinal, ToolCalls: []db.ToolCall{{ToolName: c.ToolName, InputJSON: c.InputJSON, ResultContentLength: c.ResultLength, SubagentSessionID: c.SubagentSessionID}}})
	}
	out := buildSessionDiagnosis(id, usage)
	require.NoError(t, out.readMessages(t.Context(), diagnosisMessageService{messages: messages}))
	return out
}
