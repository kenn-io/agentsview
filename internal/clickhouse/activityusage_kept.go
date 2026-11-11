package clickhouse

import (
	"bytes"
	"cmp"
	"context"
	"hash/maphash"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
)

// activityUsageRange keeps a range in progress: every session's prepared
// usage rows, the snapshot revisions they were read at, and the usage
// survivors derived from them. A push changes a few sessions, so a report
// re-reads those sessions and re-derives only the survivors their rows can
// change. The lock serializes the reports of one range.
type activityUsageRange struct {
	mu        sync.Mutex
	revisions map[string]string
	// sessions holds each session's rows by the number the range gave the
	// session. A session keeps its number while the range is kept, so the
	// derived state refers to rows by session number and row index.
	sessions []keptUsageSession
	number   map[string]int32
	// rows counts the rows of every session.
	rows      int
	survivors usageSurvivors
}

// keptUsageSession is one session's rows in a kept range.
type keptUsageSession struct {
	id string
	// rows are the session's rows in report order; nil when it has none.
	rows *activityUsageKept
	// minUS and maxUS bound the timestamps of the rows that have one.
	minUS, maxUS int64
	timed        bool
}

// reset replaces every session's rows with rows, a read of the whole range.
// The survivors are derived again from scratch.
func (r *activityUsageRange) reset(revisions map[string]string, rows *activityUsageKept) {
	r.revisions = revisions
	r.sessions = nil
	r.number = make(map[string]int32)
	r.rows = 0
	r.survivors = usageSurvivors{}
	for _, session := range splitKeptUsage(rows) {
		r.setSession(session)
	}
}

// patch replaces the rows of the changed sessions with their rows in fresh;
// a changed session without rows in fresh has none left. The survivors
// remember each replaced session's previous rows until they are re-derived.
func (r *activityUsageRange) patch(revisions map[string]string, changed []string, fresh *activityUsageKept) {
	r.revisions = revisions
	split := make(map[string]keptUsageSession)
	for _, session := range splitKeptUsage(fresh) {
		split[session.id] = session
	}
	u := &r.survivors
	for _, id := range changed {
		session, found := split[id]
		n, known := r.number[id]
		if !known && !found {
			continue
		}
		var previous *activityUsageKept
		if known {
			previous = r.sessions[n].rows
		}
		if !found {
			session = keptUsageSession{id: id}
		}
		n = r.setSession(session)
		if !u.valid {
			continue
		}
		if u.replaced == nil {
			u.replaced = make(map[int32]*activityUsageKept)
		}
		if _, pending := u.replaced[n]; !pending {
			u.replaced[n] = previous
		}
	}
}

// setSession stores a session's rows and returns its number.
func (r *activityUsageRange) setSession(session keptUsageSession) int32 {
	n, ok := r.number[session.id]
	if !ok {
		n = int32(len(r.sessions))
		r.number[session.id] = n
		r.sessions = append(r.sessions, keptUsageSession{})
	}
	if old := r.sessions[n].rows; old != nil {
		r.rows -= old.count
	}
	if session.rows != nil {
		r.rows += session.rows.count
	}
	r.sessions[n] = session
	return n
}

// splitKeptUsage splits a kept read into each session's rows, in report
// order. Each session's rows get buffers of their own and share the read's
// dictionary. A push replaces a few sessions while the rest stay kept, so
// rows sharing one buffer would keep every replaced session's bytes alive
// beyond what the range's byte estimate counts.
func splitKeptUsage(k *activityUsageKept) []keptUsageSession {
	type key struct {
		session     uint64
		data, arena int
		arenaBytes  int
		us          int64
	}
	keys := make([]key, 0, k.count)
	data, arena := k.data, k.arena
	var r keptUsageRow
	var us int64
	for range k.count {
		at := key{data: len(k.data) - len(data), arena: len(k.arena) - len(arena)}
		data, arena = k.decode(data, arena, us, &r)
		if r.validTS {
			us = r.us
		}
		at.session, at.us = r.refs[0], r.us
		at.arenaBytes = len(k.arena) - len(arena) - at.arena
		keys = append(keys, at)
	}
	slices.SortStableFunc(keys, func(a, b key) int { return cmp.Compare(a.session, b.session) })
	// The encoder's data buffer is scratch reused by every session; each
	// session's arena is built at its exact size.
	e := &keptUsageEncoder{k: &activityUsageKept{dict: k.dict}}
	var out []keptUsageSession
	for i := 0; i < len(keys); {
		ref := keys[i].session
		session := keptUsageSession{id: k.dict[ref]}
		j, arenaBytes := i, 0
		for ; j < len(keys) && keys[j].session == ref; j++ {
			arenaBytes += keys[j].arenaBytes
		}
		e.data, e.us = e.data[:0], 0
		e.arena.Reset()
		e.arena.Grow(arenaBytes)
		for _, at := range keys[i:j] {
			k.decode(k.data[at.data:], k.arena[at.arena:], 0, &r)
			r.us = at.us
			e.encode(&r.clickActivityReportUsageRow, r.refs, r.us, r.validTS)
			if !r.validTS {
				continue
			}
			if !session.timed {
				session.minUS, session.timed = r.us, true
			}
			session.maxUS = r.us
		}
		session.rows = &activityUsageKept{
			dict: k.dict, count: j - i, sessions: []uint64{ref},
			data: bytes.Clone(e.data), arena: e.arena.String(),
		}
		out = append(out, session)
		i = j
	}
	return out
}

