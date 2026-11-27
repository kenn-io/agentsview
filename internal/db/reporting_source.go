package db

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/parser"
)

type reportingSourceLoadStats struct {
	ActivitySessions    int
	ActivityHistoryRows int
	UsageSessions       int
	PaddedUsageRows     int
}

type reportingExportSource struct {
	localMachineID      string
	machineAliases      map[string]string
	codexCloudAccounts  map[string]string
	codexCloudRepeats   reportingCodexCloudRepeats
	activitySessions    []reportingSourceSession
	activityEvents      []activity.ActivityEvent
	usageSessions       []activity.SessionMeta
	usageCandidates     []activityReportUsageCandidate
	standaloneUsage     []activity.UsageRow
	accountUsage        []reportingAccountRow
	cursorEvents        []reportingCursorEventRow
	cursorPolls         []reportingCursorPollRow
	pricing             []export.EffectivePricingRow
	createdAt           map[string]time.Time
	projectObservations []export.ProjectIdentityObservation
	archiveScope        export.IdentityScope
	references          map[string]export.ProjectReference

	loadStats reportingSourceLoadStats
}

type reportingSourceSession struct {
	meta              activity.SessionMeta
	effectiveStart    string
	effectiveEnd      string
	terminalTimestamp string
}

type reportingDaySource struct {
	localMachineID   string
	machineAliases   map[string]string
	archiveID        string
	activitySessions []activity.SessionMeta
	activityIDs      []string
	activityEvents   []activity.ActivityEvent
	usageSessions    []activity.SessionMeta
	usageCandidates  []activityReportUsageCandidate
	standaloneUsage  []activity.UsageRow
	accountUsage     []reportingAccountRow
	cursorEvents     []reportingCursorEventRow
	cursorPolls      []reportingCursorPollRow
	pricing          []export.EffectivePricingRow
	createdAt        map[string]time.Time
	projects         map[string]export.ProjectMapEntry
	references       map[string]export.ProjectReference

	// codexCloudAccounts maps each Codex cloud session ID to the account
	// key it was fetched for, in schemas that report those sessions by
	// account. codexCloudRepeats holds the copies of chats the archive
	// holds more than once that no origin reports.
	codexCloudAccounts map[string]string
	codexCloudRepeats  reportingCodexCloudRepeats
}

// reportingCodexCloudRepeats records how a Codex cloud chat that the
// archive holds more than once is reported. activity holds the copies whose
// activity no origin reports. usageOwner maps every copy of such a chat to
// the copy its usage reports under, or to empty when its usage is dropped.
type reportingCodexCloudRepeats struct {
	activity   map[string]bool
	usageOwner map[string]string
}

