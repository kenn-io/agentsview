package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMusePersonalFixture(t *testing.T) {
	provider, ok := NewProvider(AgentType("muse-personal"), ProviderConfig{Roots: []string{filepath.Join("testdata", "muse_personal")}, Machine: "fixture"})
	require.True(t, ok, "Muse Personal must have a distinct registered provider")
	sources, err := provider.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 3)
	for _, source := range sources {
		out, err := provider.Parse(t.Context(), ParseRequest{Source: source})
		require.NoError(t, err)
		require.Len(t, out.Results, 1)
		assert.True(t, out.ForceReplace)
		result := out.Results[0].Result
		assert.Empty(t, result.Session.UsageEvents)
		assert.Empty(t, result.Session.ParentSessionID)
		assert.Empty(t, result.Session.SessionKind)
		assert.Empty(t, result.Session.SessionName)
		if result.Session.SourceSessionID != "11111111-2222-4333-8444-555555555555" {
			continue
		}
		assert.Equal(t, "muse-personal:11111111-2222-4333-8444-555555555555", result.Session.ID)
		require.Len(t, result.Messages, 11)
		assert.Equal(t, 2, result.Session.UserMessageCount)
		assert.Equal(t, RoleUser, result.Messages[0].Role)
		assert.Contains(t, result.Messages[0].Content, "kite-chart.png")
		assert.Equal(t, RoleSystem, result.Messages[1].Role)
		assert.Equal(t, "Synthetic reasoning text for the fixture.", result.Messages[2].ThinkingText)
		assert.Equal(t, "commentary", result.Messages[3].SourceSubtype)
		assert.Empty(t, *result.Messages[3].DialogueText)
		require.Len(t, result.Messages[4].ToolCalls, 1)
		require.Len(t, result.Messages[4].ToolCalls[0].ResultEvents, 1)
		assert.Equal(t, "synthetic widget inventory: 3 widgets", result.Messages[4].ToolCalls[0].ResultEvents[0].Content)
		assert.Equal(t, `{"ok":true,"items":1}`, result.Messages[5].ToolCalls[0].ResultEvents[0].Content)
		assert.True(t, result.Messages[8].IsCompactBoundary)
		assert.Equal(t, "2026-10-05T15:06:00Z", result.Messages[8].Timestamp.Format("2006-01-02T15:04:05Z07:00"))
		for _, msg := range result.Messages {
			assert.Empty(t, msg.ToolResults)
			assert.Empty(t, msg.Model)
		}
	}
}

func TestMusePersonalHeaderFieldsMatchExactJSONNames(t *testing.T) {
	p, path := musePersonalTestProvider(t, "")
	header := strings.Replace(musePersonalTestHeader, `"type"`, `"Type"`, 1)
	require.NoError(t, os.WriteFile(path, []byte(header+musePersonalTestItem(0, `{"type":"message","role":"user","text":"hello"}`)), 0o600))

	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	assert.Empty(t, sources)
}

const (
	musePersonalTestID     = "11111111-2222-4333-8444-555555555555"
	musePersonalTestHeader = `{"type":"session_header","version":1,"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z"}` + "\n"
)

func musePersonalTestProvider(t *testing.T, body string) (Provider, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "agent-66666666-7777-4888-8999-000000000000", "sessions", musePersonalTestID+".jsonl")
	writeSourceFile(t, path, musePersonalTestHeader+body)
	p, ok := NewProvider(AgentType("muse-personal"), ProviderConfig{Roots: []string{root}})
	require.True(t, ok)
	return p, path
}

func musePersonalTestItem(seq int, item string) string {
	return fmt.Sprintf(`{"type":"item","seq":%d,"source":"runtime.future","created_at":"2026-10-05T15:04:06Z","item":%s}`+"\n", seq, item)
}

func musePersonalTestParse(t *testing.T, p Provider) ParseOutcome {
	t.Helper()
	sources, err := p.Discover(t.Context())
	require.NoError(t, err)
	require.Len(t, sources, 1)
	out, err := p.Parse(t.Context(), ParseRequest{Source: sources[0]})
	require.NoError(t, err)
	return out
}

