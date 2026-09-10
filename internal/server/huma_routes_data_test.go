package server_test

import (
	"context"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/server"
	syncpkg "go.kenn.io/agentsview/internal/sync"
)

func TestDataProjectsEndpoint(t *testing.T) {
	te := setup(t)
	te.seedSession(t, "alpha-1", "alpha", 1, func(s *db.Session) {
		s.Machine = "m1"
		s.Cwd = "/w/a"
	})
	te.seedSession(t, "beta-1", "beta", 1, func(s *db.Session) {
		s.Machine = "m2"
		s.Cwd = "/w/b"
	})

	w := te.get(t, "/api/v1/data/projects")
	assertStatus(t, w, http.StatusOK)

	var inv db.ProjectInventory
	decodeInto(t, w, &inv)
	assert.Equal(t, 2, inv.TotalProjects)
	assert.Equal(t, 2, inv.TotalSessions)
	require.Len(t, inv.Projects, 2)
}

func TestDataProjectRulesEndpoint(t *testing.T) {
	te := setup(t)
	_, err := te.db.CreateWorktreeProjectMapping(context.Background(), db.WorktreeProjectMapping{
		Machine: "ws", PathPrefix: "/work", Layout: db.WorktreeMappingLayoutExplicit,
		Project: "outer", Enabled: true,
	})
	require.NoError(t, err, "create /work mapping")
	te.seedSession(t, "repo-1", "misc", 1, func(s *db.Session) {
		s.Machine = "ws"
		s.Cwd = "/work/a"
	})

	w := te.get(t, "/api/v1/data/project-rules?machine=ws")
	assertStatus(t, w, http.StatusOK)

	var rules db.ProjectRules
	decodeInto(t, w, &rules)
	assert.Equal(t, "ws", rules.Machine)
	assert.Contains(t, rules.Machines, "ws")
	require.Len(t, rules.Rules, 1)
	assert.Equal(t, "/work", rules.Rules[0].PathPrefix)
	assert.Equal(t, 1, rules.Rules[0].GovernedSessions)
}

func TestDataProjectRulesDefaultsToLocalMachine(t *testing.T) {
	te := setup(t)

	w := te.get(t, "/api/v1/data/project-rules")
	assertStatus(t, w, http.StatusOK)

	var body struct {
		Machine      string `json:"machine"`
		LocalMachine string `json:"local_machine"`
	}
	decodeInto(t, w, &body)
	assert.Equal(t, "test", body.LocalMachine)
	assert.Equal(t, body.LocalMachine, body.Machine)
}

func TestDataProjectReclassificationCandidatesEndpoint(t *testing.T) {
	te := setup(t)
	const rawProject = "branch-label"
	te.seedSession(t, "selected", rawProject, 1, func(s *db.Session) {
		s.Machine = "host-a.example"
		s.Cwd = "/srv/worktrees/example/selected"
	})

	projects, err := te.db.BuildProjectIdentityMap(context.Background(), []string{rawProject})
	require.NoError(t, err)

	w := te.get(t, buildPathURL(
		"/api/v1/data/project-reclassification/candidates",
		map[string]string{
			"project_label": export.SafeProjectDisplayLabel(rawProject),
			"project_key":   projects[rawProject].ProjectKey,
		},
	))
	assertStatus(t, w, http.StatusOK)

	var response struct {
		Candidates []db.WorktreeReclassificationCandidate `json:"candidates"`
	}
	decodeInto(t, w, &response)
	require.Len(t, response.Candidates, 1)
	assert.Equal(t, "host-a.example", response.Candidates[0].Machine)
	assert.Equal(t, 1, response.Candidates[0].ContributingSessions)
}

func TestDataProjectReclassificationCandidatesMissingProjectKey(t *testing.T) {
	te := setup(t)
	for _, key := range []string{"", "   "} {
		w := te.get(t, buildPathURL(
			"/api/v1/data/project-reclassification/candidates",
			map[string]string{"project_label": "example", "project_key": key},
		))
		assertStatus(t, w, http.StatusBadRequest)
	}
}

