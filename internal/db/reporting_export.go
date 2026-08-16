package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// ReportingExportOptions selects one UTC calendar date and fixes the current
// time used to decide which hours have closed.
type ReportingExportOptions struct {
	Date          time.Time
	Now           time.Time
	SchemaVersion int

	// afterSnapshot is a deterministic test seam for proving that every source
	// read uses the transaction established before this callback.
	afterSnapshot func()
}

// ExportReportingDay builds an hourly reporting document from one read
// transaction. Only closed UTC hours are included for the current date.
func (db *DB) ExportReportingDay(
	ctx context.Context, opts ReportingExportOptions,
) (export.ReportingDay, error) {
	schemaVersion := opts.SchemaVersion
	if schemaVersion == 0 {
		schemaVersion = export.ReportingSchemaVersion
	}
	if !export.IsSupportedReportingSchemaVersion(schemaVersion) {
		return export.ReportingDay{}, fmt.Errorf(
			"unsupported reporting schema version %d", schemaVersion,
		)
	}
	date, _, hourCount, complete, err := resolveReportingExportRange(opts)
	if err != nil {
		return export.ReportingDay{}, err
	}

	tx, err := db.getReader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return export.ReportingDay{}, fmt.Errorf("begin reporting snapshot: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	// Establish the snapshot before constructing the in-memory document. The
	// following implementation stages load all source rows through this tx.
	var snapshotMarker int
	if err := tx.QueryRowContext(
		ctx, "SELECT COUNT(*) FROM archive_metadata",
	).Scan(&snapshotMarker); err != nil {
		return export.ReportingDay{}, fmt.Errorf("establish reporting snapshot: %w", err)
	}
	if opts.afterSnapshot != nil {
		opts.afterSnapshot()
	}

	hours, err := db.reportingHoursFromSnapshot(
		ctx, tx, date, hourCount, schemaVersion,
	)
	if err != nil {
		return export.ReportingDay{}, err
	}
	day, _, err := export.FinalizeReportingDay(export.ReportingDay{
		SchemaVersion: schemaVersion,
		Date:          date.Format("2006-01-02"),
		Complete:      complete,
		Hours:         hours,
	})
	if err != nil {
		return export.ReportingDay{}, fmt.Errorf("finalize reporting date: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return export.ReportingDay{}, fmt.Errorf("commit reporting snapshot: %w", err)
	}
	return day, nil
}

func (db *DB) reportingHoursFromSnapshot(
	ctx context.Context, tx *sql.Tx, date time.Time, hourCount, schemaVersion int,
) ([]export.ReportingHour, error) {
	hours := make([]export.ReportingHour, hourCount)
	if hourCount == 0 {
		return hours, nil
	}
	end := date.Add(time.Duration(hourCount) * time.Hour)
	query, err := activity.ResolveQuery(activity.QueryInput{
		Preset:         "custom",
		From:           date.Format(time.RFC3339),
		To:             end.Format(time.RFC3339),
		Timezone:       "UTC",
		BucketOverride: "5m",
	}, end)
	if err != nil {
		return nil, fmt.Errorf("resolve reporting snapshot range: %w", err)
	}
	filter := AnalyticsFilter{
		Timezone:         "UTC",
		IncludeSubagents: true,
		IncludeForks:     true,
	}
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(query)
	sessions, ids, err := db.activityReportSessionsFrom(
		ctx, tx, filter, rangeStartUTC, rangeEndUTC,
	)
	if err != nil {
		return nil, err
	}
	events, err := db.activityReportActivityFrom(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	lowerBound := paddedUTCBound(date.Format(time.RFC3339), -14)
	upperBound := paddedUTCBound(end.Format(time.RFC3339), 14)
	usageSessions, usageIDs, err := db.reportingUsageSessionsFrom(
		ctx, tx, lowerBound, upperBound,
	)
	if err != nil {
		return nil, err
	}
	sessionUsage, _, err := db.activityReportUsageCandidatesFrom(
		ctx,
		tx,
		usageIDs,
		lowerBound,
		upperBound,
		schemaVersion >= export.ReportingSchemaVersion,
	)
	if err != nil {
		return nil, err
	}
	standaloneUsage, err := db.reportingStandaloneUsageCandidatesFrom(
		ctx, tx, query,
	)
	if err != nil {
		return nil, err
	}
	usage := append(
		append([]activity.UsageRow(nil), sessionUsage...),
		standaloneUsage...,
	)
	allSessions := mergeReportingSessions(sessions, usageSessions)
	sessionByID := make(map[string]activity.SessionMeta, len(allSessions))
	for _, session := range allSessions {
		sessionByID[session.SessionID] = session
	}
	usage, err = finalizeReportingUsageForSchema(
		schemaVersion, query, usage, sessionByID,
	)
	if err != nil {
		return nil, err
	}
	activityIDs := reportingSessionIDSet(ids)
	activityUsage := reportingActivityUsage(usage, activityIDs)

	projectLabels := activityReportProjectLabels(allSessions)
	projects, err := db.reportingProjectIdentityMapFrom(ctx, tx, projectLabels)
	if err != nil {
		return nil, err
	}
	createdAt, err := reportingSessionCreatedAtFrom(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
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
		hourStart := date.Add(time.Duration(i) * time.Hour)
		hourEnd := hourStart.Add(time.Hour)
		gapCap := time.Duration(query.GapCapSeconds) * time.Second
		candidates := activity.PairActivityEvents(events, hourStart, hourEnd, gapCap)
		report, aggregateErr := activity.AggregateCandidates(ctx, activity.Params{
			RangeStart:    hourStart,
			RangeEnd:      hourEnd,
			Loc:           time.UTC,
			EffectiveEnd:  hourEnd,
			GapCapSeconds: query.GapCapSeconds,
			Bucket:        activity.BucketSpec{Unit: activity.BucketMinute, NominalSeconds: 300},
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
			hour = quietReportingHour(hourStart, schemaVersion)
		}
		hours[i] = hour
	}
	return hours, nil
}

func (db *DB) reportingUsageSessionsFrom(
	ctx context.Context,
	tx *sql.Tx,
	lowerBound, upperBound string,
) ([]activity.SessionMeta, []string, error) {
	usageRows := dailyUsageRowsSQLWithWhere(
		usageMessageEligibility,
		usageEventEligibility,
	)
	rows, err := tx.QueryContext(ctx, `
		WITH usage_candidates AS (`+usageRows+`),
		usage_session_ids AS (
			SELECT DISTINCT session_id
			FROM usage_candidates
			WHERE session_id != ''
				AND ts >= ?
				AND ts <= ?
		)
		SELECT
			s.id,
			COALESCE(NULLIF(s.display_name, ''), NULLIF(s.session_name, ''),
				NULLIF(s.project, ''), s.id),
			s.project,
			s.agent,
			s.machine,
			COALESCE(s.started_at, ''),
			COALESCE(s.ended_at, ''),
			COALESCE(s.is_automated, 0)
		FROM sessions s
		JOIN usage_session_ids u ON u.session_id = s.id
		ORDER BY s.id`,
		lowerBound, upperBound,
	)
	if err != nil {
		return nil, nil, fmt.Errorf(
			"querying reporting usage sessions: %w", err,
		)
	}
	defer rows.Close()

	sessions := []activity.SessionMeta{}
	ids := []string{}
	for rows.Next() {
		var session activity.SessionMeta
		if err := rows.Scan(
			&session.SessionID,
			&session.Title,
			&session.Project,
			&session.Agent,
			&session.Machine,
			&session.StartedAt,
			&session.EndedAt,
			&session.IsAutomated,
		); err != nil {
			return nil, nil, fmt.Errorf(
				"scanning reporting usage session: %w", err,
			)
		}
		sessions = append(sessions, session)
		ids = append(ids, session.SessionID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf(
			"iterating reporting usage sessions: %w", err,
		)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].SessionID < sessions[j].SessionID
	})
	sort.Strings(ids)
	return sessions, ids, nil
}

func (db *DB) reportingStandaloneUsageCandidatesFrom(
	ctx context.Context,
	tx *sql.Tx,
	query activity.Query,
) ([]activity.UsageRow, error) {
	var tableExists int
	err := tx.QueryRowContext(
		ctx,
		`SELECT 1 FROM sqlite_master
		 WHERE type = 'table' AND name = 'cursor_usage_events'`,
	).Scan(&tableExists)
	if err == sql.ErrNoRows {
		return []activity.UsageRow{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"checking standalone reporting usage table: %w", err,
		)
	}

	lowerBound := paddedUTCBound(
		query.RangeStart.UTC().Format(time.RFC3339), -14,
	)
	upperBound := paddedUTCBound(
		query.RangeEnd.UTC().Format(time.RFC3339), 14,
	)
	rows, err := tx.QueryContext(ctx, `
		SELECT occurred_at, model,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_microdollars, dedup_key
		FROM cursor_usage_events
		WHERE model != ''
			AND occurred_at >= ?
			AND occurred_at <= ?`,
		lowerBound, upperBound,
	)
	if err != nil {
		return nil, fmt.Errorf("querying standalone reporting usage: %w", err)
	}
	defer rows.Close()

	loaded := []activity.UsageRow{}
	for rows.Next() {
		var row activity.UsageRow
		var inputTokens, outputTokens int
		var cacheCreationTokens, cacheReadTokens int
		if err := rows.Scan(
			&row.Timestamp,
			&row.Model,
			&inputTokens,
			&outputTokens,
			&cacheCreationTokens,
			&cacheReadTokens,
			&row.Cost,
			&row.UsageDedupKey,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning standalone reporting usage: %w", err,
			)
		}
		row.InputTokens,
			row.OutputTokens,
			row.CacheCreationTokens,
			row.CacheReadTokens = usageEventRowTokens(
			"cursor",
			inputTokens,
			outputTokens,
			cacheCreationTokens,
			cacheReadTokens,
		)
		row.Agent = "cursor"
		row.MessageOrdinal = -1
		row.UsageSource = "cursor"
		row.CostSource = export.CostSourceReported
		row.Priced = true
		row.Contributes = true
		loaded = append(loaded, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating standalone reporting usage: %w", err,
		)
	}
	return loaded, nil
}

func sortReportingUsage(rows []activity.UsageRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		// Reporting export deliberately mirrors the SQLite BINARY ordering
		// used by GetDailyUsage's first-seen-wins survivor pass. Ordinary
		// activity reports instead sort timestamps by parsed instant.
		if a.Timestamp != b.Timestamp {
			return a.Timestamp < b.Timestamp
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		if a.MessageOrdinal != b.MessageOrdinal {
			return a.MessageOrdinal < b.MessageOrdinal
		}
		for _, compared := range []int{
			cmp.Compare(a.UsageSource, b.UsageSource),
			cmp.Compare(a.UsageDedupKey, b.UsageDedupKey),
			cmp.Compare(a.ClaudeMessageID, b.ClaudeMessageID),
			cmp.Compare(a.ClaudeRequestID, b.ClaudeRequestID),
			cmp.Compare(a.SourceUUID, b.SourceUUID),
			cmp.Compare(a.Model, b.Model),
			cmp.Compare(a.Agent, b.Agent),
			cmp.Compare(a.Project, b.Project),
			cmp.Compare(a.Machine, b.Machine),
			cmp.Compare(a.InputTokens, b.InputTokens),
			cmp.Compare(a.OutputTokens, b.OutputTokens),
			cmp.Compare(a.CacheCreationTokens, b.CacheCreationTokens),
			cmp.Compare(a.CacheReadTokens, b.CacheReadTokens),
			cmp.Compare(a.Cost.Microdollars, b.Cost.Microdollars),
			cmp.Compare(a.CostSource, b.CostSource),
			compareReportingMoneyPointers(a.SessionCost, b.SessionCost),
			compareReportingBool(a.Priced, b.Priced),
			compareReportingBool(a.Contributes, b.Contributes),
		} {
			if compared != 0 {
				return compared < 0
			}
		}
		return false
	})
}

func finalizeReportingUsage(
	query activity.Query,
	rows []activity.UsageRow,
	sessionByID map[string]activity.SessionMeta,
) ([]activity.UsageRow, error) {
	return finalizeReportingUsageForSchema(
		export.ReportingSchemaVersion, query, rows, sessionByID,
	)
}

func finalizeReportingUsageForSchema(
	schemaVersion int,
	query activity.Query,
	rows []activity.UsageRow,
	sessionByID map[string]activity.SessionMeta,
) ([]activity.UsageRow, error) {
	sortReportingUsage(rows)
	if schemaVersion == export.ReportingLegacySchemaVersion {
		mask := activity.LegacyUsageSurvivorMask(
			query.RangeStart, query.RangeEnd, query.EffectiveEnd, rows,
		)
		survivors := make([]activity.UsageRow, 0, len(rows))
		for i, keep := range mask {
			if keep {
				survivors = append(survivors, rows[i])
			}
		}
		return allocateReportingUsageCosts(survivors)
	}
	mask, attribution, webSearchRequests := activity.UsageSurvivorSelection(
		query.RangeStart, query.RangeEnd, query.EffectiveEnd, rows,
	)
	survivors := make([]activity.UsageRow, 0, len(rows))
	for i, keep := range mask {
		if keep {
			row := rows[i]
			row.SessionID = attribution[i]
			if row.CostSource == export.CostSourceComputed &&
				webSearchRequests[i] > row.WebSearchRequests {
				additionalFee, err := export.WebSearchFee(
					webSearchRequests[i] - row.WebSearchRequests)
				if err != nil {
					return nil, err
				}
				row.Cost, err = money.Add(row.Cost, additionalFee)
				if err != nil {
					return nil, err
				}
			}
			row.WebSearchRequests = webSearchRequests[i]
			row.Contributes = row.Contributes || row.WebSearchRequests > 0
			if session, ok := sessionByID[row.SessionID]; ok {
				row.Agent = session.Agent
				row.Project = session.Project
				row.Machine = session.Machine
			}
			survivors = append(survivors, row)
		}
	}
	return allocateReportingUsageCosts(survivors)
}

func compareReportingMoneyPointers(a, b *money.Money) int {
	if a == nil || b == nil {
		switch {
		case a == nil && b != nil:
			return -1
		case a != nil && b == nil:
			return 1
		default:
			return 0
		}
	}
	return cmp.Compare(a.Microdollars, b.Microdollars)
}

func compareReportingBool(a, b bool) int {
	switch {
	case !a && b:
		return -1
	case a && !b:
		return 1
	default:
		return 0
	}
}

func reportingSessionIDSet(ids []string) map[string]struct{} {
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func reportingActivityUsage(
	rows []activity.UsageRow,
	activityIDs map[string]struct{},
) []activity.UsageRow {
	out := make([]activity.UsageRow, 0, len(rows))
	for _, row := range rows {
		if _, ok := activityIDs[row.SessionID]; ok {
			out = append(out, row)
		}
	}
	return out
}

func mergeReportingSessions(
	activitySessions, usageSessions []activity.SessionMeta,
) []activity.SessionMeta {
	byID := make(
		map[string]activity.SessionMeta,
		len(activitySessions)+len(usageSessions),
	)
	for _, session := range usageSessions {
		byID[session.SessionID] = session
	}
	for _, session := range activitySessions {
		byID[session.SessionID] = session
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]activity.SessionMeta, 0, len(ids))
	for _, id := range ids {
		out = append(out, byID[id])
	}
	return out
}

func allocateReportingUsageCosts(
	rows []activity.UsageRow,
) ([]activity.UsageRow, error) {
	out := append([]activity.UsageRow(nil), rows...)
	type sessionCost struct {
		carrier int
		cost    money.Money
		indices map[usageCostAllocationKey][]int
	}
	sessionCosts := make(map[string]*sessionCost)
	for i := range out {
		if out[i].SessionCost != nil {
			sessionCosts[out[i].SessionID] = &sessionCost{
				carrier: i,
				cost:    *out[i].SessionCost,
				indices: make(map[usageCostAllocationKey][]int),
			}
		}
	}
	for i, row := range out {
		selected := sessionCosts[row.SessionID]
		if selected == nil {
			continue
		}
		key := usageCostAllocationKey{
			date:    localDate(row.Timestamp, time.UTC),
			project: row.Project,
			agent:   row.Agent,
			machine: row.Machine,
			model:   row.Model,
		}
		selected.indices[key] = append(selected.indices[key], i)
	}
	for _, selected := range sessionCosts {
		if len(selected.indices) == 0 {
			out[selected.carrier].Cost = selected.cost
			out[selected.carrier].CostSource = export.CostSourceReported
			out[selected.carrier].Priced = true
			out[selected.carrier].Contributes = true
			continue
		}
		estimated := make(
			map[usageCostAllocationKey]money.Money,
			len(selected.indices),
		)
		for key, indices := range selected.indices {
			for _, index := range indices {
				cost, err := money.Add(estimated[key], rows[index].Cost)
				if err != nil {
					return nil, fmt.Errorf(
						"summing reporting allocation weights: %w", err,
					)
				}
				estimated[key] = cost
			}
		}
		keyCosts := allocateUsageCostByKey(selected.cost, estimated)
		for key, indices := range selected.indices {
			weights := make([]money.Money, len(indices))
			for i, index := range indices {
				weights[i] = rows[index].Cost
			}
			costs := export.AllocateCostByWeight(keyCosts[key], weights)
			for i, index := range indices {
				out[index].Cost = costs[i]
				out[index].CostSource = export.CostSourceReported
				out[index].Priced = true
				out[index].Contributes = true
			}
		}
	}
	for i := range out {
		out[i].SessionCost = nil
	}
	return out, nil
}

func (db *DB) reportingProjectIdentityMapFrom(
	ctx context.Context,
	tx *sql.Tx,
	labels []string,
) (map[string]export.ProjectMapEntry, error) {
	if len(labels) == 0 {
		return map[string]export.ProjectMapEntry{}, nil
	}
	observations, err := db.listProjectIdentityObservationsFrom(ctx, tx, labels)
	if err != nil {
		return nil, err
	}
	archiveID, err := sessionExportMetadataValue(
		ctx,
		tx,
		archiveMetadataArchiveIDKey,
		ErrArchiveIDMissing,
		"archive id",
	)
	if err != nil {
		return nil, err
	}
	archiveSalt, err := sessionExportMetadataValue(
		ctx,
		tx,
		archiveMetadataArchiveSaltKey,
		ErrArchiveSaltMissing,
		"archive salt",
	)
	if err != nil {
		return nil, err
	}
	return export.BuildProjectsMapWithScope(
		labels,
		observations,
		export.IdentityScope{ArchiveID: archiveID, ArchiveSalt: archiveSalt},
	), nil
}

func reportingSessionCreatedAtFrom(
	ctx context.Context, tx *sql.Tx, ids []string,
) (map[string]time.Time, error) {
	out := make(map[string]time.Time, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		rows, err := tx.QueryContext(
			ctx,
			`SELECT id, created_at FROM sessions WHERE id IN `+placeholders,
			args...,
		)
		if err != nil {
			return fmt.Errorf("querying reporting session creation times: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				return fmt.Errorf("scanning reporting session creation time: %w", err)
			}
			if created, err := parseTimestamp(raw); err == nil {
				out[id] = created.UTC()
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating reporting session creation times: %w", err)
		}
		return nil
	})
	return out, err
}

type reportingFirstSeenCounts struct {
	sessions            int
	automatedSessions   int
	interactiveSessions int
	untimedSessions     int
	projects            int
	models              int
}

func (c reportingFirstSeenCounts) hasAny() bool {
	return c.sessions != 0 || c.projects != 0 || c.models != 0
}

func buildReportingFirstSeen(
	start, end time.Time,
	gapCap time.Duration,
	sessions []activity.SessionMeta,
	createdAt map[string]time.Time,
	events []activity.ActivityEvent,
	usage []activity.UsageRow,
) map[int]reportingFirstSeenCounts {
	sessionFirst := make(map[string]time.Time, len(sessions))
	timed := make(map[string]bool, len(sessions))
	eventsBySession := make(map[string][]activity.ActivityEvent)
	for _, event := range events {
		eventsBySession[event.SessionID] = append(
			eventsBySession[event.SessionID],
			event,
		)
	}
	for sessionID, sessionEvents := range eventsBySession {
		for i := 1; i < len(sessionEvents); i++ {
			previous, previousErr := parseTimestamp(sessionEvents[i-1].Timestamp)
			current, currentErr := parseTimestamp(sessionEvents[i].Timestamp)
			if previousErr != nil || currentErr != nil {
				continue
			}
			intervalStart, _, ok := activity.EffectiveIntervalBounds(
				previous.UTC(), current.UTC(), start, end, gapCap,
			)
			if ok {
				setEarlierReportingTime(sessionFirst, sessionID, intervalStart)
				timed[sessionID] = true
			}
		}
	}

	usageFirst := make(map[string]time.Time, len(sessions))
	for _, row := range usage {
		at, err := parseTimestamp(row.Timestamp)
		if err == nil && !at.Before(start) && at.Before(end) {
			setEarlierReportingTime(usageFirst, row.SessionID, at.UTC())
		}
	}
	for sessionID, at := range usageFirst {
		if _, ok := sessionFirst[sessionID]; !ok {
			sessionFirst[sessionID] = at
		}
	}

	eventFirst := make(map[string]time.Time, len(sessions))
	for _, event := range events {
		at, err := parseTimestamp(event.Timestamp)
		if err == nil && !at.Before(start) && at.Before(end) {
			setEarlierReportingTime(eventFirst, event.SessionID, at.UTC())
		}
	}
	for sessionID, at := range eventFirst {
		if _, ok := sessionFirst[sessionID]; !ok {
			sessionFirst[sessionID] = at
		}
	}

	for _, session := range sessions {
		if _, ok := sessionFirst[session.SessionID]; ok {
			continue
		}
		if at, err := parseTimestamp(session.StartedAt); err == nil {
			if at = at.UTC(); !at.Before(start) && at.Before(end) {
				sessionFirst[session.SessionID] = at
				continue
			}
		}
		if at, ok := createdAt[session.SessionID]; ok {
			if at = at.UTC(); !at.Before(start) && at.Before(end) {
				sessionFirst[session.SessionID] = at
			}
		}
	}

	projectFirst := map[string]time.Time{}
	modelFirst := map[string]time.Time{}
	sessionByID := make(map[string]activity.SessionMeta, len(sessions))
	for _, session := range sessions {
		sessionByID[session.SessionID] = session
		if at, ok := sessionFirst[session.SessionID]; ok && session.Project != "" {
			setEarlierReportingTime(projectFirst, session.Project, at)
		}
	}
	for _, event := range events {
		if event.Model == "" {
			continue
		}
		if at, err := parseTimestamp(event.Timestamp); err == nil &&
			!at.Before(start) && at.Before(end) {
			setEarlierReportingTime(modelFirst, event.Model, at.UTC())
		}
	}
	for _, row := range usage {
		if row.Model == "" {
			continue
		}
		if at, err := parseTimestamp(row.Timestamp); err == nil &&
			!at.Before(start) && at.Before(end) {
			setEarlierReportingTime(modelFirst, row.Model, at.UTC())
		}
	}

	counts := make(map[int]reportingFirstSeenCounts, 24)
	for sessionID, at := range sessionFirst {
		index, ok := reportingHourIndex(start, end, at)
		if !ok {
			continue
		}
		count := counts[index]
		count.sessions++
		if sessionByID[sessionID].IsAutomated {
			count.automatedSessions++
		} else {
			count.interactiveSessions++
		}
		if !timed[sessionID] {
			count.untimedSessions++
		}
		counts[index] = count
	}
	for _, at := range projectFirst {
		if index, ok := reportingHourIndex(start, end, at); ok {
			count := counts[index]
			count.projects++
			counts[index] = count
		}
	}
	for _, at := range modelFirst {
		if index, ok := reportingHourIndex(start, end, at); ok {
			count := counts[index]
			count.models++
			counts[index] = count
		}
	}
	return counts
}

func setEarlierReportingTime(values map[string]time.Time, key string, at time.Time) {
	if current, ok := values[key]; !ok || at.Before(current) {
		values[key] = at
	}
}

func reportingHourIndex(start, end, at time.Time) (int, bool) {
	if at.Before(start) || !at.Before(end) {
		return 0, false
	}
	return int(at.Sub(start) / time.Hour), true
}

func applyReportingFirstSeen(
	totals *export.ReportingActivityTotals, counts reportingFirstSeenCounts,
) {
	totals.NewSessions = counts.sessions
	totals.NewAutomatedSessions = counts.automatedSessions
	totals.NewInteractiveSessions = counts.interactiveSessions
	totals.NewUntimedSessions = counts.untimedSessions
	totals.NewProjects = counts.projects
	totals.NewModels = counts.models
}

func reportingHourFromActivity(
	start time.Time, report activity.Report, schemaVersion int,
) (export.ReportingHour, error) {
	totalAgentMinutes, err := reportingDerivedAgentMinutes(
		"activity totals",
		report.Totals.AgentMinutes,
		report.Totals.AutomatedAgentMinutes,
		report.Totals.InteractiveAgentMinutes,
	)
	if err != nil {
		return export.ReportingHour{}, err
	}
	byModel, err := reportingActivityBreakdowns("model", report.ByModel)
	if err != nil {
		return export.ReportingHour{}, err
	}
	byAgent, err := reportingActivityBreakdowns("agent", report.ByAgent)
	if err != nil {
		return export.ReportingHour{}, err
	}
	byProject, err := reportingActivityProjectBreakdowns(report.ByProject)
	if err != nil {
		return export.ReportingHour{}, err
	}
	buckets := make([]export.ReportingActivityBucket, len(report.Buckets))
	for i, bucket := range report.Buckets {
		buckets[i] = export.ReportingActivityBucket{
			Start:             bucket.Start,
			AgentMinutes:      bucket.AgentMinutes,
			MaxAgents:         bucket.MaxAgents,
			OutputTokens:      int64(bucket.OutputTokens),
			Cost:              bucket.Cost,
			AutomatedAtPeak:   bucket.AutomatedAtPeak,
			InteractiveAtPeak: bucket.InteractiveAtPeak,
		}
	}
	return export.ReportingHour{
		SchemaVersion: schemaVersion,
		Period:        start.Format("2006-01-02-15"),
		Activity: export.ReportingActivity{
			Totals: export.ReportingActivityTotals{
				ActiveMinutes:           report.Totals.ActiveMinutes,
				IdleMinutes:             report.Totals.IdleMinutes,
				AgentMinutes:            totalAgentMinutes,
				AutomatedAgentMinutes:   report.Totals.AutomatedAgentMinutes,
				InteractiveAgentMinutes: report.Totals.InteractiveAgentMinutes,
				OutputTokens:            int64(report.Totals.OutputTokens),
				Cost:                    report.Totals.Cost,
				AutomatedCost:           report.Totals.AutomatedCost,
				InteractiveCost:         report.Totals.InteractiveCost,
			},
			Peak: export.ReportingActivityPeak{
				Agents: report.Peak.Agents,
				At:     report.Peak.At,
			},
			Buckets:   buckets,
			ByModel:   byModel,
			ByAgent:   byAgent,
			ByProject: byProject,
		},
	}, nil
}

const reportingMinuteToleranceFactor = 1e-9

func reportingDerivedAgentMinutes(
	field string,
	original, automated, interactive float64,
) (float64, error) {
	if math.IsNaN(original) || math.IsInf(original, 0) || original < 0 {
		return 0, fmt.Errorf("%s agent minutes total is invalid", field)
	}
	if math.IsNaN(automated) || math.IsInf(automated, 0) || automated < 0 {
		return 0, fmt.Errorf(
			"%s automated agent minutes is invalid", field,
		)
	}
	if math.IsNaN(interactive) || math.IsInf(interactive, 0) ||
		interactive < 0 {
		return 0, fmt.Errorf(
			"%s interactive agent minutes is invalid", field,
		)
	}
	derived := automated + interactive
	if math.IsNaN(derived) || math.IsInf(derived, 0) {
		return 0, fmt.Errorf("%s derived agent minutes is invalid", field)
	}
	scale := math.Max(1, math.Max(math.Abs(original), math.Abs(derived)))
	tolerance := reportingMinuteToleranceFactor * scale
	if math.Abs(original-derived) > tolerance {
		return 0, fmt.Errorf(
			"%s agent minutes do not match components", field,
		)
	}
	return derived, nil
}

func reportingActivityBreakdowns(
	scope string,
	rows []activity.KeyMinutes,
) ([]export.ReportingActivityBreakdown, error) {
	out := make([]export.ReportingActivityBreakdown, 0, len(rows))
	for _, row := range rows {
		agentMinutes, err := reportingDerivedAgentMinutes(
			fmt.Sprintf("activity %s %q", scope, row.Key),
			row.AgentMinutes,
			row.AutomatedAgentMinutes,
			row.InteractiveAgentMinutes,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, export.ReportingActivityBreakdown{
			Key:                     row.Key,
			AgentMinutes:            agentMinutes,
			AutomatedAgentMinutes:   row.AutomatedAgentMinutes,
			InteractiveAgentMinutes: row.InteractiveAgentMinutes,
			Cost:                    row.Cost,
			AutomatedCost:           row.AutomatedCost,
			InteractiveCost:         row.InteractiveCost,
		})
	}
	return out, nil
}

func reportingActivityProjectBreakdowns(
	rows []activity.KeyMinutes,
) ([]export.ReportingActivityProjectBreakdown, error) {
	out := make([]export.ReportingActivityProjectBreakdown, 0, len(rows))
	for _, row := range rows {
		agentMinutes, err := reportingDerivedAgentMinutes(
			fmt.Sprintf("activity project %q", row.Key),
			row.AgentMinutes,
			row.AutomatedAgentMinutes,
			row.InteractiveAgentMinutes,
		)
		if err != nil {
			return nil, err
		}
		out = append(out, export.ReportingActivityProjectBreakdown{
			Project:                 row.Key,
			ProjectKey:              row.ProjectKey,
			AgentMinutes:            agentMinutes,
			AutomatedAgentMinutes:   row.AutomatedAgentMinutes,
			InteractiveAgentMinutes: row.InteractiveAgentMinutes,
			Cost:                    row.Cost,
			AutomatedCost:           row.AutomatedCost,
			InteractiveCost:         row.InteractiveCost,
		})
	}
	return out, nil
}

type reportingUsageAccum struct {
	inputTokens         int64
	outputTokens        int64
	cacheCreationTokens int64
	cacheReadTokens     int64
	cost                money.Money
}

func (a *reportingUsageAccum) add(row activity.UsageRow) error {
	a.inputTokens += int64(row.InputTokens)
	a.outputTokens += int64(row.OutputTokens)
	a.cacheCreationTokens += int64(row.CacheCreationTokens)
	a.cacheReadTokens += int64(row.CacheReadTokens)
	cost, err := money.Add(a.cost, row.Cost)
	if err != nil {
		return err
	}
	a.cost = cost
	return nil
}

func reportingUsageForHour(
	start, end time.Time,
	rows []activity.UsageRow,
	sessionByID map[string]activity.SessionMeta,
	projects map[string]export.ProjectMapEntry,
) (export.ReportingUsage, bool, error) {
	var totals reportingUsageAccum
	byModel := map[string]*reportingUsageAccum{}
	byAgent := map[string]*reportingUsageAccum{}
	byProject := map[string]*reportingUsageAccum{}
	hasRows := false
	for _, row := range rows {
		at, err := parseTimestamp(row.Timestamp)
		if err != nil || at.Before(start) || !at.Before(end) {
			continue
		}
		hasRows = true
		if err := totals.add(row); err != nil {
			return export.ReportingUsage{}, false,
				fmt.Errorf("summing reporting usage total: %w", err)
		}
		session := sessionByID[row.SessionID]
		agent := row.Agent
		if agent == "" {
			agent = session.Agent
		}
		for _, keyed := range []struct {
			key    string
			target map[string]*reportingUsageAccum
		}{
			{key: row.Model, target: byModel},
			{key: agent, target: byAgent},
			{key: session.Project, target: byProject},
		} {
			if err := addReportingUsageBreakdown(
				keyed.target, keyed.key, row,
			); err != nil {
				return export.ReportingUsage{}, false, err
			}
		}
	}
	return export.ReportingUsage{
		Totals: export.ReportingUsageTotals{
			InputTokens:         totals.inputTokens,
			OutputTokens:        totals.outputTokens,
			CacheCreationTokens: totals.cacheCreationTokens,
			CacheReadTokens:     totals.cacheReadTokens,
			Cost:                totals.cost,
		},
		ByModel:   reportingUsageBreakdowns(byModel),
		ByAgent:   reportingUsageBreakdowns(byAgent),
		ByProject: reportingUsageProjectBreakdowns(byProject, projects),
	}, hasRows, nil
}

func addReportingUsageBreakdown(
	target map[string]*reportingUsageAccum,
	key string,
	row activity.UsageRow,
) error {
	if key == "" {
		return nil
	}
	accum := target[key]
	if accum == nil {
		accum = &reportingUsageAccum{}
		target[key] = accum
	}
	if err := accum.add(row); err != nil {
		return fmt.Errorf("summing reporting usage breakdown: %w", err)
	}
	return nil
}

func reportingUsageBreakdowns(
	values map[string]*reportingUsageAccum,
) []export.ReportingUsageBreakdown {
	keys := reportingSortedKeys(values)
	out := make([]export.ReportingUsageBreakdown, 0, len(keys))
	for _, key := range keys {
		value := values[key]
		if value == nil {
			continue
		}
		out = append(out, export.ReportingUsageBreakdown{
			Key:                 key,
			InputTokens:         value.inputTokens,
			OutputTokens:        value.outputTokens,
			CacheCreationTokens: value.cacheCreationTokens,
			CacheReadTokens:     value.cacheReadTokens,
			Cost:                value.cost,
		})
	}
	return out
}

func reportingUsageProjectBreakdowns(
	values map[string]*reportingUsageAccum,
	projects map[string]export.ProjectMapEntry,
) []export.ReportingUsageProjectBreakdown {
	keys := reportingSortedKeys(values)
	out := make([]export.ReportingUsageProjectBreakdown, 0, len(keys))
	for _, project := range keys {
		value := values[project]
		if value == nil {
			continue
		}
		out = append(out, export.ReportingUsageProjectBreakdown{
			Project:             export.SafeProjectDisplayLabel(project),
			ProjectKey:          export.ProjectKeyForEntry(projects[project]),
			InputTokens:         value.inputTokens,
			OutputTokens:        value.outputTokens,
			CacheCreationTokens: value.cacheCreationTokens,
			CacheReadTokens:     value.cacheReadTokens,
			Cost:                value.cost,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ProjectKey != out[j].ProjectKey {
			return out[i].ProjectKey < out[j].ProjectKey
		}
		return out[i].Project < out[j].Project
	})
	return out
}

func reportingSortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func resolveReportingExportRange(
	opts ReportingExportOptions,
) (date, now time.Time, hourCount int, complete bool, err error) {
	date = opts.Date
	_, offset := date.Zone()
	if offset != 0 ||
		date.Hour() != 0 ||
		date.Minute() != 0 ||
		date.Second() != 0 ||
		date.Nanosecond() != 0 {
		err = fmt.Errorf("reporting date must be UTC midnight")
		return
	}
	date = date.UTC()

	now = opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()
	closedThrough := now.Truncate(time.Hour)
	if date.After(closedThrough) {
		err = fmt.Errorf("reporting date is in the future")
		return
	}

	dayEnd := date.Add(24 * time.Hour)
	if !closedThrough.Before(dayEnd) {
		return date, now, 24, true, nil
	}
	hourCount = int(closedThrough.Sub(date) / time.Hour)
	if hourCount < 0 {
		err = fmt.Errorf("reporting date is in the future")
	}
	return
}

func quietReportingHour(start time.Time, schemaVersion int) export.ReportingHour {
	buckets := make([]export.ReportingActivityBucket, 12)
	for i := range buckets {
		buckets[i].Start = start.Add(time.Duration(i) * 5 * time.Minute).
			Format(time.RFC3339)
	}
	return export.ReportingHour{
		SchemaVersion: schemaVersion,
		Period:        start.Format("2006-01-02-15"),
		Activity: export.ReportingActivity{
			Buckets: buckets,
		},
	}
}