// usageRowRef names a kept row by session number and row index.
type usageRowRef struct{ session, row int32 }

// usageTokenKind is which dedup token a usage row carries; see
// activity.UsageDedupToken.
type usageTokenKind uint8

const (
	usageTokenNone usageTokenKind = iota
	usageTokenClaude
	usageTokenSource
	usageTokenUsage
)

// usageToken returns the dedup token a row carries, the way
// UsageSurvivorSelectionForSessions picks it: the Claude response identity,
// else the agent and source UUID, else the usage key. The
// token's value joins a and b with a colon, except a usage key, which is a.
func usageToken(r *clickActivityReportUsageRow) (usageTokenKind, string, string) {
	switch {
	case r.claudeMessageID != "":
		return usageTokenClaude, r.claudeMessageID, r.claudeRequestID
	case r.agent != "" && r.sourceUUID != "":
		return usageTokenSource, r.agent, r.sourceUUID
	case r.usageDedupKey != "":
		return usageTokenUsage, r.usageDedupKey, ""
	}
	return usageTokenNone, "", ""
}

// usageSelectionRow is what survivor selection reads from a kept row.
type usageSelectionRow struct {
	us                  int64
	output, webSearches int
	// hash is the hash of the token's kind and value.
	hash    uint64
	kind    usageTokenKind
	a, b    string
	message bool
	validTS bool
}

// sameToken reports whether two rows carry the same dedup token value.
// Two Claude pairs can join to the same value, as "a:b" and "c" and as "a"
// and "b:c" do; the selection then keeps only the first survivor of both.
func sameToken(x, y *usageSelectionRow) bool {
	if x.kind != y.kind {
		return false
	}
	if x.kind == usageTokenUsage || (x.a == y.a && x.b == y.b) {
		return x.a == y.a
	}
	return len(x.a)+len(x.b) == len(y.a)+len(y.b) && x.a+":"+x.b == y.a+":"+y.b
}

// usageTokenRow is a row that carries a token, by the token's hash.
type usageTokenRow struct {
	hash uint64
	ref  usageRowRef
}

// usageTokenSurvivor is the survivor of one token: the row that counts, the
// session it counts for, and the web-search count it is priced with.
type usageTokenSurvivor struct {
	hash        uint64
	row         usageRowRef
	attributed  int32
	webSearches int
}

// usageTotalRow is a sum of one session's survivors, or one survivor of a
// session that carries a session total.
type usageTotalRow struct {
	model, ts                  string
	input, output, webSearches int
	cost                       money.Money
	costSource                 export.CostSource
	sessionCost                *money.Money
	priced, contributes        bool
}

// usageSessionTotals are one candidate session's survivors, summed per
// model, bucket window, and price status. A session whose survivors carry a
// session total keeps each survivor in report order instead, because the
// report apportions the total over them by their costs.
type usageSessionTotals struct {
	rows    []usageTotalRow
	pricing []usagePricingCount
}

// usagePricingKey identifies a pricing record by what the resolver's
// pricing block shows of it.
type usagePricingKey struct {
	kind                                usagePricingKind
	unattributed                        bool
	reportedModel, pricedModel          string
	band                                int
	hasBand                             bool
	pattern, adjustment                 string
	ok                                  bool
	source                              export.PricingRowSource
	input, output                       int64
	cacheWrite, cacheWrite1h, cacheRead int64
	bands                               string
}

// usagePricingTally counts the kept survivors that make one record.
type usagePricingTally struct {
	key    usagePricingKey
	record usagePricingRecord
	count  int
}

type usagePricingCount struct {
	tally *usagePricingTally
	n     int
}