func TestDataRoutesRegistered(t *testing.T) {
	te := setup(t)
	paths := []string{
		"/api/v1/data/projects",
		"/api/v1/data/project-rules",
		buildPathURL(
			"/api/v1/data/project-reclassification/candidates",
			map[string]string{"project_label": "example", "project_key": "pl1-example"},
		),
	}
	for _, path := range paths {
		w := te.get(t, path)
		assert.NotEqual(t, http.StatusNotFound, w.Code, "path %q must be registered", path)
		assert.Contains(t, w.Header().Get("Content-Type"), "application/json",
			"path %q must be routed to a JSON handler, not the SPA fallback", path)
	}
	w := te.post(t, "/api/v1/data/compact", `{}`)
	assert.NotEqual(t, http.StatusNotFound, w.Code)
	assert.Contains(t, w.Header().Get("Content-Type"), "application/json")
	wPreview := te.post(t, "/api/v1/data/strip-images/preview", `{}`)
	assert.NotEqual(t, http.StatusNotFound, wPreview.Code)
	assert.Contains(t, wPreview.Header().Get("Content-Type"), "application/json")
	wStrip := te.post(t, "/api/v1/data/strip-images", `{}`)
	assert.NotEqual(t, http.StatusNotFound, wStrip.Code)
	assert.Contains(t, wStrip.Header().Get("Content-Type"), "application/json")
}

func TestDataStripImagesRejectsNonLocalhost(t *testing.T) {
	const sid = "img-nonlocal-1"
	te := setup(t)
	seedSessionWithImage(t, te, sid, "test-project")

	// Read the row before any request so we can verify byte-identity afterwards.
	originalContent := readToolCallContent(t, te, sid)

	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/v1/data/strip-images/preview", `{}`},
		{"/api/v1/data/strip-images", `{"confirmed":true}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "198.51.100.7:5555"
		w := httptest.NewRecorder()
		// te.handler supplies the allowed Host and Origin, so the request
		// reaches the localhost gate instead of stopping at the host check.
		te.handler.ServeHTTP(w, req)
		assertStatus(t, w, http.StatusForbidden)
		assertBodyContains(t, w, "only permitted from localhost")
	}

	// The stored row must be byte-identical: the non-localhost gate fires before any query.
	assert.Equal(t, originalContent, readToolCallContent(t, te, sid), "row must be unchanged after non-localhost rejection")
}

func TestDataStripImagesRejectsForwardedLoopback(t *testing.T) {
	const sid = "img-fwd-1"
	te := setup(t)
	seedSessionWithImage(t, te, sid, "test-project")
	originalContent := readToolCallContent(t, te, sid)

	for _, tc := range []struct {
		path string
		body string
	}{
		{"/api/v1/data/strip-images/preview", `{}`},
		{"/api/v1/data/strip-images", `{"confirmed":true}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", "10.0.0.1")
		req.RemoteAddr = "127.0.0.1:5555"
		w := httptest.NewRecorder()
		te.handler.ServeHTTP(w, req)
		assertStatus(t, w, http.StatusForbidden)
		assertBodyContains(t, w, "only permitted from localhost")
	}

	assert.Equal(t, originalContent, readToolCallContent(t, te, sid), "row must be unchanged after forwarded-loopback rejection")
}

func TestDataStripImagesPreviewEmptyBody(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/data/strip-images/preview", `{}`)
	assertStatus(t, w, http.StatusOK)
	var report db.StripImagesReport
	decodeInto(t, w, &report)
	assert.Equal(t, int64(0), report.Payloads)
}

func TestDataStripImagesRequiresConfirmation(t *testing.T) {
	const sid = "img-noconfirm-1"
	te := setup(t)
	seedSessionWithImage(t, te, sid, "test-project")
	originalContent := readToolCallContent(t, te, sid)

	for _, body := range []string{`{}`, `{"confirmed":false}`} {
		w := te.post(t, "/api/v1/data/strip-images", body)
		assertStatus(t, w, http.StatusBadRequest)
	}

	assert.Equal(t, originalContent, readToolCallContent(t, te, sid), "row must be unchanged after unconfirmed requests")
}

func TestDataStripImagesRejectsInvalidBefore(t *testing.T) {
	te := setup(t)
	w := te.post(t, "/api/v1/data/strip-images/preview", `{"before":"2026-13-40"}`)
	assertStatus(t, w, http.StatusBadRequest)
	w2 := te.post(t, "/api/v1/data/strip-images", `{"before":"2026-13-40","confirmed":true}`)
	assertStatus(t, w2, http.StatusBadRequest)
}

const testImgPayload = `[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="}]`

const testURLPayload = `[{"type":"input_image","image_url":"https://example.com/a.png"}]`

