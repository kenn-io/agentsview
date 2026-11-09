package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

type changeStreamUsageWire struct {
	InputTokens         int   `json:"input_tokens"`
	OutputTokens        int   `json:"output_tokens"`
	CacheCreationTokens int   `json:"cache_creation_tokens"`
	CacheReadTokens     int   `json:"cache_read_tokens"`
	CostMicrodollars    int64 `json:"cost_microdollars"`
	Priced              bool  `json:"priced"`
}

type changeStreamSessionWire struct {
	SessionID      string                 `json:"session_id"`
	Deleted        bool                   `json:"deleted"`
	Agent          string                 `json:"agent"`
	Automation     string                 `json:"automation"`
	Project        map[string]any         `json:"project"`
	Models         []string               `json:"models"`
	StartedAt      *string                `json:"started_at"`
	LastActivityAt *string                `json:"last_activity_at"`
	MessageCount   int                    `json:"message_count"`
	Usage          *changeStreamUsageWire `json:"usage"`
}

type changeStreamMessageWire struct {
	SessionID string                 `json:"session_id"`
	MessageID string                 `json:"message_id"`
	Revision  string                 `json:"revision"`
	Deleted   bool                   `json:"deleted"`
	Role      string                 `json:"role"`
	Timestamp *string                `json:"timestamp"`
	Model     string                 `json:"model"`
	Project   map[string]any         `json:"project"`
	Usage     *changeStreamUsageWire `json:"usage"`
}

type changeStreamChangesWire struct {
	SchemaVersion int                       `json:"schema_version"`
	ArchiveID     string                    `json:"archive_id"`
	DatabaseID    string                    `json:"database_id"`
	From          string                    `json:"from"`
	To            string                    `json:"to"`
	Reset         bool                      `json:"reset"`
	Sessions      []changeStreamSessionWire `json:"sessions"`
	Messages      []changeStreamMessageWire `json:"messages"`
}

type changeStreamReadyWire struct {
	SchemaVersion int    `json:"schema_version"`
	ArchiveID     string `json:"archive_id"`
	DatabaseID    string `json:"database_id"`
	Head          string `json:"head"`
}

type changeStreamClient struct {
	w      *flushRecorder
	cancel context.CancelFunc
	done   chan struct{}
}

func openChangeStream(t *testing.T, te *testEnv, query string) *changeStreamClient {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/changes/stream"+query, nil)
	client := &changeStreamClient{w: newFlushRecorder(), cancel: cancel, done: make(chan struct{})}
	go func() {
		te.handler.ServeHTTP(client.w, req)
		close(client.done)
	}()
	t.Cleanup(client.close)
	return client
}

func (c *changeStreamClient) close() {
	c.cancel()
	<-c.done
}

// events waits until the stream has sent at least n events named name and
// returns every event sent so far.
func (c *changeStreamClient) events(t *testing.T, name string, n int) []SSEEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		events := parseSSE(c.w.BodyString())
		count := 0
		for _, event := range events {
			if event.Event == name {
				count++
			}
		}
		if count >= n {
			return events
		}
		if time.Now().After(deadline) {
			require.FailNowf(t, "change stream", "timed out waiting for %d %s events, got: %s", n, name, c.w.BodyString())
		}
		// Each response write queues a signal, so a write after the parse
		// above wakes this wait.
		select {
		case <-c.w.writes:
		case <-time.After(time.Until(deadline)):
		}
	}
}

func (c *changeStreamClient) changes(t *testing.T, n int) []changeStreamChangesWire {
	t.Helper()
	var out []changeStreamChangesWire
	for _, event := range c.events(t, "changes", n) {
		if event.Event != "changes" {
			continue
		}
		var changes changeStreamChangesWire
		require.NoError(t, json.Unmarshal([]byte(event.Data), &changes))
		out = append(out, changes)
	}
	return out
}

// changesWithMessages waits until the changes events carry at least n
// messages in total.
func (c *changeStreamClient) changesWithMessages(t *testing.T, n int) []changeStreamChangesWire {
	t.Helper()
	for events := 1; ; events++ {
		changes := c.changes(t, events)
		if len(messageIDs(changes)) >= n {
			return changes
		}
	}
}

func (c *changeStreamClient) ready(t *testing.T) changeStreamReadyWire {
	t.Helper()
	events := c.events(t, "ready", 1)
	require.Equal(t, "ready", events[0].Event, "ready must be the first event")
	var ready changeStreamReadyWire
	require.NoError(t, json.Unmarshal([]byte(events[0].Data), &ready))
	return ready
}

