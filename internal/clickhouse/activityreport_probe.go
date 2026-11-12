package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"maps"
	"math"
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
	// messageIDs and usageEventIDs keep the highest ids per part; see
	// maxFinalID.
	messageIDs, usageEventIDs finalIDs
}

// activityProbeTables are the tables whose parts the cached probe names.
var activityProbeTables = []string{
	"sessions", "messages", "usage_events",
	"model_pricing", "genai_pricing", "sync_metadata", "usage_session_snapshots", "prepared_usage",
}

func (s *Store) ActivityReportSourceProbe(
	ctx context.Context,
) (activity.SourceProbe, error) {
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return activity.SourceProbe{}, err
	}
	fingerprint, err := s.tablePartsFingerprint(ctx, activityProbeTables)
	if err != nil {
		return activity.SourceProbe{}, err
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

// readActivitySourceProbe reads the probe. The parts snapshot in ctx names
// the message and usage event parts whose highest ids it starts from.
func (s *Store) readActivitySourceProbe(ctx context.Context) (activity.SourceProbe, error) {
	var probe activity.SourceProbe
	err := s.queryRowContext(ctx, `SELECT
		(SELECT toInt64(count()) FROM sessions),
		ifNull(toString((SELECT max(local_modified_at) FROM sessions)), ''),
		ifNull((SELECT max(data_version) FROM sessions), 0),
		greatest(
			ifNull((SELECT max(updated_at) FROM model_pricing), ''),
			ifNull((SELECT max(updated_at) FROM genai_pricing), '')
		),
		ifNull((
			SELECT max(toInt64OrZero(value))
			FROM sync_metadata
			WHERE startsWith(key, ?)
		), 0)`,
		identityRevisionKeyBase).Scan(
		&probe.SessionCount, &probe.MaxSessionModified, &probe.MaxDataVersion,
		&probe.MaxPricingUpdated, &probe.ProjectIdentityGeneration,
	)
	if err != nil {
		return activity.SourceProbe{}, fmt.Errorf("probing clickhouse activity report source: %w", err)
	}
	snapshot, _ := ctx.Value(partsSnapshotKey{}).(partsSnapshot)
	if probe.MaxMessageID, err = s.maxFinalID(ctx, "messages", snapshot["messages"], &s.probeCache.messageIDs); err != nil {
		return activity.SourceProbe{}, err
	}
	if probe.MaxUsageID, err = s.maxFinalID(ctx, "usage_events", snapshot["usage_events"], &s.probeCache.usageEventIDs); err != nil {
		return activity.SourceProbe{}, err
	}
	ready, err := s.preparedUsageReady(ctx)
	if err != nil {
		return activity.SourceProbe{}, err
	}
	if ready {
		// Count and sum row hashes so merges and row ordering do not change the
		// fingerprint, while duplicate rows still contribute to it.
		err = s.queryRowContext(ctx, `SELECT hex(SHA256(toString(tuple(count(),sumWithOverflow(row_hash)))))
			FROM (SELECT `+chUsageRowHashSQL("*")+` AS row_hash
			FROM prepared_usage) SETTINGS final=0`).Scan(&probe.PreparedUsageFingerprint)
		if err != nil {
			return activity.SourceProbe{}, fmt.Errorf("probing prepared usage: %w", err)
		}
	}
	return probe, nil
}

// topIDsPerPart is how many of each part's highest ids finalIDs keeps.
const topIDsPerPart = 16

// finalIDs keeps the highest ids of each part of a table whose rows are
// keyed by session, with their sessions. Rows of a part change only through
// a lightweight delete, which hides them without renaming the part; any
// other write makes a new part. So every row of a part with an id above the
// lowest kept one is kept, as long as the part keeps all its ids or the
// part's top list is full.
type finalIDs struct {
	mu    sync.Mutex
	parts map[string][]sessionIDRow
}

type sessionIDRow struct {
	id      int64
	session string
}

// maxFinalID returns the highest id of the rows FINAL returns from table,
// as max(id) under FINAL does, without reading every id. The rows above
// the highest part threshold are among the kept ones, so a FINAL read of
// their sessions finds the answer when one of them survives FINAL. When
// every such row was replaced or deleted, it reads the whole table.
func (s *Store) maxFinalID(ctx context.Context, table string, tp tableParts, ids *finalIDs) (int64, error) {
	ids.mu.Lock()
	defer ids.mu.Unlock()
	if ids.parts == nil {
		ids.parts = map[string][]sessionIDRow{}
	}
	current := map[string]bool{}
	var missing []string
	for _, p := range tp.parts {
		if p.isPatch() {
			continue
		}
		current[p.name] = true
		if _, ok := ids.parts[p.name]; !ok {
			missing = append(missing, p.name)
		}
	}
	maps.DeleteFunc(ids.parts, func(name string, _ []sessionIDRow) bool { return !current[name] })
	if len(missing) > 0 {
		rows, err := s.queryContext(ctx, `SELECT _part, id, session_id FROM `+table+` WHERE `+
			partsClause(missing)+fmt.Sprintf(` ORDER BY id DESC LIMIT %d BY _part SETTINGS final=0`, topIDsPerPart))
		if err != nil {
			return 0, fmt.Errorf("reading %s top ids: %w", table, err)
		}
		defer rows.Close()
		// A part's top ids are kept only once the whole read succeeds: a
		// partial list would pass for a part with fewer ids.
		read := map[string][]sessionIDRow{}
		for rows.Next() {
			var part string
			var r sessionIDRow
			if err := rows.Scan(&part, &r.id, &r.session); err != nil {
				return 0, fmt.Errorf("scanning %s top id: %w", table, err)
			}
			read[part] = append(read[part], r)
		}
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("iterating %s top ids: %w", table, err)
		}
		merged, err := s.keepEmptyTopIDParts(ctx, table, missing, read)
		if err != nil {
			return 0, err
		}
		maps.Copy(ids.parts, read)
		if merged {
			// A part merged away before its ids were read, so the kept ids
			// may miss the highest one.
			return s.readMaxFinalID(ctx, table)
		}
	}
	threshold := int64(math.MinInt64)
	for _, top := range ids.parts {
		if len(top) == topIDsPerPart {
			threshold = max(threshold, top[len(top)-1].id)
		}
	}
	sessions := map[string]bool{}
	for _, top := range ids.parts {
		for _, r := range top {
			if r.id >= threshold {
				sessions[r.session] = true
			}
		}
	}
	if len(sessions) == 0 {
		return 0, nil
	}
	var found int64
	err := s.queryRowContext(chdriver.Context(ctx, chdriver.WithParameters(chdriver.Parameters{
		"top_sessions": chStringArrayLiteral(slices.Sorted(maps.Keys(sessions))),
	})), `SELECT max(id) FROM `+table+` WHERE has({top_sessions:Array(String)}, session_id)`).Scan(&found)
	if err != nil {
		return 0, fmt.Errorf("reading %s highest id: %w", table, err)
	}
	if found >= threshold {
		return found, nil
	}
	return s.readMaxFinalID(ctx, table)
}