// usageSurvivors are the usage survivors of a kept range for one candidate
// set, eligibility limit, pricing, and bucket windows.
//
// UsageSurvivorSelectionForSessions decomposes by dedup token: the
// survivor of a token depends only on the rows that carry it, and a row
// without a token survives on its own. So the survivors are kept per token,
// and a change re-derives only the tokens of the rows it touches. A row's
// token is found by its hash; rows whose tokens share a hash are told apart
// by their values.
type usageSurvivors struct {
	valid bool
	seed  maphash.Seed
	// candidate marks the candidate sessions by number.
	candidate []bool
	// start and limit bound the rows that count: start <= ts < limit.
	// startUS and limitUS are their microsecond ceilings.
	start, limit     time.Time
	startUS, limitUS int64
	digest           string
	windows          []activity.BucketWindow
	// tokens lists every row that carries a token, in (hash, session, row)
	// order.
	tokens []usageTokenRow
	// results are the survivors of the tokens, in hash order.
	results []usageTokenSurvivor
	// totals are the candidate sessions' summed survivors, by number.
	totals  []usageSessionTotals
	pricing map[usagePricingKey]*usagePricingTally
	// replaced holds each session whose rows changed since the survivors
	// were derived, with the rows they were derived from.
	replaced map[int32]*activityUsageKept
	// decoded counts the rows the last derivation decoded.
	decoded int
}

// usageDerivation is the scratch state of one derivation.
type usageDerivation struct {
	r      *activityUsageRange
	u      *usageSurvivors
	hasher maphash.Hash
	// selection holds the decoded selection rows by session number; nil
	// until a session's rows are decoded.
	selection [][]usageSelectionRow
	// bandKeys caches the key text of each band list priced so far.
	bandKeys []usageBandKey
}

// usage derives the range's usage survivors for the candidate sessions ids
// and query q, prices the survivors whose price is not kept, and returns
// the survivors for the report: each candidate session's totals. It records
// the survivors' pricing on resolver, whose rows digest names.
func (r *activityUsageRange) usage(
	ctx context.Context, ids []string, q activity.Query,
	resolver *export.PricingResolver, digest string,
) ([]activity.UsageRow, error) {
	u := &r.survivors
	start, limit := q.RangeStart, q.RangeEnd
	if q.EffectiveEnd.Before(limit) {
		limit = q.EffectiveEnd
	}
	candidate := make([]bool, len(r.sessions))
	for _, id := range ids {
		if n, ok := r.number[id]; ok {
			candidate[n] = true
		}
	}
	windows := activity.QueryWindows(q)
	if u.valid && !u.start.Equal(start) {
		*u = usageSurvivors{}
	}
	cold := !u.valid
	if cold {
		*u = usageSurvivors{
			valid: true, seed: maphash.MakeSeed(),
			pricing: make(map[usagePricingKey]*usagePricingTally),
		}
	}
	d := &usageDerivation{
		r: r, u: u,
		selection: make([][]usageSelectionRow, len(r.sessions)),
	}
	d.hasher.SetSeed(u.seed)
	u.decoded = 0

	// The sessions whose own rows may count differently: replaced, entering
	// or leaving the candidate set, or holding a row the moved limit
	// crosses.
	affected := make([]bool, len(r.sessions))
	dirty := 0
	if !cold {
		for n := range u.replaced {
			affected[n] = true
		}
		lo, hi := u.limit, limit
		if hi.Before(lo) {
			lo, hi = hi, lo
		}
		for n, session := range r.sessions {
			was := n < len(u.candidate) && u.candidate[n]
			if was != candidate[n] || (session.timed && lo.Before(hi) &&
				!time.UnixMicro(session.maxUS).Before(lo) && time.UnixMicro(session.minUS).Before(hi)) {
				affected[n] = true
			}
			if affected[n] {
				dirty++
			}
		}
	}
	u.candidate, u.start, u.limit = candidate, start, limit
	u.startUS, u.limitUS = ceilMicros(start), ceilMicros(limit)
	repriced := cold || u.digest != digest || !slices.Equal(u.windows, windows)
	u.digest, u.windows = digest, windows
	if !repriced && dirty == 0 {
		return u.report(r, ids, resolver), nil
	}

	// The tokens whose survivors may change are those of the dirty
	// sessions' previous and current rows.
	var hashes []uint64
	if cold {
		if err := d.indexAllTokens(ctx); err != nil {
			return nil, err
		}
		for i, token := range u.tokens {
			if i == 0 || token.hash != u.tokens[i-1].hash {
				hashes = append(hashes, token.hash)
			}
		}
	} else {
		for n, dirty := range affected {
			if !dirty {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if previous, ok := u.replaced[int32(n)]; ok {
				for _, row := range d.decode(previous) {
					if row.kind != usageTokenNone {
						hashes = append(hashes, row.hash)
					}
				}
			}
			for _, row := range d.rows(int32(n)) {
				if row.kind != usageTokenNone {
					hashes = append(hashes, row.hash)
				}
			}
		}
		slices.Sort(hashes)
		hashes = slices.Compact(hashes)
		d.indexReplacedTokens()
	}

	// Re-derive those tokens' survivors. A survivor counts for the session
	// of one of its token's rows, so the sessions whose totals change are
	// the dirty sessions and the sessions of those tokens' rows.
	results := make([]usageTokenSurvivor, 0, len(hashes))
	for _, hash := range hashes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		members := u.members(hash)
		for _, m := range members {
			affected[m.ref.session] = true
		}
		results = d.survivors(results, members)
	}
	if cold {
		u.results = results
	} else {
		u.results = spliceSorted(u.results, dropHashes(hashes), results, compareTokenSurvivors)
	}
	if repriced {
		for n := range affected {
			affected[n] = true
		}
	}
	if err := d.total(ctx, affected, resolver); err != nil {
		return nil, err
	}
	u.replaced = nil
	return u.report(r, ids, resolver), nil
}

