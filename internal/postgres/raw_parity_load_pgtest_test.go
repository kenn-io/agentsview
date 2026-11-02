//go:build pgtest

package postgres

import (
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/money"
	"hegel.dev/go/hegel"
)

func parityTx(t testing.TB, pg *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := pg.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}

// A public reader's visibility filter or transcript cap must not affect parity.
// Sparse call indexes also catch attaching results by slice position.
func TestLoadRawParitySnapshotsCompletePhysicalRows(t *testing.T) {
	f := newProjectionFixture(t)
	m, _ := f.accept(t, "device-a", "capture-a", "")
	require.NoError(t, f.sink.Project(t.Context(), f.lease(t, m), m, projectionOutcome("raw content")))
	raw, err := f.sink.Resolve(t.Context(), "codex:portable")
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `
		INSERT INTO sessions(id,project,machine,agent,cwd,first_message,started_at,ended_at,
		 message_count,user_message_count,parent_session_id,relationship_type,deleted_at)
		VALUES ('legacy','sample','workstation','codex','/project','hello','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z',1007,1,'parent','subagent',NULL),
		 ('trashed','sample','workstation','codex','','',NULL,NULL,0,0,NULL,'',now());
		INSERT INTO messages(session_id,ordinal,role,content,source_uuid,model,is_sidechain,is_system)
		 SELECT 'legacy',n,'assistant','message-'||n,'uuid-'||n,'sample-model',true,true FROM generate_series(0,1006) n;
		INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category,input_json,file_path,result_content_length)
		 VALUES ('legacy',1006,7,'Read','Read','{"path":"example"}','example',4);
		INSERT INTO tool_result_events(session_id,tool_call_message_ordinal,call_index,source,status,content,content_length,timestamp,event_index,subagent_session_id)
		 VALUES ('legacy',1006,7,'tool','success','done',4,'2026-01-01 01:00:00+01',9,'child');
		INSERT INTO usage_events(session_id,message_ordinal,source,model,provider_id,input_tokens,output_tokens,
		 cache_creation_input_tokens,cache_read_input_tokens,reasoning_tokens,cost_microdollars,cost_status,cost_source,occurred_at,dedup_key)
		 VALUES ('legacy',1006,'api','sample-model','provider',11,7,3,5,2,19,'known','provider','2026-01-01T00:00:00Z','usage-1'),
		 ('legacy',NULL,'session','sample-model','',0,0,0,0,0,NULL,'unknown','',NULL,'');
		INSERT INTO secret_findings(session_id,rule_name,confidence,location_kind,message_ordinal,call_index,event_index,
		 match_start,match_end,match_index,redacted_match,rules_version)
		 VALUES ('legacy','token','definite','tool_result_event',1006,7,9,1,3,2,'[redacted]','rules-1');
		INSERT INTO friction_findings(session_id,kind,detector,message_ordinal,call_index,tool_name,label,text,evidence,title,fingerprint,occurred_at,seq,rules_version)
		 VALUES ('legacy','error','tool',1006,7,'Read','failure','failed','evidence','masked','masked','2026-01-01T00:00:00Z',3,'rules-1')`)
	require.NoError(t, err)
	tx := parityTx(t, f.runtime)
	got, err := LoadRawParitySnapshots(t.Context(), tx, []string{"legacy", raw.SessionID, "trashed", "absent", "legacy"})
	require.NoError(t, err)
	require.Len(t, got, 3)
	s := got["legacy"]
	assert.Equal(t, db.Session{ID: "legacy", Agent: "codex", Project: "sample", Cwd: "/project", FirstMessage: new("hello"),
		StartedAt: new("2026-01-01T00:00:00Z"), EndedAt: new("2026-01-02T00:00:00Z"), MessageCount: 1007,
		UserMessageCount: 1, ParentSessionID: new("parent"), RelationshipType: "subagent"}, s.Session)
	require.Len(t, s.Messages, 1007)
	last := s.Messages[1006]
	assert.Equal(t, "message-1006", last.Content)
	assert.Equal(t, "uuid-1006", last.SourceUUID)
	assert.True(t, last.IsSidechain)
	assert.True(t, last.IsSystem)
	require.Len(t, last.ToolCalls, 1)
	assert.Equal(t, db.ToolCall{CallIndex: 7, ToolName: "Read", Category: "Read", InputJSON: `{"path":"example"}`,
		FilePath: "example", ResultContentLength: 4, ResultContent: "done", ResultEvents: []db.ToolResultEvent{{
			Source: "tool", Status: "success", Content: "done", ContentLength: 4, Timestamp: "2026-01-01T00:00:00Z", EventIndex: 9, SubagentSessionID: "child",
		}}}, last.ToolCalls[0])
	assert.Equal(t, []db.UsageEvent{{MessageOrdinal: new(1006), Source: "api", Model: "sample-model", ProviderID: "provider",
		InputTokens: 11, OutputTokens: 7, CacheCreationInputTokens: 3, CacheReadInputTokens: 5, ReasoningTokens: 2,
		Cost: &money.Money{Microdollars: 19}, CostStatus: "known", CostSource: "provider", OccurredAt: "2026-01-01T00:00:00Z", DedupKey: "usage-1"},
		{Source: "session", Model: "sample-model", CostStatus: "unknown"}}, s.Usage)
	assert.Equal(t, []db.SecretFinding{{RuleName: "token", Confidence: "definite", LocationKind: "tool_result_event", MessageOrdinal: 1006,
		CallIndex: new(7), EventIndex: new(9), MatchStart: 1, MatchEnd: 3, MatchIndex: 2, RedactedMatch: "[redacted]"}}, s.Secrets)
	assert.Equal(t, []db.FrictionFinding{{Kind: "error", Detector: "tool", MessageOrdinal: new(1006), CallIndex: new(7), ToolName: "Read",
		Label: "failure", Text: "failed", Evidence: "evidence", OccurredAt: new(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), Seq: 3}}, s.Friction)
	assert.Equal(t, "raw content", got[raw.SessionID].Messages[0].Content)
	assert.Equal(t, "trashed", got["trashed"].Session.ID)
	empty, err := LoadRawParitySnapshots(t.Context(), tx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// The second load must use the caller's snapshot even after a writer commits.
func TestLoadRawParitySnapshotsUsesCallerTransaction(t *testing.T) {
	f := newHostedFixture(t, "tenant-parity")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('session','sample','device','codex');
	 INSERT INTO messages(session_id,ordinal,role,content) VALUES('session',0,'user','before')`)
	require.NoError(t, err)
	tx := parityTx(t, f.runtime)
	before, err := LoadRawParitySnapshots(t.Context(), tx, []string{"session"})
	require.NoError(t, err)
	_, err = f.runtime.ExecContext(t.Context(), `UPDATE messages SET content='after' WHERE session_id='session'`)
	require.NoError(t, err)
	after, err := LoadRawParitySnapshots(t.Context(), tx, []string{"session"})
	require.NoError(t, err)
	assert.Equal(t, before, after)
	require.NoError(t, tx.Commit())
	after, err = LoadRawParitySnapshots(t.Context(), parityTx(t, f.runtime), []string{"session"})
	require.NoError(t, err)
	assert.Equal(t, "after", after["session"].Messages[0].Content)
}

// Malformed usage is stored TEXT. Public transcript readers may hide it, but
// parity must preserve it so different malformed payloads cannot compare equal.
func TestLoadRawParitySnapshotsPreservesMalformedUsage(t *testing.T) {
	f := newHostedFixture(t, "tenant-parity")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('malformed','sample','device','codex');
	 INSERT INTO messages(session_id,ordinal,role,content,token_usage) VALUES('malformed',0,'user','hello','{broken')`)
	require.NoError(t, err)
	got, err := LoadRawParitySnapshots(t.Context(), parityTx(t, f.runtime), []string{"malformed"})
	require.NoError(t, err)
	require.Len(t, got["malformed"].Messages, 1)
	assert.Equal(t, "{broken", string(got["malformed"].Messages[0].TokenUsage))
}