// keepEmptyTopIDParts records an empty id list in read for each requested
// part that returned no rows and is still active: every row it holds is
// deleted. It reports true when such a part is no longer active, which
// means a merge folded its rows into a part the snapshot did not name.
func (s *Store) keepEmptyTopIDParts(
	ctx context.Context, table string, requested []string, read map[string][]sessionIDRow,
) (bool, error) {
	if !slices.ContainsFunc(requested, func(name string) bool { _, ok := read[name]; return !ok }) {
		return false, nil
	}
	fresh, err := s.readPartsSnapshot(ctx)
	if err != nil {
		return false, err
	}
	active := map[string]bool{}
	for _, p := range fresh[table].parts {
		active[p.name] = true
	}
	merged := false
	for _, name := range requested {
		if _, ok := read[name]; ok {
			continue
		}
		if active[name] {
			read[name] = []sessionIDRow{}
		} else {
			merged = true
		}
	}
	return merged, nil
}

// readMaxFinalID reads max(id) under FINAL over the whole table.
func (s *Store) readMaxFinalID(ctx context.Context, table string) (int64, error) {
	var found int64
	if err := s.queryRowContext(ctx, `SELECT ifNull(max(id), 0) FROM `+table).Scan(&found); err != nil {
		return 0, fmt.Errorf("reading %s highest id: %w", table, err)
	}
	return found, nil
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
	// revisions maps every session with a snapshot to its current snapshot
	// revision. A ready state's rows for a session are derived from that
	// snapshot alone, whether stored or in the delta. A session of a host
	// that is not ready maps to "raw:" and its push version instead.
	// Shared; not modified.
	revisions map[string]string
	// raw lists the sessions of the hosts that are not ready: some of
	// their sessions lack a current snapshot. Reads take these sessions
	// from the raw message and event tables and every other session from
	// the prepared rows, so one host that has not published snapshots
	// does not turn prepared reads off for the others.
	raw []string
}

