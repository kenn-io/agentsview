package service_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
	"go.kenn.io/agentsview/internal/service"
	"go.kenn.io/agentsview/internal/sync"
	"go.kenn.io/agentsview/internal/testjsonl"
)

// directTestEnv is a lightweight environment helper for testing
// the directBackend. It holds the underlying *db.DB so test
// cases can seed fixture rows directly.
type directTestEnv struct {
	db *db.DB
}

// InsertSession upserts a minimal session row and returns its ID.
// Callers can use the returned ID to exercise the Get/List APIs
// without having to parse a real session fixture.
func (e *directTestEnv) InsertSession(t *testing.T) string {
	t.Helper()
	const sid = "test-session-1"
	dbtest.SeedSession(t, e.db, sid, "p1")
	return sid
}

// newDirectTestSvc builds a SessionService backed by an in-memory
// SQLite database with a nil sync engine (so Sync returns
// db.ErrReadOnly, matching the PG-serve read path).
func newDirectTestSvc(t *testing.T) (service.SessionService, *directTestEnv) {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	return service.NewDirectBackend(d, nil), &directTestEnv{db: d}
}

func TestDirectBackend_Get_Roundtrip(t *testing.T) {
	t.Parallel()
	svc, env := newDirectTestSvc(t)
	sessionID := env.InsertSession(t)

	detail, err := svc.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, sessionID, detail.ID)
}

func TestDirectBackend_Get_HealthBreakdownIncludesHeuristics(
	t *testing.T,
) {
	t.Parallel()
	svc, env := newDirectTestSvc(t)
	sessionID := env.InsertSession(t)
	dbtest.SeedMessages(t, env.db,
		dbtest.UserMsg(sessionID, 0,
			"Fix the backend test failure in the codebase."),
		dbtest.AsstMsg(sessionID, 1, "I'll inspect it."),
		dbtest.UserMsg(sessionID, 2,
			"Fix the backend test failure in the codebase."),
	)
	score := 90
	grade := "A"
	err := env.db.UpdateSessionSignals(
		sessionID,
		db.SessionSignalUpdate{
			Outcome:           "completed",
			OutcomeConfidence: "high",
			EndedWithRole:     "assistant",
			HealthScore:       &score,
			HealthGrade:       &grade,
			QualitySignals: db.QualitySignals{
				Version:              db.CurrentQualitySignalVersion,
				DuplicatePromptCount: 1,
				NoCodeContextCount:   1,
			},
		},
	)
	require.NoError(t, err)

	detail, err := svc.Get(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, detail)

	assert.Contains(t, detail.HealthScoreBasis, "prompt_quality")
	assert.Contains(t, detail.HealthScoreBasis, "context_quality")
	assert.NotContains(t, detail.HealthPenalties, "repeated_prompts")
	assert.NotContains(t, detail.HealthPenalties, "stuck_repeated_prompts")
	assert.Equal(t, 4,
		detail.HealthPenalties["code_task_without_context"])
}

func TestDirectBackend_List_Empty(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)
	list, err := svc.List(context.Background(), service.ListFilter{Limit: 10})
	require.NoError(t, err)
	assert.Equal(t, 0, list.Total)
}

func TestDirectBackend_List_HidesStaleSecretIndicators(t *testing.T) {
	t.Parallel()
	svc, env := newDirectTestSvc(t)
	for _, id := range []string{"current", "stale"} {
		dbtest.SeedSession(t, env.db, id, "proj",
			dbtest.WithMessageCounts(2, 2))
	}
	require.NoError(t, env.db.ReplaceSessionSecretFindings(
		"current", nil, 2, secrets.RulesVersion()))
	require.NoError(t, env.db.ReplaceSessionSecretFindings(
		"stale", nil, 1, "old-rules"))

	list, err := svc.List(context.Background(),
		service.ListFilter{IncludeOneShot: true, Limit: 10})
	require.NoError(t, err)
	counts := map[string]int{}
	for _, s := range list.Sessions {
		counts[s.ID] = s.SecretLeakCount
	}
	require.Equal(t, 2, counts["current"])
	require.Equal(t, 0, counts["stale"])

	staleDetail, err := svc.Get(context.Background(), "stale")
	require.NoError(t, err)
	require.Equal(t, 0, staleDetail.SecretLeakCount)

	hasSecret, err := svc.List(context.Background(),
		service.ListFilter{IncludeOneShot: true, HasSecret: true, Limit: 10})
	require.NoError(t, err)
	require.Len(t, hasSecret.Sessions, 1)
	require.Equal(t, "current", hasSecret.Sessions[0].ID)
}