func TestLoadRawParitySnapshotsRejectsUnsafeTransactions(t *testing.T) {
	f := newHostedFixture(t, "tenant-parity")
	for _, options := range []sql.TxOptions{{}, {Isolation: sql.LevelRepeatableRead}, {ReadOnly: true}} {
		tx, err := f.runtime.BeginTx(t.Context(), &options)
		require.NoError(t, err)
		_, err = LoadRawParitySnapshots(t.Context(), tx, nil)
		assert.ErrorContains(t, err, "REPEATABLE READ READ ONLY")
		require.NoError(t, tx.Rollback())
	}
	_, err := LoadRawParitySnapshots(t.Context(), nil, nil)
	assert.Error(t, err)
	tx := parityTx(t, f.runtime)
	require.NoError(t, tx.Rollback())
	_, err = LoadRawParitySnapshots(t.Context(), tx, nil)
	assert.ErrorIs(t, err, sql.ErrTxDone)
}

func TestLoadRawParitySnapshotsUncappedChildren(t *testing.T) {
	f := newHostedFixture(t, "tenant-parity")
	_, err := f.runtime.ExecContext(t.Context(), `
	 INSERT INTO sessions(id,project,machine,agent) VALUES('large','sample','device','codex');
	 INSERT INTO messages(session_id,ordinal,role,content) VALUES('large',0,'assistant','hello');
	 INSERT INTO tool_calls(session_id,message_ordinal,call_index,tool_name,category)
	 SELECT 'large',0,n,'Read','Read' FROM generate_series(0,1006) n;
	 INSERT INTO tool_result_events(session_id,tool_call_message_ordinal,call_index,event_index,source,status,content)
	 SELECT 'large',0,n,0,'tool','success','result-'||n FROM generate_series(0,1006) n;
	 INSERT INTO usage_events(session_id,source,model,input_tokens) SELECT 'large','api','model',n FROM generate_series(0,1006) n;
	 INSERT INTO secret_findings(session_id,rule_name,confidence,location_kind,message_ordinal,match_start,match_end,match_index,redacted_match,rules_version)
	 SELECT 'large','token','definite','message',0,n,n+1,n,'[redacted]','rules-1' FROM generate_series(0,1006) n;
	 INSERT INTO friction_findings(session_id,kind,detector,title,fingerprint,seq,rules_version)
	 SELECT 'large','error','tool','masked','masked',n,'rules-1' FROM generate_series(0,1006) n`)
	require.NoError(t, err)
	got, err := LoadRawParitySnapshots(t.Context(), parityTx(t, f.runtime), []string{"large"})
	require.NoError(t, err)
	s := got["large"]
	require.Len(t, s.Messages, 1)
	require.Len(t, s.Messages[0].ToolCalls, 1007)
	for n, c := range s.Messages[0].ToolCalls {
		require.Len(t, c.ResultEvents, 1)
		assert.Equal(t, n, c.CallIndex)
		assert.Equal(t, "result-"+strconv.Itoa(n), c.ResultEvents[0].Content)
	}
	require.Len(t, s.Usage, 1007)
	assert.Equal(t, 1006, s.Usage[1006].InputTokens)
	require.Len(t, s.Secrets, 1007)
	assert.Equal(t, 1006, s.Secrets[1006].MatchIndex)
	require.Len(t, s.Friction, 1007)
	assert.Equal(t, 1006, s.Friction[1006].Seq)
}