func seedSessionWithImage(t *testing.T, te *testEnv, sessionID, project string) {
	t.Helper()
	seedSessionWithContent(t, te, sessionID, project, testImgPayload)
}

// testImgDecodedBytes is the decoded length of the single image payload that
// seedSessionWithImage stores, so a report total can be asserted for equality
// rather than positivity.
func testImgDecodedBytes(t *testing.T) int64 {
	t.Helper()
	const prefix = `data:image/png;base64,`
	start := strings.Index(testImgPayload, prefix)
	require.GreaterOrEqual(t, start, 0, "seed payload must carry a data: URI")
	encoded := testImgPayload[start+len(prefix):]
	end := strings.IndexByte(encoded, '"')
	require.Greater(t, end, 0, "seed payload data: URI must be quoted")
	decoded, err := base64.StdEncoding.DecodeString(encoded[:end])
	require.NoError(t, err, "seed payload must be valid base64")
	return int64(len(decoded))
}

// seedSessionWithImageEndedAt seeds an image-bearing session whose end
// timestamp differs from the shared default, so the `before` bound has
// something to select against.
func seedSessionWithImageEndedAt(
	t *testing.T, te *testEnv, sessionID, project, endedAt string,
) {
	t.Helper()
	te.seedSession(t, sessionID, project, 1, func(s *db.Session) {
		s.EndedAt = &endedAt
	})
	require.NoError(t, te.db.ReplaceSessionMessages(sessionID, []db.Message{
		{
			SessionID:     sessionID,
			Ordinal:       0,
			Role:          "assistant",
			Content:       "[computer call]",
			ContentLength: 15,
			Timestamp:     tsSeed,
			HasToolUse:    true,
			ToolCalls: []db.ToolCall{
				{
					SessionID:           sessionID,
					ToolName:            "computer",
					Category:            "Other",
					ToolUseID:           "tu-img-" + sessionID,
					ResultContent:       testImgPayload,
					ResultContentLength: len(testImgPayload),
				},
			},
		},
	}))
}

func seedSessionWithContent(t *testing.T, te *testEnv, sessionID, project, resultContent string) {
	t.Helper()
	te.seedSession(t, sessionID, project, 1)
	require.NoError(t, te.db.ReplaceSessionMessages(sessionID, []db.Message{
		{
			SessionID:     sessionID,
			Ordinal:       0,
			Role:          "assistant",
			Content:       "[computer call]",
			ContentLength: 15,
			Timestamp:     tsSeed,
			HasToolUse:    true,
			ToolCalls: []db.ToolCall{
				{
					SessionID:           sessionID,
					ToolName:            "computer",
					Category:            "Other",
					ToolUseID:           "tu-img-" + sessionID,
					ResultContent:       resultContent,
					ResultContentLength: len(resultContent),
				},
			},
		},
	}))
}

// readToolCallContent returns the result_content of the first tool call in the
// given session. Used to verify that a row is byte-identical after a request
// that should have made no writes.
func readToolCallContent(t *testing.T, te *testEnv, sessionID string) string {
	t.Helper()
	msgs, err := te.db.GetMessages(context.Background(), sessionID, 0, 100, true)
	require.NoError(t, err, "readToolCallContent: GetMessages")
	for _, msg := range msgs {
		if len(msg.ToolCalls) > 0 {
			return msg.ToolCalls[0].ResultContent
		}
	}
	t.Fatal("readToolCallContent: no tool call found in session " + sessionID)
	return ""
}

func TestDataStripImagesPreviewReportsPayloads(t *testing.T) {
	const sid = "img-preview-1"
	te := setup(t)
	seedSessionWithImage(t, te, sid, "test-project")

	w := te.post(t, "/api/v1/data/strip-images/preview", `{}`)
	assertStatus(t, w, http.StatusOK)

	var report db.StripImagesReport
	decodeInto(t, w, &report)
	assert.Equal(t, 1, report.Sessions, "preview should count exactly the seeded session")
	assert.Equal(t, int64(1), report.Payloads, "preview should count exactly one image payload")
	// decoded_bytes must match the actual base64-decoded length of the seeded payload.
	assert.Equal(t, testImgDecodedBytes(t), report.DecodedBytes,
		"decoded_bytes must equal the decoded length of the seeded payload")
}

