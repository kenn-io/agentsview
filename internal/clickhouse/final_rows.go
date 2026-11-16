package clickhouse

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"strings"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"
)

// A finalRows tracker keeps the count and hash sum of the rows a FINAL read
// of one ReplacingMergeTree table returns, and optionally a few fields of
// each key's row. A check after a push reads only the rows of the keys the
// new parts hold, so its cost follows the push instead of the table.
//
// The tracker relates the table's active parts to the parts it last saw.
// Block numbers come from one counter and grow with every insert and
// mutation, and a merge keeps the block range of the parts it combines. A
// new part whose range the vanished parts cover exactly, each already at
// the new part's mutation, is a merge of rows already counted: a merge
// never changes what FINAL returns, so it costs nothing. Any other new
// part holds rows the tracker has not seen. Each key such a part holds is
// read again: its rows in the parts the tracker had already seen give the
// row FINAL returned
// before, and a FINAL read of the key gives the row it returns now. The
// tracker keeps the rows of small parts, so the row before is still
// available after a merge folds a small part into a new one. Anything else,
// such as a mutation, a lightweight delete's patch part, a dropped part, or
// a merge of a large part with rows not seen yet, rereads the whole table.

// finalRowsQueryMark starts the queries a tracker runs, so tests can find
// them in the query log. The log drops it from queries with parameters.
const finalRowsQueryMark = "/* final rows */ "

// smallFinalPartRows is the most rows a part may hold for a tracker to keep
// them. Inserts and the merges of recent inserts stay below it.
const smallFinalPartRows = 8192

// maxFinalKeys is the most keys an incremental check reads, which travel in
// one query parameter; more reread the table.
const maxFinalKeys = 2_000

// partInfo is one active part of a table, as system.parts lists it.
type partInfo struct {
	name, partition    string
	minBlock, maxBlock int64
	// mutation is the number of the last mutation applied to the part, zero
	// for none.
	mutation int64
	rows     uint64
	// files is the part's hash_of_all_files.
	files string
}

// dataVersion is the block number the part's rows are current as of: its
// last mutation, or its first block when none applied. A mutation numbered
// above it has not reached the part's rows.
func (p partInfo) dataVersion() int64 {
	if p.mutation != 0 {
		return p.mutation
	}
	return p.minBlock
}

// isPatch reports whether the part is a lightweight update's patch part,
// which changes rows of other parts without renaming them.
func (p partInfo) isPatch() bool { return strings.HasPrefix(p.name, "patch-") }

// rowHash is an unsigned 128-bit value: a row hash, a sum of row hashes that
// wraps like sumWithOverflow, or a version.
type rowHash struct{ lo, hi uint64 }

func (h rowHash) add(o rowHash) rowHash {
	lo, carry := bits.Add64(h.lo, o.lo, 0)
	hi, _ := bits.Add64(h.hi, o.hi, carry)
	return rowHash{lo, hi}
}

func (h rowHash) sub(o rowHash) rowHash {
	lo, borrow := bits.Sub64(h.lo, o.lo, 0)
	hi, _ := bits.Sub64(h.hi, o.hi, borrow)
	return rowHash{lo, hi}
}

func (h rowHash) compare(o rowHash) int {
	return cmp.Or(cmp.Compare(h.hi, o.hi), cmp.Compare(h.lo, o.lo))
}

// parseRowHash reads the 16 little-endian bytes reinterpretAsFixedString
// and sipHash128Reference return.
func parseRowHash(b string) (rowHash, error) {
	if len(b) != 16 {
		return rowHash{}, fmt.Errorf("128-bit value has %d bytes", len(b))
	}
	return rowHash{binary.LittleEndian.Uint64([]byte(b[:8])), binary.LittleEndian.Uint64([]byte(b[8:]))}, nil
}

// finalTable describes a table a tracker follows.
type finalTable struct {
	table string
	// key is the leading sorting key column the tracker names rows by.
	key string
	// version is the ReplacingMergeTree version column.
	version string
	// hash lists the columns the row hash covers.
	hash string
	// fields selects the push version, text, and flag kept per key, or is
	// empty to keep none.
	fields string
	// byDigest limits the rows to one pricing digest's partition.
	byDigest bool
}

