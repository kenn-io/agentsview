package readbase

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/activity"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/kept"
	"go.kenn.io/agentsview/internal/money"
)

// The DuckDB and ClickHouse Activity reports select, price, and sum usage
// survivors in SQL. Go resolves each model's rates over the report range
// into a UsageRateSchedule, the backend's SQL picks each survivor's segment
// and band and does the integer cost math, and AssembleActivityUsage turns
// the summed groups into report usage rows and records their pricing.

// ActivityUsageNullPricingUS is the pricing timestamp, in microseconds, of a
// usage row without one. Such a row prices as usage without a timestamp.
const ActivityUsageNullPricingUS = math.MinInt64

// Pricing kinds. Each survivor makes exactly one.
const (
	UsageKindReported    = "r" // a reported cost prices it
	UsageKindZero        = "z" // no tokens and no web searches
	UsageKindUnresolved  = "u" // no catalog rate
	UsageKindRequest     = "q" // one request, priced with bands
	UsageKindAggregate   = "a" // a sum of requests, priced at base rates
	UsageKindBillingErr  = "e" // the provider adjustment failed
	UsageKindMissingRate = "m" // no rate was sent for its model
)

// UsageRateKey is a model and the provider that reported it.
type UsageRateKey struct{ Model, ProviderID string }

// CompareUsageRateKeys orders keys by model, then provider.
func CompareUsageRateKeys(a, b UsageRateKey) int {
	return cmp.Or(cmp.Compare(a.Model, b.Model), cmp.Compare(a.ProviderID, b.ProviderID))
}

// UsageRate is one resolution of a model and provider, valid from FromUS
// until the next segment of the same key.
type UsageRate struct {
	export.UsageRate
	Model, ProviderID string
	FromUS            int64
}

// UsageRateSchedule lists every key's rate segments. A segment's ID is its
// index.
type UsageRateSchedule struct {
	Rates []UsageRate
}

// BuildUsageRateSchedule resolves every key over [startUS, endUS) and for
// usage without a timestamp. A segment starts at the range start and at
// every instant the resolver says a price can change; adjacent segments
// that resolve alike merge.
//
// A usage row's pricing timestamp is its own timestamp or missing, so a
// report over [startUS, endUS) prices no row outside the span. The first
// segment would price an earlier timestamp at the rate in effect at
// startUS.
func BuildUsageRateSchedule(
	pricing *export.PricingResolver, keys []UsageRateKey, startUS, endUS int64,
) UsageRateSchedule {
	starts := []int64{startUS}
	for _, change := range pricing.PriceChanges(time.UnixMicro(startUS), time.UnixMicro(endUS)) {
		starts = append(starts, CeilMicros(change))
	}
	starts = slices.Compact(starts)
	var schedule UsageRateSchedule
	for _, key := range keys {
		schedule.Rates = append(schedule.Rates, UsageRate{
			UsageRate: pricing.UsageRateAt(key.ProviderID, key.Model, time.Time{}),
			Model:     key.Model, ProviderID: key.ProviderID,
			FromUS: ActivityUsageNullPricingUS,
		})
		var previous *export.UsageRate
		for i, from := range starts {
			rate := pricing.UsageRateAt(key.ProviderID, key.Model, time.UnixMicro(from).UTC())
			if previous != nil && rate.Equal(*previous) {
				continue
			}
			if i == 0 {
				// Real timestamps sort after every row without one.
				from = ActivityUsageNullPricingUS + 1
			}
			schedule.Rates = append(schedule.Rates, UsageRate{
				UsageRate: rate, Model: key.Model, ProviderID: key.ProviderID, FromUS: from,
			})
			previous = &schedule.Rates[len(schedule.Rates)-1].UsageRate
		}
	}
	return schedule
}

// UsageRateSegment is a schedule segment as the SQL reads it: the segment's
// billed rates in microdollars per million tokens and its resolution flags.
type UsageRateSegment struct {
	ID                                  int64
	Model, ProviderID                   string
	FromUS                              int64
	BilledOK, CatalogOK, BilledErr      bool
	Input, Output                       int64
	CacheWrite, CacheWrite1h, CacheRead int64
}

