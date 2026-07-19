package extract

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

func newTestArchive(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("closing archive: %v", err)
		}
	})
	return d
}

// seedSession stores an ended, extractable session with the given messages.
func seedSession(t *testing.T, d *db.DB, id string, msgs []db.Message, mutate func(*db.Session)) {
	t.Helper()
	ended := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	s := db.Session{
		ID:           id,
		Project:      "proj",
		Machine:      "local",
		Agent:        "claude",
		Cwd:          "/work/proj",
		GitBranch:    "main",
		EndedAt:      &ended,
		MessageCount: len(msgs),
	}
	if mutate != nil {
		mutate(&s)
	}
	if err := d.UpsertSession(s); err != nil {
		t.Fatalf("seeding session %s: %v", id, err)
	}
	for i := range msgs {
		msgs[i].SessionID = id
		msgs[i].Ordinal = i
	}
	if len(msgs) > 0 {
		if err := d.InsertMessages(msgs); err != nil {
			t.Fatalf("seeding messages for %s: %v", id, err)
		}
	}
	// Extraction requires a current clean secret scan, not just a zero
	// leak count.
	if err := d.ReplaceSessionSecretFindings(
		id, nil, 0, secrets.RulesVersion(),
	); err != nil {
		t.Fatalf("stamping secret scan for %s: %v", id, err)
	}
	settleSessionWrite()
}

// settleSessionWrite pushes subsequent pass cutoffs into a later millisecond
// than the seed writes. Timestamps carry millisecond precision, and the
// done-revisit gate treats a write in the same millisecond as the cutoff as
// after it (the safe direction); without the gap a session extracted in the
// same millisecond it was seeded reads as perpetually changed. Production
// has the quiet period between the last write and any pass.
func settleSessionWrite() {
	time.Sleep(2 * time.Millisecond)
}

// growSession appends messages and settles the session the way a sync pass
// followed by a full secret rescan would: the row's message count matches
// the transcript again and the full-scan stamp is restored (the append
// itself revokes it), which also bumps local_modified_at.
func growSession(t *testing.T, d *db.DB, id string, msgs []db.Message, startOrdinal int) {
	t.Helper()
	for i := range msgs {
		msgs[i].SessionID = id
		msgs[i].Ordinal = startOrdinal + i
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("growing session %s: %v", id, err)
	}
	session, err := d.GetSessionFull(context.Background(), id)
	if err != nil || session == nil {
		t.Fatalf("loading grown session %s: %v", id, err)
	}
	session.MessageCount = startOrdinal + len(msgs)
	if err := d.UpsertSession(*session); err != nil {
		t.Fatalf("updating grown session %s: %v", id, err)
	}
	if err := d.ReplaceSessionSecretFindings(
		id, nil, 0, secrets.RulesVersion(),
	); err != nil {
		t.Fatalf("re-stamping secret scan for %s: %v", id, err)
	}
	settleSessionWrite()
}

func turnMessages(pairs ...string) []db.Message {
	msgs := make([]db.Message, 0, len(pairs))
	for i, content := range pairs {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, db.Message{Role: role, Content: content})
	}
	return msgs
}

type callLog struct {
	mu    sync.Mutex
	texts []string
}

func (c *callLog) add(text string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts = append(c.texts, text)
	return len(c.texts)
}

func (c *callLog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.texts)
}

func completionBody(t *testing.T, content string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"finish_reason": "stop",
			"message":       map[string]any{"role": "assistant", "content": content},
		}},
		"usage": map[string]any{"prompt_tokens": 7, "completion_tokens": 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// modelServer answers each distillation call through respond, which receives
// the unit text and returns a status plus raw response body.
func modelServer(
	t *testing.T, respond func(text string, call int) (int, string),
) (*httptest.Server, *callLog) {
	t.Helper()
	log := &callLog{}
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var payload struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decoding request: %v", err)
			}
			text := payload.Messages[len(payload.Messages)-1].Content
			call := log.add(text)
			status, body := respond(text, call)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	t.Cleanup(server.Close)
	return server, log
}

func alwaysEntries(t *testing.T, titles ...string) func(string, int) (int, string) {
	return func(string, int) (int, string) {
		return http.StatusOK, completionBody(t, entriesJSON(t, titles...))
	}
}

