package db

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// TestReportingHoursMatchPerHourPairing compares every hour produced from the
// shared daily pairs with the original implementation that paired events
// separately for each hour.
func TestReportingHoursMatchPerHourPairing(t *testing.T) {
	d := testDB(t)
	seedReportingPairingArchive(t, d)
	date := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	for _, variant := range []struct {
		schema int
		bucket string
	}{
		{export.ReportingSchemaVersion, ""},
		{export.ReportingJointSchemaVersion, "5m"},
		{export.ReportingJointSchemaVersion, "1m"},
	} {
		for _, hourCount := range []int{24, 13} {
			t.Run(fmt.Sprintf("v%d/%s/hours-%d", variant.schema, variant.bucket, hourCount), func(t *testing.T) {
				bucket, err := export.ParseReportingBucket(variant.schema, variant.bucket)
				require.NoError(t, err)
				end := date.Add(time.Duration(hourCount) * time.Hour)
				load := func() reportingDaySource {
					tx, err := d.getReader().BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
					require.NoError(t, err)
					defer func() { _ = tx.Rollback() }()
					source, err := d.loadReportingExportSource(t.Context(), tx, date, end, variant.schema)
					require.NoError(t, err)
					require.NoError(t, tx.Commit())
					return source.forDate(date, end)
				}
				want, err := perHourPairingReportingHoursFromSource(
					t.Context(), load(), date, hourCount, variant.schema, nil, bucket,
				)
				require.NoError(t, err)
				got, err := reportingHoursFromSource(
					t.Context(), load(), date, hourCount, variant.schema, nil, bucket,
				)
				require.NoError(t, err)
				require.Len(t, want, hourCount)
				require.Len(t, got, hourCount)
				// Compare the finalized hours that exports publish. Joint cells
				// come out of aggregation in map order and are sorted only on
				// finalization.
				for i := range want {
					wantHour, _, err := export.FinalizeReportingHour(want[i])
					require.NoError(t, err)
					gotHour, _, err := export.FinalizeReportingHour(got[i])
					require.NoError(t, err)
					assert.Equal(t, wantHour, gotHour, "hour %02d", i)
				}
				// Keep the fixture reaching the hour boundaries this comparison
				// exists to cover.
				dataHours := []int{0, 1, 2, 5, 6, 7, 10, 11, 12}
				if hourCount == 24 {
					dataHours = append(dataHours, 13, 14, 23)
				}
				for _, hour := range dataHours {
					assert.True(t, want[hour].HasData, "hour %02d", hour)
				}
			})
		}
	}
}

type reportingPairingMessage struct {
	role, ts, model, tokens string
}

type reportingPairingSession struct {
	id, project, parent string
	automated           bool
	messages            []reportingPairingMessage
}

