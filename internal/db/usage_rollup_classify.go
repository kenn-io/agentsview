package db

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// usageCacheStatementRunner runs statements on the usage cache through a
// connection, transaction, or pool handle.
type usageCacheStatementRunner interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// usageDedupIdentitySet holds distinct deduplication identities: Claude
// snapshot pairs, message source UUIDs, and usage-event dedup keys.
type usageDedupIdentitySet struct {
	snapshot   map[[2]string]bool
	sourceUUID map[string]bool
	usageKey   map[string]bool
}

func newUsageDedupIdentitySet() usageDedupIdentitySet {
	return usageDedupIdentitySet{
		snapshot:   make(map[[2]string]bool),
		sourceUUID: make(map[string]bool),
		usageKey:   make(map[string]bool),
	}
}

func (set usageDedupIdentitySet) add(
	messageID, requestID, sourceUUID, usageKey string,
) {
	if messageID != "" && requestID != "" {
		set.snapshot[[2]string{messageID, requestID}] = true
	}
	if sourceUUID != "" {
		set.sourceUUID[sourceUUID] = true
	}
	if usageKey != "" {
		set.usageKey[usageKey] = true
	}
}

func (set usageDedupIdentitySet) isEmpty() bool {
	return len(set.snapshot) == 0 && len(set.sourceUUID) == 0 &&
		len(set.usageKey) == 0
}

func (set usageDedupIdentitySet) subsetOf(other usageDedupIdentitySet) bool {
	for identity := range set.snapshot {
		if !other.snapshot[identity] {
			return false
		}
	}
	for identity := range set.sourceUUID {
		if !other.sourceUUID[identity] {
			return false
		}
	}
	for identity := range set.usageKey {
		if !other.usageKey[identity] {
			return false
		}
	}
	return true
}

// mergeDifferences adds every identity present in exactly one of left and
// right, accumulating the symmetric difference of a fact replacement.
func (set usageDedupIdentitySet) mergeDifferences(
	left, right usageDedupIdentitySet,
) {
	for identity := range left.snapshot {
		if !right.snapshot[identity] {
			set.snapshot[identity] = true
		}
	}
	for identity := range right.snapshot {
		if !left.snapshot[identity] {
			set.snapshot[identity] = true
		}
	}
	for identity := range left.sourceUUID {
		if !right.sourceUUID[identity] {
			set.sourceUUID[identity] = true
		}
	}
	for identity := range right.sourceUUID {
		if !left.sourceUUID[identity] {
			set.sourceUUID[identity] = true
		}
	}
	for identity := range left.usageKey {
		if !right.usageKey[identity] {
			set.usageKey[identity] = true
		}
	}
	for identity := range right.usageKey {
		if !left.usageKey[identity] {
			set.usageKey[identity] = true
		}
	}
}

func (set usageDedupIdentitySet) containsSnapshotIdentity(fact usageRollupFact) bool {
	return set.snapshot[[2]string{
		fact.Fact.ClaudeMessageID, fact.Fact.ClaudeRequestID,
	}]
}

func (set usageDedupIdentitySet) containsGeneralIdentity(fact usageRollupFact) bool {
	if usageRollupGeneralKeyIsSource(fact) {
		return set.sourceUUID[fact.Fact.SourceUUID]
	}
	if fact.Fact.UsageDedupKey != "" {
		return set.usageKey[fact.Fact.UsageDedupKey]
	}
	return false
}

// usageRollupSurvivor is one build-time-finalized daily contribution: the
// deduplication winner plus the output tokens its group discarded.
type usageRollupSurvivor struct {
	Fact                          usageRollupFact
	DiscardedSnapshotOutputTokens int64
}