func TestMusePersonalOccurrencePairing(t *testing.T) {
	call := `{"type":"function_call","call_id":"same","name":"exec","arguments":"{\"x\":1}"}`
	output := func(text string) string {
		return fmt.Sprintf(`{"type":"function_call_output","call_id":"same","output":%q,"success":true}`, text)
	}
	p, _ := musePersonalTestProvider(t,
		musePersonalTestItem(0, output("before"))+
			musePersonalTestItem(1, call)+musePersonalTestItem(2, output("first"))+
			musePersonalTestItem(3, call)+musePersonalTestItem(4, output("second"))+
			musePersonalTestItem(5, call)+musePersonalTestItem(6, call)+musePersonalTestItem(7, output("ambiguous")))
	out := musePersonalTestParse(t, p)
	require.Len(t, out.Results, 1)
	msgs := out.Results[0].Result.Messages
	require.Len(t, msgs, 6)
	assert.Equal(t, RoleTool, msgs[0].Role)
	assert.Equal(t, "before", msgs[0].Content)
	assert.Equal(t, "first", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "second", msgs[2].ToolCalls[0].ResultEvents[0].Content)
	assert.Empty(t, msgs[3].ToolCalls[0].ResultEvents)
	assert.Empty(t, msgs[4].ToolCalls[0].ResultEvents)
	assert.Equal(t, RoleTool, msgs[5].Role)
	assert.Equal(t, "ambiguous", msgs[5].Content)
	for _, m := range msgs {
		assert.Empty(t, m.ToolResults)
	}
}

func TestMusePersonalReplayAndSequenceOrder(t *testing.T) {
	one := musePersonalTestItem(9, `{"type":"message","role":"user","text":"first"}`)
	different := musePersonalTestItem(9, `{"type":"message","role":"assistant","text":"different"}`)
	checkpoint := `{"type":"compaction_checkpoint","checkpoint_seq":0,"created_at":1791212760,"summary":"summary","details_json":"hidden replacement"}` + "\n"
	p, _ := musePersonalTestProvider(t, one+one+checkpoint+different+musePersonalTestItem(1, `{"type":"future","text":"SECRET","output":"SECRET"}`))
	out := musePersonalTestParse(t, p)
	msgs := out.Results[0].Result.Messages
	require.Len(t, msgs, 4)
	assert.Equal(t, "first", msgs[1].Content)
	assert.True(t, msgs[2].IsCompactBoundary)
	assert.Equal(t, "different", msgs[3].Content)
	assert.NotEqual(t, msgs[1].SourceUUID, msgs[3].SourceUUID)
	assert.Contains(t, msgs[1].SourceUUID, "item:9:")
	assert.Equal(t, "runtime.future", msgs[0].SourceType)
	assert.Equal(t, "unsupported", msgs[0].SourceSubtype)
	assert.NotContains(t, msgs[0].Content, "SECRET")
	assert.NotContains(t, msgs[2].Content, "hidden replacement")
}

func TestMusePersonalDefersUnterminatedTail(t *testing.T) {
	for _, tail := range []string{`{"type":"item"`, strings.TrimSuffix(musePersonalTestItem(1, `{"type":"message","role":"assistant","text":"complete JSON but no newline"}`), "\n")} {
		p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"prefix"}`)+tail)
		out := musePersonalTestParse(t, p)
		assert.Empty(t, out.Results)
		assert.False(t, out.ResultSetComplete)
		assert.False(t, out.ForceReplace)
		writeSourceFile(t, path, musePersonalTestHeader+musePersonalTestItem(0, `{"type":"message","role":"user","text":"replacement"}`))
		out = musePersonalTestParse(t, p)
		require.Len(t, out.Results, 1)
		require.Len(t, out.Results[0].Result.Messages, 1)
		assert.Equal(t, "replacement", out.Results[0].Result.Messages[0].Content)
	}
}

func TestMusePersonalAdvisoryIndex(t *testing.T) {
	for _, tc := range []struct{ name, index, want string }{
		{"missing", "", "2026-10-05T15:04:06Z"},
		{"invalid", "{", "2026-10-05T15:04:06Z"},
		{"unknown version", `{"version":2,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`, "2026-10-05T15:04:06Z"},
		{"matched", `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z","item_count":999999,"transcript_path":"../../secret"}]}`, "2026-10-05T16:00:00Z"},
		{"stale", `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T15:04:05Z"}]}`, "2026-10-05T15:04:06Z"},
		{"wrong agent", `{"version":1,"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z"}]}`, "2026-10-05T15:04:06Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, path := musePersonalTestProvider(t, musePersonalTestItem(0, `{"type":"message","role":"user","text":"hello"}`))
			if tc.index != "" {
				writeSourceFile(t, filepath.Join(filepath.Dir(path), "sessions.json"), tc.index)
			}
			out := musePersonalTestParse(t, p)
			require.Len(t, out.Results, 1)
			sess := out.Results[0].Result.Session
			assert.Equal(t, tc.want, sess.EndedAt.Format(time.RFC3339))
			assert.Equal(t, 1, sess.MessageCount)
		})
	}
}

