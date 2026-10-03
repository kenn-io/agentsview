package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/friction"
	"go.kenn.io/agentsview/internal/ingest"
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
	raw, calls := ingest.FrictionRawInput(msgs)
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

func TestComputeSessionFrictionEditChurnUsesTranscriptFilePath(t *testing.T) {
	const sessionID = "friction-transcript-edit-churn"
	messages := make([]db.Message, 3)
	toolNames := []string{"Edit", "apply_patch", "write_file"}
	for i := range messages {
		messages[i] = db.Message{
			SessionID: sessionID, Ordinal: i, Role: "assistant",
			ToolCalls: []db.ToolCall{{
				ToolName: toolNames[i], Category: "Edit",
				InputJSON: `{"file_path":"src/main.go"}`,
				FilePath:  "src/main.go",
			}},
		}
	}

	session, projected := db.ProjectSessionForStoragePolicy(
		db.Session{ID: sessionID, Agent: "codex"},
		messages, config.ArchiveContentTranscripts,
	)
	for _, message := range projected {
		require.Len(t, message.ToolCalls, 1)
		assert.Empty(t, message.ToolCalls[0].InputJSON)
		assert.Equal(t, "src/main.go", message.ToolCalls[0].FilePath)
	}

	update, err := computeSessionFriction(
		t.Context(), session, projected, nil,
		frictionOptions{redacted: true},
	)
	require.NoError(t, err)
	require.Len(t, update.Findings, 1)
	assert.Equal(t, string(friction.KindPattern), update.Findings[0].Kind)
	assert.Equal(t, friction.PatternDetector(friction.PatternEditChurn),
		update.Findings[0].Detector)
	assert.Equal(t, "edit churn: `main.go` edited 3 times within 10 messages",
		update.Findings[0].Text)
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
			assert.Equal(t, tt.want, ingest.FrictionIsSubAgent(tt.s))
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
	}, kinds, "detector order; frustration and interruption persist")
	assert.Equal(t, db.FrictionHash(u.Findings, u.Dims, friction.RulesVersion), u.Hash)
	require.NotNil(t, u.Findings[0].MessageOrdinal)
	assert.Equal(t, 2, *u.Findings[0].MessageOrdinal)
	assert.Equal(t, "no, use the config file instead", u.Findings[0].Text)
	require.NotNil(t, u.Findings[3].MessageOrdinal)
	assert.Equal(t, 5, *u.Findings[3].MessageOrdinal)
	assert.Empty(t, u.Findings[3].Text, "interruption findings carry no text")
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

func TestLinkSubagentSessionsRefreshesHierarchyDependentFriction(t *testing.T) {
	for _, mode := range []string{"global", "changed_batch"} {
		t.Run(mode, func(t *testing.T) {
			fx := newEngineFixture(t)
			ctx := t.Context()
			for _, session := range []db.Session{
				{ID: "parent", Project: "project", Machine: "local", Agent: "claude"},
				{ID: "child", Project: "project", Machine: "local", Agent: "claude", RelationshipType: "fork"},
			} {
				require.NoError(t, fx.db.UpsertSession(ctx, session))
			}
			require.NoError(t, fx.db.ReplaceSessionMessages(ctx, "child", []db.Message{{
				SessionID: "child", Ordinal: 0, Role: "user", Content: "run the task",
			}}))
			require.NoError(t, fx.db.ReplaceSessionMessages(ctx, "parent", []db.Message{{
				SessionID: "parent", Ordinal: 0, Role: "assistant", HasToolUse: true,
				ToolCalls: []db.ToolCall{{
					ToolName: "Agent", Category: "Task", SubagentSessionID: "child",
				}},
			}}))
			fx.engine.frictionReviewHook = func(in friction.SessionInput) []friction.Signal {
				if in.IsSubAgent {
					return nil
				}
				return []friction.Signal{{
					Kind: friction.KindPattern, SubjectID: in.SubjectID,
					SubjectKind: friction.SubjectSession, Detector: "test.hierarchy",
					Label: "hierarchy", Text: "root-only finding", Evidence: "root-only",
				}}
			}
			require.NoError(t, fx.engine.recomputeFrictionFromDB(ctx, "child"))
			require.Len(t, frictionKinds(t, fx.db, "child"), 1)

			if mode == "global" {
				linked, err := fx.engine.linkSubagentSessions(ctx)
				require.NoError(t, err)
				assert.Equal(t, 1, linked)
			} else {
				var stats SyncStats
				links := changedSessionLinks{"parent": {}}
				require.NoError(t, links.link(ctx, fx.engine, &stats))
				assert.Equal(t, 1, stats.LinksUpdated)
			}
			child, err := fx.db.GetSessionFull(ctx, "child")
			require.NoError(t, err)
			require.NotNil(t, child.ParentSessionID)
			assert.Equal(t, "parent", *child.ParentSessionID)
			assert.Equal(t, "subagent", child.RelationshipType)
			assert.Equal(t, friction.RulesVersion, child.FrictionRulesVersion)
			assert.Zero(t, child.FrictionCount)
			assert.Empty(t, frictionKinds(t, fx.db, "child"))
		})
	}
}