// finalFields are the fields a tracker keeps of each key's row.
type finalFields struct {
	pushVersion uint64
	text        string
	flag        bool
	hash        rowHash
}

var (
	sessionsFinalTable = finalTable{
		table: "sessions", key: "id", version: "push_version", hash: "*",
		fields: "push_version, machine, false",
	}
	// The snapshot's revision names its usage arrays and push_version its
	// publication, so the hash skips the arrays.
	snapshotsFinalTable = finalTable{
		table: usageSessionSnapshotTable, key: "id", version: "revision", hash: "id, revision, push_version",
		fields: "push_version, toString(revision), " + chSnapshotPreparesUsage,
	}
	pricesFinalTable = finalTable{
		table: chUsagePriceTable, key: "price_key", version: "push_version", hash: "*", byDigest: true,
	}
)

// finalRow is one stored row of a tracked key.
type finalRow struct {
	key     string
	version rowHash
	// block and offset order rows of equal version: FINAL keeps the last
	// inserted, which is in the part with the higher blocks, and within a
	// part the later row.
	block  int64
	offset uint64
	hash   rowHash
}

// finalRows is the state of one tracker. The usageVersions mutex guards it.
type finalRows struct {
	spec   finalTable
	digest string
	// partition is the partition id of the digest's rows, found once rows
	// exist; noPartitionAt is the table's parts digest at which none did.
	partition, noPartitionAt string
	// parts are the parts the state was derived from; nil before the first
	// read.
	parts map[string]partInfo
	count uint64
	sum   rowHash
	// cached holds the rows of the small parts in parts, by part name, for
	// a tracker that keeps no rows.
	cached map[string][]finalRow
	// rows holds each key's row when spec.fields is set.
	rows map[string]finalFields
}

// keepsRows reports whether the tracker keeps each key's row, which then
// gives the row FINAL returned before a change.
func (f *finalRows) keepsRows() bool { return f.spec.fields != "" }

// identity names the rows the state counts.
func (f *finalRows) identity() string {
	return fmt.Sprintf("(%d,%016x%016x)", f.count, f.sum.hi, f.sum.lo)
}

// finalRowsChange is a new state for a tracker, applied once the parts it
// was read from proved stable.
type finalRowsChange struct {
	// full replaces cached and rows; otherwise cached adds parts and
	// set and del update rows.
	full   bool
	parts  map[string]partInfo
	count  uint64
	sum    rowHash
	cached map[string][]finalRow
	rows   map[string]finalFields
	set    map[string]finalFields
	del    []string
	// rowParts names the parts that gave a full read of kept rows at
	// least one returned row; see settle.
	rowParts map[string]bool
}

// settle checks the change against fresh, the table's parts after the
// reads, and reports whether the change still describes the rows of its
// parts. The reads named their parts, so a part that went inactive
// before a read started gave it no rows. A part still active in fresh
// was active through every read. A full read of kept rows lists the parts
// its returned rows came from. A vanished part that returned a row was
// read, so it stays. A vanished part that returned none changed no
// returned row whether or not the read saw it, so the change drops it
// from its parts and still holds; a later check reads the keys of the
// part its rows moved to. Any other change with a vanished part may miss
// rows, and settle reports false.
func (c *finalRowsChange) settle(fresh tableParts) bool {
	active := make(map[string]partInfo, len(fresh.parts))
	for _, p := range fresh.parts {
		active[p.name] = p
	}
	var dropped []string
	for name, p := range c.parts {
		if active[name] == p {
			continue
		}
		switch {
		case !c.full || c.rowParts == nil:
			return false
		case !c.rowParts[name]:
			dropped = append(dropped, name)
		}
	}
	if len(dropped) > 0 {
		c.parts = maps.Clone(c.parts)
		for _, name := range dropped {
			delete(c.parts, name)
		}
	}
	return true
}

func (f *finalRows) apply(c *finalRowsChange) {
	f.parts, f.count, f.sum = c.parts, c.count, c.sum
	if c.full {
		f.cached, f.rows = c.cached, c.rows
		return
	}
	maps.DeleteFunc(f.cached, func(name string, _ []finalRow) bool { _, ok := c.parts[name]; return !ok })
	maps.Copy(f.cached, c.cached)
	if f.rows != nil {
		for _, key := range c.del {
			delete(f.rows, key)
		}
		maps.Copy(f.rows, c.set)
	}
}

