package extract

import (
	"context"
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
}

// growSession appends messages and stamps the transcript write, the way a
// sync pass would.
func growSession(t *testing.T, d *db.DB, id string, msgs []db.Message, startOrdinal int) {
	t.Helper()
	for i := range msgs {
		msgs[i].SessionID = id
		msgs[i].Ordinal = startOrdinal + i
	}
	if err := d.InsertMessages(msgs); err != nil {
		t.Fatalf("growing session %s: %v", id, err)
	}
	if err := d.BumpLocalModifiedAt(id); err != nil {
		t.Fatalf("bumping local_modified_at for %s: %v", id, err)
	}
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