func TestDataStripImagesAppliesAndIsIdempotent(t *testing.T) {
	const sid = "img-apply-1"
	var notified int
	te := setupWithServerOpts(t, []server.Option{
		server.WithSessionMutationNotifier(func() { notified++ }),
	})
	seedSessionWithImage(t, te, sid, "test-project")

	// First apply: must report changed=1 and the stored row must hold the placeholder.
	w := te.post(t, "/api/v1/data/strip-images", `{"confirmed":true}`)
	assertStatus(t, w, http.StatusOK)
	var report db.StripImagesReport
	decodeInto(t, w, &report)
	assert.Equal(t, 1, report.Changed, "first apply must change exactly one session")
	// The stored row must now hold the agentsview_image placeholder, not the original data: URI.
	assert.Contains(t, readToolCallContent(t, te, sid), "agentsview_image", "stored row must hold the placeholder after apply")
	assert.Equal(t, 1, notified, "committed cleanup must notify session consumers")

	// Second apply (idempotent): sessions=0, changed=0, payloads=0 (PI-5).
	w2 := te.post(t, "/api/v1/data/strip-images", `{"confirmed":true}`)
	assertStatus(t, w2, http.StatusOK)
	var report2 db.StripImagesReport
	decodeInto(t, w2, &report2)
	assert.Equal(t, 0, report2.Sessions, "second apply must select no sessions (PI-5)")
	assert.Equal(t, 0, report2.Changed, "second apply must change nothing (PI-5)")
	assert.Equal(t, int64(0), report2.Payloads, "second apply must find no payloads (PI-5)")
	assert.Equal(t, 1, notified, "unchanged cleanup must not notify again")
}

func TestDataStripImagesNotifiesAfterPartialCommit(t *testing.T) {
	var notified int
	te := setupWithServerOpts(t, []server.Option{
		server.WithSessionMutationNotifier(func() { notified++ }),
	})
	seedSessionWithImage(t, te, "img-a", "test-project")
	seedSessionWithImage(t, te, "img-b", "test-project")
	originalContent := readToolCallContent(t, te, "img-b")
	// The cleanup visits sessions by ID within the project. Fail the
	// second session's write after the first session has committed.
	require.NoError(t, te.db.Update(func(tx *sql.Tx) error {
		_, err := tx.Exec(`
			CREATE TRIGGER fail_second_image_cleanup
			BEFORE UPDATE OF result_content ON tool_calls
			WHEN OLD.session_id = 'img-b'
			BEGIN
				SELECT RAISE(ABORT, 'forced image cleanup failure');
			END`)
		return err
	}))

	w := te.post(t, "/api/v1/data/strip-images", `{"confirmed":true}`)
	assertStatus(t, w, http.StatusInternalServerError)
	assert.Contains(t, readToolCallContent(t, te, "img-a"), "agentsview_image")
	assert.Equal(t, originalContent, readToolCallContent(t, te, "img-b"))
	assert.Equal(t, 1, notified, "committed sessions must notify even when cleanup fails")
}

func TestDataStripImagesPreviewLeavesArchiveUnchanged(t *testing.T) {
	const sid = "img-prev-unchanged-1"
	te := setup(t)
	seedSessionWithImage(t, te, sid, "test-project")

	// Snapshot the stored content and transcript_revision before preview.
	originalContent := readToolCallContent(t, te, sid)
	session, err := te.db.GetSession(context.Background(), sid)
	require.NoError(t, err, "GetSession before preview")
	originalRevision := session.TranscriptRevision

	w := te.post(t, "/api/v1/data/strip-images/preview", `{}`)
	assertStatus(t, w, http.StatusOK)

	// Both must be unchanged: preview writes nothing.
	assert.Equal(t, originalContent, readToolCallContent(t, te, sid), "result_content must not change after preview")
	sessionAfter, err := te.db.GetSession(context.Background(), sid)
	require.NoError(t, err, "GetSession after preview")
	assert.Equal(t, originalRevision, sessionAfter.TranscriptRevision, "transcript_revision must not change after preview")
}