func TestQueuedSubagentParentRepairRefreshesFriction(t *testing.T) {
	fx := newEngineFixture(t)
	ctx := t.Context()
	oldParent := "old-parent"
	for _, session := range []db.Session{
		{ID: "spawner", Project: "project", Machine: "local", Agent: "claude"},
		{ID: "old-parent", Project: "project", Machine: "local", Agent: "claude"},
		{
			ID: "queued-child", Project: "project", Machine: "local",
			Agent: "claude", ParentSessionID: &oldParent,
			RelationshipType: "continuation",
		},
	} {
		require.NoError(t, fx.db.UpsertSession(ctx, session))
	}
	require.NoError(t, fx.db.ReplaceSessionMessages(ctx, "queued-child", []db.Message{{
		SessionID: "queued-child", Ordinal: 0, Role: "user", Content: "run the task",
	}}))
	require.NoError(t, fx.db.ReplaceSessionMessages(ctx, "spawner", []db.Message{{
		SessionID: "spawner", Ordinal: 0, Role: "assistant", HasToolUse: true,
		ToolCalls: []db.ToolCall{{
			ToolName: "Agent", Category: "Task", SubagentSessionID: "queued-child",
		}},
	}}))
	fx.engine.frictionReviewHook = func(in friction.SessionInput) []friction.Signal {
		if in.IsSubAgent {
			return nil
		}
		return []friction.Signal{{
			Kind: friction.KindPattern, SubjectID: in.SubjectID,
			SubjectKind: friction.SubjectSession, Detector: "test.hierarchy",
			Label: "hierarchy", Text: "root-only finding", Evidence: "root-only",
		}}
	}
	require.NoError(t, fx.engine.recomputeFrictionFromDB(ctx, "queued-child"))
	require.Len(t, frictionKinds(t, fx.db, "queued-child"), 1)
	require.NoError(t, fx.db.QueueSubagentParentRepairs(ctx, []string{"queued-child"}))

	fx.engine.syncMu.Lock()
	repaired, err := fx.engine.repairQueuedSubagentParents(ctx, nil)
	fx.engine.syncMu.Unlock()
	require.NoError(t, err)
	assert.Equal(t, 1, repaired)

	child, err := fx.db.GetSessionFull(ctx, "queued-child")
	require.NoError(t, err)
	require.NotNil(t, child.ParentSessionID)
	assert.Equal(t, "spawner", *child.ParentSessionID)
	assert.Equal(t, "subagent", child.RelationshipType)
	assert.Equal(t, friction.RulesVersion, child.FrictionRulesVersion)
	assert.Empty(t, frictionKinds(t, fx.db, "queued-child"))
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

func TestFrictionSameOnSyncResyncAndBackfill(t *testing.T) {
	fx := newEngineFixture(t)
	path := writeFrictionClaudeSession(t, fx.claudeDir, "friction-stable")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	id := fx.sessionIDFor(t, path)
	first, err := fx.db.SessionFrictionFindings(t.Context(), id)
	require.NoError(t, err)
	require.NotEmpty(t, first)
	firstSess, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)

	fx.engine.ResyncAll(t.Context(), nil)
	afterResync, err := fx.db.SessionFrictionFindings(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, first, afterResync, "resync reproduces findings")

	require.NoError(t, seedStaleFriction(t.Context(), fx.db, id))
	processed, err := fx.engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	afterBackfill, err := fx.db.SessionFrictionFindings(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, first, afterBackfill, "backfill reproduces findings")
	s, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, firstSess.FrictionHash, s.FrictionHash)

	processed, err = fx.engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	assert.Zero(t, processed, "nothing stale: the tick is a no-op")
	state, err := fx.db.FrictionBackfillState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "completed", state)
}