// filter returns the condition limiting a read to the tracked rows.
func (f *finalRows) filter() string {
	if f.spec.byDigest {
		return "pricing_digest = {final_digest:String}"
	}
	return "1"
}

// queryContext adds the parameters a tracker query names.
func (f *finalRows) queryContext(ctx context.Context, keys []string) context.Context {
	params := chdriver.Parameters{"final_digest": f.digest}
	if keys != nil {
		params["final_keys"] = chStringArrayLiteral(keys)
	}
	return chdriver.Context(ctx, chdriver.WithParameters(params))
}

func partsClause(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = chStringLiteral(name)
	}
	return "_part IN (" + strings.Join(quoted, ",") + ")"
}

// prepareFinalRows reads what changed since the tracker's state, against
// the table's parts in the snapshot. It returns nil when nothing did.
func (s *Store) prepareFinalRows(ctx context.Context, f *finalRows, tp tableParts) (*finalRowsChange, error) {
	cur, err := s.finalRowsParts(ctx, f, tp)
	if err != nil {
		return nil, err
	}
	if f.parts != nil && maps.EqualFunc(f.parts, cur, func(a, b partInfo) bool { return a == b }) {
		return nil, nil
	}
	if f.parts != nil {
		if plan, ok := f.plan(cur); ok {
			return s.readFinalRowsPlan(ctx, f, cur, plan)
		}
	}
	return s.readFinalRows(ctx, f, cur, true)
}

// finalRowsParts returns the parts that can hold the tracked rows.
func (s *Store) finalRowsParts(ctx context.Context, f *finalRows, tp tableParts) (map[string]partInfo, error) {
	if f.spec.byDigest && f.partition == "" && f.noPartitionAt != tp.digest {
		var partition string
		err := s.queryRowContext(f.queryContext(ctx, nil), finalRowsQueryMark+`SELECT _partition_id FROM `+
			f.spec.table+` WHERE `+f.filter()+` LIMIT 1 SETTINGS final=0`).Scan(&partition)
		switch {
		case err == nil:
			f.partition = partition
		case errors.Is(err, sql.ErrNoRows):
			f.noPartitionAt = tp.digest
		default:
			return nil, fmt.Errorf("finding %s partition: %w", f.spec.table, err)
		}
	}
	cur := map[string]partInfo{}
	if f.spec.byDigest && f.partition == "" {
		return cur, nil
	}
	for _, p := range tp.parts {
		if !f.spec.byDigest || p.partition == f.partition {
			cur[p.name] = p
		}
	}
	return cur, nil
}

// finalRowsPlan is an incremental check: the new parts holding rows the
// state has not seen, the kept parts they replaced, and the small new parts
// whose rows to keep.
type finalRowsPlan struct {
	changed  []partInfo
	replaced []string
	keep     []partInfo
}

// plan relates cur to the state's parts. It reports false when only a
// full read can tell what changed.
func (f *finalRows) plan(cur map[string]partInfo) (finalRowsPlan, bool) {
	var plan finalRowsPlan
	var appeared, vanished []partInfo
	for name, p := range cur {
		if _, ok := f.parts[name]; !ok {
			if p.isPatch() {
				return plan, false
			}
			appeared = append(appeared, p)
		}
	}
	for name, p := range f.parts {
		if _, ok := cur[name]; !ok && !p.isPatch() {
			vanished = append(vanished, p)
		}
	}
	consumed := map[string]bool{}
	for _, p := range appeared {
		var sources []partInfo
		for _, q := range vanished {
			if q.partition == p.partition && q.minBlock >= p.minBlock && q.maxBlock <= p.maxBlock {
				sources = append(sources, q)
			}
		}
		for name, q := range f.parts {
			if _, kept := cur[name]; kept && !q.isPatch() && q.partition == p.partition &&
				q.minBlock <= p.maxBlock && p.minBlock <= q.maxBlock {
				return plan, false
			}
		}
		mutation := int64(0)
		for _, q := range sources {
			mutation = max(mutation, q.mutation)
			consumed[q.name] = true
		}
		// A source that had not yet taken the merged part's mutation was
		// mutated after the state read it, which can change its rows.
		if p.mutation != mutation || slices.ContainsFunc(sources, func(q partInfo) bool {
			return q.dataVersion() < p.mutation
		}) {
			return plan, false
		}
		if p.rows <= smallFinalPartRows && !f.keepsRows() {
			plan.keep = append(plan.keep, p)
		}
		if coversBlocks(sources, p) {
			continue
		}
		if p.rows > smallFinalPartRows {
			return plan, false
		}
		// A merge drops no key FINAL returns, so the part holds every key
		// its sources did; only the rows they returned before are needed.
		for _, q := range sources {
			if f.keepsRows() {
				break
			}
			if _, ok := f.cached[q.name]; !ok {
				return plan, false
			}
			plan.replaced = append(plan.replaced, q.name)
		}
		plan.changed = append(plan.changed, p)
	}
	for _, q := range vanished {
		if !consumed[q.name] {
			return plan, false
		}
	}
	return plan, true
}