// Segments returns the schedule's segments in ID order.
func (s UsageRateSchedule) Segments() []UsageRateSegment {
	out := make([]UsageRateSegment, len(s.Rates))
	for id, rate := range s.Rates {
		r := rate.Billed.Rates
		out[id] = UsageRateSegment{
			ID: int64(id), Model: rate.Model, ProviderID: rate.ProviderID, FromUS: rate.FromUS,
			BilledOK: rate.Billed.OK, CatalogOK: rate.Catalog.OK, BilledErr: rate.BilledErr != nil,
			Input: r.InputPerMTok.Microdollars, Output: r.OutputPerMTok.Microdollars,
			CacheWrite: r.CacheWritePerMTok.Microdollars, CacheWrite1h: r.EffectiveCacheWrite1hPerMTok().Microdollars,
			CacheRead: r.CacheReadPerMTok.Microdollars,
		}
	}
	return out
}

// UsageRateBand is a request band of a segment as the SQL reads it. A request
// whose input, cache write, and cache read tokens sum to at least Floor, one
// more than Above, prices at the band's rates, unless a band with a higher
// floor also applies.
type UsageRateBand struct {
	RateID, Floor, Above                int64
	Input, Output                       int64
	CacheWrite, CacheWrite1h, CacheRead int64
}

// Bands returns the request bands of every segment. Of several bands with
// one threshold, the first prices a request, so only it is listed.
func (s UsageRateSchedule) Bands() []UsageRateBand {
	var out []UsageRateBand
	for id, rate := range s.Rates {
		seen := map[int]bool{}
		for _, band := range rate.Billed.Rates.Bands {
			if seen[band.AboveInputTokens] {
				continue
			}
			seen[band.AboveInputTokens] = true
			bandRates := export.ModelRates{
				CacheWritePerMTok: band.CacheWritePerMTok, CacheWrite1hPerMTok: band.CacheWrite1hPerMTok,
			}
			out = append(out, UsageRateBand{
				RateID: int64(id), Floor: int64(band.AboveInputTokens) + 1, Above: int64(band.AboveInputTokens),
				Input: band.InputPerMTok.Microdollars, Output: band.OutputPerMTok.Microdollars,
				CacheWrite: band.CacheWritePerMTok.Microdollars, CacheWrite1h: bandRates.EffectiveCacheWrite1hPerMTok().Microdollars,
				CacheRead: band.CacheReadPerMTok.Microdollars,
			})
		}
	}
	return out
}

// ActivityUsageBucketStarts returns the first whole microsecond of each of
// the report's bucket windows; a survivor belongs to the last window that
// starts at or before it.
func ActivityUsageBucketStarts(q activity.Query) []int64 {
	windows := activity.QueryWindows(q)
	out := make([]int64, len(windows))
	for i, window := range windows {
		out[i] = CeilMicros(window.Start)
	}
	return out
}

// ActivityUsageSpan returns the microseconds a report's usage may fall in:
// from its range start up to the earlier of its range end and effective
// end, both rounded up to whole microseconds.
func ActivityUsageSpan(q activity.Query) (startUS, endUS int64) {
	end := q.RangeEnd
	if q.EffectiveEnd.Before(end) {
		end = q.EffectiveEnd
	}
	return CeilMicros(q.RangeStart), CeilMicros(end)
}

// ActivityUsageGroup is a sum of usage survivors the SQL returns: the
// survivors of one session, model, provider, bucket, rate segment, pricing
// kind, and band. A survivor of a session that carries a session total is
// a group of its own, Raw set and keyed by its report order: its timestamp,
// session, ordinal, source, and usage key, then RawTie, for a backend that
// orders rows tying on those columns by another value.
type ActivityUsageGroup struct {
	Session, Model, ProviderID, Kind string
	Bucket, Rate, Band               int64
	Raw                              bool
	RawTS, RawOrd, RawTie            int64
	RawSession, RawSource, RawKey    string
	Count, Input, Output, Cost       int64
	Overflow                         bool
	MinTS                            int64
	SessionCost                      *int64
}