const changeStreamModel = controlledSessionUsageModel

// seedChangeStreamSession writes a priced session whose assistant reply used
// 1000 input, 200 output, 100 cache-write and 2000 cache-read tokens. At the
// seeded rates that costs 3000 + 3000 + 375 + 600 = 6975 microdollars.
func seedChangeStreamSession(t *testing.T, te *testEnv, id string) {
	t.Helper()
	ctx := t.Context()
	started := "2026-05-01T10:00:00Z"
	require.NoError(t, te.db.UpsertSession(ctx, db.Session{
		ID: id, Project: "sample", Machine: "local", Agent: "claude", StartedAt: &started, MessageCount: 2,
	}))
	require.NoError(t, te.db.InsertMessages(ctx, []db.Message{
		{SessionID: id, Ordinal: 0, Role: "user", Content: "Check the build", SourceUUID: id + "-user", Timestamp: "2026-05-01T10:00:00Z"},
		{
			SessionID: id, Ordinal: 1, Role: "assistant", Content: "The build passes", SourceUUID: id + "-reply",
			Timestamp: "2026-05-01T10:00:05Z", Model: changeStreamModel,
			TokenUsage: []byte(`{"input_tokens":1000,"output_tokens":200,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000}`),
		},
	}))
}

func setupChangeStream(t *testing.T) *testEnv {
	t.Helper()
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	seedChangeStreamSession(t, te, "stream-a")
	_, err := te.db.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	return te
}

func messageIDs(events []changeStreamChangesWire) []string {
	var ids []string
	for _, event := range events {
		for _, message := range event.Messages {
			ids = append(ids, message.SessionID+"/"+message.MessageID)
		}
	}
	return ids
}