func newManager(
	t *testing.T, d *db.DB, serverURL string, mutate func(*ManagerConfig),
) *Manager {
	t.Helper()
	cfg := ManagerConfig{
		DB: d,
		Client: &Client{
			BaseURL:      serverURL,
			Model:        "test-model",
			RetryBackoff: time.Millisecond,
			Request:      RequestShape{MaxTokens: 100},
		},
		Segmenter: TurnsV1{MaxWindowChars: 50000},
		Prompts: map[PromptRole]string{
			RoleIntent: "intent prompt",
			RoleAction: "action prompt",
		},
		Identity:    ModelIdentity{Model: "test-model"},
		QuietPeriod: 10 * time.Minute,
		MaxAttempts: 2,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	m, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestManagerRunPassExtractsMapsAndActivates(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, func(text string, _ int) (int, string) {
		content := `{"entries":[{"type":"decision","title":"t",` +
			`"body":"chose sqlite","entities":["sqlite","storage"]}]}`
		return http.StatusOK, completionBody(t, content)
	})
	seedSession(t, d, "sess-1", turnMessages("fix the bug", "done, patched"), nil)
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 1 || result.Failed != 0 {
		t.Fatalf("result = %+v, want 1 session, 0 failed", result)
	}
	if result.Units != 2 || result.Entries != 2 {
		t.Fatalf("result = %+v, want 2 units, 2 entries", result)
	}
	if log.count() != 2 {
		t.Fatalf("model calls = %d, want 2 (intent + action)", log.count())
	}

	entry, err := d.GetRecallEntry(ctx, EntryID(m.Fingerprint(), "sess-1", 0, 0))
	if err != nil {
		t.Fatalf("GetRecallEntry: %v", err)
	}
	if entry == nil {
		t.Fatal("expected entry at deterministic id for unit 0")
	}
	if entry.Type != "decision" || entry.Title != "t" {
		t.Fatalf("entry type/title = %s/%s", entry.Type, entry.Title)
	}
	if entry.Body != "chose sqlite\nEntities: sqlite; storage" {
		t.Fatalf("entry body = %q, entities must be folded into the body",
			entry.Body)
	}
	if entry.Trigger != "" {
		t.Fatalf("trigger = %q, want empty", entry.Trigger)
	}
	if entry.ReviewState != "unreviewed_auto" || entry.Status != "accepted" {
		t.Fatalf("review/status = %s/%s", entry.ReviewState, entry.Status)
	}
	if entry.SourceSessionID != "sess-1" ||
		entry.SourceRunID != m.Fingerprint() {
		t.Fatalf("provenance = %+v", entry)
	}
	if entry.ExtractorMethod != "turns-v1" || entry.Model != "test-model" {
		t.Fatalf("method/model = %s/%s", entry.ExtractorMethod, entry.Model)
	}
	if entry.Project != "proj" || entry.CWD != "/work/proj" ||
		entry.GitBranch != "main" || entry.Agent != "claude" {
		t.Fatalf("session context = %+v", entry)
	}
	if len(entry.Evidence) != 1 {
		t.Fatalf("evidence rows = %d, want 1", len(entry.Evidence))
	}
	ev := entry.Evidence[0]
	if ev.SessionID != "sess-1" ||
		ev.MessageStartOrdinal != 0 || ev.MessageEndOrdinal != 0 {
		t.Fatalf("evidence = %+v, want ordinal range 0-0", ev)
	}

	generations, err := d.ExtractGenerations(ctx)
	if err != nil {
		t.Fatalf("ExtractGenerations: %v", err)
	}
	if len(generations) != 1 ||
		generations[0].State != db.ExtractGenerationActive {
		t.Fatalf("generations = %+v, want one active", generations)
	}
	if !result.Activated {
		t.Fatal("result must report activation")
	}
}

func TestManagerRunPassRetriesFailedSessionFromCursor(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	// Units: intent(u0), action(a1), intent(u2), action(a3). Call 3 (unit 2)
	// fails until the server heals, exhausting the client's attempts.
	server, log := modelServer(t, func(text string, call int) (int, string) {
		if call == 3 || call == 4 {
			return http.StatusInternalServerError, `{"error":"down"}`
		}
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	seedSession(t, d, "sess-1",
		turnMessages("first ask", "first work", "second ask", "second work"),
		nil)
	m := newManager(t, d, server.URL, func(cfg *ManagerConfig) {
		cfg.FailureBackoff = 5 * time.Millisecond
	})

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Failed != 1 || result.Sessions != 0 {
		t.Fatalf("result = %+v, want the session marked failed", result)
	}
	if result.Units != 2 || result.Entries != 2 {
		t.Fatalf("result = %+v, want 2 units done before the failure", result)
	}
	if result.Activated {
		t.Fatal("a failed-only pass must not activate")
	}

	// Let the failure row age past the (tiny) backoff before rescanning.
	time.Sleep(50 * time.Millisecond)
	result, err = m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass retry: %v", err)
	}
	if result.Sessions != 1 || result.Failed != 0 {
		t.Fatalf("retry result = %+v, want session completed", result)
	}
	if result.Units != 2 || result.Entries != 2 {
		t.Fatalf("retry result = %+v, want only units 2-3 redone", result)
	}
	// Calls: 2 ok + 2 failing attempts, then 2 for the remaining units.
	if log.count() != 6 {
		t.Fatalf("model calls = %d, want 6 (resume must skip done units)",
			log.count())
	}
	if !result.Activated {
		t.Fatal("completing pass must activate the generation")
	}
}

func TestManagerSplitsOversizedUnits(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, func(text string, _ int) (int, string) {
		if utf8.RuneCountInString(text) > 80 {
			return http.StatusBadRequest,
				`{"error":{"code":"context_length_exceeded",` +
					`"message":"too long"}}`
		}
		return http.StatusOK, completionBody(t, entriesJSON(t, "leaf"))
	})
	var long strings.Builder
	for range 30 {
		long.WriteString("abcde ")
	}
	seedSession(t, d, "sess-1", turnMessages("short ask", long.String()), nil)
	// A small window keeps the split floor (window/8) below the leaf size
	// so recursion is allowed.
	m := newManager(t, d, server.URL, func(cfg *ManagerConfig) {
		cfg.Segmenter = TurnsV1{MaxWindowChars: 400}
	})

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Failed != 0 || result.Sessions != 1 {
		t.Fatalf("result = %+v, want clean completion via splitting", result)
	}
	if result.Entries < 3 {
		t.Fatalf("entries = %d, want one per split leaf plus the intent",
			result.Entries)
	}
	// Split leaves stay inside one unit: both leaf entries carry unit index 1.
	first, err := d.GetRecallEntry(ctx, EntryID(m.Fingerprint(), "sess-1", 1, 0))
	if err != nil || first == nil {
		t.Fatalf("leaf entry 0 missing: %v", err)
	}
	second, err := d.GetRecallEntry(ctx, EntryID(m.Fingerprint(), "sess-1", 1, 1))
	if err != nil || second == nil {
		t.Fatalf("leaf entry 1 missing: %v", err)
	}
}

func TestManagerRunPassSkipsIneligibleSessions(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-automated", turnMessages("a", "b"),
		func(s *db.Session) { s.IsAutomated = true })
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("result = %+v with %d calls; automated sessions must never "+
			"reach the model", result, log.count())
	}
	if result.Activated {
		t.Fatal("nothing extracted, nothing to activate")
	}

	_, err = m.RunPass(ctx, PassOptions{SessionID: "sess-automated"})
	if err == nil {
		t.Fatal("explicit run on an automated session must be refused")
	}
	if log.count() != 0 {
		t.Fatal("refusal must happen before any model call")
	}
}

