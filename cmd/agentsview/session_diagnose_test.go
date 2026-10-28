package main

import (
	"context"
	"encoding/json/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestSessionDiagnosisEvidenceAndPrivacy(t *testing.T) {
	ordinal := 3
	usage := &sessionUsageOutput{SessionUsage: db.SessionUsage{
		SessionID: "root", Project: "private-project", HasTokenData: true, SubagentCount: 1,
		Breakdown: []db.SessionUsageBreakdownEntry{{Source: "message", MessageOrdinal: &ordinal, InputTokens: 40, CacheReadInputTokens: 60, CacheCreationInputTokens: 20, OutputTokens: 10}, {Source: "message", SubagentSessionID: "child", InputTokens: 5, OutputTokens: 2}},
	}}
	messages := []db.Message{{Ordinal: 3, Content: "secret prompt", HasContextTokens: true, ContextTokens: 120, Model: "model-a"}, {Ordinal: 7, IsCompactBoundary: true}}
	calls := []service.ToolCall{{Ordinal: 3, ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultLength: 20}, {Ordinal: 4, ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultLength: 900}, {Ordinal: 5, ToolName: "shell", InputJSON: `{"command":"secret command"}`, ResultLength: 50}, {Ordinal: 6, ToolName: "read", ResultLength: 100, SubagentSessionID: "child"}}
	got := buildSessionDiagnosis("root", usage, messages, calls, true)
	assert.Equal(t, diagnosisTokens{UncachedInput: 45, CacheRead: 60, CacheWrite: 20, Output: 12}, got.ObservedRows)
	assert.Equal(t, []int{7}, got.Compactions)
	require.Len(t, got.RetryRuns, 1)
	assert.Equal(t, diagnosisRetry{Tool: "shell", Count: 3, FirstOrdinal: 3, LastOrdinal: 5}, got.RetryRuns[0])
	require.Len(t, got.LargeResults, 4)
	assert.Equal(t, 900, got.LargeResults[0].ResultLength)
	assert.Equal(t, 1, got.Usage.SubagentCount)
	assert.Equal(t, []diagnosisChildLink{{Ordinal: 6, Tool: "read", SessionID: "child"}}, got.ChildLinks)
	assert.Nil(t, got.Coverage.RequestCount)
	one, err := json.Marshal(got)
	require.NoError(t, err)
	two, err := json.Marshal(buildSessionDiagnosis("root", usage, messages, calls, true))
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
	got := buildSessionDiagnosis("root", &sessionUsageOutput{}, nil, calls, false)
	assert.Empty(t, got.RetryRuns)
	assert.False(t, got.Coverage.MessagesComplete)
	assert.Equal(t, "no token data reported", got.Coverage.UsageStatus)
	assert.Nil(t, got.Coverage.RequestCount)
}

type diagnosisPageService struct {
	service.SessionService
	filters []service.MessageFilter
}

func (s *diagnosisPageService) Messages(_ context.Context, _ string, f service.MessageFilter) (*service.MessageList, error) {
	s.filters = append(s.filters, f)
	if len(s.filters) == 1 {
		m := make([]db.Message, db.MaxMessageLimit)
		for i := range m {
			m[i].Ordinal = i
		}
		return &service.MessageList{Messages: m, TranscriptRevision: "rev", EvidenceSource: "archive"}, nil
	}
	return &service.MessageList{Messages: []db.Message{{Ordinal: 1000}}}, nil
}

func TestDiagnosisMessagesPagesAndPinsRevision(t *testing.T) {
	svc := &diagnosisPageService{}
	messages, complete, err := diagnosisMessages(t.Context(), svc, "session")
	require.NoError(t, err)
	assert.True(t, complete)
	require.Len(t, messages, 1001)
	require.Len(t, svc.filters, 2)
	assert.Equal(t, 1000, *svc.filters[1].From)
	assert.Equal(t, "rev", svc.filters[1].ExpectedRevision)
	assert.Equal(t, "archive", svc.filters[1].EvidenceSource)
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
	assert.True(t, report.Coverage.MessagesComplete)
	assert.Nil(t, report.Coverage.RequestCount)
	assert.NotContains(t, out, "msg-1")
}

