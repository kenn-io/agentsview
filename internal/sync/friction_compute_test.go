package sync

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestFrictionRawInput(t *testing.T) {
	msgs := []db.Message{
		{
			Ordinal: 0, Role: "user", Content: "please fix it",
			Timestamp: "2026-09-16T01:00:00Z",
		},
		{
			Ordinal: 2, Role: "assistant", Content: "[Bash] make test",
			ThinkingText: "hmm", Timestamp: "2026-09-16T01:00:01.5Z",
			ContextTokens: 1200, HasContextTokens: true,
			ToolCalls: []db.ToolCall{
				{
					ToolName: "Bash", Category: "Bash",
					InputJSON: `{"command":"make test"}`, ResultContent: "old",
					ResultEvents: []db.ToolResultEvent{
						{Status: "running", Content: "partial"},
						{Status: "errored", Content: "boom"},
					},
				},
				{
					ToolName: "Read", Category: "Read", InputJSON: `{}`,
					ResultContent: "file body",
				},
			},
		},
		{
			Ordinal: 4, Role: "user", Content: "[Request interrupted by user]",
			IsSystem: true, SourceSubtype: "interrupted",
			Timestamp: "2026-09-16T01:00:04Z",
		},
		{
			Ordinal: 5, Role: "assistant", Content: "summary", IsSystem: true,
			IsCompactBoundary: true, SourceSubtype: "compact_boundary",
			Timestamp: "not a time",
		},
	}
	raw, calls := frictionRawInput(msgs)
	require.Len(t, raw, 4, "every stored row is handed to the adapter")
	assert.Equal(t, 2, raw[1].Ordinal)
	assert.Equal(t, "hmm", raw[1].ThinkingText)
	assert.Equal(t, 1200, raw[1].ContextTokens)
	assert.True(t, raw[1].HasContextTokens)
	assert.True(t, raw[2].IsSystem)
	assert.Equal(t, "interrupted", raw[2].SourceSubtype)
	assert.True(t, raw[3].IsCompactBoundary)
	assert.True(t, raw[3].Timestamp.IsZero(), "unparseable timestamps map to zero")
	msgTime := time.Date(2026, 9, 16, 1, 0, 1, 500000000, time.UTC)
	require.Len(t, calls, 2)
	assert.Equal(t, friction.RawToolCall{
		MessageOrdinal: 2, CallIndex: 0, ToolName: "Bash", Category: "Bash",
		InputJSON: `{"command":"make test"}`, ResultContent: "old",
		LastEventContent: "boom", EventStatus: "errored",
		Timestamp: msgTime,
	}, calls[0])
	assert.Equal(t, 1, calls[1].CallIndex)
	assert.Empty(t, calls[1].EventStatus)
	assert.Equal(t, msgTime, calls[1].Timestamp,
		"calls take the owning message timestamp")
}

func TestFrictionIsSubAgent(t *testing.T) {
	tests := []struct {
		name string
		s    db.Session
		want bool
	}{
		{"root", db.Session{}, false},
		{"subagent", db.Session{ParentSessionID: new("p"), RelationshipType: "subagent"}, true},
		{"continuation", db.Session{ParentSessionID: new("p"), RelationshipType: "continuation"}, false},
		{"empty parent", db.Session{ParentSessionID: new(""), RelationshipType: "subagent"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, frictionIsSubAgent(tt.s))
		})
	}
}

func frictionTestMessages(sessionID string) []db.Message {
	return []db.Message{
		{
			SessionID: sessionID, Ordinal: 0, Role: "user", Content: "please fix the failing build",
			Timestamp: "2026-09-16T01:00:00Z",
		},
		{
			SessionID: sessionID, Ordinal: 1, Role: "assistant", Content: "Sure, I'll hardcode the path for now.",
			Timestamp: "2026-09-16T01:00:01Z",
		},
		{
			SessionID: sessionID, Ordinal: 2, Role: "user", Content: "no, use the config file instead",
			Timestamp: "2026-09-16T01:00:02Z",
		},
		{
			SessionID: sessionID, Ordinal: 3, Role: "assistant", Content: "Understood.",
			Timestamp: "2026-09-16T01:00:03Z",
		},
		{
			SessionID: sessionID, Ordinal: 4, Role: "user", Content: "this is broken again, same error!!!",
			Timestamp: "2026-09-16T01:00:04Z",
		},
		{
			SessionID: sessionID, Ordinal: 5, Role: "user", Content: "[Request interrupted by user]",
			IsSystem: true, SourceSubtype: "interrupted",
			Timestamp: "2026-09-16T01:00:05Z",
		},
	}
}

