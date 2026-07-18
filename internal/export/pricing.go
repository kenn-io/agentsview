package export

import (
	"sort"
	"strings"
	"time"

	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

type PricingRowSource string

const (
	PricingRowSourceCustom   PricingRowSource = "custom"
	PricingRowSourceFetched  PricingRowSource = "fetched"
	PricingRowSourceEmbedded PricingRowSource = "embedded"
)

type ModelRates struct {
	InputPerMTok      float64
	OutputPerMTok     float64
	CacheWritePerMTok float64
	CacheReadPerMTok  float64
	UpdatedAt         *time.Time
	Source            PricingRowSource
}

func (r ModelRates) CostForTokens(
	inputTokens, outputTokens, reasoningTokens, cacheWriteTokens, cacheReadTokens int,
) float64 {
	// reasoningTokens is a breakdown of outputTokens for current sources, not
	// additional billable output. Reasoning-only rows still bill at output rate.
	billableOutputTokens := outputTokens
	if billableOutputTokens == 0 {
		billableOutputTokens = reasoningTokens
	}
	return (float64(inputTokens)*r.InputPerMTok +
		float64(billableOutputTokens)*r.OutputPerMTok +
		float64(cacheWriteTokens)*r.CacheWritePerMTok +
		float64(cacheReadTokens)*r.CacheReadPerMTok) / 1_000_000
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