func TestMusePersonalOrdersItemsBySequenceBeforePairing(t *testing.T) {
	p, _ := musePersonalTestProvider(t,
		musePersonalTestItem(0, `{"type":"message","role":"user","text":"start"}`)+
			musePersonalTestItem(2, `{"type":"function_call_output","call_id":"out-of-order","output":"result without metadata","success":false}`)+
			musePersonalTestItem(1, `{"type":"function_call","call_id":"out-of-order","name":"exec","arguments":"{}"}`)+
			`{"type":"compaction_checkpoint","checkpoint_seq":0,"first_kept_seq":1,"created_at":1791212760,"summary":"boundary"}`+"\n"+
			musePersonalTestItem(3, `{"type":"message","role":"assistant","text":"done"}`))
	out := musePersonalTestParse(t, p)
	msgs := out.Results[0].Result.Messages
	require.Len(t, msgs, 4)
	assert.Equal(t, "start", msgs[0].Content)
	require.Len(t, msgs[1].ToolCalls, 1)
	require.Len(t, msgs[1].ToolCalls[0].ResultEvents, 1)
	assert.Equal(t, "result without metadata", msgs[1].ToolCalls[0].ResultEvents[0].Content)
	assert.Equal(t, "error", msgs[1].ToolCalls[0].ResultEvents[0].Status)
	assert.True(t, msgs[2].IsCompactBoundary)
	assert.Equal(t, "done", msgs[3].Content)
}

func TestMusePersonalLateSignedThinkingFixture(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "muse_personal", "seq-inversion.jsonl"))
	require.NoError(t, err)
	p, path := musePersonalTestProvider(t, "")
	writeSourceFile(t, path, string(body))
	out := musePersonalTestParse(t, p)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	assert.Zero(t, r.Session.MalformedLines)
	assert.False(t, r.Session.IsTruncated)
	require.Len(t, r.Messages, 4)
	assert.Equal(t, "start", r.Messages[0].Content)
	assert.Equal(t, "consider", r.Messages[1].ThinkingText)
	assert.Equal(t, "2026-10-05T15:04:07.074Z", r.Messages[1].Timestamp.Format(time.RFC3339Nano))
	assert.Equal(t, "2026-10-05T15:04:07Z", r.Messages[2].Timestamp.Format(time.RFC3339Nano))
	assert.Equal(t, "commentary", r.Messages[2].SourceSubtype)
	assert.Equal(t, "working", r.Messages[2].Content)
	assert.Equal(t, "done", r.Messages[3].Content)
	for i, m := range r.Messages {
		assert.Contains(t, m.SourceUUID, fmt.Sprintf(":item:%d:1", i))
	}
}

func TestMusePersonalIgnoresUnknownIndexKeys(t *testing.T) {
	p, path := musePersonalTestProvider(t,
		musePersonalTestItem(0, `{"type":"message","role":"user","text":"hello"}`))
	writeSourceFile(t, filepath.Join(filepath.Dir(path), "sessions.json"),
		`{"version":1,"future":{"v":true},"sessions":[{"session_id":"11111111-2222-4333-8444-555555555555","agent_id":"66666666-7777-4888-8999-000000000000","created_at":"2026-10-05T15:04:05Z","updated_at":"2026-10-05T16:00:00Z","context_window_usage":{"measured_envelope":{"tool_fingerprints":["opaque-tool"]}},"future_row":42}]}`)
	out := musePersonalTestParse(t, p)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	assert.Equal(t, "2026-10-05T16:00:00Z", r.Session.EndedAt.Format(time.RFC3339))
	assert.Empty(t, r.Session.UsageEvents)
	assert.Empty(t, r.Session.SessionName)
	require.Len(t, r.Messages, 1)
	assert.Equal(t, "hello", r.Messages[0].Content)
	assert.Empty(t, r.Messages[0].Model)
}