func TestManagerExplicitSessionBypassesQuietPeriod(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-fresh", turnMessages("a", "b"),
		func(s *db.Session) {
			ended := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
			s.EndedAt = &ended
		})
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 {
		t.Fatalf("scan must respect the quiet period, got %+v", result)
	}

	result, err = m.RunPass(ctx, PassOptions{SessionID: "sess-fresh"})
	if err != nil {
		t.Fatalf("explicit RunPass: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("explicit run must bypass the quiet period, got %+v", result)
	}
}

func TestManagerFullPassTopsUpGrownSession(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("ask", "answer"), nil)
	m := newManager(t, d, server.URL, nil)

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	firstCalls := log.count()

	// A plain rescan leaves done sessions alone.
	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass rescan: %v", err)
	}
	if result.Sessions != 0 || log.count() != firstCalls {
		t.Fatalf("rescan must skip done sessions, got %+v", result)
	}

	// The session grows; a full pass re-derives units, replaces the
	// session's generated entries, and extracts the new units.
	growSession(t, d, "sess-1",
		turnMessages("ask", "answer", "follow-up", "more work")[2:], 2)
	result, err = m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("full pass must revisit the grown session, got %+v", result)
	}
	if result.Entries != 4 {
		t.Fatalf("entries = %d, want 4 (digest change rebuilds the "+
			"session's corpus)", result.Entries)
	}
	var count int
	entries, err := d.ListRecallEntries(ctx, db.RecallQuery{Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries: %v", err)
	}
	count = len(entries)
	if count != 4 {
		t.Fatalf("stored entries = %d, want exactly 4 (no stale leftovers)",
			count)
	}
}

func TestManagerFullPassReplacesEntriesOfChangedUnits(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	// Titles encode the unit text length so re-extraction of changed
	// content is observable.
	server, _ := modelServer(t, func(text string, _ int) (int, string) {
		content := fmt.Sprintf(
			`{"entries":[{"type":"fact","title":"len-%d",`+
				`"body":"b","entities":[]}]}`,
			utf8.RuneCountInString(text))
		return http.StatusOK, completionBody(t, content)
	})
	seedSession(t, d, "sess-1", turnMessages("ask", "first answer"), nil)
	m := newManager(t, d, server.URL, nil)

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	before, err := d.GetRecallEntry(ctx, EntryID(m.Fingerprint(), "sess-1", 1, 0))
	if err != nil || before == nil {
		t.Fatalf("unit-1 entry missing after first pass: %v", err)
	}

	// The assistant run grows: unit 1 now packs both messages, so its
	// content — and the entry extracted from it — changes.
	growSession(t, d, "sess-1",
		[]db.Message{{Role: "assistant", Content: "second answer"}}, 2)
	result, err := m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("full pass must revisit the changed session, got %+v", result)
	}
	after, err := d.GetRecallEntry(ctx, EntryID(m.Fingerprint(), "sess-1", 1, 0))
	if err != nil || after == nil {
		t.Fatalf("unit-1 entry missing after re-extraction: %v", err)
	}
	if after.Title == before.Title {
		t.Fatalf("unit-1 entry still says %q; a changed unit must not "+
			"keep its stale entry", after.Title)
	}
	entries, err := d.ListRecallEntries(ctx, db.RecallQuery{Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("stored entries = %d, want 2 (stale entries removed)",
			len(entries))
	}
}

func TestManagerSkipsSessionsWithoutCurrentSecretScan(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	// Seed without the scan stamp: leak count 0 but never scanned.
	ended := time.Now().Add(-time.Hour).UTC().Format("2006-01-02T15:04:05.000Z")
	if err := d.UpsertSession(db.Session{
		ID: "sess-unscanned", Project: "proj", Machine: "local",
		Agent: "claude", EndedAt: &ended, MessageCount: 2,
	}); err != nil {
		t.Fatal(err)
	}
	msgs := turnMessages("a", "b")
	for i := range msgs {
		msgs[i].SessionID = "sess-unscanned"
		msgs[i].Ordinal = i
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("unscanned session reached the model: %+v, %d calls",
			result, log.count())
	}

	_, err = m.RunPass(ctx, PassOptions{SessionID: "sess-unscanned"})
	if err == nil {
		t.Fatal("explicit run on an unscanned session must be refused")
	}
	if log.count() != 0 {
		t.Fatal("refusal must happen before any model call")
	}
}

func TestManagerZeroEntryGenerationNeverActivates(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, func(string, int) (int, string) {
		return http.StatusOK, completionBody(t, `{"entries":[]}`)
	})
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("result = %+v, want the session completed", result)
	}
	if result.Activated {
		t.Fatal("a generation with no entries must not auto-activate: " +
			"it would replace the active corpus with nothing")
	}
	if err := m.Activate(ctx); err == nil {
		t.Fatal("explicit activation of an entryless generation must be refused")
	}
}