// seedReportingPairingArchive builds sessions around the pairing window edges:
// intervals and models inherited from the prior day, gaps equal to and above
// the gap cap, candidates that start exactly at an hour's lower and upper
// bound, sub-second and backwards timestamps, malformed timestamps, identical
// start times across sessions, subagent and automated sessions, reported
// session usage, and intervals that cross midnight in both directions.
func seedReportingPairingArchive(t *testing.T, d *DB) {
	t.Helper()
	for _, model := range []string{"model-a", "model-b", "prior-model"} {
		require.NoError(t, d.UpsertModelPricing([]ModelPricing{{
			ModelPattern: model, InputPerMTok: money.MustParseDollars("1"),
			OutputPerMTok: money.MustParseDollars("3"),
		}}))
	}
	sessions := []reportingPairingSession{
		{id: "carry", project: "project-a", messages: []reportingPairingMessage{
			{"user", "2026-07-27T23:50:00Z", "", ""},
			{"assistant", "2026-07-27T23:52:00Z", "prior-model", `{"output_tokens":40}`},
			{"user", "2026-07-27T23:57:00Z", "", ""},
			{"assistant", "2026-07-28T00:01:00Z", "", `{"output_tokens":7}`},
			{"user", "2026-07-28T00:30:00Z", "", ""},
			{"assistant", "2026-07-28T00:31:30Z", "model-b", `{"input_tokens":5,"output_tokens":9}`},
		}},
		{id: "unordered", project: "project-a", messages: []reportingPairingMessage{
			{"user", "2026-07-28T10:58:00Z", "", ""},
			{"assistant", "2026-07-28T11:01:00Z", "model-a", `{"output_tokens":11}`},
			{"assistant", "2026-07-28T10:59:00Z", "backwards-model", ""},
			{"user", "2026-07-28T11:02:00Z", "", ""},
		}},
		{id: "edges", project: "project-b", messages: []reportingPairingMessage{
			{"user", "2026-07-28T04:55:00Z", "", ""},
			{"assistant", "2026-07-28T05:00:00Z", "model-a", `{"output_tokens":13}`},
			{"user", "2026-07-28T06:00:00Z", "", ""},
			{"assistant", "2026-07-28T06:03:00Z", "model-b", ""},
			{"user", "2026-07-28T06:59:59.5Z", "", ""},
			{"assistant", "2026-07-28T07:00:00.25Z", "model-b", `{"output_tokens":17}`},
			{"user", "", "", ""},
			{"assistant", "not-a-timestamp", "model-a", ""},
			{"user", "2026-07-28T07:04:00Z", "", ""},
			{"assistant", "2026-07-28T14:10:00Z", "model-a", `{"output_tokens":19}`},
			{"user", "2026-07-28T16:40:00Z", "", ""},
		}},
		{id: "tie-a", project: "project-a", messages: []reportingPairingMessage{
			{"user", "2026-07-28T12:00:00Z", "", ""},
			{"assistant", "2026-07-28T12:02:00Z", "model-a", `{"output_tokens":23}`},
			{"user", "2026-07-28T12:58:00Z", "", ""},
			{"assistant", "2026-07-28T13:02:00Z", "model-b", `{"output_tokens":29}`},
		}},
		{id: "tie-b", project: "project-b", automated: true, messages: []reportingPairingMessage{
			{"user", "2026-07-28T12:00:00Z", "", ""},
			{"assistant", "2026-07-28T12:02:00Z", "model-a", `{"output_tokens":31}`},
		}},
		{id: "child", project: "project-a", parent: "tie-a", messages: []reportingPairingMessage{
			{"user", "2026-07-28T12:01:00Z", "", ""},
			{"assistant", "2026-07-28T12:04:30Z", "model-b", `{"output_tokens":37}`},
			{"user", "2026-07-28T12:59:00Z", "", ""},
			{"assistant", "2026-07-28T13:01:00Z", "", ""},
		}},
		{id: "overnight", project: "project-b", messages: []reportingPairingMessage{
			{"user", "2026-07-28T23:58:00Z", "", ""},
			{"assistant", "2026-07-29T00:03:00Z", "model-a", `{"output_tokens":41}`},
			{"user", "2026-07-29T00:05:00Z", "", ""},
		}},
	}
	// A session that runs from the prior evening into the early hours, with the
	// model switching every 30 messages.
	long := reportingPairingSession{id: "long", project: "project-a"}
	longStart := time.Date(2026, 7, 27, 22, 0, 0, 0, time.UTC)
	for i := range 150 {
		m := reportingPairingMessage{
			role: "user", ts: longStart.Add(time.Duration(i) * 2 * time.Minute).Format(time.RFC3339),
		}
		if i%2 == 1 {
			m.role = "assistant"
			m.tokens = `{"output_tokens":3}`
			if i%30 == 1 {
				m.model = []string{"model-a", "model-b"}[(i/30)%2]
			}
		}
		long.messages = append(long.messages, m)
	}
	sessions = append(sessions, long)

	for _, session := range sessions {
		var first, last string
		for _, m := range session.messages {
			if _, err := time.Parse(time.RFC3339Nano, m.ts); err != nil {
				continue
			}
			if first == "" {
				first = m.ts
			}
			last = m.ts
		}
		insertSession(t, d, session.id, session.project, func(s *Session) {
			s.Agent = "agent-a"
			s.StartedAt = Ptr(first)
			s.EndedAt = Ptr(last)
			s.MessageCount = len(session.messages)
			s.IsAutomated = session.automated
			if session.parent != "" {
				s.ParentSessionID = Ptr(session.parent)
				s.RelationshipType = "subagent"
			}
		})
		messages := make([]Message, len(session.messages))
		for i, m := range session.messages {
			messages[i] = Message{
				SessionID: session.id, Ordinal: i + 1, Role: m.role,
				Content: "x", Timestamp: m.ts, Model: m.model,
			}
			if m.tokens != "" {
				messages[i].TokenUsage = jsontext.Value(m.tokens)
			}
		}
		insertMessages(t, d, messages...)
	}
	sessionCost := money.MustParseDollars("0.002")
	require.NoError(t, d.ReplaceSessionUsageEvents(t.Context(), "tie-a", []UsageEvent{{
		Source: "session-source", Model: "model-a", InputTokens: 50, OutputTokens: 10,
		Cost: &sessionCost, CostStatus: "exact", CostSource: "reported",
		OccurredAt: "2026-07-28T12:01:00Z", DedupKey: "tie-a-usage",
	}}))
}