// rows returns the selection rows of session n's current rows.
func (d *usageDerivation) rows(n int32) []usageSelectionRow {
	if d.selection[n] == nil {
		d.selection[n] = d.decode(d.r.sessions[n].rows)
	}
	return d.selection[n]
}

// decode returns the selection rows of kept rows k.
func (d *usageDerivation) decode(k *activityUsageKept) []usageSelectionRow {
	if k == nil {
		return []usageSelectionRow{}
	}
	out := make([]usageSelectionRow, 0, k.count)
	for r := range k.rows() {
		kind, a, b := usageToken(&r.clickActivityReportUsageRow)
		row := usageSelectionRow{
			us: r.us, validTS: r.validTS,
			output: r.outputTok, webSearches: r.webSearchRequests,
			kind: kind, a: a, b: b, message: r.source == "message",
		}
		if kind != usageTokenNone {
			d.hasher.Reset()
			_ = d.hasher.WriteByte(byte(kind))
			_, _ = d.hasher.WriteString(a)
			if kind != usageTokenUsage {
				_ = d.hasher.WriteByte(':')
				_, _ = d.hasher.WriteString(b)
			}
			row.hash = d.hasher.Sum64()
		}
		out = append(out, row)
	}
	d.u.decoded += k.count
	return out
}

// tokenRows returns the token rows of session n's current rows.
func (d *usageDerivation) tokenRows(n int32, out []usageTokenRow) []usageTokenRow {
	for i, row := range d.rows(n) {
		if row.kind != usageTokenNone {
			out = append(out, usageTokenRow{hash: row.hash, ref: usageRowRef{n, int32(i)}})
		}
	}
	return out
}

// indexAllTokens indexes the token rows of every session.
func (d *usageDerivation) indexAllTokens(ctx context.Context) error {
	tokens := make([]usageTokenRow, 0, d.r.rows)
	for n := range d.r.sessions {
		if err := ctx.Err(); err != nil {
			return err
		}
		tokens = d.tokenRows(int32(n), tokens)
	}
	slices.SortFunc(tokens, compareTokenRows)
	d.u.tokens = tokens
	return nil
}

// indexReplacedTokens replaces the token rows of the replaced sessions with
// their current rows' tokens.
func (d *usageDerivation) indexReplacedTokens() {
	u := d.u
	if len(u.replaced) == 0 {
		return
	}
	var added []usageTokenRow
	replaced := make([]bool, len(d.r.sessions))
	for n := range u.replaced {
		added = d.tokenRows(n, added)
		replaced[n] = true
	}
	slices.SortFunc(added, compareTokenRows)
	u.tokens = spliceSorted(u.tokens, func(t usageTokenRow) bool {
		return replaced[t.ref.session]
	}, added, compareTokenRows)
}

func compareTokenRows(a, b usageTokenRow) int {
	if a.hash != b.hash {
		return cmp.Compare(a.hash, b.hash)
	}
	return cmp.Compare(uint64(a.ref.session)<<32|uint64(a.ref.row), uint64(b.ref.session)<<32|uint64(b.ref.row))
}

func compareTokenSurvivors(a, b usageTokenSurvivor) int {
	return cmp.Compare(a.hash, b.hash)
}

// dropHashes reports the survivors of hashes, which are sorted, when it is
// called on survivors in hash order.
func dropHashes(hashes []uint64) func(usageTokenSurvivor) bool {
	next := 0
	return func(s usageTokenSurvivor) bool {
		for next < len(hashes) && hashes[next] < s.hash {
			next++
		}
		return next < len(hashes) && hashes[next] == s.hash
	}
}

