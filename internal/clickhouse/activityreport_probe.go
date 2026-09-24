package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
)

type activityProbeCache struct {
	mu          sync.Mutex
	fingerprint string
	probe       activity.SourceProbe
}

func (s *Store) ActivityReportSourceProbe(
	ctx context.Context,
) (activity.SourceProbe, error) {
	var fingerprint string
	err := s.queryRowContext(ctx, `SELECT
		hex(SHA256(toString(arraySort(groupArray((table, name, hash_of_all_files))))))
		FROM system.parts
		WHERE database = currentDatabase() AND active
		AND table IN ('sessions', 'messages', 'usage_events', 'model_pricing', 'genai_pricing', 'sync_metadata', 'usage_session_snapshots', 'prepared_usage')`).Scan(&fingerprint)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("reading clickhouse activity source parts: %w", err)
	}
	s.probeCache.mu.Lock()
	probe, cached := s.probeCache.probe, s.probeCache.fingerprint == fingerprint
	s.probeCache.mu.Unlock()
	if cached {
		return probe, nil
	}

	// Parts also change during merges. Recompute the original probe so a merge
	// alone cannot invalidate report tokens or reset session pagination.
	probe, err = s.readActivitySourceProbe(ctx)
	if err != nil {
		return activity.SourceProbe{}, err
	}
	s.probeCache.mu.Lock()
	s.probeCache.fingerprint, s.probeCache.probe = fingerprint, probe
	s.probeCache.mu.Unlock()
	return probe, nil
}

func (s *Store) readActivitySourceProbe(ctx context.Context) (activity.SourceProbe, error) {
	var probe activity.SourceProbe
	err := s.queryRowContext(ctx, `SELECT
		(SELECT toInt64(count()) FROM sessions),
		ifNull(toString((SELECT max(local_modified_at) FROM sessions)), ''),
		ifNull((SELECT max(data_version) FROM sessions), toInt64(0)),
		ifNull((SELECT max(id) FROM messages), toInt64(0)),
		ifNull((SELECT max(id) FROM usage_events), toInt64(0)),
		greatest(
			ifNull((SELECT max(updated_at) FROM model_pricing), ''),
			ifNull((SELECT max(updated_at) FROM genai_pricing), '')
		),
		ifNull((
			SELECT max(toInt64OrZero(value))
			FROM sync_metadata
			WHERE startsWith(key, ?)
		), toInt64(0))`,
		identityRevisionKeyBase).Scan(
		&probe.SessionCount, &probe.MaxSessionModified, &probe.MaxDataVersion,
		&probe.MaxMessageID, &probe.MaxUsageID, &probe.MaxPricingUpdated,
		&probe.ProjectIdentityGeneration,
	)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("probing clickhouse activity report source: %w", err)
	}
	ready, err := s.preparedUsageReady(ctx)
	if err != nil {
		return activity.SourceProbe{}, err
	}
	if ready {
		// Count and sum row hashes so merges and row ordering do not change the
		// fingerprint, while duplicate rows still contribute to it.
		err = s.queryRowContext(ctx, `SELECT hex(SHA256(toString(tuple(count(),sumWithOverflow(row_hash)))))
			FROM (SELECT reinterpretAsUInt128(sipHash128Reference(tuple(*))) AS row_hash
			FROM prepared_usage) SETTINGS final=0`).Scan(&probe.PreparedUsageFingerprint)
		if err != nil {
			return activity.SourceProbe{}, fmt.Errorf("probing prepared usage: %w", err)
		}
	}
	return probe, nil
}

// preparedUsageState reports whether prepared_usage can answer reads in
// place of the raw messages and usage events, and which pricing digest its
// stored price records serve. It is ready when an archive has published
// complete snapshots, the table was prepared by this binary's query, and
// every session has a current snapshot. Each prepared row names the snapshot
// it was derived from, so a push between refreshes only retires the rows of
// the sessions it republished or removed; a read prepares the rows of the
// sessions pushed since the refresh itself, from their snapshots. The stored
// price records serve reads while the mirror still holds the records they
// were copied from.
type preparedUsageState struct {
	ready         bool
	pricingDigest string
	// stamp identifies the prepared rows: the query that built them and the
	// snapshot and price record sets they were derived from. It changes
	// exactly when a refresh changes the rows.
	stamp string
	// changed lists the sessions whose current snapshot the prepared rows do
	// not cover; stale lists the sessions whose prepared rows come from a
	// snapshot that no longer exists. A republished session is in both.
	changed, stale []string
	// deltaRows are the changed sessions' rows, prepared as a refresh would
	// and priced under the catalog digest the state's pricingDigest names,
	// in chPreparedUsageDeltaColumns order.
	deltaRows [][]any
}