// perHourPairingReportingHoursFromSource is reportingHoursFromSource as it was
// before hourly exports reused the daily pairs: it pairs every event again for
// each hour. It is kept only as the parity oracle for the test above.
func perHourPairingReportingHoursFromSource(
	ctx context.Context,
	source reportingDaySource,
	date time.Time,
	hourCount, schemaVersion int,
	projectKeys []string,
	bucket time.Duration,
) ([]export.ReportingHour, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	hours := make([]export.ReportingHour, hourCount)
	if hourCount == 0 {
		return hours, nil
	}
	end := date.Add(time.Duration(hourCount) * time.Hour)
	query, err := activity.ResolveQuery(activity.QueryInput{
		Preset:   "custom",
		From:     date.Format(time.RFC3339),
		To:       end.Format(time.RFC3339),
		Timezone: "UTC",
	}, end)
	if err != nil {
		return nil, fmt.Errorf("resolve reporting snapshot range: %w", err)
	}
	// Preserve the shared range and inactivity-gap policy. Export bucket sizes
	// have their own validated complete-hour contract, separate from UI presets.
	query.Bucket = activity.BucketSpec{Unit: activity.BucketMinute, NominalSeconds: int(bucket / time.Second)}
	resolver := export.NewPricingResolver(source.pricing)
	sessionUsageCandidates := source.usageCandidates
	sessionUsage, _, err := materializeActivityReportUsageCandidates(
		sessionUsageCandidates, nil, nil, nil, resolver,
	)
	if err != nil {
		return nil, err
	}
	standaloneUsage := source.standaloneUsage
	usage := append(
		append([]activity.UsageRow(nil), sessionUsage...),
		standaloneUsage...,
	)
	sessions := source.activitySessions
	ids := source.activityIDs
	events := source.activityEvents
	usageSessions := source.usageSessions
	allSessions := mergeReportingSessions(sessions, usageSessions)
	sessionByID := make(map[string]activity.SessionMeta, len(allSessions))
	for _, session := range allSessions {
		sessionByID[session.SessionID] = session
	}
	usage, err = finalizeReportingUsage(
		query, usage, sessionByID,
	)
	if err != nil {
		return nil, err
	}
	projects := source.projects
	var references map[string]export.ProjectReference
	if schemaVersion == export.ReportingJointSchemaVersion {
		references = source.references
		sessions, ids, events, usage = scopeJointReporting(sessions, events, usage, sessionByID, projects, projectKeys)
		for i := range sessions {
			sessions[i].ProjectKey = export.ProjectKeyForEntry(projects[sessions[i].Project])
		}
	}
	activityIDs := reportingSessionIDSet(ids)
	activityUsage := reportingActivityUsage(usage, activityIDs)
	createdAt := source.createdAt
	firstSeen := buildReportingFirstSeen(
		date,
		end,
		time.Duration(query.GapCapSeconds)*time.Second,
		sessions,
		createdAt,
		events,
		activityUsage,
	)
	for i := range hours {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		hourStart := date.Add(time.Duration(i) * time.Hour)
		hourEnd := hourStart.Add(time.Hour)
		gapCap := time.Duration(query.GapCapSeconds) * time.Second
		candidates := activity.PairActivityEvents(events, hourStart, hourEnd, gapCap)
		aggregate := activity.AggregateCandidates
		if schemaVersion == export.ReportingJointSchemaVersion {
			aggregate = activity.AggregateCandidatesWithJointActivity
		}
		report, aggregateErr := aggregate(ctx, activity.Params{
			RangeStart:    hourStart,
			RangeEnd:      hourEnd,
			Loc:           time.UTC,
			EffectiveEnd:  hourEnd,
			GapCapSeconds: query.GapCapSeconds,
			Bucket:        query.Bucket,
		}, append([]activity.SessionMeta(nil), sessions...), candidates, activityUsage)
		if aggregateErr != nil {
			return nil, fmt.Errorf(
				"aggregate reporting hour %s: %w",
				hourStart.Format("2006-01-02-15"),
				aggregateErr,
			)
		}
		activity.SanitizeProjectLabels(&report, projects)
		hour, conversionErr := reportingHourFromActivity(
			hourStart, report, schemaVersion,
		)
		if conversionErr != nil {
			return nil, fmt.Errorf(
				"convert reporting hour %s: %w",
				hourStart.Format("2006-01-02-15"),
				conversionErr,
			)
		}
		reportingUsage, hourHasUsage, usageErr := reportingUsageForHour(
			hourStart, hourEnd, usage, sessionByID, projects,
		)
		if usageErr != nil {
			return nil, usageErr
		}
		hour.Usage = reportingUsage
		applyReportingFirstSeen(&hour.Activity.Totals, firstSeen[i])
		hour.HasData = hourHasUsage ||
			hour.Activity.Totals.AgentMinutes > 0 ||
			hour.Activity.Totals.ActiveMinutes > 0 ||
			firstSeen[i].hasAny()
		if !hour.HasData {
			hour = quietReportingHour(hourStart, schemaVersion, bucket)
		}
		if schemaVersion == export.ReportingJointSchemaVersion {
			hour.BucketSeconds = int(bucket / time.Second)
			hour.Joint, err = jointReportingHour(hourStart, bucket, report.JointActivity, report.BySession, usage, sessionByID, projects, references, projectKeys)
			if err != nil {
				return nil, err
			}
		}
		hours[i] = hour
	}
	return hours, nil
}
