package db

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/kept"
)

type trendPartsFixture struct {
	versions map[string]string
	messages map[string][]struct{ content, timestamp string }
	reads    [][]string
}

type trendWindowRace struct {
	*DB
	beforeWindow func()
}

func (s trendWindowRace) TrendWindowSessions(ctx context.Context, f AnalyticsFilter, ids []string) ([]string, error) {
	s.beforeWindow()
	return s.DB.TrendWindowSessions(ctx, f, ids)
}

func TestTrendsPartsConcurrentWindowMiss(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	d := testDB(t)
	ctx := t.Context()
	for _, id := range []string{"a", "b"} {
		insertSession(t, d, id, "project", func(s *Session) { s.MessageCount, s.UserMessageCount = 1, 1 })
		insertMessages(t, d, Message{SessionID: id, Role: "user", Content: "apple", Timestamp: "2025-03-10T12:00:00Z"})
	}
	terms, err := ParseTrendTerms([]string{"apple"})
	require.NoError(t, err)
	f := AnalyticsFilter{From: "2025-03-10", To: "2025-03-10", Timezone: "UTC"}
	first, err := d.trendsTerms(ctx, f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 2, first.Series[0].Total)
	require.NoError(t, d.ReplaceSessionMessages(ctx, "a", []Message{{SessionID: "a", Role: "user", Content: "apple apple", Timestamp: "2025-03-10T12:00:00Z"}}))
	var writes []SessionBatchWrite
	for _, item := range []struct{ id, content, timestamp string }{
		{"a", "banana", "2025-01-01T12:00:00Z"}, {"b", "apple apple", "2025-03-10T12:00:00Z"},
	} {
		session, err := d.GetSession(ctx, item.id)
		require.NoError(t, err)
		require.NotNil(t, session)
		writes = append(writes, SessionBatchWrite{Session: *session, ReplaceMessages: true, Messages: []Message{{SessionID: item.id, Role: "user", Content: item.content, Timestamp: item.timestamp}}})
	}
	identity, err := d.GetDatabaseID(ctx)
	require.NoError(t, err)
	var writeErr error
	_, handled, err := TrendsFromParts(ctx, &d.trendsDisk, identity, f, terms, "day", trendWindowRace{d, func() {
		_, writeErr = d.WriteSessionBatchAtomic(ctx, writes)
	}})
	require.NoError(t, writeErr)
	require.NoError(t, err)
	require.False(t, handled)
	direct, err := OpenReadOnly(ctx, d.Path())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, direct.Close()) })
	got, err := direct.trendsTerms(ctx, f, terms, "day")
	require.NoError(t, err)
	require.Equal(t, 2, got.Series[0].Total)
}

func (s *trendPartsFixture) TrendSessionVersions(context.Context, AnalyticsFilter) (map[string]string, error) {
	return s.versions, nil
}

func (s *trendPartsFixture) TrendWindowSessions(_ context.Context, _ AnalyticsFilter, ids []string) ([]string, error) {
	return ids, nil
}

func (s *trendPartsFixture) VisitTrendMessages(_ context.Context, f AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	s.reads = append(s.reads, append([]string(nil), ids...))
	for _, id := range ids {
		for _, msg := range s.messages[id] {
			ts, err := time.Parse(time.RFC3339, msg.timestamp)
			if err != nil {
				return err
			}
			visit(id, msg.content, ts.In(f.location()))
		}
	}
	return nil
}

func TestTrendsPartsReuseAndBuckets(t *testing.T) {
	t.Setenv("CACHE_DIRECTORY", t.TempDir())
	var disk kept.Disk
	require.NoError(t, disk.Open("trends-test", "fixture", 128<<20, 256<<20))
	terms, err := ParseTrendTerms([]string{"apple"})
	require.NoError(t, err)
	source := &trendPartsFixture{versions: map[string]string{"a": "1", "b": "1"}, messages: map[string][]struct{ content, timestamp string }{
		"a": {{"apple apples", "2025-03-03T12:00:00Z"}, {"apple", "2025-03-04T12:00:00Z"}},
		"b": {{"apple", "2025-03-10T12:00:00Z"}},
	}}
	f := AnalyticsFilter{From: "2025-03-03", To: "2025-03-10", Timezone: "UTC"}
	got, handled, err := TrendsFromParts(t.Context(), &disk, "fixture", f, terms, "week", source)
	require.NoError(t, err)
	require.True(t, handled)
	assert.Equal(t, 3, got.MessageCount)
	assert.Equal(t, 4, got.Series[0].Total)
	assert.Equal(t, []TrendPoint{{Date: "2025-03-03", Count: 3}, {Date: "2025-03-10", Count: 1}}, got.Series[0].Points)
	require.Equal(t, [][]string{{"a", "b"}}, source.reads)
	var reopened kept.Disk
	require.NoError(t, reopened.Open("trends-test", "fixture", 128<<20, 256<<20))
	monday := 0
	f.DayOfWeek = &monday
	got, _, err = TrendsFromParts(t.Context(), &reopened, "fixture", f, terms, "month", source)
	require.NoError(t, err)
	assert.Equal(t, 2, got.MessageCount)
	assert.Equal(t, 3, got.Series[0].Total)
	assert.Len(t, source.reads, 1)
	delete(source.versions, "b")
	source.versions["a"] = "2"
	_, _, err = TrendsFromParts(t.Context(), &reopened, "fixture", f, terms, "month", source)
	require.NoError(t, err)
	require.Equal(t, [][]string{{"a", "b"}, {"a"}}, source.reads)
	source.versions["a"] = "3"
	canceled, cancel := context.WithCancel(t.Context())
	reset := SetTrendPartsReadObserver(func([]string) { cancel() })
	_, handled, err = TrendsFromParts(canceled, &reopened, "fixture", f, terms, "month", source)
	reset()
	require.True(t, handled)
	require.ErrorIs(t, err, context.Canceled)
	hour := 12
	f.Hour = &hour
	_, handled, err = TrendsFromParts(t.Context(), &reopened, "fixture", f, terms, "day", source)
	require.NoError(t, err)
	assert.False(t, handled)
	f.Hour = nil
	f.Model = "model"
	_, handled, err = TrendsFromParts(t.Context(), &reopened, "fixture", f, terms, "day", source)
	require.NoError(t, err)
	assert.False(t, handled)
	f.Model = ""
	for _, filter := range []AnalyticsFilter{
		{Timezone: "Local"},
		{Timezone: "UTC", ActiveSince: "2025-01-01T00:00:00Z"},
		{Timezone: "UTC", Termination: "active"},
	} {
		_, handled, err = TrendsFromParts(t.Context(), &reopened, "fixture", filter, terms, "day", source)
		require.NoError(t, err)
		assert.False(t, handled)
	}
}
