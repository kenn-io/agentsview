package service_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/service"
)

// seedTermTrendSessions stores a multi-turn session in proj-a, one in
// proj-b, and a one-shot session in proj-a, all on 2024-06-03 and all
// mentioning "seam", so project filtering and the one-shot default show up
// in the totals.
func seedTermTrendSessions(t *testing.T, d *db.DB) {
	t.Helper()
	const start = "2024-06-03T09:00:00Z"
	seed := func(id, project string, userMessages int, texts ...string) {
		dbtest.SeedSession(t, d, id, project, func(s *db.Session) {
			s.StartedAt = new(start)
			s.CreatedAt = start
			s.MessageCount = len(texts)
			s.UserMessageCount = userMessages
		})
		msgs := make([]db.Message, 0, len(texts))
		for i, text := range texts {
			m := dbtest.UserMsg(id, i, text)
			if i%2 == 1 {
				m = dbtest.AsstMsg(id, i, text)
			}
			m.Timestamp = start
			msgs = append(msgs, m)
		}
		dbtest.SeedMessages(t, d, msgs...)
	}
	seed("a1", "proj-a", 2, "is this seam load-bearing?", "the seams are load bearing")
	seed("b1", "proj-b", 2, "seam", "ok")
	seed("one", "proj-a", 1, "seam seam seam")
}

func TestTermTrendsBothBackends(t *testing.T) {
	t.Parallel()
	env := newHTTPBackendEnv(t)
	seedTermTrendSessions(t, env.DB)
	backends := map[string]service.SessionService{
		"direct": service.NewDirectBackend(env.DB, nil),
		"http":   env.Backend("", false),
	}
	for name, svc := range backends {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := svc.TermTrends(t.Context(), service.TermTrendsRequest{
				From: "2024-06-01", To: "2024-06-09", Project: "proj-a",
				Terms: []string{"load bearing | load-bearing", "seam"},
			})
			require.NoError(t, err)
			require.NotNil(t, got)
			assert.Equal(t, "week", got.Granularity, "default granularity")
			// Only a1 counts: b1 is another project, and the one-shot
			// session is excluded by default.
			assert.Equal(t, 2, got.MessageCount)
			totals := map[string]int{}
			for _, s := range got.Series {
				totals[s.Term] = s.Total
			}
			assert.Equal(t, map[string]int{"load bearing": 2, "seam": 2}, totals)

			var inputErr *service.AnalyticsInputError
			for _, bad := range []service.TermTrendsRequest{
				{},
				{Terms: []string{"seam"}, Granularity: "year"},
				{Terms: []string{"seam"}, From: "2024-06-09", To: "2024-06-01"},
			} {
				_, err := svc.TermTrends(t.Context(), bad)
				require.ErrorAs(t, err, &inputErr, "request %+v", bad)
			}
		})
	}
}