// classifyUsageRollupFacts splits one session's facts into contributions that
// finalize into daily rows and irreducible deduplication exceptions. A group
// finalizes only when its resolution cannot vary with the query window or
// live filters: every member shares one local date (and one model and
// headless state for general dedup), no member's identity appears outside the
// session, no member links snapshot and general dedup, and no member carries
// a Copilot authoritative cost whose per-session selection is
// order-dependent. Query windows are whole local days, so a single-date group
// is inside or outside any window as a unit.
func classifyUsageRollupFacts(
	facts []usageRollupFact, cross usageDedupIdentitySet,
) ([]usageRollupSurvivor, []usageRollupFact) {
	snapshots := make(map[string][]int)
	generals := make(map[string][]int)
	var plain []int
	for index := range facts {
		if !facts[index].Fact.TokenEligible {
			continue
		}
		snapshotKey := usageRollupSnapshotKey(facts[index])
		generalKey := usageRollupGeneralKey(facts[index])
		if snapshotKey == "" && generalKey == "" {
			plain = append(plain, index)
			continue
		}
		if snapshotKey != "" {
			snapshots[snapshotKey] = append(snapshots[snapshotKey], index)
		}
		if generalKey != "" {
			generals[generalKey] = append(generals[generalKey], index)
		}
	}

	unsafeSnapshots := make(map[string]bool)
	unsafeGenerals := make(map[string]bool)
	for key, members := range snapshots {
		for _, index := range members {
			fact := facts[index]
			if cross.containsSnapshotIdentity(fact) ||
				fact.LocalDate != facts[members[0]].LocalDate {
				unsafeSnapshots[key] = true
			}
			// A snapshot survivor carrying a usage dedup key re-enters
			// general ranking at read time, so both linked groups stay on
			// the exception tier.
			if generalKey := usageRollupGeneralKey(fact); generalKey != "" {
				unsafeSnapshots[key] = true
				unsafeGenerals[generalKey] = true
			}
		}
	}
	for key, members := range generals {
		for _, index := range members {
			fact := facts[index]
			first := facts[members[0]]
			if cross.containsGeneralIdentity(fact) ||
				fact.LocalDate != first.LocalDate ||
				fact.Model != first.Model ||
				fact.IsHeadless != first.IsHeadless ||
				usageFactHasAuthoritativeCost(fact) {
				unsafeGenerals[key] = true
			}
		}
	}

	exceptional := make(map[int]bool)
	var survivors []usageRollupSurvivor
	for _, index := range plain {
		survivors = append(survivors, usageRollupSurvivor{Fact: facts[index]})
	}
	for _, key := range usageSortedMapKeys(snapshots) {
		members := snapshots[key]
		if len(members) == 0 {
			continue
		}
		if unsafeSnapshots[key] {
			for _, index := range members {
				exceptional[index] = true
			}
			continue
		}
		survivors = append(survivors, rankSafeSnapshotGroup(facts, members))
	}
	for _, key := range usageSortedMapKeys(generals) {
		members := generals[key]
		if len(members) == 0 {
			continue
		}
		if unsafeGenerals[key] {
			for _, index := range members {
				exceptional[index] = true
			}
			continue
		}
		winner := members[0]
		for _, index := range members[1:] {
			if compareUsageGeneralWinner(facts[index], facts[winner]) < 0 {
				winner = index
			}
		}
		survivors = append(survivors, usageRollupSurvivor{Fact: facts[winner]})
	}

	exceptions := make([]usageRollupFact, 0, len(exceptional))
	for index := range exceptional {
		exceptions = append(exceptions, facts[index])
	}
	slices.SortFunc(exceptions, compareUsageRollupFactIdentity)
	slices.SortFunc(survivors, func(left, right usageRollupSurvivor) int {
		return compareUsageRollupFactIdentity(left.Fact, right.Fact)
	})
	return survivors, exceptions
}

// rankSafeSnapshotGroup applies the request-time snapshot ranking rules to a
// group proven safe to finalize: greatest-output winner, earliest-row
// attribution, maximum web-search count, and discarded losing output.
func rankSafeSnapshotGroup(
	facts []usageRollupFact, members []int,
) usageRollupSurvivor {
	winnerIndex, attributionIndex := members[0], members[0]
	webSearch := facts[members[0]].Fact.WebSearchRequests
	for _, index := range members[1:] {
		if compareUsageSnapshotWinner(facts[index], facts[winnerIndex]) > 0 {
			winnerIndex = index
		}
		if compareUsageSnapshotAttribution(facts[index], facts[attributionIndex]) < 0 {
			attributionIndex = index
		}
		webSearch = max(webSearch, facts[index].Fact.WebSearchRequests)
	}
	survivor := usageRollupSurvivor{Fact: facts[winnerIndex]}
	survivor.Fact.AttributionSessionID = facts[attributionIndex].SourceSessionID
	survivor.Fact.Fact.WebSearchRequests = webSearch
	for _, index := range members {
		if index != winnerIndex {
			survivor.DiscardedSnapshotOutputTokens += facts[index].Fact.OutputTokens
		}
	}
	return survivor
}

func usageFactHasAuthoritativeCost(fact usageRollupFact) bool {
	return fact.Fact.ReportedCostMicrodollars != nil &&
		fact.Fact.CostSource == CopilotReportedCostSource
}

func usageRollupGeneralKeyIsSource(fact usageRollupFact) bool {
	return fact.Fact.Source == "message" && fact.Agent != "" &&
		fact.Fact.SourceUUID != "" && usageRollupSnapshotKey(fact) == ""
}