// PriceActivityUsageGroups reads the usage groups of a report over
// [startUS, endUS) under a schedule of the rates of keys, and returns the
// schedule and the groups in a fixed order. A push may add a model after
// keys were read; its rows come back without a rate, and read runs again
// with that model added, up to three reads in all.
func PriceActivityUsageGroups(
	pricing *export.PricingResolver, keys []UsageRateKey, startUS, endUS int64,
	read func(UsageRateSchedule) ([]ActivityUsageGroup, error),
) (UsageRateSchedule, []ActivityUsageGroup, error) {
	for attempt := 0; ; attempt++ {
		schedule := BuildUsageRateSchedule(pricing, keys, startUS, endUS)
		groups, err := read(schedule)
		if err != nil {
			return UsageRateSchedule{}, nil, err
		}
		missing := missingUsageRates(groups)
		if len(missing) == 0 {
			sortActivityUsageGroups(groups)
			return schedule, groups, nil
		}
		if attempt == 2 {
			return UsageRateSchedule{}, nil, fmt.Errorf(
				"pricing activity usage for model %q: no rate was resolved for it", missing[0].Model)
		}
		keys = append(keys, missing...)
	}
}

// missingUsageRates returns the keys of groups the schedule had no rate for.
func missingUsageRates(groups []ActivityUsageGroup) []UsageRateKey {
	var missing []UsageRateKey
	for _, g := range groups {
		if g.Kind == UsageKindMissingRate {
			missing = append(missing, UsageRateKey{Model: g.Model, ProviderID: g.ProviderID})
		}
	}
	slices.SortFunc(missing, CompareUsageRateKeys)
	return slices.Compact(missing)
}

func (g ActivityUsageGroup) check(schedule UsageRateSchedule) error {
	switch {
	case g.Rate < 0 || int(g.Rate) >= len(schedule.Rates):
		return fmt.Errorf("pricing activity usage for model %q: unknown rate %d", g.Model, g.Rate)
	case g.Kind == UsageKindBillingErr:
		return schedule.Rates[g.Rate].BilledErr
	case g.Overflow:
		return fmt.Errorf("pricing activity usage for model %q: %w", g.Model, money.ErrOverflow)
	}
	return nil
}

func (g ActivityUsageGroup) usageRow() activity.UsageRow {
	row := activity.UsageRow{
		SessionID:    g.Session,
		Model:        g.Model,
		Timestamp:    time.UnixMicro(g.MinTS).UTC().Format(time.RFC3339Nano),
		InputTokens:  int(g.Input),
		OutputTokens: int(g.Output),
		Cost:         money.Money{Microdollars: g.Cost},
		CostSource:   export.CostSourceComputed,
		Priced:       g.Kind != UsageKindUnresolved,
		Contributes:  g.Kind != UsageKindZero,
	}
	if g.Kind == UsageKindReported {
		row.CostSource = export.CostSourceReported
	}
	if g.SessionCost != nil {
		row.SessionCost = &money.Money{Microdollars: *g.SessionCost}
	}
	return row
}

// sortActivityUsageGroups puts groups in a fixed order. A query returns its
// groups in no set order; sorting them keeps the rows of equal reads equal.
func sortActivityUsageGroups(groups []ActivityUsageGroup) {
	slices.SortFunc(groups, func(a, b ActivityUsageGroup) int {
		return cmp.Or(
			cmp.Compare(a.Session, b.Session), cmp.Compare(a.Bucket, b.Bucket),
			cmp.Compare(a.Model, b.Model), cmp.Compare(a.ProviderID, b.ProviderID),
			cmp.Compare(a.Rate, b.Rate), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Band, b.Band))
	})
}