func TestManagerTryPassDropsWhenBusy(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	release := make(chan struct{})
	inFlight := make(chan struct{}, 1)
	server, _ := modelServer(t, func(text string, _ int) (int, string) {
		inFlight <- struct{}{}
		<-release
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, nil)

	done := make(chan error, 1)
	go func() {
		_, err := m.RunPass(ctx, PassOptions{})
		done <- err
	}()
	// The first model call proves the background pass holds the pass lock.
	<-inFlight
	started, _, err := m.TryPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("TryPass: %v", err)
	}
	if started {
		t.Fatal("TryPass must drop while a pass is running")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	<-inFlight // drain the second unit's signal
}

func TestManagerActivateRefusesEmptyGeneration(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	m := newManager(t, d, server.URL, nil)

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if err := m.Activate(ctx); err == nil {
		t.Fatal("activating a generation with no completed sessions " +
			"must be refused")
	}
}

func TestManagerStatusReportsCoverage(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("ask", "answer"), nil)
	seedSession(t, d, "sess-fresh", turnMessages("a", "b"),
		func(s *db.Session) {
			ended := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
			s.EndedAt = &ended
		})
	m := newManager(t, d, server.URL, nil)

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	status, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Fingerprint != m.Fingerprint() {
		t.Fatalf("fingerprint = %s", status.Fingerprint)
	}
	if status.Stats.Done != 1 || status.Stats.Entries != 2 {
		t.Fatalf("stats = %+v, want 1 done session with 2 entries",
			status.Stats)
	}
	if len(status.Generations) != 1 {
		t.Fatalf("generations = %+v", status.Generations)
	}
	if status.EligibleBacklog != 0 {
		t.Fatalf("backlog = %d; the quiet-period session is not yet eligible",
			status.EligibleBacklog)
	}
}

func TestNewManagerValidatesConfig(t *testing.T) {
	d := newTestArchive(t)
	base := func() ManagerConfig {
		return ManagerConfig{
			DB:        d,
			Client:    &Client{BaseURL: "http://x", Model: "m", Request: RequestShape{MaxTokens: 10}},
			Segmenter: TurnsV1{MaxWindowChars: 100},
			Prompts: map[PromptRole]string{
				RoleIntent: "i", RoleAction: "a",
			},
			Identity: ModelIdentity{Model: "m"},
		}
	}
	cases := []struct {
		name   string
		mutate func(*ManagerConfig)
	}{
		{"missing db", func(c *ManagerConfig) { c.DB = nil }},
		{"missing client", func(c *ManagerConfig) { c.Client = nil }},
		{"zero window", func(c *ManagerConfig) { c.Segmenter.MaxWindowChars = 0 }},
		{"missing prompt role", func(c *ManagerConfig) {
			delete(c.Prompts, RoleAction)
		}},
		{"missing model identity", func(c *ManagerConfig) {
			c.Identity = ModelIdentity{}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			if _, err := NewManager(cfg); err == nil {
				t.Fatal("expected config validation error")
			}
		})
	}
	if _, err := NewManager(base()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestManagerRefusesDefiniteOnlyScan(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	// Only the fast inline sync scan ran: definite rules, no candidate
	// detection. Candidate-confidence secrets could be present undetected.
	seedSession(t, d, "sess-inline", turnMessages("a", "b"), nil)
	if err := d.ReplaceSessionSecretFindings(
		"sess-inline", nil, 0, secrets.DefiniteRulesVersion(),
	); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("definite-only scanned session reached the model: %+v, "+
			"%d calls", result, log.count())
	}

	_, err = m.RunPass(ctx, PassOptions{SessionID: "sess-inline"})
	if err == nil {
		t.Fatal("explicit run on a definite-only scanned session must be refused")
	}
	if !strings.Contains(err.Error(), "--backfill") {
		t.Fatalf("refusal must point at the full scan: %v", err)
	}
	if log.count() != 0 {
		t.Fatal("refusal must happen before any model call")
	}
}

func TestManagerRefusesSessionsWithCandidateFindings(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	// A full scan found a candidate-confidence secret: the leak count stays
	// zero, but the finding is recorded.
	seedSession(t, d, "sess-candidate", turnMessages("a", "b"), nil)
	if err := d.ReplaceSessionSecretFindings(
		"sess-candidate",
		[]db.SecretFinding{{
			SessionID:     "sess-candidate",
			RuleName:      "jwt",
			Confidence:    "candidate",
			LocationKind:  "message",
			RedactedMatch: "eyJh…",
		}},
		0, secrets.RulesVersion(),
	); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("session with a candidate finding reached the model: %+v, "+
			"%d calls", result, log.count())
	}

	_, err = m.RunPass(ctx, PassOptions{SessionID: "sess-candidate"})
	if err == nil {
		t.Fatal("explicit run on a session with candidate findings must be refused")
	}
	if log.count() != 0 {
		t.Fatal("refusal must happen before any model call")
	}
}

