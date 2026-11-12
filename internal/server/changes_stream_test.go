package server_test

import (
	"context"
	"database/sql"
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

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/server"
	"go.kenn.io/agentsview/internal/sync"
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
	MachineID      *string                `json:"machine_id"`
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
	MachineID *string                `json:"machine_id"`
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
	SchemaVersion  int     `json:"schema_version"`
	ArchiveID      string  `json:"archive_id"`
	DatabaseID     string  `json:"database_id"`
	Head           string  `json:"head"`
	LocalMachineID *string `json:"local_machine_id"`
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
	seedChangeStreamSessionFrom(t, te, id, "local")
}

// seedChangeStreamSessionFrom seeds the same session under a stored machine
// key, as sync records a session from this machine or a copy from another.
func seedChangeStreamSessionFrom(t *testing.T, te *testEnv, id, machine string) {
	t.Helper()
	ctx := t.Context()
	started := "2026-05-01T10:00:00Z"
	require.NoError(t, te.db.UpsertSession(ctx, db.Session{
		ID: id, Project: "sample", Machine: machine, Agent: "claude", StartedAt: &started, MessageCount: 2,
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
	assert.ElementsMatch(t, []string{"session_id", "machine_id", "deleted", "agent", "automation", "project", "models", "started_at", "last_activity_at", "message_count", "usage"},
		slices.Collect(maps.Keys(objects.Sessions[0])))
	assert.ElementsMatch(t, []string{"session_id", "message_id", "machine_id", "revision", "deleted", "role", "timestamp", "model", "project", "usage"},
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
	// The seed writes the session before its messages, so the stream's first
	// catch-up can send the session alone before the messages arrive.
	live := caughtUp.changesWithMessages(t, 2)
	assert.Equal(t, changes.To, live[0].From)
	var sessions []string
	for _, event := range live {
		for _, message := range event.Messages {
			sessions = append(sessions, message.SessionID)
		}
	}
	assert.Equal(t, []string{"stream-c", "stream-c"}, sessions)
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

// seedLateUsageSession writes a priced session whose reply has no usage yet,
// then opens a stream and returns it with the replay event.
func seedLateUsageSession(t *testing.T, te *testEnv) ([]db.Message, *changeStreamClient, changeStreamChangesWire) {
	t.Helper()
	ctx := t.Context()
	started := "2026-05-02T09:00:00Z"
	require.NoError(t, te.db.UpsertSession(ctx, db.Session{
		ID: "late-usage", Project: "sample", Machine: "local", Agent: "codex", StartedAt: &started, MessageCount: 2,
	}))
	msgs := []db.Message{
		{SessionID: "late-usage", Ordinal: 0, Role: "user", Content: "Run it", SourceUUID: "late-user", Timestamp: "2026-05-02T09:00:00Z"},
		{
			SessionID: "late-usage", Ordinal: 1, Role: "assistant", Content: "Done", SourceUUID: "late-reply",
			Timestamp: "2026-05-02T09:00:01Z", Model: changeStreamModel,
		},
	}
	require.NoError(t, te.db.InsertMessages(ctx, msgs))
	stream := openChangeStream(t, te, "")
	return msgs, stream, stream.changes(t, 1)[0]
}

func lateUsageMessageID(t *testing.T, replay changeStreamChangesWire) string {
	t.Helper()
	for _, message := range replay.Messages {
		if message.SessionID == "late-usage" && message.Role == "assistant" {
			assert.Nil(t, message.Usage, "the reply has no usage before the later write")
			return message.MessageID
		}
	}
	require.FailNow(t, "replay has no late-usage reply")
	return ""
}

// 2000 input at $3/MTok plus 100 output at $15/MTok.
var lateUsage = &changeStreamUsageWire{InputTokens: 2000, OutputTokens: 100, CostMicrodollars: 7500, Priced: true}

func TestChangeStreamResendsMessagesWhoseUsageChangesWithoutText(t *testing.T) {
	for name, write := range map[string]func(*testing.T, *testEnv, []db.Message){
		"token-only rewrite": func(t *testing.T, te *testEnv, msgs []db.Message) {
			t.Helper()
			msgs[1].TokenUsage = []byte(`{"input_tokens":2000,"output_tokens":100}`)
			require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "late-usage", msgs))
		},
		"incremental token-usage update": func(t *testing.T, te *testEnv, _ []db.Message) {
			t.Helper()
			_, err := te.db.WriteSessionIncremental(t.Context(), "late-usage", nil, db.IncrementalSessionUpdate{
				MsgCount: 2, NextOrdinal: 2,
				MessageTokenUsageUpdates: []db.MessageTokenUsageUpdate{{
					Ordinal: 1, TokenUsage: jsontext.Value(`{"input_tokens":2000,"output_tokens":100}`),
					OutputTokens: 100, HasOutputTokens: true,
				}},
			})
			require.NoError(t, err)
		},
		"usage event for the message": func(t *testing.T, te *testEnv, _ []db.Message) {
			t.Helper()
			require.NoError(t, te.db.ReplaceSessionUsageEvents(t.Context(), "late-usage", []db.UsageEvent{{
				SessionID: "late-usage", MessageOrdinal: new(1), Source: "turn", Model: changeStreamModel,
				InputTokens: 2000, OutputTokens: 100, OccurredAt: "2026-05-02T09:00:01Z", DedupKey: "turn-1",
			}}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			te := setupChangeStream(t)
			msgs, stream, replay := seedLateUsageSession(t, te)
			replyID := lateUsageMessageID(t, replay)
			var replyRevision string
			for _, message := range replay.Messages {
				if message.MessageID == replyID {
					replyRevision = message.Revision
				}
			}

			write(t, te, msgs)
			te.broadcaster.Emit("messages")

			update := stream.changes(t, 2)[1]
			assert.Equal(t, replay.To, update.From)
			require.Len(t, update.Messages, 1, "only the message whose usage changed is sent again")
			resent := update.Messages[0]
			assert.Equal(t, replyID, resent.MessageID)
			assert.Equal(t, replyRevision, resent.Revision, "its text did not change, so its revision stays")
			assert.Equal(t, lateUsage, resent.Usage)
			assert.Equal(t, "2026-05-02T09:00:01Z", *resent.Timestamp)
			require.Len(t, update.Sessions, 1)
			assert.Equal(t, lateUsage, update.Sessions[0].Usage)

			// Resuming after the re-send does not send it again.
			resumed := openChangeStream(t, te, "?after="+update.To)
			resumed.ready(t)
			seedChangeStreamSession(t, te, "after-resend")
			te.broadcaster.Emit("messages")
			next := resumed.changes(t, 1)[0]
			assert.Equal(t, update.To, next.From)
			for _, message := range next.Messages {
				assert.Equal(t, "after-resend", message.SessionID)
			}
		})
	}
}

func TestChangeStreamSendsATextAndUsageChangeOnce(t *testing.T) {
	te := setupChangeStream(t)
	msgs, stream, replay := seedLateUsageSession(t, te)
	replyID := lateUsageMessageID(t, replay)

	msgs[1].Content = "Done, all green"
	msgs[1].TokenUsage = []byte(`{"input_tokens":2000,"output_tokens":100}`)
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "late-usage", msgs))
	te.broadcaster.Emit("messages")

	update := stream.changes(t, 2)[1]
	require.Len(t, update.Messages, 1)
	assert.Equal(t, replyID, update.Messages[0].MessageID)
	assert.Equal(t, lateUsage, update.Messages[0].Usage)

	resumed := openChangeStream(t, te, "?after="+update.To)
	resumed.ready(t)
	seedChangeStreamSession(t, te, "after-text")
	te.broadcaster.Emit("messages")
	for _, message := range resumed.changes(t, 1)[0].Messages {
		assert.Equal(t, "after-text", message.SessionID, "the changed reply is not sent a second time")
	}
}

func TestChangeStreamIgnoresRewritesThatKeepUsage(t *testing.T) {
	te := setupChangeStream(t)
	msgs, stream, replay := seedLateUsageSession(t, te)
	lateUsageMessageID(t, replay)
	msgs[1].TokenUsage = []byte(`{"input_tokens":2000,"output_tokens":100}`)
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "late-usage", msgs))
	extra := db.Message{SessionID: "late-usage", Ordinal: 2, Role: "user", Content: "One more", SourceUUID: "late-extra"}
	require.NoError(t, te.db.InsertMessages(t.Context(), []db.Message{extra}))
	te.broadcaster.Emit("messages")
	before := stream.changesWithMessages(t, len(replay.Messages)+2)

	// Dropping a message forces a full rewrite, which deletes and reinserts
	// every message row. Only the dropped message changed.
	require.NoError(t, te.db.ReplaceSessionMessages(t.Context(), "late-usage", msgs))
	te.broadcaster.Emit("messages")

	update := stream.changes(t, len(before)+1)[len(before)]
	require.Len(t, update.Messages, 1, "a rewrite that keeps usage re-sends nothing else")
	assert.True(t, update.Messages[0].Deleted)
	assert.Equal(t, "user", update.Messages[0].Role)
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

func TestChangeStreamInitializesAColdArchive(t *testing.T) {
	te := setup(t)
	seedSessionUsagePricing(t, te.db)
	seedChangeStreamSession(t, te, "cold")
	active, err := te.db.ConversationExportActive(t.Context())
	require.NoError(t, err)
	require.False(t, active, "the archive starts without a conversation index")

	stream := openChangeStream(t, te, "")
	replay := stream.changes(t, 1)[0]
	assert.True(t, replay.Reset)
	require.Len(t, replay.Messages, 2, "the stream builds the index and replays messages")
	assert.Equal(t, []string{"user", "assistant"}, []string{replay.Messages[0].Role, replay.Messages[1].Role})
	assert.Equal(t, int64(6975), replay.Messages[1].Usage.CostMicrodollars)

	seedChangeStreamSession(t, te, "warm")
	te.broadcaster.Emit("messages")
	live := stream.changesWithMessages(t, len(replay.Messages)+2)[1:]
	assert.Len(t, messageIDs(live), 2, "later writes keep the index current")
	cursor := replay.To
	for _, event := range live {
		assert.Equal(t, cursor, event.From)
		cursor = event.To
		for _, message := range event.Messages {
			assert.Equal(t, "warm", message.SessionID)
		}
	}
}

func TestChangeStreamWakesWithoutEventCoalescing(t *testing.T) {
	// The data_changed events coalesce emits within an hour; the change
	// stream must not wait for that window.
	coalesced := server.NewBroadcaster(time.Hour)
	te := setupWithServerOpts(t, []server.Option{server.WithBroadcaster(coalesced)})
	seedSessionUsagePricing(t, te.db)
	seedChangeStreamSession(t, te, "first")
	stream := openChangeStream(t, te, "")
	replay := stream.changes(t, 1)[0]
	events, unsubscribe := coalesced.Subscribe()
	defer unsubscribe()
	coalesced.Emit("sessions")
	<-events // The leading edge goes out at once; later emits wait an hour.

	seedChangeStreamSession(t, te, "second")
	coalesced.Emit("sessions")

	// The leading wake can read the session before its messages commit.
	// Wait for both messages without assuming they share one event.
	updates := stream.changesWithMessages(t, len(replay.Messages)+2)[1:]
	assert.Len(t, messageIDs(updates), 2)
	cursor := replay.To
	for _, event := range updates {
		assert.Equal(t, cursor, event.From)
		cursor = event.To
		for _, message := range event.Messages {
			assert.Equal(t, "second", message.SessionID)
		}
	}
	select {
	case <-events:
		assert.Fail(t, "the coalesced data_changed signal must still wait for its window")
	default:
	}
}

func TestChangeStreamShortensSyncIntervalWhileConnected(t *testing.T) {
	override := &sync.IntervalOverride{}
	te := setupWithServerOpts(t, []server.Option{server.WithSyncIntervalOverride(override)})
	interval := func() time.Duration {
		d, _ := override.Current()
		return d
	}
	require.Zero(t, interval(), "no subscriber keeps the configured interval")

	first := openChangeStream(t, te, "")
	first.ready(t)
	assert.Equal(t, time.Second, interval())
	second := openChangeStream(t, te, "")
	second.ready(t)

	first.close()
	assert.Equal(t, time.Second, interval(), "a remaining subscriber keeps the shorter interval")

	_, changed := override.Current()
	second.close()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the last subscriber leaving must clear the override")
	}
	assert.Zero(t, interval(), "the configured interval returns after the last subscriber leaves")
}

