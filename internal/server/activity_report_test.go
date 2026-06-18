package server_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/db"
)

// activityDate is a fixed past calendar day so the activity report is
// deterministic and complete (non-partial) regardless of wall clock.
const activityDate = "2025-06-02"

// seedActivityReportFixture seeds two sessions that overlap in wall-clock
// on activityDate so peak concurrency is 2. Each session gets distinct,
// increasing per-message timestamps inside the day (under the 300s gap
// cap) so the aggregator produces active intervals; the default
// seedMessages timestamp would yield zero activity.
func seedActivityReportFixture(t *testing.T, te *testEnv) {
	t.Helper()

	type entry struct {
		id, project, agent, started, ended string
		msgTimes                           []string
	}
	entries := []entry{
		{
			id: "d1", project: "alpha", agent: "claude",
			started: activityDate + "T10:00:00Z",
			ended:   activityDate + "T10:08:00Z",
			msgTimes: []string{
				activityDate + "T10:00:00Z",
				activityDate + "T10:02:00Z",
				activityDate + "T10:05:00Z",
				activityDate + "T10:07:00Z",
			},
		},
		{
			id: "d2", project: "beta", agent: "codex",
			started: activityDate + "T10:01:00Z",
			ended:   activityDate + "T10:09:00Z",
			msgTimes: []string{
				activityDate + "T10:01:00Z",
				activityDate + "T10:03:00Z",
				activityDate + "T10:06:00Z",
				activityDate + "T10:08:00Z",
			},
		},
	}

	for _, e := range entries {
		started, ended := e.started, e.ended
		te.seedSession(t, e.id, e.project, len(e.msgTimes),
			func(s *db.Session) {
				s.Agent = e.agent
				s.StartedAt = &started
				s.EndedAt = &ended
			},
		)
		times := e.msgTimes
		te.seedMessages(t, e.id, len(times),
			func(i int, m *db.Message) {
				m.Timestamp = times[i]
			},
		)
	}
}

func TestActivityReportEndpoint_Day(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[activity.Report](t, w)
	assert.Equal(t, 2, resp.Peak.Agents)
	assert.Equal(t, 2, resp.Totals.Sessions)
	assert.Equal(t, "minute", resp.BucketUnit)
	assert.False(t, resp.Partial)
}

func TestActivityReportEndpoint_Week(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "week", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[activity.Report](t, w)
	assert.Equal(t, "hour", resp.BucketUnit, "a 7-day week auto-buckets hourly")
	assert.Equal(t, 168, resp.BucketCount)
}

func TestActivityReportEndpoint_Month(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "month", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[activity.Report](t, w)
	assert.Equal(t, "day", resp.BucketUnit, "a 30-day month auto-buckets daily")
}

func TestActivityReportEndpoint_Custom(t *testing.T) {
	te := setup(t)
	seedActivityReportFixture(t, te)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset":   "custom",
		"from":     activityDate + "T00:00:00Z",
		"to":       activityDate + "T23:59:59Z",
		"timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)
	resp := decode[activity.Report](t, w)
	assert.Equal(t, 2, resp.Totals.Sessions)
}

// TestActivityReportEndpoint_IncludesOneShotAndAutomated locks in the
// activity endpoint's hardest binding constraint: unlike the sibling
// analytics endpoints, it sets ExcludeOneShot=false and
// ExcludeAutomated=false (see huma_routes_activity.go), so one-shot
// (user_message_count <= 1) and automated (is_automated=1) sessions
// MUST appear in the report. A refactor flipping those flags to match
// the analytics defaults would drop these sessions and fail here.
func TestActivityReportEndpoint_IncludesOneShotAndAutomated(t *testing.T) {
	te := setup(t)

	// One-shot: a single user message (user_message_count = 1).
	started, ended := activityDate+"T11:00:00Z", activityDate+"T11:04:00Z"
	te.seedSession(t, "oneshot", "alpha", 2, func(s *db.Session) {
		s.Agent = "claude"
		s.StartedAt = &started
		s.EndedAt = &ended
		s.UserMessageCount = 1
	})
	te.seedMessages(t, "oneshot", 2, func(i int, m *db.Message) {
		m.Timestamp = []string{
			activityDate + "T11:00:00Z", activityDate + "T11:02:00Z",
		}[i]
	})

	// Automated: a first message matching a known automated (roborev)
	// prompt prefix, which UpsertSession turns into is_automated = 1.
	autoStart, autoEnd := activityDate+"T12:00:00Z", activityDate+"T12:04:00Z"
	te.seedSession(t, "automated", "beta", 2, func(s *db.Session) {
		s.Agent = "codex"
		s.StartedAt = &autoStart
		s.EndedAt = &autoEnd
		s.FirstMessage = new("You are a code reviewer.")
		s.UserMessageCount = 1
	})
	te.seedMessages(t, "automated", 2, func(i int, m *db.Message) {
		m.Timestamp = []string{
			activityDate + "T12:00:00Z", activityDate + "T12:02:00Z",
		}[i]
	})

	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusOK)

	resp := decode[activity.Report](t, w)
	ids := make(map[string]struct{}, len(resp.BySession))
	for _, s := range resp.BySession {
		ids[s.SessionID] = struct{}{}
	}
	assert.Contains(t, ids, "oneshot",
		"one-shot session must be included (ExcludeOneShot=false)")
	assert.Contains(t, ids, "automated",
		"automated session must be included (ExcludeAutomated=false)")
	assert.Equal(t, 2, resp.Totals.Sessions,
		"both one-shot and automated sessions count toward the total")
}

func TestActivityReportEndpoint_BadDate(t *testing.T) {
	te := setup(t)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": "not-a-date", "timezone": "UTC",
	}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityReportEndpoint_BadTimezone(t *testing.T) {
	te := setup(t)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "day", "date": activityDate, "timezone": "Fake/Zone",
	}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityReportEndpoint_CustomMissingBound(t *testing.T) {
	te := setup(t)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "custom", "from": activityDate + "T00:00:00Z",
	}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityReportEndpoint_FromAfterTo(t *testing.T) {
	te := setup(t)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "custom",
		"from":   activityDate + "T12:00:00Z",
		"to":     activityDate + "T00:00:00Z",
	}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityReportEndpoint_ZeroLengthRange(t *testing.T) {
	te := setup(t)
	ts := activityDate + "T00:00:00Z"
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "custom", "from": ts, "to": ts,
	}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityReportEndpoint_RangeExceedsYear(t *testing.T) {
	te := setup(t)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "custom",
		"from":   "2026-01-01T00:00:00Z",
		"to":     "2027-01-02T00:00:00Z",
	}))
	assertStatus(t, w, http.StatusBadRequest)
}

func TestActivityReportEndpoint_BucketCountCap(t *testing.T) {
	te := setup(t)
	w := te.get(t, buildPathURL("/api/v1/activity/report", map[string]string{
		"preset": "custom",
		"from":   "2026-01-01T00:00:00Z",
		"to":     "2026-12-31T00:00:00Z",
		"bucket": "5m",
	}))
	assertStatus(t, w, http.StatusBadRequest)
}