// spliceSorted removes from sorted s, in place, the elements drop reports,
// called on each element in order, and merges sorted add into what is left.
// No element of add may compare equal to one that is left.
func spliceSorted[T any](s []T, drop func(T) bool, add []T, compare func(T, T) int) []T {
	kept := 0
	for _, v := range s {
		if !drop(v) {
			s[kept] = v
			kept++
		}
	}
	s = s[:kept]
	if len(add) == 0 {
		return s
	}
	s = slices.Grow(s, len(add))[:kept+len(add)]
	i, j := kept-1, len(add)-1
	for k := len(s) - 1; j >= 0; k-- {
		if i >= 0 && compare(s[i], add[j]) > 0 {
			s[k] = s[i]
			i--
		} else {
			s[k] = add[j]
			j--
		}
	}
	return s
}

// members returns the token rows with hash.
func (u *usageSurvivors) members(hash uint64) []usageTokenRow {
	lo, _ := slices.BinarySearchFunc(u.tokens, hash, func(t usageTokenRow, h uint64) int { return cmp.Compare(t.hash, h) })
	hi := lo
	for hi < len(u.tokens) && u.tokens[hi].hash == hash {
		hi++
	}
	return u.tokens[lo:hi]
}

// tokenSurvivors returns the survivors of the tokens with hash.
func (u *usageSurvivors) tokenSurvivors(hash uint64) []usageTokenSurvivor {
	lo, _ := slices.BinarySearchFunc(u.results, hash, func(s usageTokenSurvivor, h uint64) int { return cmp.Compare(s.hash, h) })
	hi := lo
	for hi < len(u.results) && u.results[hi].hash == hash {
		hi++
	}
	return u.results[lo:hi]
}

// usageMember is a token row with its selection row.
type usageMember struct {
	ref usageRowRef
	row *usageSelectionRow
}

// eligible reports whether the row is in the range's elapsed part.
func (u *usageSurvivors) eligible(row *usageSelectionRow) bool {
	return row.validTS && row.us >= u.startUS && row.us < u.limitUS
}

// ceilMicros returns the first whole microsecond at or after t. A row's
// timestamp t, stored in microseconds, is at or after a bound exactly when
// it is at or after the bound's ceiling.
func ceilMicros(t time.Time) int64 {
	us := t.Unix() * 1_000_000
	ns := int64(t.Nanosecond())
	return us + (ns+999)/1000
}

// before reports whether a comes before b in report order: time, then
// session, then the order of the session's rows.
func (d *usageDerivation) before(a, b usageMember) bool {
	if a.row.us != b.row.us {
		return a.row.us < b.row.us
	}
	if a.ref.session != b.ref.session {
		return d.r.sessions[a.ref.session].id < d.r.sessions[b.ref.session].id
	}
	return a.ref.row < b.ref.row
}

// laterSnapshot reports whether Claude snapshot a replaces b: it has more
// output, then is later, then belongs to the later session. Of equal
// snapshots of one session the first in report order stays.
func (d *usageDerivation) laterSnapshot(a, b usageMember) bool {
	if a.row.output != b.row.output {
		return a.row.output > b.row.output
	}
	if a.row.us != b.row.us {
		return a.row.us > b.row.us
	}
	if a.ref.session != b.ref.session {
		return d.r.sessions[a.ref.session].id > d.r.sessions[b.ref.session].id
	}
	return a.ref.row < b.ref.row
}