// AssembleActivityUsage returns the report usage rows of groups, which hold
// no missing rates, in the order of groups, and records their pricing on
// pricing as pricing each survivor one by one would. It leaves groups
// unchanged.
func AssembleActivityUsage(
	pricing *export.PricingResolver, schedule UsageRateSchedule, groups []ActivityUsageGroup,
) ([]activity.UsageRow, error) {
	out := []activity.UsageRow{}
	var raw []ActivityUsageGroup
	type recordKey struct {
		rate int64
		kind string
		band int64
	}
	records := map[recordKey]int{}
	unattributed := false
	for _, g := range groups {
		if err := g.check(schedule); err != nil {
			return nil, err
		}
		records[recordKey{g.Rate, g.Kind, g.Band}] += int(g.Count)
		if g.SessionCost != nil {
			unattributed = true
		}
		if g.Raw {
			raw = append(raw, g)
			continue
		}
		out = append(out, g.usageRow())
	}
	if unattributed {
		pricing.RecordUnattributedReported()
	}
	for key, n := range records {
		replayUsagePricing(pricing, schedule.Rates[key.rate], key.kind, key.band, n)
	}
	// A session's carrier is its last session cost in report order, and
	// its remainder goes to its last row, so its rows stay in that order.
	slices.SortFunc(raw, func(a, b ActivityUsageGroup) int {
		return cmp.Or(
			cmp.Compare(a.Session, b.Session), cmp.Compare(a.RawTS, b.RawTS),
			cmp.Compare(a.RawSession, b.RawSession), cmp.Compare(a.RawOrd, b.RawOrd),
			cmp.Compare(a.RawSource, b.RawSource), cmp.Compare(a.RawKey, b.RawKey),
			cmp.Compare(a.RawTie, b.RawTie))
	})
	for _, g := range raw {
		out = append(out, g.usageRow())
	}
	return out, nil
}

// replayUsagePricing records n survivors priced under rate as kind, as
// pricing each of them one by one would record them.
func replayUsagePricing(
	pricing *export.PricingResolver, rate UsageRate, kind string, band int64, n int,
) {
	switch kind {
	case UsageKindReported:
		pricing.RecordResolvedReported(rate.Model, rate.CatalogModel, rate.Catalog)
	case UsageKindUnresolved:
		pricing.RecordResolvedComputed(rate.Model, rate.BilledModel, rate.Billed)
	case UsageKindRequest:
		var threshold *int
		if band >= 0 {
			b := int(band)
			threshold = &b
		}
		pricing.RecordResolvedCounts(rate.Model, rate.BilledModel, rate.Billed, 0, n, 0, threshold)
	case UsageKindAggregate:
		pricing.RecordResolvedCounts(rate.Model, rate.BilledModel, rate.Billed, 0, 0, n, nil)
	}
}

// CeilMicros returns the first whole microsecond at or after t, so a
// microsecond timestamp is at or after t exactly when it is at or after
// CeilMicros(t).
func CeilMicros(t time.Time) int64 {
	us := t.UnixMicro()
	if time.UnixMicro(us).Before(t) {
		us++
	}
	return us
}

// ActivityUsageSums holds priced groups through EndUS and the next source row.
type ActivityUsageSums struct {
	Schedule      UsageRateSchedule
	Groups        []ActivityUsageGroup
	EndUS, NextUS int64
}

// Covers reports whether no source row has entered or left the saved span.
func (u *ActivityUsageSums) Covers(endUS int64) bool {
	return u.EndUS <= endUS && endUS <= u.NextUS
}

// Bytes estimates the memory held by the sums.
func (u *ActivityUsageSums) Bytes() int64 {
	n := int64(unsafe.Sizeof(*u)) + int64(cap(u.Schedule.Rates))*int64(unsafe.Sizeof(UsageRate{})) + int64(cap(u.Groups))*int64(unsafe.Sizeof(ActivityUsageGroup{}))
	for _, rate := range u.Schedule.Rates {
		n += kept.JSONBytes(rate)
	}
	for _, g := range u.Groups {
		for _, v := range []string{g.Session, g.Model, g.ProviderID, g.Kind, g.RawSession, g.RawSource, g.RawKey} {
			n += int64(len(v))
		}
	}
	return n
}