func TestChangeStreamNamesEachRecordsOriginMachine(t *testing.T) {
	const (
		localID  = "0123456789abcdef0123456789abcdef"
		remoteID = "fedcba9876543210fedcba9876543210"
	)
	te := setup(t, func(c *config.Config) { c.InstallationID = localID })
	ctx := t.Context()
	_, err := te.db.EnsureInstallationIdentity(ctx, localID)
	require.NoError(t, err)
	seedSessionUsagePricing(t, te.db)
	seedChangeStreamSessionFrom(t, te, "native", localID)
	seedChangeStreamSessionFrom(t, te, "native-legacy", "local")
	seedChangeStreamSessionFrom(t, te, "copy", remoteID)
	// An older archive recorded the other machine by host name; an alias
	// maps that name to the machine's identity.
	seedChangeStreamSessionFrom(t, te, "copy-legacy", "other-host")
	require.NoError(t, te.db.SetSyncState(ctx, db.MachineAliasKeyPrefix+"other-host", remoteID))
	_, err = te.db.EnsureConversationExportInitialized(ctx)
	require.NoError(t, err)

	stream := openChangeStream(t, te, "")
	ready := stream.ready(t)
	require.NotNil(t, ready.LocalMachineID)
	assert.Equal(t, localID, *ready.LocalMachineID)
	replay := stream.changes(t, 1)[0]
	want := map[string]string{"native": localID, "native-legacy": localID, "copy": remoteID, "copy-legacy": remoteID}
	sessionOrigins := map[string]string{}
	for _, session := range replay.Sessions {
		require.NotNil(t, session.MachineID, session.SessionID)
		sessionOrigins[session.SessionID] = *session.MachineID
	}
	assert.Equal(t, want, sessionOrigins)
	require.Len(t, replay.Messages, 8)
	for _, message := range replay.Messages {
		require.NotNil(t, message.MachineID, message.MessageID)
		assert.Equal(t, want[message.SessionID], *message.MachineID, message.MessageID)
	}

	// A session whose machine changes is sent again with its new origin.
	require.NoError(t, te.db.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE sessions SET machine = ? WHERE id = 'copy-legacy'`, localID)
		return err
	}))
	te.broadcaster.Emit("sessions")
	moved := stream.changes(t, 2)[1]
	require.Len(t, moved.Sessions, 1)
	assert.Equal(t, "copy-legacy", moved.Sessions[0].SessionID)
	require.NotNil(t, moved.Sessions[0].MachineID)
	assert.Equal(t, localID, *moved.Sessions[0].MachineID)

	// A trashed session keeps its record, so it and its messages keep their
	// origin. A permanently deleted session's record is gone, so its origin
	// is unknown.
	require.NoError(t, te.db.SoftDeleteSession(ctx, "copy"))
	require.NoError(t, te.db.DeleteSession(ctx, "native"))
	te.broadcaster.Emit("sessions")
	update := stream.changes(t, 3)[2]
	assert.Equal(t, moved.To, update.From)
	deleted := map[string]*string{}
	for _, session := range update.Sessions {
		assert.True(t, session.Deleted, session.SessionID)
		deleted[session.SessionID] = session.MachineID
	}
	assert.Equal(t, map[string]*string{"copy": new(remoteID), "native": nil}, deleted)
	require.Len(t, update.Messages, 4)
	for _, message := range update.Messages {
		assert.True(t, message.Deleted)
		assert.Equal(t, deleted[message.SessionID], message.MachineID, message.SessionID+"/"+message.MessageID)
	}
}