func TestDataStripImagesIgnoresUndecodableImageBlocks(t *testing.T) {
	const sid = "img-url-1"
	te := setup(t)
	// Seed a block whose image_url is an https URL, not a data: URI.
	seedSessionWithContent(t, te, sid, "test-project", testURLPayload)

	w := te.post(t, "/api/v1/data/strip-images/preview", `{}`)
	assertStatus(t, w, http.StatusOK)
	var report db.StripImagesReport
	decodeInto(t, w, &report)
	assert.Equal(t, 0, report.Sessions, "non-data: URL must not be counted as a session (PI-3)")
	assert.Equal(t, int64(0), report.Payloads, "non-data: URL must not be counted as a payload (PI-3)")

	// Apply: must change nothing.
	w2 := te.post(t, "/api/v1/data/strip-images", `{"confirmed":true}`)
	assertStatus(t, w2, http.StatusOK)
	var report2 db.StripImagesReport
	decodeInto(t, w2, &report2)
	assert.Equal(t, 0, report2.Changed, "apply must change nothing for a non-data: URL block")
	assert.Equal(t, testURLPayload, readToolCallContent(t, te, sid), "row must be unchanged (PI-3)")
}

func TestDataStripImagesFiltersByProjectAndBefore(t *testing.T) {
	te := setup(t)
	// Three sessions: two projects, two end dates. The alpha pair straddles the
	// bound so a `before` filter has something to include and something to drop.
	seedSessionWithImageEndedAt(t, te, "img-filter-a1", "alpha", "2025-01-05T11:00:00Z")
	seedSessionWithImage(t, te, "img-filter-a2", "alpha")
	seedSessionWithImage(t, te, "img-filter-b1", "beta")

	// Filter by project "alpha": must select exactly 2 sessions.
	w := te.post(t, "/api/v1/data/strip-images/preview", `{"project":"alpha"}`)
	assertStatus(t, w, http.StatusOK)
	var rAlpha db.StripImagesReport
	decodeInto(t, w, &rAlpha)
	assert.Equal(t, 2, rAlpha.Sessions, "project filter 'alpha' must select 2 sessions")

	// Filter by project "beta": must select exactly 1 session.
	w2 := te.post(t, "/api/v1/data/strip-images/preview", `{"project":"beta"}`)
	assertStatus(t, w2, http.StatusOK)
	var rBeta db.StripImagesReport
	decodeInto(t, w2, &rBeta)
	assert.Equal(t, 1, rBeta.Sessions, "project filter 'beta' must select 1 session")

	// Filter by project " alpha" (leading space): the substring is not trimmed,
	// so it must select 0 sessions, not 2.
	w3 := te.post(t, "/api/v1/data/strip-images/preview", `{"project":" alpha"}`)
	assertStatus(t, w3, http.StatusOK)
	var rSpaced db.StripImagesReport
	decodeInto(t, w3, &rSpaced)
	assert.Equal(t, 0, rSpaced.Sessions, "project filter ' alpha' (leading space) must not match 'alpha'")

	// Date bound: only the session that ended before 2025-01-10 is selected.
	w4 := te.post(t, "/api/v1/data/strip-images/preview", `{"before":"2025-01-10"}`)
	assertStatus(t, w4, http.StatusOK)
	var rBefore db.StripImagesReport
	decodeInto(t, w4, &rBefore)
	assert.Equal(t, 1, rBefore.Sessions, "before '2025-01-10' must select only the earlier session")
	require.Len(t, rBefore.Projects, 1, "before filter must report one project")
	assert.Equal(t, "alpha", rBefore.Projects[0].Project)

	// Project and date together narrow to the same single session.
	w5 := te.post(t, "/api/v1/data/strip-images/preview",
		`{"project":"alpha","before":"2025-01-10"}`)
	assertStatus(t, w5, http.StatusOK)
	var rBoth db.StripImagesReport
	decodeInto(t, w5, &rBoth)
	assert.Equal(t, 1, rBoth.Sessions, "project and date together must select 1 session")

	// A bound before every seeded session selects nothing.
	w6 := te.post(t, "/api/v1/data/strip-images/preview", `{"before":"2024-12-31"}`)
	assertStatus(t, w6, http.StatusOK)
	var rNone db.StripImagesReport
	decodeInto(t, w6, &rNone)
	assert.Equal(t, 0, rNone.Sessions, "a bound before every session must select nothing")
}

func TestDataStripImagesReturnsBusyWhileEngineHeld(t *testing.T) {
	var notified int
	te := setupWithServerOpts(t, []server.Option{
		server.WithSessionMutationNotifier(func() { notified++ }),
	})
	seedSessionWithImage(t, te, "img-busy-1", "test-project")

	// Hold the exclusive engine lock in a goroutine.
	lockAcquired := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = te.engine.RunExclusive(func() error {
			close(lockAcquired)
			<-release
			return nil
		})
	}()
	<-lockAcquired
	defer func() {
		close(release)
		<-done
	}()

	w := te.post(t, "/api/v1/data/strip-images", `{"confirmed":true}`)
	assertStatus(t, w, http.StatusConflict)
	assert.Zero(t, notified, "cleanup rejected before writing must not notify")
}