func (db *DB) loadReportingExportSource(
	ctx context.Context,
	tx *sql.Tx,
	start, end time.Time,
	schemaVersion int,
) (reportingExportSource, error) {
	source := reportingExportSource{
		activityEvents:  []activity.ActivityEvent{},
		usageSessions:   []activity.SessionMeta{},
		usageCandidates: []activityReportUsageCandidate{},
		standaloneUsage: []activity.UsageRow{},
		createdAt:       map[string]time.Time{},
		references:      map[string]export.ProjectReference{},
	}
	if !start.Before(end) {
		return source, nil
	}

	query, err := activity.ResolveQuery(activity.QueryInput{
		Preset:   "custom",
		From:     start.Format(time.RFC3339),
		To:       end.Format(time.RFC3339),
		Timezone: "UTC",
	}, end)
	if err != nil {
		return source, fmt.Errorf("resolve reporting source range: %w", err)
	}
	rangeStartUTC, rangeEndUTC := activityReportRangeBoundsUTC(query)
	sessions, activityIDs, err := db.activityReportSessionsFrom(
		ctx,
		tx,
		AnalyticsFilter{
			Timezone:         "UTC",
			IncludeSubagents: true,
			IncludeForks:     true,
		},
		rangeStartUTC,
		rangeEndUTC,
	)
	if err != nil {
		return source, err
	}
	events, err := db.activityReportActivityFrom(ctx, tx, activityIDs)
	if err != nil {
		return source, err
	}
	createdAtRaw, createdAt, err := reportingCreatedAtFrom(ctx, tx, activityIDs)
	if err != nil {
		return source, err
	}
	terminalTimestamps, err := reportingTerminalTimestampsFrom(
		ctx, tx, activityIDs, rangeStartUTC,
	)
	if err != nil {
		return source, err
	}

	lowerBound := PaddedUTCBound(start.UTC().Format(time.RFC3339), -14)
	upperBound := PaddedUTCBound(end.UTC().Format(time.RFC3339), 14)
	usageSessions, usageIDs, err := db.reportingUsageSessionsFrom(
		ctx, tx, lowerBound, upperBound,
	)
	if err != nil {
		return source, err
	}
	usageCandidates, pricing, err := db.loadReportingUsageCandidates(
		ctx, tx, usageIDs, lowerBound, upperBound,
	)
	if err != nil {
		return source, err
	}
	standaloneUsage, err := db.reportingStandaloneUsageCandidatesFrom(
		ctx, tx, query, schemaVersion, &source,
	)
	if err != nil {
		return source, err
	}

	maxMessageTimestamp := make(map[string]string, len(activityIDs))
	for _, event := range events {
		if event.Timestamp > maxMessageTimestamp[event.SessionID] {
			maxMessageTimestamp[event.SessionID] = event.Timestamp
		}
	}
	activitySessions := make([]reportingSourceSession, 0, len(sessions))
	for _, session := range sessions {
		startTimestamp := session.StartedAt
		if startTimestamp == "" {
			startTimestamp = createdAtRaw[session.SessionID]
		}
		endTimestamp := session.EndedAt
		if endTimestamp == "" {
			endTimestamp = maxMessageTimestamp[session.SessionID]
			if endTimestamp == "" {
				endTimestamp = startTimestamp
			}
		}
		activitySessions = append(activitySessions, reportingSourceSession{
			meta:              session,
			effectiveStart:    startTimestamp,
			effectiveEnd:      endTimestamp,
			terminalTimestamp: terminalTimestamps[session.SessionID],
		})
	}

	allSessions := mergeReportingSessions(sessions, usageSessions)
	if schemaVersion == export.ReportingCursorSchemaVersion {
		source.codexCloudAccounts, source.codexCloudRepeats, err = reportingCodexCloudAccountsFrom(ctx, tx, allSessions)
		if err != nil {
			return source, err
		}
	}
	labels := activity.ProjectLabels(allSessions)
	observations := []export.ProjectIdentityObservation{}
	archiveScope := export.IdentityScope{}
	if len(labels) > 0 || export.IsJointReportingSchema(schemaVersion) {
		archiveID, archiveSalt, scopeErr := reportingArchiveScope(ctx, tx)
		if scopeErr != nil {
			return source, scopeErr
		}
		archiveScope = export.IdentityScope{
			ArchiveID:   archiveID,
			ArchiveSalt: archiveSalt,
		}
		if len(labels) > 0 {
			observations, err = db.listProjectIdentityObservationsFrom(
				ctx, tx, labels,
			)
			if err != nil {
				return source, err
			}
		}
	}
	var references map[string]export.ProjectReference
	if export.IsJointReportingSchema(schemaVersion) {
		references, err = db.reportingSessionReferences(ctx, tx, allSessions)
		if err != nil {
			return source, err
		}
	}

	source.activitySessions = activitySessions
	source.activityEvents = events
	source.usageSessions = usageSessions
	source.usageCandidates = usageCandidates
	source.standaloneUsage = standaloneUsage
	source.pricing = pricing
	source.createdAt = createdAt
	source.projectObservations = observations
	source.archiveScope = archiveScope
	source.references = references
	source.loadStats = reportingSourceLoadStats{
		ActivitySessions:    len(activitySessions),
		ActivityHistoryRows: len(events),
		UsageSessions:       len(usageSessions),
		PaddedUsageRows:     len(usageCandidates) + len(standaloneUsage),
	}
	return source, nil
}