// survivors appends the survivors of the token rows members, which share
// one hash. It keeps the rules of UsageSurvivorSelectionForSessions over the
// rows a report reads: every row of a candidate session, and the message
// rows of other sessions that carry a candidate's Claude pair.
//
//   - Of each Claude pair's rows in the elapsed range, the snapshot with
//     the most output survives, counted for the session of the pair's
//     earliest row, with the pair's largest web-search count. A pair's
//     rows in sessions outside the candidate set take part only as message
//     rows; a pair no candidate holds counts for none.
//   - Of a token's counted survivors, the first in report order survives.
func (d *usageDerivation) survivors(out []usageTokenSurvivor, members []usageTokenRow) []usageTokenSurvivor {
	u := d.u
	if len(members) == 1 {
		// Most tokens are carried by one row.
		m := usageMember{ref: members[0].ref, row: &d.rows(members[0].ref.session)[members[0].ref.row]}
		if u.eligible(m.row) && u.candidate[m.ref.session] {
			out = append(out, usageTokenSurvivor{
				hash: m.row.hash, row: m.ref, attributed: m.ref.session, webSearches: m.row.webSearches,
			})
		}
		return out
	}
	ms := make([]usageMember, len(members))
	for i, m := range members {
		ms[i] = usageMember{ref: m.ref, row: &d.rows(m.ref.session)[m.ref.row]}
	}
	done := make([]bool, len(ms))
	for i := range ms {
		if done[i] {
			continue
		}
		var survivor *usageTokenSurvivor
		var survivorRow usageMember
		take := func(s usageTokenSurvivor, row usageMember) {
			if survivor == nil || d.before(row, survivorRow) {
				survivor, survivorRow = &s, row
			}
		}
		pairDone := make([]bool, len(ms))
		for j := i; j < len(ms); j++ {
			if done[j] || !sameToken(ms[i].row, ms[j].row) {
				continue
			}
			done[j] = true
			m := ms[j]
			if m.row.kind != usageTokenClaude {
				if u.eligible(m.row) && u.candidate[m.ref.session] {
					take(usageTokenSurvivor{hash: m.row.hash, row: m.ref, attributed: m.ref.session, webSearches: m.row.webSearches}, m)
				}
				continue
			}
			if pairDone[j] {
				continue
			}
			var best, earliest usageMember
			found := false
			webSearches := 0
			for k := j; k < len(ms); k++ {
				p := ms[k]
				if pairDone[k] || p.row.kind != usageTokenClaude || p.row.a != m.row.a || p.row.b != m.row.b {
					continue
				}
				pairDone[k] = true
				if !u.eligible(p.row) || (!u.candidate[p.ref.session] && !p.row.message) {
					continue
				}
				webSearches = max(webSearches, p.row.webSearches)
				if !found {
					best, earliest, found = p, p, true
					continue
				}
				if d.laterSnapshot(p, best) {
					best = p
				}
				if d.before(p, earliest) {
					earliest = p
				}
			}
			if found && u.candidate[earliest.ref.session] {
				take(usageTokenSurvivor{hash: m.row.hash, row: best.ref, attributed: earliest.ref.session, webSearches: webSearches}, best)
			}
		}
		if survivor != nil {
			out = append(out, *survivor)
		}
	}
	return out
}

// usagePriced is a survivor priced for its session's totals.
type usagePriced struct {
	member      usageMember
	model, ts   string
	input       int
	output      int
	webSearches int
	price       clickActivityUsagePriced
	// tally is the pricing record the survivor makes; nil when none.
	tally *usagePricingTally
}