func TestMusePersonalUnsupportedKeepsTypeAndSource(t *testing.T) {
	p, _ := musePersonalTestProvider(t, musePersonalTestItem(42,
		`{"type":"future_widget","text":"opaque-sentinel-body"}`))
	out := musePersonalTestParse(t, p)
	require.Len(t, out.Results, 1)
	require.Len(t, out.Results[0].Result.Messages, 1)
	msg := out.Results[0].Result.Messages[0]
	assert.Contains(t, msg.Content, "future_widget")
	assert.NotContains(t, msg.Content, "opaque-sentinel-body")
	assert.Equal(t, "runtime.future", msg.SourceType)
	assert.Contains(t, msg.SourceUUID, ":item:42:1")
}

func TestMusePersonalUnknownDiscriminators(t *testing.T) {
	for _, tc := range []struct{ name, record, want string }{
		{"record", `{"type":"future_record","source":"runtime.future","created_at":"2026-10-05T15:04:06Z","text":"opaque-sentinel-body"}` + "\n", "future_record"},
		{"role", musePersonalTestItem(0, `{"type":"message","role":"future_role","text":"opaque-sentinel-body"}`), "future_role"},
		{"part", musePersonalTestItem(0, `{"type":"message_parts","role":"user","parts":[{"type":"future_part","text":"opaque-sentinel-body"}]}`), "future_part"},
		{"delimiter", musePersonalTestItem(0, `{"type":"future_\n]body","text":"opaque-sentinel-body"}`), "future___body"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := musePersonalTestProvider(t, tc.record)
			out := musePersonalTestParse(t, p)
			require.Len(t, out.Results, 1)
			require.Len(t, out.Results[0].Result.Messages, 1)
			msg := out.Results[0].Result.Messages[0]
			assert.Contains(t, msg.Content, tc.want)
			assert.NotContains(t, msg.Content, "opaque-sentinel-body")
			assert.NotContains(t, msg.Content, "\n")
			assert.LessOrEqual(t, len(msg.Content), 160)
			assert.Equal(t, "runtime.future", msg.SourceType)
		})
	}
}

func TestMusePersonalNullTimestampsAreMalformed(t *testing.T) {
	p, _ := musePersonalTestProvider(t,
		`{"type":"item","seq":0,"created_at":null,"item":{"type":"message","role":"user","text":"invalid"}}`+"\n"+
			`{"type":"compaction_checkpoint","checkpoint_seq":1,"created_at":null,"summary":"invalid"}`+"\n"+
			musePersonalTestItem(1, `{"type":"message","role":"user","text":"valid"}`))
	out := musePersonalTestParse(t, p)
	require.Len(t, out.Results, 1)
	r := out.Results[0].Result
	assert.Equal(t, 2, r.Session.MalformedLines)
	require.Len(t, r.Messages, 1)
	assert.Equal(t, "valid", r.Messages[0].Content)
}

func TestMusePersonalFutureBodiesStayOpaque(t *testing.T) {
	for _, tc := range []struct{ name, record, want string }{
		{"item", musePersonalTestItem(0, `{"type":"future_item","text":{},"role":[],"parts":false}`), "future_item"},
		{"role", musePersonalTestItem(0, `{"type":"message","role":"future_role","text":{}}`), "future_role"},
		{"part", musePersonalTestItem(0, `{"type":"message_parts","role":"user","parts":[{"type":"future_part","text":{},"filename":[]}]}`), "future_part"},
		{"record", `{"type":"future_record","created_at":"2026-10-05T15:04:06Z","source":"runtime.future","item":false,"summary":{}}` + "\n", "future_record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := musePersonalTestProvider(t, tc.record)
			out := musePersonalTestParse(t, p)
			require.Len(t, out.Results, 1)
			r := out.Results[0].Result
			assert.Zero(t, r.Session.MalformedLines)
			require.Len(t, r.Messages, 1)
			assert.Contains(t, r.Messages[0].Content, tc.want)
			assert.Equal(t, "runtime.future", r.Messages[0].SourceType)
		})
	}
}