func TestChangeStreamReplaysArchiveFromEmptyCursor(t *testing.T) {
	te := setupChangeStream(t)
	stream := openChangeStream(t, te, "")

	ready := stream.ready(t)
	assert.Equal(t, 1, ready.SchemaVersion)
	assert.NotEmpty(t, ready.ArchiveID)
	assert.NotEmpty(t, ready.DatabaseID)

	events := stream.events(t, "changes", 1)
	var raw map[string]jsontext.Value
	require.NoError(t, json.Unmarshal([]byte(events[1].Data), &raw))
	assert.ElementsMatch(t, []string{"schema_version", "archive_id", "database_id", "from", "to", "reset", "sessions", "messages"},
		slices.Collect(maps.Keys(raw)))
	var objects struct {
		Sessions []map[string]jsontext.Value `json:"sessions"`
		Messages []map[string]jsontext.Value `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[1].Data), &objects))
	require.Len(t, objects.Sessions, 1)
	require.Len(t, objects.Messages, 2)
	assert.ElementsMatch(t, []string{"session_id", "deleted", "agent", "automation", "project", "models", "started_at", "last_activity_at", "message_count", "usage"},
		slices.Collect(maps.Keys(objects.Sessions[0])))
	assert.ElementsMatch(t, []string{"session_id", "message_id", "revision", "deleted", "role", "timestamp", "model", "project", "usage"},
		slices.Collect(maps.Keys(objects.Messages[0])))

	changes := stream.changes(t, 1)[0]
	assert.True(t, changes.Reset)
	assert.Empty(t, changes.From)
	assert.Equal(t, ready.Head, changes.To, "a replay of an idle archive ends at the head announced by ready")
	assert.Equal(t, ready.ArchiveID, changes.ArchiveID)
	assert.Equal(t, ready.DatabaseID, changes.DatabaseID)

	session := changes.Sessions[0]
	assert.Equal(t, "stream-a", session.SessionID)
	assert.False(t, session.Deleted)
	assert.Equal(t, "claude", session.Agent)
	assert.Equal(t, "interactive", session.Automation)
	assert.Equal(t, "sample", session.Project["display_label"])
	assert.Equal(t, []string{changeStreamModel}, session.Models)
	require.NotNil(t, session.StartedAt)
	assert.Equal(t, "2026-05-01T10:00:00Z", *session.StartedAt)
	require.NotNil(t, session.LastActivityAt)
	assert.Equal(t, "2026-05-01T10:00:05Z", *session.LastActivityAt)
	assert.Equal(t, 2, session.MessageCount)
	assert.Equal(t, &changeStreamUsageWire{
		InputTokens: 1000, OutputTokens: 200, CacheCreationTokens: 100, CacheReadTokens: 2000,
		CostMicrodollars: 6975, Priced: true,
	}, session.Usage)

	user, reply := changes.Messages[0], changes.Messages[1]
	assert.Equal(t, "user", user.Role)
	assert.Nil(t, user.Usage, "a message without usage reports null usage")
	assert.Equal(t, "assistant", reply.Role)
	assert.Equal(t, changeStreamModel, reply.Model)
	require.NotNil(t, reply.Timestamp)
	assert.Equal(t, "2026-05-01T10:00:05Z", *reply.Timestamp)
	assert.Equal(t, "sample", reply.Project["display_label"])
	assert.Equal(t, &changeStreamUsageWire{
		InputTokens: 1000, OutputTokens: 200, CacheCreationTokens: 100, CacheReadTokens: 2000,
		CostMicrodollars: 6975, Priced: true,
	}, reply.Usage)
	assert.NotEqual(t, user.MessageID, reply.MessageID)
}

func TestChangeStreamResumesAfterCursorWithoutRepeats(t *testing.T) {
	te := setupChangeStream(t)
	first := openChangeStream(t, te, "")
	replay := first.changes(t, 1)[0]
	first.close()

	seedChangeStreamSession(t, te, "stream-b")

	resumed := openChangeStream(t, te, "?after="+replay.To)
	changes := resumed.changes(t, 1)[0]
	assert.False(t, changes.Reset)
	assert.Equal(t, replay.To, changes.From, "the first event resumes from the subscriber's cursor")
	assert.Len(t, changes.Messages, 2)
	for _, message := range changes.Messages {
		assert.Equal(t, "stream-b", message.SessionID, "messages before the cursor must not repeat")
	}
	require.Len(t, changes.Sessions, 1)
	assert.Equal(t, "stream-b", changes.Sessions[0].SessionID)

	// Resuming from the newest cursor sends nothing until a change arrives.
	caughtUp := openChangeStream(t, te, "?after="+changes.To)
	caughtUp.ready(t)
	seedChangeStreamSession(t, te, "stream-c")
	te.broadcaster.Emit("messages")
	live := caughtUp.changes(t, 1)
	require.Len(t, live, 1)
	assert.Equal(t, changes.To, live[0].From)
	assert.Equal(t, []string{"stream-c", "stream-c"}, []string{live[0].Messages[0].SessionID, live[0].Messages[1].SessionID})
}

func TestChangeStreamResetsForeignAndInvalidCursors(t *testing.T) {
	te := setupChangeStream(t)
	head := openChangeStream(t, te, "").ready(t).Head
	// The archive's own head, but naming another database generation.
	foreign := base64.RawURLEncoding.EncodeToString([]byte(
		`{"version":1,"kind":"changes","database_id":"0b8f2c4e-5d6a-4b7c-8e9f-0a1b2c3d4e5f","conversation":"2","usage":"4"}`))

	for name, cursor := range map[string]string{"foreign generation": foreign, "malformed": "not-a-cursor"} {
		t.Run(name, func(t *testing.T) {
			stream := openChangeStream(t, te, "?after="+cursor)
			changes := stream.changes(t, 1)[0]
			assert.True(t, changes.Reset)
			assert.Empty(t, changes.From)
			assert.Len(t, changes.Messages, 2, "a reset replays the archive from the beginning")
			assert.Equal(t, head, changes.To)
		})
	}
}

func TestChangeStreamDeliversLiveChangesInOrder(t *testing.T) {
	te := setupChangeStream(t)
	stream := openChangeStream(t, te, "")
	replay := stream.changes(t, 1)[0]

	// This write lands after the replay read and announces nothing. The next
	// wake must still deliver it, because each read asks for everything after
	// the last cursor sent.
	seedChangeStreamSession(t, te, "unannounced")
	seedChangeStreamSession(t, te, "announced")
	te.broadcaster.Emit("messages")

	live := stream.changesWithMessages(t, 2+4)[1:]
	assert.Equal(t, replay.To, live[0].From)
	for i := 1; i < len(live); i++ {
		assert.Equal(t, live[i-1].To, live[i].From, "consecutive events must chain")
	}
	sessions := map[string]int{}
	for _, event := range live {
		for _, message := range event.Messages {
			sessions[message.SessionID]++
		}
	}
	assert.Equal(t, map[string]int{"unannounced": 2, "announced": 2}, sessions)
}

func TestChangeStreamCarriesUsageAddedAfterMessage(t *testing.T) {
	te := setupChangeStream(t)
	ctx := t.Context()
	started := "2026-05-02T09:00:00Z"
	require.NoError(t, te.db.UpsertSession(ctx, db.Session{
		ID: "late-usage", Project: "sample", Machine: "local", Agent: "claude", StartedAt: &started, MessageCount: 1,
	}))
	reply := db.Message{
		SessionID: "late-usage", Ordinal: 0, Role: "assistant", Content: "Done", SourceUUID: "late-reply",
		Timestamp: "2026-05-02T09:00:01Z", Model: changeStreamModel,
	}
	require.NoError(t, te.db.InsertMessages(ctx, []db.Message{reply}))

	stream := openChangeStream(t, te, "")
	replay := stream.changes(t, 1)[0]
	var before *changeStreamSessionWire
	for i := range replay.Sessions {
		if replay.Sessions[i].SessionID == "late-usage" {
			before = &replay.Sessions[i]
		}
	}
	require.NotNil(t, before)
	assert.Equal(t, &changeStreamUsageWire{}, before.Usage)

	// The message text is unchanged, so its conversation revision stays put;
	// the usage change must still reach the subscriber.
	reply.TokenUsage = []byte(`{"input_tokens":2000,"output_tokens":100}`)
	require.NoError(t, te.db.ReplaceSessionMessages(ctx, "late-usage", []db.Message{reply}))
	te.broadcaster.Emit("messages")

	update := stream.changes(t, 2)[1]
	assert.Equal(t, replay.To, update.From)
	assert.Empty(t, update.Messages, "a usage-only change publishes no message revision")
	require.Len(t, update.Sessions, 1)
	assert.Equal(t, "late-usage", update.Sessions[0].SessionID)
	// 2000 input at $3/MTok plus 100 output at $15/MTok.
	assert.Equal(t, &changeStreamUsageWire{InputTokens: 2000, OutputTokens: 100, CostMicrodollars: 7500, Priced: true},
		update.Sessions[0].Usage)
}

func TestChangeStreamPagesLargeReplays(t *testing.T) {
	te := setup(t)
	ctx := t.Context()
	require.NoError(t, te.db.UpsertSession(ctx, db.Session{ID: "long", Project: "sample", Machine: "local", Agent: "claude"}))
	msgs := make([]db.Message, db.MaxSessionLimit+20)
	for i := range msgs {
		msgs[i] = db.Message{SessionID: "long", Ordinal: i, Role: "user", Content: fmt.Sprintf("Prompt %d", i), SourceUUID: fmt.Sprintf("prompt-%d", i)}
	}
	require.NoError(t, te.db.InsertMessages(ctx, msgs))
	_, err := te.db.EnsureConversationExportInitialized(ctx)
	require.NoError(t, err)

	stream := openChangeStream(t, te, "")
	ready := stream.ready(t)
	events := stream.changes(t, 2)
	require.Len(t, events, 2)
	assert.True(t, events[0].Reset)
	assert.Len(t, events[0].Messages, db.MaxSessionLimit, "one event holds at most one page")
	assert.False(t, events[1].Reset)
	assert.Equal(t, events[0].To, events[1].From)
	assert.Equal(t, ready.Head, events[1].To)
	ids := messageIDs(events)
	assert.Len(t, ids, len(msgs))
	slices.Sort(ids)
	assert.Len(t, slices.Compact(ids), len(msgs), "no message repeats across pages")
}

func TestChangeStreamQueryTokenAuth(t *testing.T) {
	te := setup(t, withAuth("secret"))
	_, err := te.db.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)

	stream := openChangeStream(t, te, "?token=secret")
	assert.Equal(t, 1, stream.ready(t).SchemaVersion)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/changes/stream?token=wrong", nil)
	w := httptest.NewRecorder()
	te.handler.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestChangeStreamReportsTrashedSessionsAsDeleted(t *testing.T) {
	te := setupChangeStream(t)
	stream := openChangeStream(t, te, "")
	replay := stream.changes(t, 1)[0]

	require.NoError(t, te.db.SoftDeleteSession(t.Context(), "stream-a"))
	te.broadcaster.Emit("sessions")

	update := stream.changes(t, 2)[1]
	assert.Equal(t, replay.To, update.From)
	assert.Equal(t, []changeStreamSessionWire{{SessionID: "stream-a", Deleted: true, Models: []string{}}}, update.Sessions)
	require.Len(t, update.Messages, 2)
	for _, message := range update.Messages {
		assert.True(t, message.Deleted)
		assert.Empty(t, message.Model)
		assert.Nil(t, message.Project)
		assert.Nil(t, message.Usage)
	}
}