// Hegel varies content and accounting independently of generated physical IDs.
// PostgreSQL text cannot store NUL; retain every other generated character.
func TestLoadRawParitySnapshotsContentProperty(t *testing.T) {
	requireHegel(t)
	f := newHostedFixture(t, "tenant-parity")
	_, err := f.runtime.ExecContext(t.Context(), `INSERT INTO sessions(id,project,machine,agent) VALUES('property','sample','device','codex');
	 INSERT INTO messages(session_id,ordinal,role,content) VALUES('property',0,'user','')`)
	require.NoError(t, err)
	hegel.Test(t, func(ht *hegel.T) {
		content := strings.ReplaceAll(hegel.Draw(ht, hegel.Text()), "\x00", "\uFFFD")
		usageText := strings.ReplaceAll(hegel.Draw(ht, hegel.Text()), "\x00", "\uFFFD")
		tokens := int(hegel.Draw(ht, hegel.Integers(math.MinInt32, math.MaxInt32)))
		validUsage, err := json.Marshal(map[string]string{"value": usageText})
		require.NoError(ht, err)
		// A leading '!' is invalid JSON for every generated suffix.
		usage := [][]byte{nil, validUsage, []byte("!" + usageText)}[hegel.Draw(ht, hegel.Integers(0, 2))]
		_, err = f.runtime.ExecContext(t.Context(), `UPDATE messages SET content=$1,output_tokens=$2,token_usage=$3 WHERE session_id='property'`, content, tokens, string(usage))
		require.NoError(ht, err)
		tx, err := f.runtime.BeginTx(t.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
		require.NoError(ht, err)
		defer func() { _ = tx.Rollback() }()
		got, err := LoadRawParitySnapshots(t.Context(), tx, []string{"property"})
		require.NoError(ht, err)
		require.Len(ht, got["property"].Messages, 1)
		message := got["property"].Messages[0]
		assert.Equal(ht, content, message.Content)
		assert.Equal(ht, usage, []byte(message.TokenUsage))
		assert.Equal(ht, tokens, message.OutputTokens)
	}, hegel.WithTestCases(1000))
}