func (src reportingExportSource) forDate(
	date, end time.Time,
) reportingDaySource {
	startBound, endBound := reportingRangeStringBounds(date, end)
	selectedSessions := filterReportingRows(src.activitySessions, func(candidate *reportingSourceSession) bool {
		eligible := candidate.effectiveEnd >= startBound || candidate.terminalTimestamp >= startBound
		return eligible && candidate.effectiveStart < endBound
	})
	activitySessions := make([]activity.SessionMeta, len(selectedSessions))
	activityIDs := make([]string, len(selectedSessions))
	selected := make(map[string]struct{}, len(selectedSessions))
	for i, candidate := range selectedSessions {
		activitySessions[i] = candidate.meta
		activityIDs[i] = candidate.meta.SessionID
		selected[candidate.meta.SessionID] = struct{}{}
	}

	activityEvents := filterReportingRows(src.activityEvents, func(event *activity.ActivityEvent) bool {
		_, ok := selected[event.SessionID]
		return ok
	})

	lowerBound := PaddedUTCBound(date.Format(time.RFC3339), -14)
	upperBound := PaddedUTCBound(end.Format(time.RFC3339), 14)
	usageCandidates := filterReportingRows(src.usageCandidates, func(candidate *activityReportUsageCandidate) bool {
		return candidate.scan.ts >= lowerBound && candidate.scan.ts <= upperBound
	})
	usageIDs := make(map[string]struct{}, len(usageCandidates))
	for i := range usageCandidates {
		if id := usageCandidates[i].scan.sessionID; id != "" {
			usageIDs[id] = struct{}{}
		}
	}
	usageSessions := filterReportingRows(src.usageSessions, func(session *activity.SessionMeta) bool {
		_, ok := usageIDs[session.SessionID]
		return ok
	})
	standaloneUsage := filterReportingRows(src.standaloneUsage, func(row *activity.UsageRow) bool {
		return row.Timestamp >= lowerBound && row.Timestamp <= upperBound
	})
	var accountUsage []reportingAccountRow
	for _, row := range src.accountUsage {
		if !row.at.Before(date) && row.at.Before(end) {
			accountUsage = append(accountUsage, row)
		}
	}

	allSessions := mergeReportingSessions(activitySessions, usageSessions)
	labels := activity.ProjectLabels(allSessions)
	labelSet := make(map[string]struct{}, len(labels))
	for _, label := range labels {
		labelSet[label] = struct{}{}
	}
	observations := filterReportingRows(src.projectObservations, func(observation *export.ProjectIdentityObservation) bool {
		_, ok := labelSet[observation.Project]
		return ok
	})
	projects := export.BuildProjectsMapWithScope(
		labels, observations, src.archiveScope,
	)
	references := make(map[string]export.ProjectReference, len(allSessions))
	for _, session := range allSessions {
		if reference, ok := src.references[session.SessionID]; ok {
			references[session.SessionID] = reference
		}
	}
	createdAt := make(map[string]time.Time, len(activityIDs))
	for _, id := range activityIDs {
		if created, ok := src.createdAt[id]; ok {
			createdAt[id] = created
		}
	}
	return reportingDaySource{
		localMachineID:   src.localMachineID,
		machineAliases:   src.machineAliases,
		archiveID:        src.archiveScope.ArchiveID,
		activitySessions: activitySessions,
		activityIDs:      activityIDs,
		activityEvents:   activityEvents,
		usageSessions:    usageSessions,
		usageCandidates:  usageCandidates,
		standaloneUsage:  standaloneUsage,
		accountUsage:     accountUsage,
		cursorEvents:     filterReportingRows(src.cursorEvents, func(row *reportingCursorEventRow) bool { return !row.at.Before(date) && row.at.Before(end) }),
		cursorPolls:      filterReportingRows(src.cursorPolls, func(row *reportingCursorPollRow) bool { return !row.at.Before(date) && row.at.Before(end) }),
		pricing:          src.pricing,
		createdAt:        createdAt,
		projects:         projects,
		references:       references,

		codexCloudAccounts: src.codexCloudAccounts,
		codexCloudRepeats:  src.codexCloudRepeats,
	}
}

// filterReportingRows copies the rows that keep accepts into a slice with no
// spare capacity. A digest calls forDate once per date over source rows loaded
// for the whole range, so sizing each date's copy by the source would allocate
// about the range's rows for every date and multiply the digest's peak memory.
func filterReportingRows[T any](rows []T, keep func(*T) bool) []T {
	count := 0
	for i := range rows {
		if keep(&rows[i]) {
			count++
		}
	}
	out := make([]T, 0, count)
	for i := range rows {
		if keep(&rows[i]) {
			out = append(out, rows[i])
		}
	}
	return out
}