// replaced lists the sessions whose stored prepared rows a read skips: the
// stale ones, the raw hosts' ones, and every one the delta supplies. A refresh that lands after
// the state was taken may already hold a changed session's rows, and the
// delta supplies them too.
func (st preparedUsageState) replaced() []string {
	ids := slices.Concat(st.changed, st.stale, st.raw)
	slices.Sort(ids)
	return slices.Compact(ids)
}

func (s *Store) preparedUsageState(ctx context.Context) (preparedUsageState, error) {
	return s.readPreparedUsageState(ctx, true)
}

// preparedUsageReady reports whether prepared usage answers reads. It
// prepares no delta rows: the read that follows prepares them.
func (s *Store) preparedUsageReady(ctx context.Context) (bool, error) {
	state, err := s.readPreparedUsageState(ctx, false)
	return state.ready, err
}

// readPreparedUsageState reads the state. Without withDelta it leaves the
// delta rows and the pricing digest out, so the state only says whether
// prepared usage is ready.
func (s *Store) readPreparedUsageState(ctx context.Context, withDelta bool) (preparedUsageState, error) {
	var filled uint64
	var comment string
	err := s.queryRowContext(ctx, `SELECT
		(SELECT max(toUInt64OrZero(value)) FROM sync_metadata WHERE startsWith(key,?)),
		ifNull((SELECT comment FROM system.tables WHERE database = currentDatabase() AND name = 'prepared_usage'), '')`,
		usageSnapshotReadyKeyBase+":").Scan(&filled, &comment)
	if err != nil {
		return preparedUsageState{}, fmt.Errorf("checking prepared usage readiness: %w", err)
	}
	// Existing reports remain available while every archive prepares its first
	// complete snapshot. A table prepared by another query may lack the
	// stamp columns, so it is checked before they are read.
	if filled == 0 || comment != chPreparedUsageComment() {
		s.logUsageReadiness(fmt.Sprintf("not ready: snapshots published=%v, prepared table from this binary=%v",
			filled != 0, comment == chPreparedUsageComment()))
		return preparedUsageState{}, nil
	}
	var stored usageStamp
	err = s.queryRowContext(ctx, `SELECT snapshot_count, snapshot_hash, pricing_digest, price_count
		FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(
		&stored.snapshotCount, &stored.snapshotHash, &stored.pricingDigest, &stored.priceCount)
	if errors.Is(err, sql.ErrNoRows) {
		s.logUsageReadiness("not ready: prepared table is empty")
		return preparedUsageState{}, nil
	}
	if err != nil {
		return preparedUsageState{}, fmt.Errorf("reading prepared usage stamp: %w", err)
	}
	stamp := fmt.Sprintf("%s|%d|%d|%s|%d", comment, stored.snapshotCount, stored.snapshotHash,
		stored.pricingDigest, stored.priceCount)
	live, key, err := s.usageCoverage(ctx, stamp, stored.pricingDigest)
	if err != nil {
		return preparedUsageState{}, err
	}
	if withDelta && len(live.changed) > 0 {
		if live, err = s.usageCoverageDelta(ctx, key, live); err != nil {
			return preparedUsageState{}, err
		}
	}
	switch {
	case live.missing == 0:
		s.logUsageReadiness("ready (" + describeHosts(live.hosts) + ")")
	case len(live.raw) > 0:
		s.logUsageReadiness(fmt.Sprintf("ready except %d sessions of hosts without complete snapshots, read raw (%s)",
			len(live.raw), describeHosts(live.hosts)))
	default:
		s.logUsageReadiness(fmt.Sprintf("not ready: %d sessions lack a current usage snapshot (%s)",
			live.missing, describeHosts(live.hosts)))
	}
	state := preparedUsageState{
		// A host is ready once every one of its sessions has a current
		// snapshot. The others' sessions are read raw.
		ready:     live.missing == 0 || len(live.raw) > 0,
		stamp:     stamp,
		changed:   live.changed,
		stale:     live.stale,
		deltaRows: live.deltaRows,
		revisions: live.revisions,
		raw:       live.raw,
	}
	// The stored records and the delta's must be under the same digest for
	// a read to take either in place of the join.
	if withDelta && state.ready && stored.pricingDigest != "" && live.priceCount >= stored.priceCount &&
		(len(live.changed) == 0 || live.deltaDigest == stored.pricingDigest) {
		state.pricingDigest = stored.pricingDigest
	}
	return state, nil
}

// chUsageCoverageTables are the tables whose rows usage coverage reads.
var chUsageCoverageTables = []string{"sessions", usageSessionSnapshotTable, chUsagePriceTable}

// finalRowsAttempts is how often a coverage check rereads the parts that
// moved while it read before it reads the rows once without keeping them.
const finalRowsAttempts = 3

// usageCoverage relates the live sessions and snapshots to the prepared
// rows, from the rows the trackers keep, and counts the price records still
// under pricingDigest. It returns the key of the coverage cache entry, empty
// when the parts kept moving and nothing was kept.
func (s *Store) usageCoverage(ctx context.Context, stamp, pricingDigest string) (usageCoverage, string, error) {
	covered, err := s.preparedCoveredSnapshots(ctx, stamp)
	if err != nil {
		return usageCoverage{}, "", err
	}
	for range finalRowsAttempts {
		snapshot, err := s.readPartsSnapshot(ctx)
		if err != nil {
			return usageCoverage{}, "", err
		}
		live, key, ok := s.trackedUsageCoverage(ctx, snapshot, covered, stamp, pricingDigest)
		if ok {
			return live, key, nil
		}
		if err := ctx.Err(); err != nil {
			return usageCoverage{}, "", err
		}
	}
	live, err := s.readUsageCoverageOnce(ctx, covered, pricingDigest)
	return live, "", err
}

// trackedUsageCoverage brings the trackers to snapshot and relates their
// rows to the covered snapshots, reusing the cached coverage while the rows
// and the stamp are unchanged. It reports false when the trackers could
// not read the snapshot.
func (s *Store) trackedUsageCoverage(
	ctx context.Context, snapshot partsSnapshot, covered map[usageSnapshotKey]bool, stamp, pricingDigest string,
) (usageCoverage, string, bool) {
	s.usageVersions.Lock()
	defer s.usageVersions.Unlock()
	identities, ok := s.usageIdentitiesLocked(ctx, snapshot, chUsageCoverageTables, pricingDigest)
	if !ok {
		return usageCoverage{}, "", false
	}
	key := tableValuesFingerprint(chUsageCoverageTables, func(table string) string { return identities[table] }) +
		"|" + stamp
	s.coverageCache.mu.Lock()
	defer s.coverageCache.mu.Unlock()
	if s.coverageCache.key != key {
		live := coverageFromRows(s.usageVersions.sessions.rows, s.usageVersions.snapshots.rows, covered)
		live.priceCount = s.priceRowsTracker(pricingDigest).count
		s.coverageCache.key, s.coverageCache.live = key, live
	}
	return s.coverageCache.live, key, true
}

// readUsageCoverageOnce reads the coverage from the live tables without
// keeping their rows, for a check whose parts kept moving.
func (s *Store) readUsageCoverageOnce(
	ctx context.Context, covered map[usageSnapshotKey]bool, pricingDigest string,
) (usageCoverage, error) {
	var read [3]*finalRowsChange
	for i, spec := range []finalTable{sessionsFinalTable, snapshotsFinalTable, pricesFinalTable} {
		change, err := s.readFinalRows(ctx, &finalRows{spec: spec, digest: pricingDigest}, nil, false)
		if err != nil {
			return usageCoverage{}, err
		}
		read[i] = change
	}
	live := coverageFromRows(read[0].rows, read[1].rows, covered)
	live.priceCount = read[2].count
	return live, nil
}

// usageCoverageDelta fills the delta rows of the coverage's changed
// sessions and keeps them with the cache entry key names. One caller fills
// an entry; the requests of a page load that arrive together wait for it.
func (s *Store) usageCoverageDelta(ctx context.Context, key string, live usageCoverage) (usageCoverage, error) {
	s.coverageCache.fill.Lock()
	defer s.coverageCache.fill.Unlock()
	s.coverageCache.mu.Lock()
	if key != "" && s.coverageCache.key == key && s.coverageCache.live.deltaRead {
		live = s.coverageCache.live
		s.coverageCache.mu.Unlock()
		return live, nil
	}
	s.coverageCache.mu.Unlock()
	rows, digest, err := s.prepareUsageDelta(ctx, live.changedKeys)
	if err != nil {
		return usageCoverage{}, err
	}
	live.deltaRows, live.deltaDigest, live.deltaRead = rows, digest, true
	if key != "" {
		s.coverageCache.mu.Lock()
		if s.coverageCache.key == key {
			s.coverageCache.live = live
		}
		s.coverageCache.mu.Unlock()
	}
	return live, nil
}

// chStringArrayLiteral renders values as a ClickHouse Array(String)
// literal, the text form of a query parameter.
func chStringArrayLiteral(values []string) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(chStringLiteral(v))
	}
	b.WriteByte(']')
	return b.String()
}

// chStringLiteral quotes v as a ClickHouse string literal.
func chStringLiteral(v string) string {
	return "'" + chStringEscaper.Replace(v) + "'"
}

var chStringEscaper = strings.NewReplacer(`\`, `\\`, `'`, `\'`)

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

// coverageFromRows relates the sessions and their snapshots, as the
// trackers keep them, to the snapshots the prepared rows cover: the number
// of sessions without a current snapshot, the snapshots the prepared rows do
// not cover, and the covered snapshots that no longer exist. A session's
// fields are its push version and machine; a snapshot's are its push
// version, revision, and chSnapshotPreparesUsage. A snapshot from which the
// refresh keeps no row, such as one of a trashed session or one whose
// messages report no tokens, is covered whether or not the refresh saw it.
// The stored chSnapshotPreparesUsage flag says which snapshots those are
// without reading their usage arrays.
func coverageFromRows(sessions, snapshots map[string]finalFields, covered map[usageSnapshotKey]bool) usageCoverage {
	var live usageCoverage
	live.revisions = make(map[string]string, len(snapshots))
	current := make(map[usageSnapshotKey]bool, len(snapshots))
	for id, snapshot := range snapshots {
		live.revisions[id] = snapshot.text
		key := usageSnapshotKey{id: id, revision: snapshot.text}
		current[key] = true
		if snapshot.flag && !covered[key] {
			live.changedKeys = append(live.changedKeys, key)
		}
	}
	live.hosts = map[string]hostCoverage{}
	for id, session := range sessions {
		host := live.hosts[session.text]
		host.sessions++
		if snapshot, ok := snapshots[id]; !ok {
			host.noSnapshot++
			live.missing++
		} else if snapshot.pushVersion < session.pushVersion {
			host.olderSnapshot++
			live.missing++
		}
		live.hosts[session.text] = host
	}
	anyReady := false
	for _, host := range live.hosts {
		if !host.stale() {
			anyReady = true
			break
		}
	}
	if live.missing > 0 && anyReady {
		raw := map[string]bool{}
		for id, session := range sessions {
			if live.hosts[session.text].stale() {
				raw[id] = true
				live.raw = append(live.raw, id)
				live.revisions[id] = fmt.Sprintf("raw:%d", session.pushVersion)
			}
		}
		slices.Sort(live.raw)
		// The raw hosts' sessions take no prepared rows, so none are
		// prepared for them at read time either.
		live.changedKeys = slices.DeleteFunc(live.changedKeys, func(key usageSnapshotKey) bool { return raw[key.id] })
	}
	slices.SortFunc(live.changedKeys, func(a, b usageSnapshotKey) int { return strings.Compare(a.id, b.id) })
	for _, key := range live.changedKeys {
		live.changed = append(live.changed, key.id)
	}
	for key := range covered {
		if !current[key] {
			live.stale = append(live.stale, key.id)
		}
	}
	slices.Sort(live.stale)
	return live
}

// prepareUsageDelta prepares the rows of the changed snapshots as a refresh
// would, priced under the current catalog. A snapshot's rows depend only
// on the snapshot and the catalog, so they are kept per snapshot and
// whole-catalog digest and read again only for snapshots new since the last
// call. Sessions whose usage no refresh prepares a row for stay changed
// at every call; without the kept rows each call read all of them again.
func (s *Store) prepareUsageDelta(ctx context.Context, changed []usageSnapshotKey) ([][]any, string, error) {
	snapshot, err := s.pricingSnapshot(ctx)
	if err != nil {
		return nil, "", err
	}
	s.deltaCache.mu.Lock()
	kept := s.deltaCache.rows
	if s.deltaCache.digest != snapshot.catalogDigest {
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
	s.deltaCache.digest, s.deltaCache.rows = snapshot.catalogDigest, next
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
		record := records[row[23].(string)]
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
		priceKey:     row[23].(string),
		model:        row[5].(string),
		priceModel:   row[22].(string),
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

// usageCoverageCache memoizes the coverage of the prepared rows per rows
// of the sessions, snapshot, and price record tables and per stamp.
type usageCoverageCache struct {
	mu sync.Mutex
	// key names the tracked rows and the stamp the coverage was derived
	// from.
	key  string
	live usageCoverage
	// covered is the set of snapshots the prepared rows under coveredStamp
	// were derived from.
	coveredStamp string
	covered      map[usageSnapshotKey]bool
	// fill serializes the readers that fill the delta rows.
	fill sync.Mutex
}

// usageStamp identifies a snapshot set and the price records under one
// digest, as stored on prepared rows.
type usageStamp struct {
	snapshotCount, snapshotHash uint64
	pricingDigest               string
	priceCount                  uint64
}

// usageDeltaCache keeps, under one whole-catalog digest, the prepared rows
// of the snapshots the last delta covered; see prepareUsageDelta. The
// pricing digest would not do: it ignores models no registered model
// resolves to, and a delta row can name a model not registered yet.
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
	// deltaRead says whether deltaRows were prepared.
	deltaRead bool
	// revisions maps each session to its current snapshot revision.
	revisions map[string]string
	// hosts counts each machine's sessions and those without a current
	// snapshot.
	hosts map[string]hostCoverage
	// raw lists the sessions of the hosts that are not ready, when at least
	// one host is.
	raw []string
}

// hostCoverage counts one machine's sessions by snapshot state.
type hostCoverage struct {
	sessions, noSnapshot, olderSnapshot int
}

// stale reports whether some of the machine's sessions lack a current
// snapshot.
func (h hostCoverage) stale() bool { return h.noSnapshot+h.olderSnapshot > 0 }

// describeHosts renders each machine's snapshot coverage for the log.
func describeHosts(hosts map[string]hostCoverage) string {
	var parts []string
	for _, machine := range slices.Sorted(maps.Keys(hosts)) {
		h := hosts[machine]
		parts = append(parts, fmt.Sprintf("%s: %d sessions, %d without a snapshot, %d with an older snapshot",
			machine, h.sessions, h.noSnapshot, h.olderSnapshot))
	}
	return strings.Join(parts, "; ")
}

// logUsageReadiness logs whether prepared usage answers reads, and which
// hosts' sessions are read raw instead, once per change.
func (s *Store) logUsageReadiness(reason string) {
	s.readinessLog.Lock()
	defer s.readinessLog.Unlock()
	if s.readinessLog.last == reason {
		return
	}
	s.readinessLog.last = reason
	log.Printf("clickhouse: prepared usage %s", reason)
}