// coversBlocks reports whether the parts cover p's block range exactly,
// with no block the state did not see.
func coversBlocks(parts []partInfo, p partInfo) bool {
	if len(parts) == 0 {
		return false
	}
	parts = slices.SortedFunc(slices.Values(parts), func(a, b partInfo) int { return cmp.Compare(a.minBlock, b.minBlock) })
	next := p.minBlock
	for _, q := range parts {
		if q.minBlock != next {
			return false
		}
		next = q.maxBlock + 1
	}
	return next == p.maxBlock+1
}

// readFinalRowsPlan reads the rows an incremental plan names.
func (s *Store) readFinalRowsPlan(
	ctx context.Context, f *finalRows, cur map[string]partInfo, plan finalRowsPlan,
) (*finalRowsChange, error) {
	change := &finalRowsChange{parts: cur, count: f.count, sum: f.sum}
	if f.keepsRows() {
		return s.readKeptFinalRowsPlan(ctx, f, cur, plan, change)
	}
	kept, err := s.readFinalPartRows(ctx, f, plan.keep)
	if err != nil {
		return nil, err
	}
	change.cached = kept
	keySet := map[string]bool{}
	for _, p := range plan.changed {
		for _, r := range kept[p.name] {
			keySet[r.key] = true
		}
	}
	for _, name := range plan.replaced {
		for _, r := range f.cached[name] {
			keySet[r.key] = true
		}
	}
	if len(keySet) > maxFinalKeys {
		return s.readFinalRows(ctx, f, cur, true)
	}
	if len(keySet) == 0 {
		return change, nil
	}
	keys := slices.Sorted(maps.Keys(keySet))
	changed := map[string]bool{}
	for _, p := range plan.changed {
		changed[p.name] = true
	}
	var seen []string
	for name := range cur {
		if !changed[name] {
			seen = append(seen, name)
		}
	}
	before, err := s.lookupFinalRows(ctx, f, keys, seen, cur)
	if err != nil {
		return nil, err
	}
	for _, name := range plan.replaced {
		for _, r := range f.cached[name] {
			if keySet[r.key] {
				before = append(before, r)
			}
		}
	}
	after, err := s.readFinalKeys(ctx, f, keys, slices.Collect(maps.Keys(cur)))
	if err != nil {
		return nil, err
	}
	winners := finalWinners(before)
	change.count = change.count - uint64(len(winners)) + uint64(len(after))
	for _, r := range winners {
		change.sum = change.sum.sub(r.hash)
	}
	for _, r := range after {
		change.sum = change.sum.add(r.hash)
	}
	return change, nil
}

// readKeptFinalRowsPlan reads an incremental plan for a tracker that keeps
// each key's row, which is the row FINAL returned before.
func (s *Store) readKeptFinalRowsPlan(
	ctx context.Context, f *finalRows, cur map[string]partInfo, plan finalRowsPlan, change *finalRowsChange,
) (*finalRowsChange, error) {
	if len(plan.changed) == 0 {
		return change, nil
	}
	names := make([]string, len(plan.changed))
	for i, p := range plan.changed {
		names[i] = p.name
	}
	rows, err := s.queryContext(f.queryContext(ctx, nil), finalRowsQueryMark+`SELECT DISTINCT `+f.spec.key+
		` FROM `+f.spec.table+` WHERE `+f.filter()+` AND `+partsClause(names)+` SETTINGS final=0`)
	if err != nil {
		return nil, fmt.Errorf("reading %s keys: %w", f.spec.table, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scanning %s key: %w", f.spec.table, err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating %s keys: %w", f.spec.table, err)
	}
	if len(keys) > maxFinalKeys {
		return s.readFinalRows(ctx, f, cur, true)
	}
	if len(keys) == 0 {
		return change, nil
	}
	slices.Sort(keys)
	after, err := s.readFinalKeys(ctx, f, keys, slices.Collect(maps.Keys(cur)))
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		if r, ok := f.rows[key]; ok {
			change.count--
			change.sum = change.sum.sub(r.hash)
		}
	}
	change.set = make(map[string]finalFields, len(after))
	for key, r := range after {
		change.count++
		change.sum = change.sum.add(r.hash)
		change.set[key] = r.fields
	}
	change.del = keys
	return change, nil
}