// Cross-identity discovery requires the usage_rollup_build_sessions temp
// table populated by loadUsageRollupFacts on the same connection. The
// redundant non-empty terms inside EXISTS let SQLite prove the partial
// indexes usable.
const usageRollupCrossSnapshotSQL = `SELECT DISTINCT
	f.claude_message_id, f.claude_request_id
	FROM usage_rollup_build_sessions selected
	JOIN usage_cached_sessions cs ON cs.session_id = selected.session_id
	JOIN usage_facts f ON f.cached_session_id = cs.id
	WHERE f.claude_message_id != '' AND f.claude_request_id != ''
	  AND EXISTS (SELECT 1 FROM usage_facts other
		WHERE other.claude_message_id = f.claude_message_id
		  AND other.claude_request_id = f.claude_request_id
		  AND other.claude_message_id != '' AND other.claude_request_id != ''
		  AND other.cached_session_id != f.cached_session_id)`

const usageRollupCrossSourceUUIDSQL = `SELECT DISTINCT f.source_uuid
	FROM usage_rollup_build_sessions selected
	JOIN usage_cached_sessions cs ON cs.session_id = selected.session_id
	JOIN usage_facts f ON f.cached_session_id = cs.id
	WHERE f.source_uuid != ''
	  AND EXISTS (SELECT 1 FROM usage_facts other
		WHERE other.source_uuid = f.source_uuid AND other.source_uuid != ''
		  AND other.cached_session_id != f.cached_session_id)`

const usageRollupCrossUsageKeySQL = `SELECT DISTINCT f.usage_dedup_key
	FROM usage_rollup_build_sessions selected
	JOIN usage_cached_sessions cs ON cs.session_id = selected.session_id
	JOIN usage_facts f ON f.cached_session_id = cs.id
	WHERE f.usage_dedup_key != ''
	  AND (EXISTS (SELECT 1 FROM usage_facts other
			WHERE other.usage_dedup_key = f.usage_dedup_key
			  AND other.usage_dedup_key != ''
			  AND other.cached_session_id != f.cached_session_id)
		OR EXISTS (SELECT 1 FROM cursor_usage_facts cursor_fact
			WHERE cursor_fact.dedup_key = f.usage_dedup_key))`

// loadUsageRollupCrossIdentities returns the dedup identities of the sessions
// selected in usage_rollup_build_sessions that also appear in another cached
// session or in the Cursor fact store. Their groups cannot be finalized by a
// per-session build.
func loadUsageRollupCrossIdentities(
	ctx context.Context, runner usageCacheStatementRunner,
) (usageDedupIdentitySet, error) {
	cross := newUsageDedupIdentitySet()
	if err := scanUsageIdentityRows(ctx, runner, usageRollupCrossSnapshotSQL,
		func(first, second string) {
			cross.add(first, second, "", "")
		}); err != nil {
		return cross, fmt.Errorf("loading cross-session snapshot identities: %w", err)
	}
	if err := scanUsageIdentityColumn(ctx, runner, usageRollupCrossSourceUUIDSQL,
		func(value string) {
			cross.add("", "", value, "")
		}); err != nil {
		return cross, fmt.Errorf("loading cross-session source identities: %w", err)
	}
	if err := scanUsageIdentityColumn(ctx, runner, usageRollupCrossUsageKeySQL,
		func(value string) {
			cross.add("", "", "", value)
		}); err != nil {
		return cross, fmt.Errorf("loading cross-session usage keys: %w", err)
	}
	return cross, nil
}

func scanUsageIdentityRows(
	ctx context.Context, runner usageCacheStatementRunner, query string,
	record func(first, second string),
) error {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var first, second string
		if err := rows.Scan(&first, &second); err != nil {
			return err
		}
		record(first, second)
	}
	return rows.Err()
}

func scanUsageIdentityColumn(
	ctx context.Context, runner usageCacheStatementRunner, query string,
	record func(value string),
) error {
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		record(value)
	}
	return rows.Err()
}

// usageFactIdentitiesForSessions returns the distinct dedup identities the
// cache currently stores for the given source sessions.
func usageFactIdentitiesForSessions(
	ctx context.Context, runner usageCacheStatementRunner, sessionIDs []string,
) (usageDedupIdentitySet, error) {
	set := newUsageDedupIdentitySet()
	err := queryChunked(sessionIDs, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		return scanUsageIdentityFactRows(ctx, runner, `SELECT DISTINCT
			f.claude_message_id, f.claude_request_id, f.source_uuid,
			f.usage_dedup_key
			FROM usage_cached_sessions cs
			JOIN usage_facts f ON f.cached_session_id = cs.id
			WHERE cs.session_id IN `+placeholders, args, set)
	})
	if err != nil {
		return set, fmt.Errorf("collecting cached usage identities: %w", err)
	}
	return set, nil
}

