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

func (s *trendPartsFixture) TrendSessionVersions(context.Context, AnalyticsFilter) (map[string]string, error) {
	return s.versions, nil
}
func (s *trendPartsFixture) VisitTrendMessages(_ context.Context, _ AnalyticsFilter, ids []string, visit func(string, string, time.Time)) error {
	s.reads = append(s.reads, append([]string(nil), ids...))
	for _, id := range ids {
		for _, msg := range s.messages[id] {
			ts, err := time.Parse(time.RFC3339, msg.timestamp)
			if err != nil {
				return err
			}
			visit(id, msg.content, ts)
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
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, handled, err = TrendsFromParts(canceled, &reopened, "fixture", f, terms, "month", source)
	assert.True(t, handled)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Len(t, source.reads, 1)
	delete(source.versions, "b")
	source.versions["a"] = "2"
	source.messages["a"] = []struct{ content, timestamp string }{{"apple", "2025-03-03T12:00:00Z"}}
	got, _, err = TrendsFromParts(t.Context(), &reopened, "fixture", f, terms, "day", source)
	require.NoError(t, err)
	assert.Equal(t, 1, got.MessageCount)
	assert.Equal(t, 1, got.Series[0].Total)
	require.Equal(t, [][]string{{"a", "b"}, {"a"}}, source.reads)
	source.versions["b"] = "1"
	got, _, err = TrendsFromParts(t.Context(), &reopened, "fixture", f, terms, "day", source)
	require.NoError(t, err)
	assert.Equal(t, 2, got.MessageCount)
	assert.Equal(t, 2, got.Series[0].Total)
	assert.Len(t, source.reads, 2)
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
}