// finalWinners returns, per key, the row FINAL keeps: the highest version,
// then the last inserted.
func finalWinners(rows []finalRow) map[string]finalRow {
	winners := map[string]finalRow{}
	for _, r := range rows {
		w, ok := winners[r.key]
		if !ok || cmp.Or(r.version.compare(w.version), cmp.Compare(r.block, w.block), cmp.Compare(r.offset, w.offset)) > 0 {
			winners[r.key] = r
		}
	}
	return winners
}

func (f *finalRows) rowColumns() string {
	return f.spec.key + ", reinterpretAsFixedString(toUInt128(" + f.spec.version + ")), _part_offset, " +
		"sipHash128Reference(tuple(" + f.spec.hash + "))"
}

// readFinalPartRows reads the stored rows of the parts, by part name. Every
// part gets an entry, so an empty one is kept too.
func (s *Store) readFinalPartRows(ctx context.Context, f *finalRows, parts []partInfo) (map[string][]finalRow, error) {
	out := make(map[string][]finalRow, len(parts))
	if len(parts) == 0 {
		return out, nil
	}
	known := make(map[string]partInfo, len(parts))
	names := make([]string, len(parts))
	for i, p := range parts {
		known[p.name], names[i] = p, p.name
		out[p.name] = nil
	}
	err := s.scanFinalRows(f.queryContext(ctx, nil), finalRowsQueryMark+`SELECT `+f.rowColumns()+`, _part FROM `+
		f.spec.table+` WHERE `+f.filter()+` AND `+partsClause(names)+` SETTINGS final=0`, known,
		func(part string, r finalRow) { out[part] = append(out[part], r) })
	return out, err
}

// lookupFinalRows reads the stored rows of the keys in the named parts.
func (s *Store) lookupFinalRows(
	ctx context.Context, f *finalRows, keys, names []string, cur map[string]partInfo,
) ([]finalRow, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var out []finalRow
	err := s.scanFinalRows(f.queryContext(ctx, keys), finalRowsQueryMark+`SELECT `+f.rowColumns()+`, _part FROM `+
		f.spec.table+` WHERE `+f.filter()+` AND has({final_keys:Array(String)}, `+f.spec.key+`) AND `+
		partsClause(names)+` SETTINGS final=0`, cur, func(_ string, r finalRow) { out = append(out, r) })
	return out, err
}

func (s *Store) scanFinalRows(
	ctx context.Context, query string, parts map[string]partInfo, add func(part string, r finalRow),
) error {
	rows, err := s.queryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("reading %s rows: %w", query, err)
	}
	defer rows.Close()
	for rows.Next() {
		var r finalRow
		var version, hash, part string
		if err := rows.Scan(&r.key, &version, &r.offset, &hash, &part); err != nil {
			return fmt.Errorf("scanning tracked row: %w", err)
		}
		if r.version, err = parseRowHash(version); err != nil {
			return fmt.Errorf("reading tracked row version: %w", err)
		}
		if r.hash, err = parseRowHash(hash); err != nil {
			return fmt.Errorf("reading tracked row hash: %w", err)
		}
		r.block = parts[part].minBlock
		add(part, r)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating tracked rows: %w", err)
	}
	return nil
}

// finalKeyRow is the row FINAL returns for a key.
type finalKeyRow struct {
	hash   rowHash
	fields finalFields
}