// usageSpoolIdentitiesForSessions returns the distinct dedup identities the
// attached fill spool carries for the given source sessions.
func usageSpoolIdentitiesForSessions(
	ctx context.Context, runner usageCacheStatementRunner, sessionIDs []string,
) (usageDedupIdentitySet, error) {
	set := newUsageDedupIdentitySet()
	err := queryChunked(sessionIDs, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		return scanUsageIdentityFactRows(ctx, runner, `SELECT DISTINCT
			claude_message_id, claude_request_id, source_uuid, usage_dedup_key
			FROM usage_fill_spool.facts
			WHERE session_id IN `+placeholders, args, set)
	})
	if err != nil {
		return set, fmt.Errorf("collecting spooled usage identities: %w", err)
	}
	return set, nil
}

func scanUsageIdentityFactRows(
	ctx context.Context, runner usageCacheStatementRunner, query string,
	args []any, set usageDedupIdentitySet,
) error {
	rows, err := runner.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var messageID, requestID, sourceUUID, usageKey string
		if err := rows.Scan(&messageID, &requestID, &sourceUUID, &usageKey); err != nil {
			return err
		}
		set.add(messageID, requestID, sourceUUID, usageKey)
	}
	return rows.Err()
}

// invalidateUsageDedupSharers deletes every timezone rollup install of the
// sessions, other than the excluded ones, that store a fact matching one of
// the changed dedup identities. Their next read reclassifies groups whose
// membership changed, so a finalized daily row can never survive gaining a
// sibling.
func invalidateUsageDedupSharers(
	ctx context.Context, runner usageCacheStatementRunner,
	changed usageDedupIdentitySet, excluded map[string]bool,
) error {
	if changed.isEmpty() {
		return nil
	}
	if _, err := runner.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS
		usage_changed_identities(
			kind INTEGER NOT NULL, first TEXT NOT NULL, second TEXT NOT NULL,
			PRIMARY KEY (kind, first, second)
		) WITHOUT ROWID;
		DELETE FROM usage_changed_identities`); err != nil {
		return fmt.Errorf("preparing changed usage identities: %w", err)
	}
	rows := make([][3]any, 0,
		len(changed.snapshot)+len(changed.sourceUUID)+len(changed.usageKey))
	for identity := range changed.snapshot {
		rows = append(rows, [3]any{0, identity[0], identity[1]})
	}
	for identity := range changed.sourceUUID {
		rows = append(rows, [3]any{1, identity, ""})
	}
	for identity := range changed.usageKey {
		rows = append(rows, [3]any{2, identity, ""})
	}
	const insertBatch = 300
	for start := 0; start < len(rows); start += insertBatch {
		end := min(start+insertBatch, len(rows))
		query := `INSERT OR IGNORE INTO usage_changed_identities(kind, first, second)
			VALUES ` + strings.TrimSuffix(strings.Repeat("(?, ?, ?),", end-start), ",")
		args := make([]any, 0, (end-start)*3)
		for _, row := range rows[start:end] {
			args = append(args, row[0], row[1], row[2])
		}
		if _, err := runner.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("recording changed usage identities: %w", err)
		}
	}
	sharerRows, err := runner.QueryContext(ctx, `
		SELECT DISTINCT cs.session_id
		FROM usage_changed_identities c
		JOIN usage_facts f ON c.kind = 0
			AND f.claude_message_id = c.first AND f.claude_request_id = c.second
			AND f.claude_message_id != '' AND f.claude_request_id != ''
		JOIN usage_cached_sessions cs ON cs.id = f.cached_session_id
		UNION
		SELECT DISTINCT cs.session_id
		FROM usage_changed_identities c
		JOIN usage_facts f ON c.kind = 1
			AND f.source_uuid = c.first AND f.source_uuid != ''
		JOIN usage_cached_sessions cs ON cs.id = f.cached_session_id
		UNION
		SELECT DISTINCT cs.session_id
		FROM usage_changed_identities c
		JOIN usage_facts f ON c.kind = 2
			AND f.usage_dedup_key = c.first AND f.usage_dedup_key != ''
		JOIN usage_cached_sessions cs ON cs.id = f.cached_session_id`)
	if err != nil {
		return fmt.Errorf("finding usage dedup sharers: %w", err)
	}
	var sharers []string
	for sharerRows.Next() {
		var sessionID string
		if err := sharerRows.Scan(&sessionID); err != nil {
			_ = sharerRows.Close()
			return err
		}
		if !excluded[sessionID] {
			sharers = append(sharers, sessionID)
		}
	}
	if err := sharerRows.Close(); err != nil {
		return err
	}
	if err := sharerRows.Err(); err != nil {
		return err
	}
	return queryChunked(sharers, func(chunk []string) error {
		placeholders, args := inPlaceholders(chunk)
		if _, err := runner.ExecContext(ctx,
			`DELETE FROM usage_rollup_installs WHERE session_id IN `+placeholders,
			args...); err != nil {
			return fmt.Errorf("invalidating usage dedup sharers: %w", err)
		}
		return nil
	})
}