// total re-derives the totals of the affected sessions.
func (d *usageDerivation) total(ctx context.Context, affected []bool, resolver *export.PricingResolver) error {
	r, u := d.r, d.u
	if len(u.totals) < len(r.sessions) {
		u.totals = append(u.totals, make([]usageSessionTotals, len(r.sessions)-len(u.totals))...)
	}
	// The affected candidates' survivors: their own rows without a token,
	// and the token survivors counted for them. Most of a derivation from
	// scratch finds the latter faster in one pass over every survivor.
	type survivorRef struct {
		owner int32
		ref   usageRowRef
		web   int
	}
	var refs []survivorRef
	owners := 0
	for n, ok := range affected {
		if !ok {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, count := range u.totals[n].pricing {
			count.tally.count -= count.n
			if count.tally.count == 0 {
				delete(u.pricing, count.tally.key)
			}
		}
		u.totals[n] = usageSessionTotals{}
		if !u.candidate[n] {
			continue
		}
		owners++
		for i, row := range d.rows(int32(n)) {
			if row.kind == usageTokenNone && u.eligible(&row) {
				refs = append(refs, survivorRef{owner: int32(n), ref: usageRowRef{int32(n), int32(i)}, web: row.webSearches})
			}
		}
	}
	if owners > len(r.sessions)/16 {
		for _, s := range u.results {
			if affected[s.attributed] && u.candidate[s.attributed] {
				refs = append(refs, survivorRef{owner: s.attributed, ref: s.row, web: s.webSearches})
			}
		}
	} else {
		for n, ok := range affected {
			if !ok || !u.candidate[n] {
				continue
			}
			var hashes []uint64
			for _, row := range d.rows(int32(n)) {
				if row.kind != usageTokenNone {
					hashes = append(hashes, row.hash)
				}
			}
			slices.Sort(hashes)
			for _, hash := range slices.Compact(hashes) {
				for _, s := range u.tokenSurvivors(hash) {
					if s.attributed == int32(n) {
						refs = append(refs, survivorRef{owner: int32(n), ref: s.row, web: s.webSearches})
					}
				}
			}
		}
	}

	// priced holds the survivors grouped by owner, each owner's at its
	// span; requests are the survivors by the session that holds their row.
	type request struct {
		slot int
		row  int32
		web  int
	}
	counts := make([]int, len(r.sessions))
	for _, ref := range refs {
		counts[ref.owner]++
	}
	spans := make([][2]int, len(r.sessions))
	at := 0
	for n, count := range counts {
		spans[n] = [2]int{at, at}
		at += count
	}
	priced := make([]usagePriced, len(refs))
	requests := make([][]request, len(r.sessions))
	for _, ref := range refs {
		slot := spans[ref.owner][1]
		spans[ref.owner][1]++
		requests[ref.ref.session] = append(requests[ref.ref.session], request{slot: slot, row: ref.ref.row, web: ref.web})
	}

	// Price the survivors one session's rows at a time.
	for session, reqs := range requests {
		if len(reqs) == 0 {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		slices.SortFunc(reqs, func(a, b request) int { return cmp.Compare(a.row, b.row) })
		selection := d.rows(int32(session))
		next, i := 0, int32(0)
		for row := range r.sessions[session].rows.rows() {
			for ; next < len(reqs) && reqs[next].row == i; next++ {
				req := reqs[next]
				costRow := row.clickActivityReportUsageRow
				costRow.webSearchRequests = req.web
				price, record, err := clickActivityUsagePrice(costRow, resolver)
				if err != nil {
					return err
				}
				priced[req.slot] = usagePriced{
					member: usageMember{ref: usageRowRef{int32(session), i}, row: &selection[i]},
					model:  costRow.model, ts: costRow.ts,
					input: costRow.inputTok, output: costRow.outputTok, webSearches: req.web,
					price: price, tally: d.tally(record),
				}
			}
			if next == len(reqs) {
				break
			}
			i++
		}
		u.decoded += int(i) + 1
	}

	for n, ok := range affected {
		if !ok || !u.candidate[n] {
			continue
		}
		totals, err := d.sessionTotals(priced[spans[n][0]:spans[n][1]])
		if err != nil {
			return err
		}
		u.totals[n] = totals
	}
	return nil
}

// tally returns the kept tally of a pricing record, or nil when the record
// records nothing.
func (d *usageDerivation) tally(rec usagePricingRecord) *usagePricingTally {
	if rec.kind == usagePricingNone && !rec.unattributed {
		return nil
	}
	key := d.pricingKey(rec)
	tally := d.u.pricing[key]
	if tally == nil {
		tally = &usagePricingTally{key: key, record: rec}
		d.u.pricing[key] = tally
	}
	return tally
}

// usageTotalGroup is what survivors summed into one total row share.
type usageTotalGroup struct {
	model               string
	window              int
	costSource          export.CostSource
	priced, contributes bool
}

// sessionTotals sums one session's priced survivors.
func (d *usageDerivation) sessionTotals(entries []usagePriced) (usageSessionTotals, error) {
	u := d.u
	var totals usageSessionTotals
	perRow := false
	for _, e := range entries {
		perRow = perRow || e.price.sessionCost != nil
	}
	if perRow {
		// The report apportions the session total over the rows in report
		// order.
		slices.SortFunc(entries, func(a, b usagePriced) int {
			if d.before(a.member, b.member) {
				return -1
			}
			if d.before(b.member, a.member) {
				return 1
			}
			return 0
		})
	}
	for _, e := range entries {
		if e.tally == nil {
			continue
		}
		e.tally.count++
		i := slices.IndexFunc(totals.pricing, func(c usagePricingCount) bool { return c.tally == e.tally })
		if i < 0 {
			totals.pricing = append(totals.pricing, usagePricingCount{tally: e.tally})
			i = len(totals.pricing) - 1
		}
		totals.pricing[i].n++
	}
	var groups []usageTotalGroup
	for _, e := range entries {
		row := usageTotalRow{
			model: e.model, ts: e.ts,
			input: e.input, output: e.output, webSearches: e.webSearches,
			cost: e.price.cost, costSource: e.price.costSource, sessionCost: e.price.sessionCost,
			priced: e.price.priced, contributes: e.price.contributes,
		}
		if perRow {
			totals.rows = append(totals.rows, row)
			continue
		}
		group := usageTotalGroup{
			model: e.model, window: activity.WindowIndex(u.windows, time.UnixMicro(e.member.row.us).UTC()),
			costSource: row.costSource, priced: row.priced, contributes: row.contributes,
		}
		i := slices.Index(groups, group)
		if i < 0 {
			groups = append(groups, group)
			totals.rows = append(totals.rows, row)
			continue
		}
		sum := &totals.rows[i]
		sum.input += row.input
		sum.output += row.output
		sum.webSearches += row.webSearches
		var err error
		if sum.cost, err = money.Add(sum.cost, row.cost); err != nil {
			return usageSessionTotals{}, err
		}
	}
	return totals, nil
}

// pricingKey returns the key of a pricing record.
func (d *usageDerivation) pricingKey(rec usagePricingRecord) usagePricingKey {
	rates := rec.lookup.Rates
	key := usagePricingKey{
		kind: rec.kind, unattributed: rec.unattributed,
		reportedModel: rec.reportedModel, pricedModel: rec.pricedModel,
		band: rec.band, hasBand: rec.hasBand,
		pattern: rec.lookup.Pattern, adjustment: rec.lookup.Adjustment, ok: rec.lookup.OK,
		source: rates.Source,
		input:  rates.InputPerMTok.Microdollars, output: rates.OutputPerMTok.Microdollars,
		cacheWrite: rates.CacheWritePerMTok.Microdollars, cacheWrite1h: rates.CacheWrite1hPerMTok.Microdollars,
		cacheRead: rates.CacheReadPerMTok.Microdollars,
	}
	if len(rates.Bands) > 0 {
		key.bands = d.bandKey(rates.Bands)
	}
	return key
}

// usageBandKey is the key text of a list of price bands.
type usageBandKey struct {
	bands []export.PricingBand
	key   string
}

// bandKey returns the key text of a list of price bands: the values the
// pricing block shows of each, in order. Resolvers clone a lookup's bands
// on every lookup, so lists are matched by their values.
func (d *usageDerivation) bandKey(bands []export.PricingBand) string {
	same := func(a, b export.PricingBand) bool {
		return a.AboveInputTokens == b.AboveInputTokens && a.InputPerMTok == b.InputPerMTok &&
			a.OutputPerMTok == b.OutputPerMTok && a.CacheWritePerMTok == b.CacheWritePerMTok &&
			a.CacheWrite1hPerMTok == b.CacheWrite1hPerMTok && a.CacheReadPerMTok == b.CacheReadPerMTok
	}
	for _, cached := range d.bandKeys {
		if slices.EqualFunc(cached.bands, bands, same) {
			return cached.key
		}
	}
	var b strings.Builder
	for _, band := range bands {
		for _, v := range [...]int64{
			int64(band.AboveInputTokens), band.InputPerMTok.Microdollars, band.OutputPerMTok.Microdollars,
			band.CacheWritePerMTok.Microdollars, band.CacheWrite1hPerMTok.Microdollars, band.CacheReadPerMTok.Microdollars,
		} {
			b.WriteString(strconv.FormatInt(v, 10))
			b.WriteByte(',')
		}
		b.WriteByte(';')
	}
	d.bandKeys = append(d.bandKeys, usageBandKey{bands: slices.Clone(bands), key: b.String()})
	return b.String()
}

func compareUsagePricingKeys(a, b usagePricingKey) int {
	return cmp.Or(
		cmp.Compare(a.reportedModel, b.reportedModel), cmp.Compare(a.pricedModel, b.pricedModel),
		cmp.Compare(a.kind, b.kind), compareBool(a.unattributed, b.unattributed),
		compareBool(a.hasBand, b.hasBand), cmp.Compare(a.band, b.band),
		cmp.Compare(a.pattern, b.pattern), compareBool(a.ok, b.ok), cmp.Compare(a.adjustment, b.adjustment),
		cmp.Compare(a.source, b.source), cmp.Compare(a.input, b.input), cmp.Compare(a.output, b.output),
		cmp.Compare(a.cacheWrite, b.cacheWrite), cmp.Compare(a.cacheWrite1h, b.cacheWrite1h),
		cmp.Compare(a.cacheRead, b.cacheRead), cmp.Compare(a.bands, b.bands),
	)
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// report returns the candidate sessions' totals as report usage rows and
// records their pricing on resolver.
func (u *usageSurvivors) report(r *activityUsageRange, ids []string, resolver *export.PricingResolver) []activity.UsageRow {
	for _, tally := range slices.SortedFunc(maps.Values(u.pricing), func(a, b *usagePricingTally) int {
		return compareUsagePricingKeys(a.key, b.key)
	}) {
		tally.record.apply(resolver, tally.count)
	}
	emitted := make([]bool, len(r.sessions))
	count := 0
	for _, id := range ids {
		if n, ok := r.number[id]; ok {
			count += len(u.totals[n].rows)
		}
	}
	out := make([]activity.UsageRow, 0, count)
	for _, id := range ids {
		n, ok := r.number[id]
		if !ok || emitted[n] {
			continue
		}
		emitted[n] = true
		for _, row := range u.totals[n].rows {
			out = append(out, activity.UsageRow{
				SessionID: id, Model: row.model, Timestamp: row.ts,
				InputTokens: row.input, OutputTokens: row.output, WebSearchRequests: row.webSearches,
				Cost: row.cost, CostSource: row.costSource, SessionCost: row.sessionCost,
				Priced: row.priced, Contributes: row.contributes,
			})
		}
	}
	return out
}