func TestSessionSnapshotChanged(t *testing.T) {
	ended := "2026-01-01T00:00:00.000Z"
	revision := "rev-1"
	base := func() *db.Session {
		return &db.Session{
			ID:                  "s",
			MessageCount:        4,
			EndedAt:             &ended,
			TranscriptRevision:  &revision,
			SecretsRulesVersion: secrets.RulesVersion(),
			SecretLeakCount:     0,
		}
	}
	if sessionSnapshotChanged(base(), base()) {
		t.Fatal("identical snapshots must compare equal")
	}
	cases := []struct {
		name   string
		mutate func(*db.Session)
	}{
		{"message count", func(s *db.Session) { s.MessageCount = 5 }},
		{"transcript revision", func(s *db.Session) {
			other := "rev-2"
			s.TranscriptRevision = &other
		}},
		{"revision cleared", func(s *db.Session) { s.TranscriptRevision = nil }},
		{"scan version", func(s *db.Session) {
			s.SecretsRulesVersion = secrets.DefiniteRulesVersion()
		}},
		{"leak count", func(s *db.Session) { s.SecretLeakCount = 1 }},
		{"ended at", func(s *db.Session) {
			other := "2026-01-01T00:00:01.000Z"
			s.EndedAt = &other
		}},
		{"ended cleared", func(s *db.Session) { s.EndedAt = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after := base()
			tc.mutate(after)
			if !sessionSnapshotChanged(base(), after) {
				t.Fatal("changed snapshot must be detected")
			}
		})
	}
}

func TestExtractionBracketStable(t *testing.T) {
	ended := "2026-01-01T00:00:00.000Z"
	revision := "rev-1"
	modified := "2026-01-01T00:00:00.000Z"
	base := func() *db.Session {
		return &db.Session{
			ID:                  "s",
			MessageCount:        4,
			EndedAt:             &ended,
			TranscriptRevision:  &revision,
			SecretsRulesVersion: secrets.RulesVersion(),
			LocalModifiedAt:     &modified,
		}
	}
	if !extractionBracketStable("s", base(), base()) {
		t.Fatal("identical eligible snapshots must read as stable")
	}
	if extractionBracketStable("s", base(), nil) {
		t.Fatal("a vanished session row must read as unstable")
	}
	moved := base()
	other := "2026-01-01T00:00:01.000Z"
	moved.LocalModifiedAt = &other
	if extractionBracketStable("s", base(), moved) {
		t.Fatal("a moved local_modified_at must read as unstable")
	}
	// Trash and automation flags can flip without touching any field the
	// snapshot comparison watches; the bracket must recheck eligibility.
	trashed := base()
	deleted := "2026-01-01T00:00:02.000Z"
	trashed.DeletedAt = &deleted
	if extractionBracketStable("s", base(), trashed) {
		t.Fatal("a concurrently trashed session must read as unstable")
	}
	automated := base()
	automated.IsAutomated = true
	if extractionBracketStable("s", base(), automated) {
		t.Fatal("a concurrently automation-flagged session must read as " +
			"unstable")
	}
}

func TestManagerStagesEntriesUntilActivation(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	seedSession(t, d, "sess-2", turnMessages("c", "d"), nil)
	m := newManager(t, d, server.URL, nil)

	// One session done, one still in the backlog: the generation keeps
	// building, so its entries must not serve yet.
	result, err := m.RunPass(ctx, PassOptions{Limit: 1})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 1 || result.Activated {
		t.Fatalf("result = %+v, want 1 session and no activation", result)
	}
	served, err := d.ListRecallEntries(ctx, db.RecallQuery{Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries: %v", err)
	}
	if len(served) != 0 {
		t.Fatalf("%d entries served while the generation is still building; "+
			"want 0 (staged as archived)", len(served))
	}
	staged, err := d.ListRecallEntries(ctx,
		db.RecallQuery{Status: "archived", Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries staged: %v", err)
	}
	if len(staged) != 2 {
		t.Fatalf("staged entries = %d, want 2", len(staged))
	}

	// The backlog drains; activation promotes the staged corpus atomically.
	result, err = m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if !result.Activated {
		t.Fatalf("result = %+v, want activation once the backlog drained", result)
	}
	served, err = d.ListRecallEntries(ctx, db.RecallQuery{Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries: %v", err)
	}
	if len(served) != 4 {
		t.Fatalf("served entries = %d, want all 4 after activation", len(served))
	}
	staged, err = d.ListRecallEntries(ctx,
		db.RecallQuery{Status: "archived", Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries staged: %v", err)
	}
	if len(staged) != 0 {
		t.Fatalf("staged entries = %d after activation, want 0", len(staged))
	}
}

func TestManagerSnapshotReadSeesMetadataOnlyWrites(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, "http://unused", nil)

	before, err := m.sessionSnapshot(ctx, "sess-1")
	if err != nil || before == nil {
		t.Fatalf("sessionSnapshot before: %v", err)
	}
	// A rescan that finds only candidate findings replaces the findings
	// under the same rules version and leak count: the only session-row
	// signal is local_modified_at, so the snapshot read must load it.
	time.Sleep(5 * time.Millisecond)
	if err := d.ReplaceSessionSecretFindings(
		"sess-1", nil, 0, secrets.RulesVersion(),
	); err != nil {
		t.Fatal(err)
	}
	after, err := m.sessionSnapshot(ctx, "sess-1")
	if err != nil || after == nil {
		t.Fatalf("sessionSnapshot after: %v", err)
	}
	if !sessionSnapshotChanged(before, after) {
		t.Fatal("a metadata-only write between snapshot reads must be " +
			"detected; the snapshot read is blind to local_modified_at")
	}
}

func TestManagerWatermarkLimitsScanDiscovery(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, nil)

	// A watermark ahead of the session's last write must hide it from
	// scheduled passes — incremental and full alike: steady-state scans
	// only look at recent writes, and the hourly backstop must not walk
	// the whole archive.
	m.watermark = time.Now().Add(time.Hour)
	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("result = %+v with %d calls; incremental discovery must "+
			"respect the watermark", result, log.count())
	}
	result, err = m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("result = %+v with %d calls; full-pass discovery must "+
			"respect the watermark too", result, log.count())
	}

	// Recovery is a fresh manager — daemon restart or a manual CLI run —
	// whose zero watermark scans unbounded.
	fresh := newManager(t, d, server.URL, nil)
	result, err = fresh.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("fresh RunPass full: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("result = %+v; a fresh manager must discover unbounded", result)
	}
}