func TestComputeSessionFriction(t *testing.T) {
	s := db.Session{ID: "s1", Agent: "claude", Machine: "local"}
	u, err := computeSessionFriction(t.Context(), s, frictionTestMessages("s1"), nil, frictionOptions{})
	require.NoError(t, err)
	require.Len(t, u.Findings, 4)
	assert.Equal(t, friction.RulesVersion, u.RulesVersion)
	assert.Nil(t, u.Dims, "no dims hook in PR 3, so no dims row")

	var kinds []string
	for i, f := range u.Findings {
		kinds = append(kinds, f.Kind+":"+f.Detector)
		assert.Equal(t, i, f.Seq)
		assert.Equal(t, "s1", f.SessionID)
		assert.Equal(t, friction.RulesVersion, f.RulesVersion)
		assert.Len(t, f.Fingerprint, len("fl1:")+64)
	}
	assert.Equal(t, []string{
		"correction:correction.coding",
		"workaround:workaround",
		"frustration:frustration",
		"interruption:interruption",
	}, kinds, "spec §6.8 run order; frustration and interruption persist")
	assert.Equal(t, db.FrictionHash(u.Findings, u.Dims, friction.RulesVersion), u.Hash)
	require.NotNil(t, u.Findings[0].MessageOrdinal)
	assert.Equal(t, 2, *u.Findings[0].MessageOrdinal)
	assert.Equal(t, "no, use the config file instead", u.Findings[0].Text)
	require.NotNil(t, u.Findings[3].MessageOrdinal)
	assert.Equal(t, 5, *u.Findings[3].MessageOrdinal)
	assert.Empty(t, u.Findings[3].Text, "interruption findings carry no text")
}

func TestComputeSessionFrictionDimsHook(t *testing.T) {
	tests := []struct {
		name      string
		hook      FrictionDimsFunc
		wantDims  *db.FrictionSessionDims
		wantEmpty bool
	}{
		{
			name: "seat pattern",
			hook: func(context.Context, db.Session) (db.FrictionSessionDims, bool) {
				return db.FrictionSessionDims{Seat: "seat-02", DimsSource: "seat_pattern"}, true
			},
			wantDims: &db.FrictionSessionDims{SessionID: "s1", Seat: "seat-02", DimsSource: "seat_pattern"},
		},
		{
			name: "excluded keeps the row and drops findings",
			hook: func(context.Context, db.Session) (db.FrictionSessionDims, bool) {
				return db.FrictionSessionDims{
					Persona: "helper", Channel: "general",
					DimsSource: "nanoclaw", ReviewExcluded: true,
				}, true
			},
			wantDims: &db.FrictionSessionDims{
				SessionID: "s1", Persona: "helper", Channel: "general",
				DimsSource: "nanoclaw", ReviewExcluded: true,
			},
			wantEmpty: true,
		},
		{
			name: "hook declines",
			hook: func(context.Context, db.Session) (db.FrictionSessionDims, bool) {
				return db.FrictionSessionDims{}, false
			},
		},
		{
			name: "all-default answer writes no row",
			hook: func(context.Context, db.Session) (db.FrictionSessionDims, bool) {
				return db.FrictionSessionDims{}, true
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := db.Session{ID: "s1", Agent: "claude"}
			u, err := computeSessionFriction(t.Context(), s, frictionTestMessages("s1"), nil,
				frictionOptions{dims: tt.hook})
			require.NoError(t, err)
			assert.Equal(t, tt.wantDims, u.Dims)
			if tt.wantEmpty {
				assert.Empty(t, u.Findings, "review_excluded sessions get no findings")
			} else {
				assert.NotEmpty(t, u.Findings)
			}
		})
	}
}