func TestSessionDiagnoseRemoteUsesReadOnlyEndpoints(t *testing.T) {
	var requests []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":999}`))
		case "/api/v1/sessions/remote":
			_, _ = w.Write([]byte(`{"id":"remote","agent":"codex"}`))
		case "/api/v1/sessions/remote/usage":
			assert.Equal(t, "true", r.URL.Query().Get("subagents"))
			assert.Equal(t, "true", r.URL.Query().Get("breakdown"))
			_, _ = w.Write([]byte(`{"session_id":"remote","agent":"codex","has_token_data":true,"total_output_tokens":2,"peak_context_tokens":10,"cost":{"microdollars":0},"has_cost":false,"models":[],"breakdown":[{"ordinal":1,"source":"message","label":"Prompt 1","timestamp":"","model":"x","input_tokens":7,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":0,"cost":{"microdollars":0},"has_cost":false}],"breakdown_count":1}`))
		case "/api/v1/sessions/remote/messages":
			_, _ = w.Write([]byte(`{"messages":[{"ordinal":1,"role":"assistant","content":"private text","has_context_tokens":true,"context_tokens":10}],"count":1}`))
		case "/api/v1/sessions/remote/tool-calls":
			_, _ = w.Write([]byte(`{"tool_calls":[{"ordinal":1,"tool_name":"read","input_json":"private args","result_length":12}],"count":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	out, err := executeCommand(newRootCommand(), "session", "diagnose", "remote", "--server", ts.URL, "--json")
	require.NoError(t, err)
	var report sessionDiagnosis
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	assert.Equal(t, 7, report.ObservedRows.UncachedInput)
	assert.Equal(t, 3, report.ObservedRows.CacheRead)
	require.Len(t, report.LargeResults, 1)
	assert.Equal(t, 12, report.LargeResults[0].ResultLength)
	assert.NotContains(t, out, "private")
	for _, request := range requests {
		assert.True(t, strings.HasPrefix(request, "GET "), request)
	}
}

func FuzzDiagnosisLargestResults(f *testing.F) {
	f.Add([]byte{1, 200, 9, 4, 3, 8, 7, 6, 5, 2, 11, 10})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, sizes []byte) {
		if len(sizes) > 200 {
			sizes = sizes[:200]
		}
		calls := make([]service.ToolCall, 0, len(sizes))
		for i, n := range sizes {
			calls = append(calls, service.ToolCall{Ordinal: i, ToolName: "read", ResultLength: int(n)})
		}
		result := buildSessionDiagnosis("session", &sessionUsageOutput{}, nil, calls, true)
		assert.LessOrEqual(t, len(result.LargeResults), 10)
		for i, c := range result.LargeResults {
			if i > 0 {
				assert.GreaterOrEqual(t, result.LargeResults[i-1].ResultLength, c.ResultLength)
			}
			assert.Equal(t, int(sizes[c.Ordinal]), c.ResultLength)
		}
		if len(result.LargeResults) == 10 {
			cutoff := result.LargeResults[9].ResultLength
			for _, n := range sizes {
				if int(n) > cutoff {
					found := false
					for _, c := range result.LargeResults {
						if c.ResultLength == int(n) {
							found = true
							break
						}
					}
					assert.True(t, found)
				}
			}
		}
	})
}

func TestSessionDiagnoseColdStartDisablesSourceSync(t *testing.T) {
	testDataDir(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/version":
			_, _ = w.Write([]byte(`{"api_version":999}`))
		case "/api/v1/sessions/remote":
			_, _ = w.Write([]byte(`{"id":"remote","agent":"codex"}`))
		case "/api/v1/sessions/remote/usage":
			_, _ = w.Write([]byte(`{"session_id":"remote","agent":"codex","has_token_data":true,"cost":{"microdollars":0},"has_cost":false,"models":[],"breakdown":[],"breakdown_count":0}`))
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
		assert.True(t, value, "every cold-start daemon must not sync sources")
	}
}

func TestSessionDiagnoseRejectsSyncBeforeStartingDaemon(t *testing.T) {
	testDataDir(t)
	started := false
	stubStartBackgroundServeForTransport(t, func(_ context.Context, _ *config.Config, _ time.Duration, _ bool) (*DaemonRuntime, error) {
		started = true
		return nil, nil
	})
	_, err := executeCommand(newRootCommand(), "session", "diagnose", "remote", "--no-sync=false")
	require.ErrorContains(t, err, "diagnose only reads archived usage")
	assert.False(t, started)
}