// replaced lists the sessions whose stored prepared rows a read skips: the
// stale ones, and every one the delta supplies. A refresh that lands after
// the state was taken may already hold a changed session's rows, and the
// delta supplies them too.
func (st preparedUsageState) replaced() []string {
	ids := slices.Concat(st.changed, st.stale)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (s *Store) preparedUsageState(ctx context.Context) (preparedUsageState, error) {
	var filled uint64
	var fingerprint, comment string
	err := s.queryRowContext(ctx, `SELECT
		(SELECT ifNull(max(toUInt64OrZero(value)),0) FROM sync_metadata WHERE startsWith(key,?)),
		(SELECT hex(SHA256(toString(arraySort(groupArray((table, name, hash_of_all_files))))))
		 FROM system.parts WHERE database = currentDatabase() AND active
		 AND table IN ('sessions', 'usage_session_snapshots', 'usage_event_prices')),
		ifNull((SELECT comment FROM system.tables WHERE database = currentDatabase() AND name = 'prepared_usage'), '')`,
		usageSnapshotReadyKeyBase+":").Scan(&filled, &fingerprint, &comment)
	if err != nil {
		return preparedUsageState{}, fmt.Errorf("checking prepared usage readiness: %w", err)
	}
	// Existing reports remain available while every archive prepares its first
	// complete snapshot. A table prepared by another query may lack the
	// stamp columns, so it is checked before they are read.
	if filled == 0 || comment != chPreparedUsageComment() {
		return preparedUsageState{}, nil
	}
	var stored usageStamp
	err = s.queryRowContext(ctx, `SELECT snapshot_count, snapshot_hash, pricing_digest, price_count
		FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(
		&stored.snapshotCount, &stored.snapshotHash, &stored.pricingDigest, &stored.priceCount)
	if errors.Is(err, sql.ErrNoRows) {
		return preparedUsageState{}, nil
	}
	if err != nil {
		return preparedUsageState{}, fmt.Errorf("reading prepared usage stamp: %w", err)
	}
	stamp := fmt.Sprintf("%s|%d|%d|%s|%d", comment, stored.snapshotCount, stored.snapshotHash,
		stored.pricingDigest, stored.priceCount)
	// Coverage and the sessions pushed since the refresh depend on the
	// tables named in the fingerprint and on the prepared rows, so they only
	// change when those parts or the stamp do.
	live, cached := s.cachedUsageCoverage(fingerprint, stamp)
	if !cached {
		// One caller fills the cache; the requests of a page load that
		// arrive together wait for it instead of each reading coverage.
		s.coverageCache.fill.Lock()
		defer s.coverageCache.fill.Unlock()
		live, cached = s.cachedUsageCoverage(fingerprint, stamp)
	}
	if !cached {
		live, err = s.readUsageCoverage(ctx, stamp, stored.pricingDigest)
		if err != nil {
			return preparedUsageState{}, err
		}
		if len(live.changed) > 0 {
			live.deltaRows, live.deltaDigest, err = s.prepareUsageDelta(ctx, live.changedKeys)
			if err != nil {
				return preparedUsageState{}, err
			}
		}
		s.coverageCache.mu.Lock()
		s.coverageCache.fingerprint, s.coverageCache.stamp, s.coverageCache.live = fingerprint, stamp, live
		s.coverageCache.mu.Unlock()
	}
	state := preparedUsageState{
		ready:     live.missing == 0,
		stamp:     stamp,
		changed:   live.changed,
		stale:     live.stale,
		deltaRows: live.deltaRows,
	}
	// The stored records and the delta's must be under the same digest for
	// a read to take either in place of the join.
	if state.ready && stored.pricingDigest != "" && live.priceCount >= stored.priceCount &&
		(len(live.changed) == 0 || live.deltaDigest == stored.pricingDigest) {
		state.pricingDigest = stored.pricingDigest
	}
	return state, nil
}

// chStringArrayLiteral renders values as a ClickHouse Array(String)
// literal, the text form of a query parameter.
func chStringArrayLiteral(values []string) string {
	escape := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('\'')
		b.WriteString(escape.Replace(v))
		b.WriteByte('\'')
	}
	b.WriteByte(']')
	return b.String()
}

// readUsageCoverage relates the live sessions and snapshots to the prepared
// rows. The comparisons run in Go over the identities ClickHouse returns:
// as anti-joins under FINAL they took most of a post-push request. The
// snapshots the prepared rows cover change only with the stamp, so they are
// read once per refresh; the price record count is read alongside.
func (s *Store) readUsageCoverage(ctx context.Context, stamp, pricingDigest string) (usageCoverage, error) {
	var live usageCoverage
	var priceCount uint64
	priceDone := make(chan error, 1)
	go func() {
		priceDone <- s.queryRowContext(ctx, `SELECT count() FROM usage_event_prices WHERE pricing_digest = ?`,
			pricingDigest).Scan(&priceCount)
	}()
	covered, err := s.preparedCoveredSnapshots(ctx, stamp)
	if err == nil {
		err = s.compareUsageCoverage(ctx, covered, &live)
	}
	if priceErr := <-priceDone; err == nil && priceErr != nil {
		err = fmt.Errorf("counting usage price records: %w", priceErr)
	}
	live.priceCount = priceCount
	return live, err
}

// usageSnapshotKey names one session snapshot.
type usageSnapshotKey struct {
	id       string
	revision string
}

// preparedCoveredSnapshots returns the snapshots the prepared rows were
// derived from, reading them only when the stamp changed.
func (s *Store) preparedCoveredSnapshots(ctx context.Context, stamp string) (map[usageSnapshotKey]bool, error) {
	s.coverageCache.mu.Lock()
	covered := s.coverageCache.covered
	cached := covered != nil && s.coverageCache.coveredStamp == stamp
	s.coverageCache.mu.Unlock()
	if cached {
		return covered, nil
	}
	rows, err := s.queryContext(ctx, `SELECT session_id, toString(snapshot_revision) FROM prepared_usage
		GROUP BY session_id, snapshot_revision SETTINGS final=0`)
	if err != nil {
		return nil, fmt.Errorf("reading prepared usage snapshots: %w", err)
	}
	defer rows.Close()
	covered = map[usageSnapshotKey]bool{}
	for rows.Next() {
		var key usageSnapshotKey
		if err := rows.Scan(&key.id, &key.revision); err != nil {
			return nil, fmt.Errorf("scanning prepared usage snapshot: %w", err)
		}
		covered[key] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating prepared usage snapshots: %w", err)
	}
	s.coverageCache.mu.Lock()
	s.coverageCache.coveredStamp, s.coverageCache.covered = stamp, covered
	s.coverageCache.mu.Unlock()
	return covered, nil
}

// compareUsageCoverage fills the number of sessions without a current
// snapshot, the snapshots the prepared rows do not cover, and the covered
// snapshots that no longer exist. A snapshot without usage facts or events
// prepares no rows, so it is covered whether or not the refresh saw it; its
// array sizes come from the size subcolumns, since under FINAL length()
// reads the arrays.
func (s *Store) compareUsageCoverage(ctx context.Context, covered map[usageSnapshotKey]bool, live *usageCoverage) error {
	rows, err := s.queryContext(ctx, `SELECT 0, id, push_version, '', false FROM sessions
		UNION ALL
		SELECT 1, id, push_version, toString(revision),
			usage_messages.size0 != 0 OR usage_events.size0 != 0 FROM usage_session_snapshots`)
	if err != nil {
		return fmt.Errorf("reading usage coverage: %w", err)
	}
	defer rows.Close()
	sessions := map[string]uint64{}
	snapshots := map[string]uint64{}
	current := map[usageSnapshotKey]bool{}
	for rows.Next() {
		var table uint8
		var id, revision string
		var version uint64
		var hasUsage bool
		if err := rows.Scan(&table, &id, &version, &revision, &hasUsage); err != nil {
			return fmt.Errorf("scanning usage coverage: %w", err)
		}
		if table == 0 {
			sessions[id] = version
			continue
		}
		snapshots[id] = version
		key := usageSnapshotKey{id: id, revision: revision}
		current[key] = true
		if hasUsage && !covered[key] {
			live.changed = append(live.changed, id)
			live.changedKeys = append(live.changedKeys, key)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating usage coverage: %w", err)
	}
	for id, version := range sessions {
		if snapshot, ok := snapshots[id]; !ok || snapshot < version {
			live.missing++
		}
	}
	for key := range covered {
		if !current[key] {
			live.stale = append(live.stale, key.id)
		}
	}
	slices.Sort(live.changed)
	slices.Sort(live.stale)
	return nil
}

func (s *Store) cachedUsageCoverage(fingerprint, stamp string) (usageCoverage, bool) {
	s.coverageCache.mu.Lock()
	defer s.coverageCache.mu.Unlock()
	return s.coverageCache.live, s.coverageCache.fingerprint == fingerprint && s.coverageCache.stamp == stamp
}

func (s *Store) preparedUsageReady(ctx context.Context) (bool, error) {
	state, err := s.preparedUsageState(ctx)
	return state.ready, err
}

// prepareUsageDelta prepares the rows of the changed snapshots as a refresh
// would, priced under the current catalog. A snapshot's rows depend only
// on the snapshot and the catalog, so they are kept per snapshot and
// catalog digest and read again only for snapshots new since the last
// call. Sessions whose usage no refresh prepares a row for stay changed
// at every call; without the kept rows each call read all of them again.
func (s *Store) prepareUsageDelta(ctx context.Context, changed []usageSnapshotKey) ([][]any, string, error) {
	snapshot, err := s.pricingSnapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	s.deltaCache.mu.Lock()
	kept := s.deltaCache.rows
	if s.deltaCache.digest != snapshot.catalog.digest {
		kept = nil
	}
	s.deltaCache.mu.Unlock()
	var out [][]any
	var missing []usageSnapshotKey
	next := make(map[usageSnapshotKey][][]any, len(changed))
	for _, key := range changed {
		rows, ok := kept[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		out = append(out, rows...)
		next[key] = rows
	}
	if len(missing) > 0 {
		ids := make([]string, len(missing))
		for i, key := range missing {
			ids[i] = key.id
		}
		read, err := s.readUsageDeltaRows(ctx, snapshot, ids)
		if err != nil {
			return nil, "", err
		}
		// The rows read are of each session's snapshot at the time of the
		// read. Revisions only grow, so a session whose snapshot is still
		// the one asked for after the read was read at that snapshot; the
		// others are used but not kept.
		current, err := s.usageSnapshotRevisions(ctx, ids)
		if err != nil {
			return nil, "", err
		}
		for _, key := range missing {
			out = append(out, read[key.id]...)
			if current[key.id] == key.revision {
				next[key] = read[key.id]
			}
		}
	}
	s.deltaCache.mu.Lock()
	s.deltaCache.digest, s.deltaCache.rows = snapshot.catalog.digest, next
	s.deltaCache.mu.Unlock()
	return out, snapshot.catalog.digest, nil
}

// usageSnapshotRevisions reads the current snapshot revision of each id.
func (s *Store) usageSnapshotRevisions(ctx context.Context, ids []string) (map[string]string, error) {
	rows, err := s.queryContext(chdriver.Context(ctx, chdriver.WithParameters(chdriver.Parameters{
		"usage_changed_sessions": chStringArrayLiteral(ids),
	})), `SELECT id, toString(revision) FROM usage_session_snapshots s WHERE `+chUsageChangedSessionsWhere)
	if err != nil {
		return nil, fmt.Errorf("reading changed usage snapshots: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string, len(ids))
	for rows.Next() {
		var id, revision string
		if err := rows.Scan(&id, &revision); err != nil {
			return nil, fmt.Errorf("scanning changed usage snapshot: %w", err)
		}
		out[id] = revision
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating changed usage snapshots: %w", err)
	}
	return out, nil
}

// readUsageDeltaRows reads and prices the current rows of the sessions,
// by session.
func (s *Store) readUsageDeltaRows(ctx context.Context, snapshot *pricingSnapshot, changed []string) (map[string][][]any, error) {
	resolver := export.NewPricingResolver(snapshot.catalog.rows)
	rows, err := s.queryContext(chdriver.Context(ctx, chdriver.WithParameters(chdriver.Parameters{
		"usage_changed_sessions": chStringArrayLiteral(changed),
	})), chPreparedUsageDeltaRowsSQL())
	if err != nil {
		return nil, fmt.Errorf("querying changed usage rows: %w", err)
	}
	defer rows.Close()
	const stored = 8
	width := len(chPreparedUsageDeltaColumns) - stored
	var out [][]any
	inputs := map[string]chUsagePriceInput{}
	for rows.Next() {
		row := make([]any, width, len(chPreparedUsageDeltaColumns))
		targets := make([]any, width)
		for i := range row {
			targets[i] = &row[i]
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scanning changed usage row: %w", err)
		}
		out = append(out, row)
		in := chUsageDeltaPriceInput(row)
		if _, seen := inputs[in.priceKey]; !seen {
			inputs[in.priceKey] = in
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating changed usage rows: %w", err)
	}
	contexts := map[string]string{}
	records := make(map[string]chUsagePriceRecord, len(inputs))
	for key, in := range inputs {
		record, err := chPriceUsageInput(in, resolver, contexts)
		if err != nil {
			return nil, err
		}
		records[key] = record
	}
	bySession := map[string][][]any{}
	for _, row := range out {
		record := records[row[22].(string)]
		row = append(row, int64(1), record.tokenCost.Microdollars, record.savings.Microdollars,
			record.billedContextID, record.unbilledContextID, record.requestScoped, record.bandAbove, record.priceError)
		id := row[0].(string)
		bySession[id] = append(bySession[id], row)
	}
	return bySession, nil
}

// chUsageDeltaPriceInput reads the pricing input a push derives from a
// normalized row out of a prepared delta row.
func chUsageDeltaPriceInput(row []any) chUsagePriceInput {
	cost, _ := row[19].(*int64)
	ordinal, _ := row[1].(*int64)
	return chUsagePriceInput{
		priceKey:     row[22].(string),
		model:        row[5].(string),
		priceModel:   row[21].(string),
		providerID:   row[6].(string),
		pricingTS:    formatDBTime(row[3]),
		source:       row[4].(string),
		hasOrdinal:   ordinal != nil,
		inputTok:     int(row[12].(int64)),
		outputTok:    int(row[13].(int64)),
		reasoningTok: int(row[17].(int64)),
		cacheCr:      int(row[14].(int64)),
		cacheCr1h:    int(row[15].(int64)),
		cacheRd:      int(row[16].(int64)),
		reported:     cost != nil && row[20].(string) != "copilot-reported",
	}
}

// usageCoverageCache memoizes the coverage of the prepared rows per parts
// of the sessions, snapshot, and price record tables and per stamp.
type usageCoverageCache struct {
	mu          sync.Mutex
	fingerprint string
	stamp       string
	live        usageCoverage
	// covered is the set of snapshots the prepared rows under coveredStamp
	// were derived from.
	coveredStamp string
	covered      map[usageSnapshotKey]bool
	// fill serializes the readers that found the cache stale.
	fill sync.Mutex
}

// usageStamp identifies a snapshot set and the price records under one
// digest, as stored on prepared rows.
type usageStamp struct {
	snapshotCount, snapshotHash uint64
	pricingDigest               string
	priceCount                  uint64
}

// usageDeltaCache keeps, under one catalog digest, the prepared rows of
// the snapshots the last delta covered; see prepareUsageDelta.
type usageDeltaCache struct {
	mu     sync.Mutex
	digest string
	rows   map[usageSnapshotKey][][]any
}

// usageCoverage relates the live tables to the prepared rows: the sessions
// without a current snapshot, the price records still under the stored
// digest, and the sessions pushed since the refresh.
type usageCoverage struct {
	missing, priceCount uint64
	changed, stale      []string
	// changedKeys are the changed sessions' current snapshots.
	changedKeys []usageSnapshotKey
	deltaRows   [][]any
	deltaDigest string
}