func TestBackfillFrictionWithSignalRecomputationDisabled(t *testing.T) {
	for _, tt := range []struct {
		name          string
		cfg           EngineConfig
		wantProcessed int
		wantRules     string
	}{
		{"usage-only startup", EngineConfig{ArchiveContent: config.ArchiveContentUsage}, 1, friction.RulesVersion},
		{"explicitly disabled", EngineConfig{DisableSignalRecomputation: true}, 0, "older-rules"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestDB(t)
			const id = "archived-session"
			require.NoError(t, store.UpsertSession(t.Context(), db.Session{
				ID: id, Agent: "codex", Project: "project-a", Machine: "local",
			}))
			// Model an upgraded archive: quality signals are current, but the
			// friction snapshot still needs settlement before the next mirror push.
			require.NoError(t, store.UpdateSessionSignals(t.Context(), id, db.SessionSignalUpdate{
				QualitySignals: db.QualitySignals{Version: db.CurrentQualitySignalVersion},
				Friction:       &db.SessionFrictionUpdate{RulesVersion: "older-rules"},
			}))
			engine := NewEngine(t.Context(), store, tt.cfg)
			t.Cleanup(engine.Close)
			loads := store.MessagesLoadCount()

			processed, err := engine.BackfillFriction(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.wantProcessed, processed)
			stored, err := store.GetSessionFull(t.Context(), id)
			require.NoError(t, err)
			require.NotNil(t, stored)
			assert.Equal(t, tt.wantRules, stored.FrictionRulesVersion)
			assert.Empty(t, frictionKinds(t, store, id))
			assert.Equal(t, loads, store.MessagesLoadCount(), "neither path reads transcript history")

			processed, err = engine.BackfillFriction(t.Context())
			require.NoError(t, err)
			assert.Zero(t, processed, "settled or disabled rows are not revisited")
		})
	}
}

func TestBackfillFrictionDuringResync(t *testing.T) {
	fx := newEngineFixture(t)
	t.Cleanup(fx.engine.Close)
	path := writeFrictionClaudeSession(t, fx.claudeDir, "friction-concurrent-resync")
	require.Equal(t, 1, fx.engine.SyncAll(t.Context(), nil).Synced)
	id := fx.sessionIDFor(t, path)
	require.NoError(t, seedStaleFriction(t.Context(), fx.db, id))

	// Exercise both stale-session and completed backfill passes while real
	// rebuilds replace the engine's database handle and close its connections.
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		var firstErr error
		for {
			select {
			case <-stop:
				done <- firstErr
				return
			default:
			}
			if _, err := fx.engine.BackfillFriction(t.Context()); err != nil && firstErr == nil {
				firstErr = err
			}
			runtime.Gosched()
		}
	}()
	for range 3 {
		assert.False(t, fx.engine.ResyncAll(t.Context(), nil).Aborted)
	}
	close(stop)
	require.NoError(t, <-done)

	assert.Equal(t, []string{
		"correction:correction.coding", "workaround:workaround",
	}, frictionKinds(t, fx.db, id))
	stale, err := fx.db.CountStaleFrictionSessions(t.Context(), friction.RulesVersion)
	require.NoError(t, err)
	assert.Zero(t, stale)
}

func TestBackfillFrictionRetriesFailedSessionWithoutBlockingOthers(t *testing.T) {
	fx := newEngineFixture(t)
	bad := writeFrictionClaudeSession(t, fx.claudeDir, "friction-backfill-bad")
	good := writeFrictionClaudeSession(t, fx.claudeDir, "friction-backfill-good")
	require.Equal(t, 2, fx.engine.SyncAll(t.Context(), nil).Synced)
	badID, goodID := fx.sessionIDFor(t, bad), fx.sessionIDFor(t, good)
	require.NoError(t, seedStaleFriction(t.Context(), fx.db, badID, goodID))
	fx.engine.frictionReviewHook = func(in friction.SessionInput) []friction.Signal {
		if in.SubjectID == badID {
			panic("detector bug")
		}
		return friction.Review(in)
	}

	processed, err := fx.engine.BackfillFriction(t.Context())
	require.Error(t, err)
	assert.Equal(t, 1, processed, "the other session should still settle")
	stale, err := fx.db.StaleFrictionSessions(t.Context(), friction.RulesVersion, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{badID}, stale)
	state, err := fx.db.FrictionBackfillState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "pending", state)

	fx.engine.frictionReviewHook = nil
	processed, err = fx.engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed)
	state, err = fx.db.FrictionBackfillState(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "completed", state)
}

func TestBackfillFrictionResumesAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "archive.db")
	for pass, tt := range []struct {
		name          string
		failDetection bool
		wantProcessed int
		wantError     bool
	}{
		{"first page fails", true, 0, true},
		{"restart reaches later session", true, 1, false},
		{"restart wraps and retries", false, 20, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store, err := db.Open(t.Context(), path)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			if pass == 0 {
				for i := range 21 {
					id := fmt.Sprintf("session-%02d", i)
					require.NoError(t, store.UpsertSession(t.Context(), db.Session{
						ID: id, Agent: "claude", Project: "test", Machine: "local",
					}))
					require.NoError(t, store.ReplaceSessionMessages(t.Context(), id, []db.Message{{
						SessionID: id, Role: "assistant", Content: "I'll come back to this later.",
					}}))
				}
			}
			engine := NewEngine(t.Context(), store, EngineConfig{})
			t.Cleanup(engine.Close)
			if tt.failDetection {
				engine.frictionReviewHook = func(in friction.SessionInput) []friction.Signal {
					if in.SubjectID != "session-20" {
						panic("detector bug")
					}
					return friction.Review(in)
				}
			}

			processed, err := engine.BackfillFriction(t.Context())
			if tt.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantProcessed, processed)
			if pass > 0 {
				assert.Equal(t, []string{"deferral:deferral"}, frictionKinds(t, store, "session-20"))
			}
		})
	}
}

func TestFrictionRecomputeDefersOversizedHistory(t *testing.T) {
	fx := newEngineFixture(t)
	const id = "large-friction"
	require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{ID: id, Agent: "codex", Project: "test", Machine: "local"}))
	require.NoError(t, fx.db.ReplaceSessionMessages(t.Context(), id, []db.Message{{SessionID: id, Role: "assistant", Content: strings.Repeat("x", 4<<20)}}))
	loads := fx.db.MessagesLoadCount()
	require.NoError(t, fx.engine.recomputeFrictionFromDB(t.Context(), id))
	session, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, session.FrictionRulesVersion, "oversized history must remain stale, not publish incomplete findings")
	assert.Equal(t, loads, fx.db.MessagesLoadCount(), "bounded recompute must not load all history")
	fx.db.SetArchiveContent(config.ArchiveContentUsage)
	processed, err := fx.engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, processed, "usage-only archives need no transcript review, regardless of size")
	session, err = fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, friction.RulesVersion, session.FrictionRulesVersion)
	assert.Zero(t, session.FrictionCount)
	assert.Equal(t, loads, fx.db.MessagesLoadCount())
}

func TestFrictionBackfillBoundsEachTick(t *testing.T) {
	fx := newEngineFixture(t)
	for i := range 25 {
		require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{ID: fmt.Sprintf("bounded-%02d", i), Agent: "codex", Project: "test", Machine: "local"}))
	}
	processed, err := fx.engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 20, processed)
	stale, err := fx.db.StaleFrictionSessions(t.Context(), friction.RulesVersion, 100)
	require.NoError(t, err)
	assert.Len(t, stale, 5)
	processed, err = fx.engine.BackfillFriction(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 5, processed)
}

// Seed the old detector version through the same signal write transaction as sync.
func seedStaleFriction(ctx context.Context, store *db.DB, ids ...string) error {
	for _, id := range ids {
		if err := store.UpdateSessionSignals(ctx, id, db.SessionSignalUpdate{Friction: &db.SessionFrictionUpdate{RulesVersion: "older-rules"}}); err != nil {
			return err
		}
	}
	return nil
}

func TestFrictionDetectorFailureDoesNotScheduleHotRetry(t *testing.T) {
	fx := newEngineFixture(t)
	const id = "failing-detector"
	require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{ID: id, Agent: "codex", Machine: "local", Project: "test"}))
	attempts := 0
	fx.engine.frictionReviewHook = func(friction.SessionInput) []friction.Signal { attempts++; panic("detector bug") }
	fx.engine.frictionSched.markDirty(id)
	fx.engine.FlushSignals()
	fx.engine.FlushSignals()
	assert.Equal(t, 1, attempts, "failure stays stale for the next reconcile tick")
	session, err := fx.db.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	assert.Empty(t, session.FrictionRulesVersion)
}