func TestDirectBackend_List_InvalidDate(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	cases := []struct {
		name   string
		filter service.ListFilter
		want   string
	}{
		{
			name:   "Date bad format",
			filter: service.ListFilter{Date: "2024/01/15"},
			want:   `invalid date "2024/01/15"`,
		},
		{
			name:   "DateFrom bad format",
			filter: service.ListFilter{DateFrom: "not-a-date"},
			want:   `invalid date "not-a-date"`,
		},
		{
			name:   "DateTo bad format",
			filter: service.ListFilter{DateTo: "2024-13-40"},
			want:   `invalid date "2024-13-40"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			list, err := svc.List(context.Background(), tc.filter)
			require.Error(t, err)
			assert.Nil(t, list)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "YYYY-MM-DD")
		})
	}
}

func TestDirectBackend_List_DateFromAfterDateTo(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	list, err := svc.List(context.Background(), service.ListFilter{
		DateFrom: "2024-12-01",
		DateTo:   "2024-01-01",
	})
	require.Error(t, err)
	assert.Nil(t, list)
	assert.Contains(t, err.Error(), "date_from must not be after date_to")
}

func TestDirectBackend_List_InvalidActiveSince(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	list, err := svc.List(context.Background(), service.ListFilter{
		ActiveSince: "yesterday",
	})
	require.Error(t, err)
	assert.Nil(t, list)
	assert.Contains(t, err.Error(), `invalid active_since "yesterday"`)
	assert.Contains(t, err.Error(), "RFC3339")
}

func TestDirectBackend_List_ValidDatesAccepted(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	list, err := svc.List(context.Background(), service.ListFilter{
		Date:        "2024-06-15",
		DateFrom:    "2024-01-01",
		DateTo:      "2024-12-31",
		ActiveSince: "2024-06-15T12:30:45Z",
	})
	require.NoError(t, err)
	require.NotNil(t, list)
}

// TestDirectBackend_List_ClampsOverMaxLimit verifies that a caller
// passing a Limit larger than db.MaxSessionLimit is clamped to
// MaxSessionLimit rather than being reset to DefaultSessionLimit
// (which is the raw db.ListSessions guard's behavior). This matches
// the HTTP handler's clampLimit semantics.
func TestDirectBackend_List_ClampsOverMaxLimit(t *testing.T) {
	t.Parallel()
	svc, env := newDirectTestSvc(t)

	// Seed DefaultSessionLimit+1 sessions so we can distinguish
	// "clamped to MaxSessionLimit" (>DefaultSessionLimit returned)
	// from "reset to DefaultSessionLimit" (only DefaultSessionLimit
	// returned).
	nSessions := db.DefaultSessionLimit + 1
	for i := range nSessions {
		dbtest.SeedSession(
			t, env.db, fmt.Sprintf("s-%04d", i), "p1",
		)
	}

	list, err := svc.List(context.Background(), service.ListFilter{
		Limit:          db.MaxSessionLimit + 500,
		IncludeOneShot: true, // seeded sessions have 1 message each
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	// If the clamp works, we get all nSessions back (since
	// nSessions < MaxSessionLimit). Without the clamp, we would
	// only get DefaultSessionLimit back.
	assert.Equal(t, nSessions, len(list.Sessions),
		"limit should clamp to MaxSessionLimit, not reset to default")
}

func TestDirectBackend_Sync_BothPathAndID(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	// Ephemeral sync engine: enough to pass the nil-engine guard
	// and reach the validation branch. We never call SyncPaths in
	// this test because validation fails first.
	engine := sync.NewEngine(d, sync.EngineConfig{Ephemeral: true})
	svc := service.NewDirectBackend(d, engine)

	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/session.jsonl",
		ID:   "abc123",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "only one of path or id allowed")
}

func TestDirectBackend_Sync_NilEngineIsReadOnly(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/session.jsonl",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"expected db.ErrReadOnly, got %v", err)
}

// TestDirectBackend_Sync_AmbiguousPath_ReturnsListedIDs verifies
// that when one JSONL file maps to multiple sessions in the DB
// (e.g. Claude forked transcripts), Sync refuses to pick one
// arbitrarily and instead returns an error naming every candidate
// id, telling the caller to disambiguate via `session sync <id>`.
func TestDirectBackend_Sync_AmbiguousPath_ReturnsListedIDs(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	// Ephemeral engine so SyncPaths is a no-op — the test only
	// exercises the post-sync resolver.
	engine := sync.NewEngine(d, sync.EngineConfig{Ephemeral: true})
	svc := service.NewDirectBackend(d, engine)

	path := "/tmp/forked-session.jsonl"
	for _, id := range []string{"fork-a", "fork-b"} {
		require.NoError(t, d.UpsertSession(db.Session{
			ID:       id,
			Project:  "proj",
			Machine:  "local",
			Agent:    "claude",
			FilePath: &path,
		}))
	}

	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: path,
	})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "2 sessions found",
		"error should state the ambiguity count")
	assert.Contains(t, msg, "fork-a")
	assert.Contains(t, msg, "fork-b")
	assert.Contains(t, msg, "session sync <id>",
		"error should tell the caller how to disambiguate")
}

// TestDirectBackend_Sync_VSCopilotPhysicalPathResolvesSession verifies
// that syncing a Visual Studio Copilot session by its physical trace
// file resolves the single session whose stored file_path is the
// <traceFile>#<conversationID> virtual key for that trace.
func TestDirectBackend_Sync_VSCopilotPhysicalPathResolvesSession(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{Ephemeral: true})
	svc := service.NewDirectBackend(d, engine)

	tracePath := "/logs/20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl"
	convID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	virtual := tracePath + "#" + convID
	sessionID := "visualstudio-copilot:" + convID
	require.NoError(t, d.UpsertSession(db.Session{
		ID:       sessionID,
		Project:  "visualstudio",
		Machine:  "local",
		Agent:    "visualstudio-copilot",
		FilePath: &virtual,
	}))

	detail, err := svc.Sync(context.Background(), service.SyncInput{
		Path: tracePath,
	})
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, sessionID, detail.ID)
}

// TestDirectBackend_Sync_VSCopilotVS2026PhysicalPathResolvesSession verifies
// that syncing a Visual Studio 2026 Copilot session by its physical session
// file resolves the single session whose stored file_path is the
// <sessionFile>#<conversationID> virtual key for that file.
func TestDirectBackend_Sync_VSCopilotVS2026PhysicalPathResolvesSession(
	t *testing.T,
) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{Ephemeral: true})
	svc := service.NewDirectBackend(d, engine)

	convID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionPath := filepath.Join(
		"/workspace", ".vs", "SampleApp", "copilot-chat", "thread",
		"sessions", convID,
	)
	virtual := parser.VisualStudioCopilotVirtualPath(sessionPath, convID)
	sessionID := "visualstudio-copilot:" + convID
	require.NoError(t, d.UpsertSession(db.Session{
		ID:       sessionID,
		Project:  "visualstudio",
		Machine:  "local",
		Agent:    "visualstudio-copilot",
		FilePath: &virtual,
	}))

	detail, err := svc.Sync(context.Background(), service.SyncInput{
		Path: sessionPath,
	})
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, sessionID, detail.ID)
}

// TestDirectBackend_Sync_VSCopilotPhysicalPathAmbiguous verifies that a
// physical trace file backing several conversations still yields the
// disambiguation error rather than picking one arbitrarily.
func TestDirectBackend_Sync_VSCopilotPhysicalPathAmbiguous(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{Ephemeral: true})
	svc := service.NewDirectBackend(d, engine)

	tracePath := "/logs/20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl"
	for _, convID := range []string{
		"4a8f63f6-7626-4416-a874-fc7bd2c3f005",
		"c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28",
	} {
		virtual := tracePath + "#" + convID
		require.NoError(t, d.UpsertSession(db.Session{
			ID:       "visualstudio-copilot:" + convID,
			Project:  "visualstudio",
			Machine:  "local",
			Agent:    "visualstudio-copilot",
			FilePath: &virtual,
		}))
	}

	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: tracePath,
	})
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "2 sessions found")
	assert.Contains(t, msg, "session sync <id>")
}

func TestDirectBackend_Sync_VSCopilotIDRefreshesOnlyRequestedConversation(t *testing.T) {
	t.Parallel()
	tracesDir := t.TempDir()
	tracePath := filepath.Join(
		tracesDir, "20260612T194439_257709a3_VSGitHubCopilot_traces.jsonl",
	)
	requestedID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	untouchedID := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"
	writeDirectVSCopilotTrace(t, tracePath, requestedID, untouchedID,
		"Before requested", "Before untouched", time.Now())

	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	svc := service.NewDirectBackend(d, engine)
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)

	writeDirectVSCopilotTrace(t, tracePath, requestedID, untouchedID,
		"After requested with more detail",
		"After untouched with more detail",
		time.Now().Add(time.Second))

	detail, err := svc.Sync(context.Background(), service.SyncInput{
		ID: "visualstudio-copilot:" + requestedID,
	})
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.NotNil(t, detail.FirstMessage)
	assert.Equal(t, "After requested with more detail", *detail.FirstMessage)

	untouched, err := svc.Get(
		context.Background(), "visualstudio-copilot:"+untouchedID,
	)
	require.NoError(t, err)
	require.NotNil(t, untouched)
	require.NotNil(t, untouched.FirstMessage)
	assert.Equal(t, "Before untouched", *untouched.FirstMessage,
		"syncing by id must not refresh sibling conversations in the same trace")
}

func TestDirectBackend_Sync_VibeFallbackIDReturnsPromotedSession(t *testing.T) {
	vibeDir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVibe: {vibeDir},
		},
		Machine: "local",
	})
	svc := service.NewDirectBackend(d, engine)

	dirName := "session_20260616_083518_abc123"
	sessionDir := filepath.Join(vibeDir, dirName)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	require.NoError(t, os.WriteFile(
		messagesPath,
		[]byte(`{"role":"user","content":"hello vibe"}`+"\n"),
		0o644,
	))

	engine.SyncPaths([]string{messagesPath})
	fallbackID := "vibe:" + dirName
	fallback, err := d.GetSession(context.Background(), fallbackID)
	require.NoError(t, err)
	require.NotNil(t, fallback)

	sessionID := "abc123def-0000-0000-0000-000000000000"
	metaPath := filepath.Join(sessionDir, "meta.json")
	require.NoError(t, os.WriteFile(
		metaPath,
		[]byte(`{"session_id":"`+sessionID+`","title":"Promoted"}`+"\n"),
		0o644,
	))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(metaPath, future, future))

	detail, err := svc.Sync(context.Background(), service.SyncInput{
		ID: fallbackID,
	})
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, "vibe:"+sessionID, detail.ID)
	require.NotNil(t, detail.DisplayName)
	assert.Equal(t, "Promoted", *detail.DisplayName)

	stale, err := d.GetSession(context.Background(), fallbackID)
	require.NoError(t, err)
	assert.Nil(t, stale)
}

// TestDirectBackend_Sync_VibeCanonicalIDResolvesFallbackAfterMetaRemoved
// verifies the reverse of the promotion case: when meta.json is removed, the
// session is demoted to the directory-name fallback ID, and syncing the old
// canonical ID resolves to that fallback session instead of reporting the
// session as not found.
func TestDirectBackend_Sync_VibeCanonicalIDResolvesFallbackAfterMetaRemoved(t *testing.T) {
	vibeDir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVibe: {vibeDir},
		},
		Machine: "local",
	})
	svc := service.NewDirectBackend(d, engine)

	dirName := "session_20260616_083518_abc123"
	sessionDir := filepath.Join(vibeDir, dirName)
	require.NoError(t, os.MkdirAll(sessionDir, 0o755))
	messagesPath := filepath.Join(sessionDir, "messages.jsonl")
	require.NoError(t, os.WriteFile(
		messagesPath,
		[]byte(`{"role":"user","content":"hello vibe"}`+"\n"),
		0o644,
	))
	sessionID := "abc123def-0000-0000-0000-000000000000"
	canonicalID := "vibe:" + sessionID
	metaPath := filepath.Join(sessionDir, "meta.json")
	require.NoError(t, os.WriteFile(
		metaPath,
		[]byte(`{"session_id":"`+sessionID+`","title":"Canonical"}`+"\n"),
		0o644,
	))

	// First sync stores the session under the canonical meta-derived ID.
	engine.SyncPaths([]string{messagesPath})
	canonical, err := d.GetSession(context.Background(), canonicalID)
	require.NoError(t, err)
	require.NotNil(t, canonical)

	// meta.json is removed, so the next sync demotes the session to the
	// directory-name fallback ID. Bump the transcript mtime so the sync does
	// not skip the otherwise-unchanged file.
	require.NoError(t, os.Remove(metaPath))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(messagesPath, future, future))

	detail, err := svc.Sync(context.Background(), service.SyncInput{
		ID: canonicalID,
	})
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Equal(t, "vibe:"+dirName, detail.ID)

	stale, err := d.GetSession(context.Background(), canonicalID)
	require.NoError(t, err)
	assert.Nil(t, stale)
}

func TestDirectBackend_Sync_MissingNonVibeIDDoesNotReturnSamePathSession(t *testing.T) {
	claudeDir := t.TempDir()
	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentClaude: {claudeDir},
		},
		Machine: "local",
	})
	svc := service.NewDirectBackend(d, engine)

	projectDir := filepath.Join(claudeDir, "ClaudeProbe")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	path := filepath.Join(projectDir, "requested.jsonl")
	const usageCmd = "<command-name>/usage</command-name>\n" +
		"            <command-message>usage</command-message>\n" +
		"            <command-args></command-args>"
	require.NoError(t, os.WriteFile(
		path,
		[]byte(testjsonl.ClaudeUserJSON(usageCmd, "2026-06-17T12:00:00Z")+"\n"),
		0o644,
	))

	require.NoError(t, d.UpsertSession(db.Session{
		ID:       "requested",
		Project:  "proj",
		Machine:  "local",
		Agent:    "claude",
		FilePath: &path,
	}))
	require.NoError(t, d.UpsertSession(db.Session{
		ID:       "unrelated",
		Project:  "proj",
		Machine:  "local",
		Agent:    "claude",
		FilePath: &path,
	}))

	detail, err := svc.Sync(context.Background(), service.SyncInput{
		ID: "requested",
	})
	assert.Nil(t, detail)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `sync: session "requested" was not found after sync`)
}

// TestDirectBackend_Watch_UnknownID_Errors verifies that Watch
// on a missing session returns a clear "session not found" error
// instead of producing an indefinite heartbeat channel.
func TestDirectBackend_Watch_UnknownID_Errors(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	_, err := svc.Watch(context.Background(), "does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session not found")
	assert.Contains(t, err.Error(), "does-not-exist")
}

func writeDirectVSCopilotTrace(
	t *testing.T,
	tracePath, requestedID, untouchedID, requestedText, untouchedText string,
	modTime time.Time,
) {
	t.Helper()
	data := strings.Join([]string{
		directVSCopilotTraceLine(requestedID, "requested",
			"1781293600000000000", "1781293610000000000", requestedText),
		directVSCopilotTraceLine(untouchedID, "untouched",
			"1781294552800436000", "1781294586729109400", untouchedText),
	}, "\n") + "\n"
	require.NoError(t, os.WriteFile(tracePath, []byte(data), 0o644))
	require.NoError(t, os.Chtimes(tracePath, modTime, modTime))
}

func directVSCopilotTraceLine(
	conversationID, spanID, start, end, prompt string,
) string {
	inputMessages, _ := json.Marshal(
		`[{"role":"user","parts":[{"type":"text","content":"` +
			prompt + `"}]}]`,
	)
	traceID := directVSCopilotTraceHexID("trace:"+conversationID, 32)
	otelSpanID := directVSCopilotTraceHexID("span:"+spanID, 16)
	return `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"` +
		traceID + `","spanId":"` + otelSpanID +
		`","name":"chat gpt-5.5","startTimeUnixNano":"` + start +
		`","endTimeUnixNano":"` + end +
		`","attributes":[` +
		`{"key":"gen_ai.conversation.id","value":{"stringValue":"` +
		conversationID + `"}},` +
		`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},` +
		`{"key":"gen_ai.input.messages","value":{"stringValue":` +
		string(inputMessages) + `}}` +
		`]}]}]}]}`
}

func directVSCopilotTraceHexID(seed string, hexChars int) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:hexChars]
}

// TestDirectBackend_Messages_InvalidDirection verifies that the
// service layer rejects direction values outside {asc, desc}. HTTP
// and CLI both route through this, so the contract is enforced
// uniformly.
func TestDirectBackend_Messages_InvalidDirection(t *testing.T) {
	t.Parallel()
	svc, _ := newDirectTestSvc(t)

	_, err := svc.Messages(context.Background(), "sid",
		service.MessageFilter{Direction: "backwards"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid direction")
	assert.Contains(t, err.Error(), "backwards")
}

// TestReadOnlyBackend_Sync_IsReadOnly verifies that a backend
// constructed via NewReadOnlyBackend rejects Sync with
// db.ErrReadOnly regardless of the input.
func TestReadOnlyBackend_Sync_IsReadOnly(t *testing.T) {
	t.Parallel()
	d := dbtest.OpenTestDB(t)
	// Pass the *db.DB as a db.Store; the constructor's type
	// parameter restricts Sync capability, not read access.
	var store db.Store = d
	svc := service.NewReadOnlyBackend(store)

	_, err := svc.Sync(context.Background(), service.SyncInput{
		Path: "/tmp/session.jsonl",
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrReadOnly),
		"expected db.ErrReadOnly, got %v", err)
}

// TestDirectBackend_Messages_DescOmittedFrom exercises the
// "omitted From in desc mode == newest page" branch: when the
// filter's From pointer is nil, the backend promotes it to
// MaxInt32 so a descending query returns the newest messages.
func TestDirectBackend_Messages_DescOmittedFrom(t *testing.T) {
	t.Parallel()
	svc, env := newDirectTestSvc(t)
	sid := env.InsertSession(t)

	// Seed 5 user messages, ordinals 0..4.
	dbtest.SeedMessages(t, env.db, dbtest.UserMessagesf(sid, 5, "m%d")...)

	list, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		Direction: "desc",
		Limit:     10,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Equal(t, 5, list.Count)
	for i, m := range list.Messages {
		wantOrd := 4 - i
		assert.Equal(t, wantOrd, m.Ordinal,
			"desc iteration should return highest ordinal first")
	}
	assert.True(t, strings.HasPrefix(list.Messages[0].Content, "m4"))
}

// TestDirectBackend_Messages_DescExplicitZeroFrom verifies that an
// explicit From=0 in descending mode starts at ordinal 0 (returning
// only the ordinal-0 message) rather than being treated as "omitted"
// and promoted to MaxInt32.
func TestDirectBackend_Messages_DescExplicitZeroFrom(t *testing.T) {
	t.Parallel()
	svc, env := newDirectTestSvc(t)
	sid := env.InsertSession(t)

	dbtest.SeedMessages(t, env.db, dbtest.UserMessagesf(sid, 5, "m%d")...)

	zero := 0
	list, err := svc.Messages(context.Background(), sid, service.MessageFilter{
		Direction: "desc",
		From:      &zero,
		Limit:     10,
	})
	require.NoError(t, err)
	require.NotNil(t, list)
	require.Equal(t, 1, list.Count,
		"explicit From=0 in desc should start at ordinal 0 and "+
			"return only that message")
	assert.Equal(t, 0, list.Messages[0].Ordinal)
}

// vsCopilotChatTraceLine builds one Visual Studio Copilot trace JSONL line
// carrying a single user-prompt chat span for the given conversation.
func vsCopilotChatTraceLine(conversationID, spanID, prompt string) string {
	encoded, _ := json.Marshal(
		`[{"role":"user","parts":[{"type":"text","content":"` + prompt +
			`"}]}]`,
	)
	return `{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"trace",` +
		`"spanId":"` + spanID + `","name":"chat gpt-5.5",` +
		`"startTimeUnixNano":"1781293600000000000",` +
		`"endTimeUnixNano":"1781293610000000000","attributes":[` +
		`{"key":"gen_ai.conversation.id","value":{"stringValue":"` +
		conversationID + `"}},` +
		`{"key":"gen_ai.operation.name","value":{"stringValue":"chat"}},` +
		`{"key":"gen_ai.input.messages","value":{"stringValue":` +
		string(encoded) + `}}]}]}]}]}`
}

// TestDirectBackendSyncVisualStudioCopilotByIDFollowsConversationToSibling
// verifies that syncing a Visual Studio Copilot session by ID preserves the
// conversation scope: when the stored representative trace is deleted and the
// conversation reappears (with a new turn) in a sibling trace, the sync must
// follow the conversation to the sibling rather than stripping the virtual path
// to the now-deleted representative and doing nothing.
func TestDirectBackendSyncVisualStudioCopilotByIDFollowsConversationToSibling(
	t *testing.T,
) {
	tracesDir := t.TempDir()
	conversationID := "4a8f63f6-7626-4416-a874-fc7bd2c3f005"
	sessionID := "visualstudio-copilot:" + conversationID
	primary := filepath.Join(
		tracesDir, "20260611T145205_aaaa1111_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(primary, []byte(
		vsCopilotChatTraceLine(conversationID, "a1", "First.")+"\n"), 0o644))

	d := dbtest.OpenTestDB(t)
	engine := sync.NewEngine(d, sync.EngineConfig{
		AgentDirs: map[parser.AgentType][]string{
			parser.AgentVSCopilot: {tracesDir},
		},
		Machine: "local",
	})
	require.NotZero(t, engine.SyncAll(context.Background(), nil).Synced)
	svc := service.NewDirectBackend(d, engine)

	// The representative trace is deleted and the conversation reappears in a
	// sibling with a second turn (log rotation). The sibling also holds an
	// unrelated conversation that must not be created by a scoped single-session
	// sync.
	otherID := "c0aca2e3-d1f2-4d28-bd5e-5dab29e2be28"
	require.NoError(t, os.Remove(primary))
	sibling := filepath.Join(
		tracesDir, "20260612T145205_bbbb2222_VSGitHubCopilot_traces.jsonl",
	)
	require.NoError(t, os.WriteFile(sibling, []byte(strings.Join([]string{
		vsCopilotChatTraceLine(conversationID, "a1", "First."),
		vsCopilotChatTraceLine(conversationID, "b1", "Second."),
		vsCopilotChatTraceLine(otherID, "o1", "Unrelated conversation."),
	}, "\n")+"\n"), 0o644))

	_, err := svc.Sync(
		context.Background(), service.SyncInput{ID: sessionID},
	)
	require.NoError(t, err)

	sess, err := d.GetSession(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, sess)
	assert.Equal(t, 2, sess.MessageCount,
		"sync by ID must follow the conversation to the sibling trace, not "+
			"strip the virtual path to the deleted representative")

	other, err := d.GetSession(
		context.Background(), "visualstudio-copilot:"+otherID,
	)
	require.NoError(t, err)
	assert.Nil(t, other,
		"a scoped single-session sync must not insert unrelated conversations "+
			"from the same trace file")
}