func TestManagerWatermarkAdvancesOnlyOnCompleteScanPasses(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, nil)

	start := time.Now()
	if _, err := m.RunPass(ctx, PassOptions{SessionID: "sess-1"}); err != nil {
		t.Fatalf("explicit RunPass: %v", err)
	}
	if !m.watermark.IsZero() {
		t.Fatal("an explicit single-session pass must not advance the watermark")
	}
	if _, err := m.RunPass(ctx, PassOptions{Limit: 1}); err != nil {
		t.Fatalf("limited RunPass: %v", err)
	}
	if !m.watermark.IsZero() {
		t.Fatal("a limited pass leaves sessions behind and must not " +
			"advance the watermark")
	}
	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if m.watermark.IsZero() {
		t.Fatal("a completed scan pass must advance the watermark")
	}
	lag := start.Add(-m.cfg.QuietPeriod)
	if m.watermark.After(start) || m.watermark.Before(lag.Add(-time.Minute)) {
		t.Fatalf("watermark = %v, want roughly pass start minus the quiet "+
			"period (start %v)", m.watermark, start)
	}
}

func TestManagerFullWatermarkBoundsDoneRevisits(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("ask", "answer"), nil)
	m := newManager(t, d, server.URL, nil)

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if !m.fullWatermark.IsZero() {
		t.Fatal("an incremental pass must not advance the full watermark")
	}
	if _, err := m.RunPass(ctx, PassOptions{Full: true}); err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if m.fullWatermark.IsZero() {
		t.Fatal("a completed unlimited full pass must advance the full " +
			"watermark")
	}
	// A limited full pass may leave revisits behind and must not advance
	// the full watermark.
	bounded := newManager(t, d, server.URL, nil)
	if _, err := bounded.RunPass(ctx, PassOptions{Full: true, Limit: 1}); err != nil {
		t.Fatalf("limited RunPass full: %v", err)
	}
	if !bounded.fullWatermark.IsZero() {
		t.Fatal("a limited full pass must not advance the full watermark")
	}
	firstCalls := log.count()

	// The session grows, but a full watermark ahead of the write hides the
	// done revisit from scheduled full passes: the steady-state backstop
	// walks recent writes, not every completed session in the archive.
	growSession(t, d, "sess-1",
		turnMessages("ask", "answer", "follow-up", "more work")[2:], 2)
	m.fullWatermark = time.Now().Add(time.Hour)
	result, err := m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if result.Sessions != 0 || log.count() != firstCalls {
		t.Fatalf("result = %+v with %d calls; done revisits must respect "+
			"the full watermark", result, log.count()-firstCalls)
	}

	// Recovery is a fresh manager — daemon restart or a manual CLI run —
	// whose zero full watermark revisits unbounded.
	fresh := newManager(t, d, server.URL, nil)
	result, err = fresh.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("fresh RunPass full: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("result = %+v; a fresh manager must revisit the grown "+
			"session", result)
	}
}

func TestManagerMarksTranscriptOutOfStepRetryable(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)

	// The sync loop writes the session row before the transcript: mid-write
	// the row can claim more (or fewer) messages than are stored. A loaded
	// transcript that does not match the approved row state must not reach
	// the model.
	session, err := d.GetSessionFull(ctx, "sess-1")
	if err != nil || session == nil {
		t.Fatalf("GetSessionFull: %v", err)
	}
	session.MessageCount = 4
	if err := d.UpsertSession(*session); err != nil {
		t.Fatal(err)
	}
	settleSessionWrite()
	m := newManager(t, d, server.URL, func(cfg *ManagerConfig) {
		cfg.FailureBackoff = 5 * time.Millisecond
	})

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 0 || log.count() != 0 {
		t.Fatalf("result = %+v with %d calls; a transcript out of step with "+
			"the session row must not reach the model", result, log.count())
	}
	// The snapshot bracket was stable, so this was not a caught-mid-write
	// race: a silent skip would let the watermarks advance past the
	// session's writes and exclude it forever. The mismatch must be
	// recorded as a retryable failure instead.
	if result.Failed != 1 {
		t.Fatalf("result = %+v; a stable out-of-step transcript must be "+
			"recorded as a retryable failure, not silently skipped", result)
	}
	progress, found, err := d.ExtractProgress(ctx, "sess-1", m.Fingerprint())
	if err != nil || !found {
		t.Fatalf("ExtractProgress: found=%v err=%v", found, err)
	}
	if progress.State != db.ExtractProgressFailed {
		t.Fatalf("state = %s, want failed", progress.State)
	}
	if progress.LastError == "" {
		t.Fatal("the failure must record why the session was refused")
	}

	// The row heals. Watermarks far ahead of every write prove the retry
	// flows through the queue arm, which discovery bounds never gate.
	session.MessageCount = 2
	if err := d.UpsertSession(*session); err != nil {
		t.Fatal(err)
	}
	settleSessionWrite()
	m.watermark = time.Now().Add(time.Hour)
	m.fullWatermark = time.Now().Add(time.Hour)
	time.Sleep(10 * time.Millisecond)
	result, err = m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass retry: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("result = %+v; the healed session must extract on retry",
			result)
	}
}

