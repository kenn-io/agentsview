package export

import (
	"math/big"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

type PricingRowSource string

const (
	PricingRowSourceCustom   PricingRowSource = "custom"
	PricingRowSourceFetched  PricingRowSource = "fetched"
	PricingRowSourceEmbedded PricingRowSource = "embedded"
)

type ModelRates struct {
	InputPerMTok      money.Money
	OutputPerMTok     money.Money
	CacheWritePerMTok money.Money
	CacheReadPerMTok  money.Money
	UpdatedAt         *time.Time
	Source            PricingRowSource
}

func (r ModelRates) CostForTokens(
	inputTokens, outputTokens, reasoningTokens, cacheWriteTokens, cacheReadTokens int,
) money.Money {
	// reasoningTokens is a breakdown of outputTokens for current sources, not
	// additional billable output. Reasoning-only rows still bill at output rate.
	billableOutputTokens := outputTokens
	if billableOutputTokens == 0 {
		billableOutputTokens = reasoningTokens
	}
	cost, err := money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(inputTokens), Rate: r.InputPerMTok},
		{Tokens: int64(billableOutputTokens), Rate: r.OutputPerMTok},
		{Tokens: int64(cacheWriteTokens), Rate: r.CacheWritePerMTok},
		{Tokens: int64(cacheReadTokens), Rate: r.CacheReadPerMTok},
	})
	if err != nil {
		panic(err)
	}
	return cost
}

type EffectivePricingRow struct {
	ModelPattern string
	Rates        ModelRates
}

type PricingLookup struct {
	Rates   ModelRates
	Pattern string
	OK      bool
}

type PricingResolver struct {
	rows                 []EffectivePricingRow
	byModel              map[string]ModelRates
	lookupCache          map[string]PricingLookup
	recorded             map[string]*pricingRecord
	unattributedReported bool
}

type pricingRecord struct {
	lookup   PricingLookup
	computed bool
	reported bool
}

func NewPricingResolver(rows []EffectivePricingRow) *PricingResolver {
	copied := make([]EffectivePricingRow, len(rows))
	copy(copied, rows)
	byModel := make(map[string]ModelRates, len(rows))
	for _, row := range rows {
		if row.ModelPattern == "" {
			continue
		}
		byModel[row.ModelPattern] = row.Rates
	}
	return &PricingResolver{
		rows:        copied,
		byModel:     byModel,
		lookupCache: make(map[string]PricingLookup),
		recorded:    make(map[string]*pricingRecord),
	}
}

func (r *PricingResolver) Lookup(model string) PricingLookup {
	if r == nil {
		return PricingLookup{}
	}
	if lookup, ok := r.lookupCache[model]; ok {
		return lookup
	}
	match := pricingpkg.ResolveMatch(model, r.byModel)
	lookup := PricingLookup{
		Rates:   match.Value,
		Pattern: match.Pattern,
		OK:      match.OK,
	}
	r.lookupCache[model] = lookup
	return lookup
}

func (r *PricingResolver) RecordComputed(model string, lookup PricingLookup) {
	if r == nil || model == "" {
		return
	}
	rec := r.record(model, lookup)
	rec.computed = true
}

func (r *PricingResolver) RecordReported(model string, lookup PricingLookup) {
	if r == nil || model == "" {
		return
	}
	rec := r.record(model, lookup)
	rec.reported = true
}

// RecordUnattributedReported records an authoritative aggregate cost that
// cannot be assigned to a model without inventing an allocation.
func (r *PricingResolver) RecordUnattributedReported() {
	if r != nil {
		r.unattributedReported = true
	}
}

func (r *PricingResolver) record(model string, lookup PricingLookup) *pricingRecord {
	rec := r.recorded[model]
	if rec == nil {
		rec = &pricingRecord{}
		r.recorded[model] = rec
	}
	rec.lookup = lookup
	return rec
}

func (r *PricingResolver) BuildBlock() (PricingBlock, error) {
	if r == nil {
		return PricingBlock{}, nil
	}
	models := make(map[string]EffectiveModelRate, len(r.recorded))
	fallbackSet := make(map[string]struct{})
	var hasComputed bool
	hasReported := r.unattributedReported
	modelNames := make([]string, 0, len(r.recorded))
	for model := range r.recorded {
		modelNames = append(modelNames, model)
	}
	sort.Strings(modelNames)
	for _, model := range modelNames {
		rec := r.recorded[model]
		if rec == nil {
			continue
		}
		source := recordCostSource(rec)
		hasComputed = hasComputed || rec.computed
		hasReported = hasReported || rec.reported
		rate := EffectiveModelRate{
			InputCostPerMTok:      rec.lookup.Rates.InputPerMTok,
			OutputCostPerMTok:     rec.lookup.Rates.OutputPerMTok,
			CacheWriteCostPerMTok: rec.lookup.Rates.CacheWritePerMTok,
			CacheReadCostPerMTok:  rec.lookup.Rates.CacheReadPerMTok,
			CostSource:            source,
		}
		if rec.lookup.OK {
			pattern := rec.lookup.Pattern
			rate.MatchedPattern = &pattern
			if rec.lookup.Rates.Source == PricingRowSourceEmbedded {
				fallbackSet[model] = struct{}{}
			}
		}
		models[model] = rate
	}

	fallbackModels := make([]string, 0, len(fallbackSet))
	for model := range fallbackSet {
		fallbackModels = append(fallbackModels, model)
	}
	sort.Strings(fallbackModels)

	digest, err := EffectivePricingDigest(r.rows)
	if err != nil {
		return PricingBlock{}, err
	}

	return PricingBlock{
		Source:              pricingSource(r.rows),
		TableVersion:        pricingTableVersion(r.rows),
		LatestRowUpdatedAt:  latestPricingRowUpdate(r.rows),
		CustomOverrideCount: customPricingRowCount(r.rows),
		EffectiveRowCount:   len(r.rows),
		Digest:              digest,
		CostSource:          CombinedCostSource(hasComputed, hasReported),
		Fallback: PricingFallback{
			Used:   len(fallbackModels) > 0,
			Models: fallbackModels,
		},
		Models: models,
	}, nil
}