func (f *finalRows) finalColumns() string {
	columns := f.spec.key + ", sipHash128Reference(tuple(" + f.spec.hash + "))"
	if f.spec.fields != "" {
		columns += ", " + f.spec.fields
	}
	return columns
}

// readFinalKeys reads the rows FINAL returns for the keys over the named
// parts.
func (s *Store) readFinalKeys(ctx context.Context, f *finalRows, keys, names []string) (map[string]finalKeyRow, error) {
	return s.scanFinalKeys(f.queryContext(ctx, keys), finalRowsQueryMark+`SELECT `+f.finalColumns()+` FROM `+
		f.spec.table+` WHERE `+f.filter()+` AND has({final_keys:Array(String)}, `+f.spec.key+`) AND `+
		partsClause(names), f.spec.fields != "", nil)
}

// scanFinalKeys scans the rows of query. With rowParts set, the query
// selects _part last and rowParts records each part a row came from.
func (s *Store) scanFinalKeys(
	ctx context.Context, query string, withFields bool, rowParts map[string]bool,
) (map[string]finalKeyRow, error) {
	rows, err := s.queryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", query, err)
	}
	defer rows.Close()
	out := map[string]finalKeyRow{}
	for rows.Next() {
		var key, hash, part string
		var r finalKeyRow
		targets := []any{&key, &hash}
		if withFields {
			targets = append(targets, &r.fields.pushVersion, &r.fields.text, &r.fields.flag)
		}
		if rowParts != nil {
			targets = append(targets, &part)
		}
		if err := rows.Scan(targets...); err != nil {
			return nil, fmt.Errorf("scanning tracked key: %w", err)
		}
		if rowParts != nil {
			rowParts[part] = true
		}
		if r.hash, err = parseRowHash(hash); err != nil {
			return nil, fmt.Errorf("reading tracked key hash: %w", err)
		}
		r.fields.hash = r.hash
		out[key] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating tracked keys: %w", err)
	}
	return out, nil
}

// readFinalRows reads the whole tracked state. With restrict it reads only
// cur's parts, so the state can be kept once they prove stable; without it
// reads whatever is active, for a state used once.
func (s *Store) readFinalRows(ctx context.Context, f *finalRows, cur map[string]partInfo, restrict bool) (*finalRowsChange, error) {
	change := &finalRowsChange{full: true, parts: cur, cached: map[string][]finalRow{}}
	if f.spec.fields != "" {
		change.rows = map[string]finalFields{}
	}
	if restrict && len(cur) == 0 {
		return change, nil
	}
	where := f.filter()
	if restrict {
		where += " AND " + partsClause(slices.Collect(maps.Keys(cur)))
	}
	if f.spec.fields != "" {
		columns := f.finalColumns()
		if restrict {
			// The parts the rows came from let settle keep the read when
			// one of cur's parts vanished as it ran.
			change.rowParts = map[string]bool{}
			columns += ", _part"
		}
		rows, err := s.scanFinalKeys(f.queryContext(ctx, nil), finalRowsQueryMark+`SELECT `+columns+
			` FROM `+f.spec.table+` WHERE `+where, true, change.rowParts)
		if err != nil {
			return nil, err
		}
		change.count = uint64(len(rows))
		for key, r := range rows {
			change.sum = change.sum.add(r.hash)
			change.rows[key] = r.fields
		}
	} else {
		var sum string
		err := s.queryRowContext(f.queryContext(ctx, nil), finalRowsQueryMark+`SELECT count(),
			reinterpretAsFixedString(sumWithOverflow(reinterpretAsUInt128(sipHash128Reference(tuple(`+f.spec.hash+`)))))
			FROM `+f.spec.table+` WHERE `+where).Scan(&change.count, &sum)
		if err != nil {
			return nil, fmt.Errorf("counting %s rows: %w", f.spec.table, err)
		}
		if change.sum, err = parseRowHash(sum); err != nil {
			return nil, fmt.Errorf("reading %s row sum: %w", f.spec.table, err)
		}
	}
	if !restrict || f.keepsRows() {
		return change, nil
	}
	var small []partInfo
	for _, p := range cur {
		if p.rows <= smallFinalPartRows && !p.isPatch() {
			small = append(small, p)
		}
	}
	kept, err := s.readFinalPartRows(ctx, f, small)
	if err != nil {
		return nil, err
	}
	change.cached = kept
	return change, nil
}