func TestDataStripImagesReadOnlyArchive(t *testing.T) {
	const sid = "img-ro-1"

	// Seed data using a writable setup, then open the same file read-only.
	te := setup(t)
	seedSessionWithImage(t, te, sid, "test-project")
	originalContent := readToolCallContent(t, te, sid)
	dbPath := filepath.Join(te.dataDir, "test.db")

	// Open the same file as a read-only DB and create a dedicated server.
	roDb, err := db.OpenReadOnly(dbPath)
	require.NoError(t, err, "OpenReadOnly")
	t.Cleanup(func() { _ = roDb.Close() })
	cfg := config.Config{Host: "127.0.0.1", Port: 0}
	roSrv := server.New(cfg, roDb, nil)
	handler := wrapTestHandler(cfg, roSrv.Handler())

	postTo := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w
	}

	// Preview must still succeed (the read path is legal on a read-only archive).
	wPrev := postTo("/api/v1/data/strip-images/preview", `{}`)
	assertStatus(t, wPrev, http.StatusOK)

	// Apply must fail with 501 (ErrReadOnly → handleHumaReadOnly).
	wApply := postTo("/api/v1/data/strip-images", `{"confirmed":true}`)
	assertStatus(t, wApply, http.StatusNotImplemented)

	// The row in the original DB must be byte-identical.
	assert.Equal(t, originalContent, readToolCallContent(t, te, sid), "row must be unchanged after read-only rejection")
}

func TestDataCompactEndpointUsesDaemonRunner(t *testing.T) {
	called := false
	te := setupWithServerOpts(t, []server.Option{
		server.WithLocalCompactRunner(func(
			_ context.Context, options db.CompactOptions,
		) (db.CompactResult, error) {
			called = true
			assert.Empty(t, options.StagingDir,
				"the daemon must not accept a client-controlled staging path")
			return db.CompactResult{ReclaimedBytes: 42}, nil
		}),
	})
	w := te.post(t, "/api/v1/data/compact", `{}`)
	assertStatus(t, w, http.StatusOK)
	assert.True(t, called)
	var result db.CompactResult
	decodeInto(t, w, &result)
	assert.Equal(t, int64(42), result.ReclaimedBytes)
}

func TestDataCompactEndpointRejectsClientStagingDir(t *testing.T) {
	te := setupWithServerOpts(t, []server.Option{
		server.WithLocalCompactRunner(func(
			context.Context, db.CompactOptions,
		) (db.CompactResult, error) {
			t.Fatal("client-controlled staging path must be rejected before execution")
			return db.CompactResult{}, nil
		}),
	})
	w := te.post(t, "/api/v1/data/compact", `{"staging_dir":"/tmp/attacker"}`)
	assertStatus(t, w, http.StatusBadRequest)
}

func TestDataCompactEndpointMapsBusyBarrierToConflict(t *testing.T) {
	for name, busy := range map[string]error{
		"sync in progress":    syncpkg.ErrSyncInProgress,
		"compact in progress": db.ErrCompactInProgress,
	} {
		t.Run(name, func(t *testing.T) {
			te := setupWithServerOpts(t, []server.Option{
				server.WithLocalCompactRunner(func(
					context.Context, db.CompactOptions,
				) (db.CompactResult, error) {
					return db.CompactResult{}, busy
				}),
			})
			w := te.post(t, "/api/v1/data/compact", `{}`)
			assertStatus(t, w, http.StatusConflict)
		})
	}
}

func TestDataCompactEndpointRejectsNonLocalhost(t *testing.T) {
	te := setupWithServerOpts(t, []server.Option{
		server.WithLocalCompactRunner(func(
			context.Context, db.CompactOptions,
		) (db.CompactResult, error) {
			t.Fatal("non-local request must not invoke the compact runner")
			return db.CompactResult{}, nil
		}),
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/data/compact", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:0")
	req.RemoteAddr = "203.0.113.10:4242"
	w := httptest.NewRecorder()
	te.srv.Handler().ServeHTTP(w, req)
	assertStatus(t, w, http.StatusForbidden)
}