func TestManagerReopensDoneSessionOnCountMismatch(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, func(cfg *ManagerConfig) {
		cfg.FailureBackoff = 5 * time.Millisecond
	})

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	doneCalls := log.count()

	// The transcript is untouched (same digest), but the session row's
	// count drifts out of step. The completed row must not stay done with a
	// freshly settled stamp — that would claim the inconsistent state as
	// covered forever — and the transcript must not reach the model.
	session, err := d.GetSessionFull(ctx, "sess-1")
	if err != nil || session == nil {
		t.Fatalf("GetSessionFull: %v", err)
	}
	session.MessageCount = 4
	if err := d.UpsertSession(*session); err != nil {
		t.Fatal(err)
	}
	// The sync batch path stamps local_modified_at on every session write;
	// the bare test upsert does not, so stamp it explicitly or the
	// done-revisit gate never re-opens the session.
	if err := d.BumpLocalModifiedAt("sess-1"); err != nil {
		t.Fatal(err)
	}
	settleSessionWrite()
	result, err := m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if result.Failed != 1 || log.count() != doneCalls {
		t.Fatalf("result = %+v with %d new calls; a same-digest count "+
			"mismatch must reopen the done row as a retryable failure",
			result, log.count()-doneCalls)
	}
	progress, found, err := d.ExtractProgress(ctx, "sess-1", m.Fingerprint())
	if err != nil || !found {
		t.Fatalf("ExtractProgress: found=%v err=%v", found, err)
	}
	if progress.State != db.ExtractProgressFailed {
		t.Fatalf("state = %s, want failed", progress.State)
	}

	// The row heals; the retry converges back to done.
	session.MessageCount = 2
	if err := d.UpsertSession(*session); err != nil {
		t.Fatal(err)
	}
	settleSessionWrite()
	time.Sleep(10 * time.Millisecond)
	result, err = m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass retry: %v", err)
	}
	if result.Sessions != 1 {
		t.Fatalf("result = %+v; the healed session must extract on retry",
			result)
	}
	progress, found, err = d.ExtractProgress(ctx, "sess-1", m.Fingerprint())
	if err != nil || !found {
		t.Fatalf("ExtractProgress after retry: found=%v err=%v", found, err)
	}
	if progress.State != db.ExtractProgressDone {
		t.Fatalf("state = %s, want done after the retry", progress.State)
	}
}

func TestManagerRevisitSyncsEntryContext(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, nil)

	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	doneCalls := log.count()

	// A metadata-only session update keeps the unit digest unchanged, so
	// the revisit settles without model calls — but the entries copied the
	// old project and branch, and leaving them would keep the corpus
	// matching Recall filters for the old context.
	session, err := d.GetSessionFull(ctx, "sess-1")
	if err != nil || session == nil {
		t.Fatalf("GetSessionFull: %v", err)
	}
	session.Project = "proj-2"
	session.GitBranch = "feature"
	if err := d.UpsertSession(*session); err != nil {
		t.Fatal(err)
	}
	// The sync batch path stamps local_modified_at on every session write;
	// the bare test upsert does not, so stamp it explicitly or the
	// done-revisit gate never re-opens the session.
	if err := d.BumpLocalModifiedAt("sess-1"); err != nil {
		t.Fatal(err)
	}
	settleSessionWrite()
	result, err := m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if log.count() != doneCalls {
		t.Fatalf("%d new model calls; a same-digest revisit must not "+
			"re-extract", log.count()-doneCalls)
	}
	if result.Sessions != 0 || result.Failed != 0 {
		t.Fatalf("result = %+v, want a settled revisit", result)
	}
	entries, err := d.ListRecallEntries(ctx, db.RecallQuery{Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected extracted entries")
	}
	for _, entry := range entries {
		if entry.Project != "proj-2" || entry.GitBranch != "feature" {
			t.Fatalf("entry %s context = %s/%s, want proj-2/feature",
				entry.ID, entry.Project, entry.GitBranch)
		}
	}
}

