package db

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A session that spans several replay pages reuses its cached usage on the
// later pages; every page must still carry the full totals and each message's
// own usage.
func TestChangeStreamUsageCacheServesLongSessionAcrossPages(t *testing.T) {
	d := testDB(t)
	_, err := d.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "long", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := make([]Message, 2*MaxSessionLimit+1)
	for i := range msgs {
		msgs[i] = Message{
			SessionID: "long", Ordinal: i, Role: "assistant", Content: "reply " + strconv.Itoa(i),
			SourceUUID: "reply-" + strconv.Itoa(i), Model: "sample-model",
			TokenUsage: []byte(`{"input_tokens":1,"output_tokens":2}`),
		}
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))

	var cache ChangeStreamUsageCache
	pages, sent := 0, 0
	cursor := ""
	for {
		batch, err := d.ReadChangeStream(t.Context(), cursor, &cache)
		require.NoError(t, err)
		pages++
		require.Len(t, batch.Sessions, 1)
		require.NotNil(t, batch.Sessions[0].Usage)
		assert.Equal(t, 1001, batch.Sessions[0].Usage.InputTokens, "page %d", pages)
		assert.Equal(t, 2002, batch.Sessions[0].Usage.OutputTokens, "page %d", pages)
		for _, message := range batch.Messages {
			require.NotNil(t, message.Usage, "page %d message %s", pages, message.MessageID)
			assert.Equal(t, 1, message.Usage.InputTokens)
			assert.Equal(t, 2, message.Usage.OutputTokens)
			sent++
		}
		cursor = batch.To
		if !batch.More {
			break
		}
	}
	assert.Equal(t, 3, pages)
	assert.Equal(t, len(msgs), sent)
}

// A write between two reads changes the archive's usage revisions, so the
// second read must recompute instead of returning the cached totals.
func TestChangeStreamUsageCacheRecomputesAfterWrite(t *testing.T) {
	d := testDB(t)
	_, err := d.EnsureConversationExportInitialized(t.Context())
	require.NoError(t, err)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	msgs := []Message{
		{SessionID: "chat", Ordinal: 0, Role: "user", Content: "Run it", SourceUUID: "user-one"},
		{
			SessionID: "chat", Ordinal: 1, Role: "assistant", Content: "Done", SourceUUID: "reply-one", Model: "sample-model",
			TokenUsage: []byte(`{"input_tokens":20,"output_tokens":5}`),
		},
	}
	require.NoError(t, d.InsertMessages(t.Context(), msgs))
	var cache ChangeStreamUsageCache
	first, err := d.ReadChangeStream(t.Context(), "", &cache)
	require.NoError(t, err)
	require.Len(t, first.Sessions, 1)
	require.Equal(t, 20, first.Sessions[0].Usage.InputTokens)

	msgs[1].TokenUsage = []byte(`{"input_tokens":30,"output_tokens":5}`)
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "chat", msgs))
	second, err := d.ReadChangeStream(t.Context(), first.To, &cache)
	require.NoError(t, err)
	require.Len(t, second.Sessions, 1)
	assert.Equal(t, 30, second.Sessions[0].Usage.InputTokens)
	require.Len(t, second.Messages, 1)
	require.NotNil(t, second.Messages[0].Usage)
	assert.Equal(t, 30, second.Messages[0].Usage.InputTokens)
}

// A change stream read waits in the bulk-read lane, so streams cannot take the
// reader connection kept free for the session list and sidebar.
func TestChangeStreamReadWaitsForBulkReadPermit(t *testing.T) {
	d := testDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), Session{ID: "chat", Project: "sample", Machine: "local", Agent: "codex"}))
	releases := make([]func(), 0, bulkReadSlots)
	for range bulkReadSlots {
		release, err := d.acquireBulkRead(t.Context())
		require.NoError(t, err)
		releases = append(releases, release)
	}
	t.Cleanup(func() {
		for _, release := range releases {
			release()
		}
	})

	type result struct {
		batch ChangeStreamBatch
		err   error
	}
	done := make(chan result, 1)
	go func() {
		batch, err := d.ReadChangeStream(t.Context(), "", nil)
		done <- result{batch, err}
	}()
	select {
	case <-done:
		require.Fail(t, "the read ran while every bulk-read permit was held")
	case <-time.After(time.Second):
	}
	releases[0]()
	releases = releases[1:]
	got := <-done
	require.NoError(t, got.err)
	require.Len(t, got.batch.Sessions, 1)
	assert.Equal(t, "chat", got.batch.Sessions[0].SessionID)
}
