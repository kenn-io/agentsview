package server_test

import (
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
)

func TestTrendsTermsEndpoint(t *testing.T) {
	te := setup(t)
	start := "2024-06-01T09:00:00Z"
	te.seedSession(t, "s1", "alpha", 2, func(s *db.Session) {
		s.StartedAt = &start
		s.EndedAt = &start
	})
	te.seedMessages(t, "s1", 2, func(i int, m *db.Message) {
		m.Timestamp = "2024-06-01T09:00:00Z"
		if i == 0 {
			m.Content = "load bearing seam"
		}
		if i == 1 {
			m.Content = "load-bearing seams"
		}
		m.ContentLength = len(m.Content)
	})

	w := te.get(t, trendsURL(url.Values{
		"from":        {"2024-06-01"},
		"to":          {"2024-06-02"},
		"timezone":    {"UTC"},
		"granularity": {"week"},
		"term":        {"load bearing | load-bearing", "seam"},
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[db.TrendsTermsResponse](t, w)
	require.True(t, slices.Equal(trendBucketDates(resp.Buckets), []string{"2024-05-27"}),
		"bucket dates = %#v", trendBucketDates(resp.Buckets))
	byTerm := trendSeriesByTerm(resp.Series)
	assert.Equal(t, 2, byTerm["load bearing"].Total, "load bearing total")
	assert.Equal(t, 2, byTerm["seam"].Total, "seam total")
}

func TestTrendsTermsValidation(t *testing.T) {
	te := setup(t)

	t.Run("missing term", func(t *testing.T) {
		w := te.get(t, trendsURL(url.Values{
			"from": {"2024-06-01"},
			"to":   {"2024-06-02"},
		}))
		assertStatus(t, w, http.StatusBadRequest)
	})

	t.Run("invalid granularity", func(t *testing.T) {
		w := te.get(t, trendsURL(url.Values{
			"from":        {"2024-06-01"},
			"to":          {"2024-06-02"},
			"granularity": {"hour"},
			"term":        {"seam"},
		}))
		assertStatus(t, w, http.StatusBadRequest)
	})
}

func TestTrendsTermsProjectFilter(t *testing.T) {
	te := setup(t)
	start := "2024-06-01T09:00:00Z"
	for _, seeded := range []struct {
		id      string
		project string
	}{
		{"s1", "alpha"},
		{"s2", "beta"},
	} {
		te.seedSession(t, seeded.id, seeded.project, 1, func(s *db.Session) {
			s.StartedAt = &start
			s.EndedAt = &start
		})
		te.seedMessages(t, seeded.id, 1, func(_ int, m *db.Message) {
			m.Timestamp = start
			m.Content = "seam"
			m.ContentLength = len(m.Content)
		})
	}

	w := te.get(t, trendsURL(url.Values{
		"from":     {"2024-06-01"},
		"to":       {"2024-06-01"},
		"timezone": {"UTC"},
		"project":  {"alpha"},
		"term":     {"seam"},
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[db.TrendsTermsResponse](t, w)
	assert.Equal(t, 1, trendSeriesByTerm(resp.Series)["seam"].Total, "project-filtered total")
}

func trendsURL(q url.Values) string {
	return "/api/v1/trends/terms?" + q.Encode()
}

func trendBucketDates(buckets []db.TrendBucket) []string {
	dates := make([]string, len(buckets))
	for i, bucket := range buckets {
		dates[i] = bucket.Date
	}
	return dates
}

func trendSeriesByTerm(series []db.TrendSeries) map[string]db.TrendSeries {
	byTerm := make(map[string]db.TrendSeries, len(series))
	for _, entry := range series {
		byTerm[entry.Term] = entry
	}
	return byTerm
}