func TestManagerDiscardsSessionTrashedMidExtraction(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, func(_ string, call int) (int, string) {
		if call == 2 {
			// The session is trashed while its second unit is at the
			// model: units after this must never be sent.
			if err := d.SoftDeleteSession("sess-1"); err != nil {
				t.Errorf("trashing mid-extraction: %v", err)
			}
		}
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	seedSession(t, d, "sess-1", turnMessages("a", "b", "c", "d"), nil)
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if log.count() != 2 {
		t.Fatalf("model calls = %d, want 2: units after the trash must not "+
			"reach the model", log.count())
	}
	if result.Failed != 1 || result.Sessions != 0 {
		t.Fatalf("result = %+v, want the trashed session recorded as a "+
			"failure with discarded output", result)
	}
	for _, status := range []string{"", "archived"} {
		entries, err := d.ListRecallEntries(ctx,
			db.RecallQuery{Status: status, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecallEntries(%q): %v", status, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%d %q entries persisted from a session trashed "+
				"mid-extraction; want none", len(entries), status)
		}
	}
	progress, found, err := d.ExtractProgress(ctx, "sess-1", m.Fingerprint())
	if err != nil || !found {
		t.Fatalf("ExtractProgress: found=%v err=%v", found, err)
	}
	if progress.State != db.ExtractProgressFailed || progress.UnitCursor != 0 {
		t.Fatalf("progress = %s at cursor %d, want failed at 0 so a "+
			"restored session re-extracts from scratch",
			progress.State, progress.UnitCursor)
	}
}

func TestManagerStopsWhenSecretFindingAppearsMidExtraction(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, log := modelServer(t, func(_ string, call int) (int, string) {
		if call == 2 {
			// A candidate-confidence finding lands mid-extraction under
			// the same rules version: no snapshot field changes except
			// the local write stamp, but the material must not reach the
			// model and already-extracted output must not persist.
			finding := db.SecretFinding{
				SessionID: "sess-1", RuleName: "jwt",
				Confidence: "candidate", LocationKind: "message",
				RedactedMatch: "eyJ…", RulesVersion: secrets.RulesVersion(),
			}
			if err := d.ReplaceSessionSecretFindings(
				"sess-1", []db.SecretFinding{finding}, 0,
				secrets.RulesVersion(),
			); err != nil {
				t.Errorf("recording finding mid-extraction: %v", err)
			}
		}
		return http.StatusOK, completionBody(t, entriesJSON(t, "x"))
	})
	seedSession(t, d, "sess-1", turnMessages("a", "b", "c", "d"), nil)
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if log.count() != 2 {
		t.Fatalf("model calls = %d, want 2: units after the finding must "+
			"not reach the model", log.count())
	}
	if result.Failed != 1 {
		t.Fatalf("result = %+v, want the session recorded as a failure "+
			"with discarded output", result)
	}
	for _, status := range []string{"", "archived"} {
		entries, err := d.ListRecallEntries(ctx,
			db.RecallQuery{Status: status, Limit: 50})
		if err != nil {
			t.Fatalf("ListRecallEntries(%q): %v", status, err)
		}
		if len(entries) != 0 {
			t.Fatalf("%d %q entries persisted after a secret finding "+
				"appeared mid-extraction; want none", len(entries), status)
		}
	}
}

func TestManagerRetriesContextSyncWhenItFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("opening archive: %v", err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("closing archive: %v", err)
		}
	})
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	m := newManager(t, d, server.URL, nil)
	if _, err := m.RunPass(ctx, PassOptions{}); err != nil {
		t.Fatalf("RunPass: %v", err)
	}

	session, err := d.GetSessionFull(ctx, "sess-1")
	if err != nil || session == nil {
		t.Fatalf("GetSessionFull: %v", err)
	}
	session.Project = "proj-2"
	if err := d.UpsertSession(*session); err != nil {
		t.Fatal(err)
	}
	if err := d.BumpLocalModifiedAt("sess-1"); err != nil {
		t.Fatal(err)
	}
	settleSessionWrite()

	// A raw side connection installs a trigger that makes the context sync
	// fail, standing in for any transient write failure at that point.
	raw, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("opening raw connection: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err := raw.Exec(`CREATE TRIGGER block_context_sync
		BEFORE UPDATE OF project ON recall_entries
		BEGIN SELECT RAISE(ABORT, 'sync blocked'); END`); err != nil {
		t.Fatalf("installing trigger: %v", err)
	}
	if _, err := m.RunPass(ctx, PassOptions{Full: true}); err == nil {
		t.Fatal("the pass must surface the failed context sync")
	}
	if _, err := raw.Exec(
		"DROP TRIGGER block_context_sync"); err != nil {
		t.Fatalf("dropping trigger: %v", err)
	}

	// The failed sync must not have settled the coverage stamp: the next
	// full pass has to revisit and repair the entries' context.
	if _, err := m.RunPass(ctx, PassOptions{Full: true}); err != nil {
		t.Fatalf("RunPass after repair: %v", err)
	}
	entries, err := d.ListRecallEntries(ctx, db.RecallQuery{Limit: 50})
	if err != nil {
		t.Fatalf("ListRecallEntries: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("expected extracted entries")
	}
	for _, entry := range entries {
		if entry.Project != "proj-2" {
			t.Fatalf("entry %s project = %s, want proj-2: a failed sync "+
				"settled the stamp and was never retried", entry.ID,
				entry.Project)
		}
	}
}

func TestManagerChangedDoneSessionBlocksActivation(t *testing.T) {
	d := newTestArchive(t)
	ctx := context.Background()
	server, _ := modelServer(t, alwaysEntries(t, "x"))
	seedSession(t, d, "sess-1", turnMessages("a", "b"), nil)
	seedSession(t, d, "sess-2", turnMessages("c", "d"), nil)
	m := newManager(t, d, server.URL, nil)

	result, err := m.RunPass(ctx, PassOptions{Limit: 1})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Sessions != 1 || result.Activated {
		t.Fatalf("result = %+v, want 1 session and no activation", result)
	}

	// The completed session's transcript changes: its extracted corpus is
	// stale, so the generation is not actually covered.
	time.Sleep(5 * time.Millisecond)
	growSession(t, d, "sess-1",
		turnMessages("a", "b", "later", "more")[2:], 2)

	status, err := m.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.EligibleBacklog != 2 {
		t.Fatalf("backlog = %d, want 2: the un-extracted session and the "+
			"changed done session both need work", status.EligibleBacklog)
	}

	result, err = m.RunPass(ctx, PassOptions{})
	if err != nil {
		t.Fatalf("RunPass: %v", err)
	}
	if result.Activated {
		t.Fatal("a generation with a stale completed session must not " +
			"activate; its corpus does not cover the changed transcript")
	}

	time.Sleep(5 * time.Millisecond)
	result, err = m.RunPass(ctx, PassOptions{Full: true})
	if err != nil {
		t.Fatalf("RunPass full: %v", err)
	}
	if !result.Activated {
		t.Fatalf("result = %+v; once the changed session is re-extracted "+
			"the generation must activate", result)
	}
}