func TestComputeSessionFrictionRecoversPanic(t *testing.T) {
	s := db.Session{ID: "s1"}
	_, err := computeSessionFriction(t.Context(), s, frictionTestMessages("s1"), nil,
		frictionOptions{review: func(friction.SessionInput) []friction.Signal {
			panic("detector bug")
		}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detector bug")
}

func writeFrictionClaudeSession(t *testing.T, dir, name string, extra ...string) string {
	t.Helper()
	b := testjsonl.NewSessionBuilder().
		AddClaudeUser("2026-09-16T01:00:00Z", "please fix the failing build").
		AddClaudeAssistant("2026-09-16T01:00:01Z", "Sure, I'll hardcode the path for now.").
		AddClaudeUser("2026-09-16T01:00:02Z", "no, use the config file instead").
		AddClaudeAssistant("2026-09-16T01:00:03Z", "Understood.")
	for _, line := range extra {
		b = b.AddRaw(line)
	}
	path := filepath.Join(dir, "proj", name+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o644))
	return path
}

func frictionKinds(t *testing.T, d *db.DB, id string) []string {
	t.Helper()
	got, err := d.SessionFrictionFindings(t.Context(), id)
	require.NoError(t, err)
	kinds := make([]string, 0, len(got))
	for _, f := range got {
		kinds = append(kinds, f.Kind+":"+f.Detector)
	}
	return kinds
}

func TestSyncPersistsFrictionFindings(t *testing.T) {
	fx := newEngineFixture(t)
	path := writeFrictionClaudeSession(t, fx.claudeDir, "friction-a",
		testjsonl.ClaudeAssistantJSON([]map[string]any{{
			"type": "tool_use", "id": "toolu_1", "name": "Bash",
			"input": map[string]string{"command": "go test ./..."},
		}}, "2026-09-16T01:00:04Z"),
		testjsonl.ClaudeToolResultUserJSON("toolu_1",
			"bash: go: command not found", "2026-09-16T01:00:05Z"),
	)
	stats := fx.engine.SyncAll(t.Context(), nil)
	require.NotZero(t, stats.Synced)
	id := fx.sessionIDFor(t, path)

	assert.Equal(t, []string{
		"correction:correction.coding",
		"error:error",
		"workaround:workaround",
	}, frictionKinds(t, fx.db, id))
	s, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, 3, s.FrictionCount)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
	assert.NotEmpty(t, s.FrictionHash)
}

func TestFrictionRefreshesAfterIncrementalAppend(t *testing.T) {
	fx := newEngineFixture(t)
	path := writeFrictionClaudeSession(t, fx.claudeDir, "friction-grow")
	fx.engine.SyncAll(t.Context(), nil)
	id := fx.sessionIDFor(t, path)
	before := frictionKinds(t, fx.db, id)

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	for _, line := range []string{
		testjsonl.ClaudeUserJSON("stop adding comments to every line", "2026-09-16T01:00:10Z"),
		testjsonl.ClaudeAssistantJSON("Leaving the rest for later; next session I will finish.", "2026-09-16T01:00:11Z"),
	} {
		_, err = f.WriteString(line + "\n")
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())
	fx.engine.SyncAll(t.Context(), nil)
	fx.engine.FlushSignals()

	after := frictionKinds(t, fx.db, id)
	assert.Len(t, after, len(before)+2, "one new correction and one deferral")
	assert.Contains(t, after, "deferral:deferral")
	s, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, friction.RulesVersion, s.FrictionRulesVersion)
}

func TestFrictionComputePanicLeavesSessionRetryable(t *testing.T) {
	fx := newEngineFixture(t)
	bad := writeFrictionClaudeSession(t, fx.claudeDir, "friction-bad")
	good := writeFrictionClaudeSession(t, fx.claudeDir, "friction-good")
	badID, goodID := fx.sessionIDFor(t, bad), fx.sessionIDFor(t, good)
	fx.engine.frictionReviewHook = func(in friction.SessionInput) []friction.Signal {
		if in.SubjectID == badID {
			panic("detector bug")
		}
		return friction.Review(in)
	}

	stats := fx.engine.SyncAll(t.Context(), nil)
	assert.Equal(t, 2, stats.Synced, "a friction failure never fails the sync")

	badSess, err := fx.db.GetSessionFull(t.Context(), badID)
	require.NoError(t, err)
	assert.Empty(t, badSess.FrictionRulesVersion, "left stale for the backfill")
	assert.Empty(t, frictionKinds(t, fx.db, badID))
	goodSess, err := fx.db.GetSessionFull(t.Context(), goodID)
	require.NoError(t, err)
	assert.Equal(t, friction.RulesVersion, goodSess.FrictionRulesVersion)
	assert.NotEmpty(t, frictionKinds(t, fx.db, goodID))
}

func TestSyncPersistsInterruptionFinding(t *testing.T) {
	fx := newEngineFixture(t)
	path := writeFrictionClaudeSession(t, fx.claudeDir, "friction-interrupt",
		testjsonl.ClaudeUserJSON("[Request interrupted by user]", "2026-09-16T01:00:06Z"),
	)
	fx.engine.SyncAll(t.Context(), nil)
	id := fx.sessionIDFor(t, path)

	got, err := fx.db.SessionFrictionFindings(t.Context(), id)
	require.NoError(t, err)
	var interruptions []db.FrictionFinding
	for _, f := range got {
		if f.Kind == "interruption" {
			interruptions = append(interruptions, f)
		}
	}
	require.Len(t, interruptions, 1, "one interrupted row, one finding")
	assert.Equal(t, "interruption", interruptions[0].Detector)
	assert.Empty(t, interruptions[0].Text)
	require.NotNil(t, interruptions[0].MessageOrdinal)
	s, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, len(got), s.FrictionCount, "friction_count counts interruptions")
}