func reportingRangeStringBounds(start, end time.Time) (string, string) {
	const layout = "2006-01-02T15:04:05"
	return start.UTC().Format(layout), end.UTC().Format(layout)
}

func (db *DB) loadReportingUsageCandidates(
	ctx context.Context,
	source sessionExportQuerier,
	ids []string,
	lowerBound, upperBound string,
) ([]activityReportUsageCandidate, []export.EffectivePricingRow, error) {
	candidates, pricing, _, err := db.loadActivityReportUsageCandidatesFrom(
		ctx, source, ids, lowerBound, upperBound, true,
	)
	if err != nil {
		return nil, nil, err
	}
	return candidates, pricing, nil
}

func reportingCreatedAtFrom(
	ctx context.Context,
	q sessionExportQuerier,
	ids []string,
) (map[string]string, map[string]time.Time, error) {
	rawValues := make(map[string]string, len(ids))
	parsedValues := make(map[string]time.Time, len(ids))
	if err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := InPlaceholders(chunk)
		rows, err := q.QueryContext(
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
			rawValues[id] = raw
			if created, err := parseTimestamp(raw); err == nil {
				parsedValues[id] = created.UTC()
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating reporting session creation times: %w", err)
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}
	return rawValues, parsedValues, nil
}

// reportingCodexCloudAccountsFrom maps each Codex cloud session to its
// account key: the directory of its mirror file, or for a chat imported
// from another computer's artifacts, the key import recorded. A session
// with neither is left out and stays under its machine.
//
// An archive that fetched a chat and also imported another computer's copy
// holds it under several session IDs. Every copy in the archive is a
// candidate, whatever range the export covers, so a daily export and a
// multi-day digest choose the same copies. The account reports the
// activity of the copy with the newest message, or the local copy when
// they tie. Only the computer that watched a chat run captures its token
// usage, and each copy's usage events add up to the running total that
// computer saw. The account therefore reports the usage of the copy with
// the most tokens, with the same tie-breaks, under the activity copy's ID.
func reportingCodexCloudAccountsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	sessions []activity.SessionMeta,
) (map[string]string, reportingCodexCloudRepeats, error) {
	// Imported IDs are the origin, "~", and the fetching computer's ID.
	native := func(id string) string { return id[strings.LastIndex(id, "~")+1:] }
	inRange := make(map[string]bool)
	for _, session := range sessions {
		if session.Agent == string(parser.AgentCodexCloud) {
			inRange[native(session.SessionID)] = true
		}
	}
	accounts := make(map[string]string)
	copies := make(map[[2]string][]string)
	if len(inRange) == 0 {
		return accounts, reportingCodexCloudRepeats{}, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT s.id, COALESCE(s.file_path, ''), COALESCE(a.account_key, '')
		FROM sessions s LEFT JOIN codex_cloud_accounts a ON a.session_id = s.id
		WHERE s.agent = ? AND s.deleted_at IS NULL`,
		string(parser.AgentCodexCloud),
	)
	if err != nil {
		return nil, reportingCodexCloudRepeats{}, fmt.Errorf("querying reporting Codex cloud accounts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, path, imported string
		if err := rows.Scan(&id, &path, &imported); err != nil {
			return nil, reportingCodexCloudRepeats{}, fmt.Errorf("scanning reporting Codex cloud account: %w", err)
		}
		account := cmp.Or(parser.CodexCloudAccountFromPath(path), imported)
		if account == "" || !inRange[native(id)] {
			continue
		}
		accounts[id] = account
		chat := [2]string{account, native(id)}
		copies[chat] = append(copies[chat], id)
	}
	if err := rows.Err(); err != nil {
		return nil, reportingCodexCloudRepeats{}, fmt.Errorf("iterating reporting Codex cloud accounts: %w", err)
	}
	var repeated []string
	for _, ids := range copies {
		if len(ids) > 1 {
			repeated = append(repeated, ids...)
		}
	}
	newest := make(map[string]time.Time, len(repeated))
	tokens := make(map[string]int64, len(repeated))
	err = queryChunked(repeated, func(chunk []string) error {
		placeholders, args := InPlaceholders(chunk)
		rows, err := q.QueryContext(ctx,
			`SELECT s.id,
				COALESCE((SELECT MAX(m.timestamp) FROM messages m WHERE m.session_id = s.id), ''),
				(SELECT COALESCE(SUM(ue.input_tokens + ue.output_tokens +
					ue.cache_creation_input_tokens + ue.cache_read_input_tokens), 0)
					FROM usage_events ue WHERE ue.session_id = s.id)
			FROM sessions s WHERE s.id IN `+placeholders,
			args...,
		)
		if err != nil {
			return fmt.Errorf("querying reporting Codex cloud copies: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, last string
			var total int64
			if err := rows.Scan(&id, &last, &total); err != nil {
				return fmt.Errorf("scanning reporting Codex cloud copy: %w", err)
			}
			newest[id], _ = time.Parse(time.RFC3339Nano, last)
			tokens[id] = total
		}
		return rows.Err()
	})
	if err != nil {
		return nil, reportingCodexCloudRepeats{}, err
	}
	// Each rank is positive when copy a should be reported over copy b.
	activityRank := func(a, b string) int {
		return cmp.Or(
			newest[a].Compare(newest[b]),
			cmp.Compare(strings.Count(b, "~"), strings.Count(a, "~")),
			strings.Compare(b, a),
		)
	}
	usageRank := func(a, b string) int {
		return cmp.Or(cmp.Compare(tokens[a], tokens[b]), activityRank(a, b))
	}
	repeats := reportingCodexCloudRepeats{
		activity:   make(map[string]bool),
		usageOwner: make(map[string]string),
	}
	for _, ids := range copies {
		if len(ids) < 2 {
			continue
		}
		activityCopy := slices.MaxFunc(ids, activityRank)
		usageCopy := slices.MaxFunc(ids, usageRank)
		for _, id := range ids {
			repeats.activity[id] = id != activityCopy
			repeats.usageOwner[id] = ""
		}
		repeats.usageOwner[usageCopy] = activityCopy
	}
	return accounts, repeats, nil
}

func reportingTerminalTimestampsFrom(
	ctx context.Context,
	q sessionExportQuerier,
	ids []string,
	lowerBound string,
) (map[string]string, error) {
	out := make(map[string]string, len(ids))
	err := queryChunked(ids, func(chunk []string) error {
		placeholders, args := InPlaceholders(chunk)
		args = append(args, lowerBound)
		rows, err := q.QueryContext(ctx, `
			SELECT session_id, MAX(timestamp)
			FROM tool_result_events
			WHERE session_id IN `+placeholders+`
				AND source = 'tool_execution'
				AND status IN ('completed', 'errored')
				AND timestamp IS NOT NULL
				AND timestamp != ''
				AND agentsview_timestamp_unix_micro(timestamp) IS NOT NULL
				AND timestamp >= ?
			GROUP BY session_id`, args...)
		if err != nil {
			return fmt.Errorf("querying reporting terminal timestamps: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id, timestamp string
			if err := rows.Scan(&id, &timestamp); err != nil {
				return fmt.Errorf("scanning reporting terminal timestamp: %w", err)
			}
			out[id] = timestamp
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterating reporting terminal timestamps: %w", err)
		}
		return nil
	})
	return out, err
}

func reportingArchiveScope(
	ctx context.Context,
	q sessionExportQuerier,
) (string, string, error) {
	archiveID, err := sessionExportMetadataValue(
		ctx, q, archiveMetadataArchiveIDKey, ErrArchiveIDMissing, "archive id",
	)
	if err != nil {
		return "", "", err
	}
	archiveSalt, err := sessionExportMetadataValue(
		ctx, q, archiveMetadataArchiveSaltKey, ErrArchiveSaltMissing, "archive salt",
	)
	if err != nil {
		return "", "", err
	}
	return archiveID, archiveSalt, nil
}

func (src reportingExportSource) observerStats() reportingSourceLoadStats {
	return src.loadStats
}