func pricingTableVersion(rows []EffectivePricingRow) string {
	source := pricingSource(rows)
	if strings.Contains(source, string(PricingRowSourceFetched)) {
		if latest := latestPricingRowUpdate(rows); latest != nil {
			return latest.UTC().Format(jsonTimeLayout)
		}
		return string(PricingRowSourceFetched)
	}
	if strings.Contains(source, string(PricingRowSourceEmbedded)) {
		return pricingpkg.FallbackVersion
	}
	if source == string(PricingRowSourceCustom) {
		return string(PricingRowSourceCustom)
	}
	return ""
}

func recordCostSource(rec *pricingRecord) CostSource {
	return CombinedCostSource(rec.computed, rec.reported)
}

// CombinedCostSource resolves normalized provenance flags into the wire enum.
func CombinedCostSource(computed, reported bool) CostSource {
	switch {
	case computed && reported:
		return CostSourceMixed
	case reported:
		return CostSourceReported
	default:
		return CostSourceComputed
	}
}

// AllocateCostByWeight distributes a reported aggregate cost across estimated
// components. The final positive-weight component receives the integer
// remainder so allocations add back to the authoritative total exactly.
func AllocateCostByWeight(total money.Money, weights []money.Money) []money.Money {
	allocated := make([]money.Money, len(weights))
	if len(weights) == 0 || total.Microdollars == 0 {
		return allocated
	}

	weightTotal := new(big.Int)
	remainderIndex := -1
	equalWeights := false
	for i, weight := range weights {
		if weight.Microdollars > 0 {
			weightTotal.Add(weightTotal, big.NewInt(weight.Microdollars))
			remainderIndex = i
		}
	}
	if weightTotal.Sign() == 0 {
		weightTotal.SetInt64(int64(len(weights)))
		remainderIndex = len(weights) - 1
		equalWeights = true
	}

	assigned := new(big.Int)
	totalInt := big.NewInt(total.Microdollars)
	for i, weight := range weights {
		if equalWeights {
			weight = money.Money{Microdollars: 1}
		}
		if i == remainderIndex || weight.Microdollars <= 0 {
			continue
		}
		share := new(big.Int).Mul(totalInt, big.NewInt(weight.Microdollars))
		share.Quo(share, weightTotal)
		if !share.IsInt64() {
			panic(money.ErrOverflow)
		}
		allocated[i] = money.Money{Microdollars: share.Int64()}
		assigned.Add(assigned, share)
	}
	remainder := new(big.Int).Sub(totalInt, assigned)
	if !remainder.IsInt64() {
		panic(money.ErrOverflow)
	}
	allocated[remainderIndex] = money.Money{Microdollars: remainder.Int64()}
	return allocated
}

func pricingSource(rows []EffectivePricingRow) string {
	var custom, fetched, embedded bool
	for _, row := range rows {
		switch row.Rates.Source {
		case PricingRowSourceCustom:
			custom = true
		case PricingRowSourceFetched:
			fetched = true
		case PricingRowSourceEmbedded:
			embedded = true
		}
	}
	var base string
	switch {
	case fetched:
		base = string(PricingRowSourceFetched)
	case embedded:
		base = string(PricingRowSourceEmbedded)
	}
	if custom {
		if base == "" {
			return string(PricingRowSourceCustom)
		}
		return string(PricingRowSourceCustom) + "+" + base
	}
	return base
}

func customPricingRowCount(rows []EffectivePricingRow) int {
	var count int
	for _, row := range rows {
		if row.Rates.Source == PricingRowSourceCustom {
			count++
		}
	}
	return count
}

func latestPricingRowUpdate(rows []EffectivePricingRow) *time.Time {
	var latest *time.Time
	for _, row := range rows {
		if row.Rates.UpdatedAt == nil {
			continue
		}
		t := row.Rates.UpdatedAt.UTC()
		if latest == nil || t.After(*latest) {
			latest = &t
		}
	}
	return latest
}
