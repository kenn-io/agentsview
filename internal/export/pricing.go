package export

import (
	"context"
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
	Bands             []PricingBand
}

func (r ModelRates) RatesForTokens(
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) ModelRates {
	band, ok := r.pricingBandForTokens(inputTokens, cacheWriteTokens, cacheReadTokens)
	if !ok {
		return r
	}
	updatedAt := r.UpdatedAt
	if band.UpdatedAt != nil {
		updatedAt = band.UpdatedAt
	}
	return ModelRates{
		InputPerMTok:      band.InputPerMTok,
		OutputPerMTok:     band.OutputPerMTok,
		CacheWritePerMTok: band.CacheWritePerMTok,
		CacheReadPerMTok:  band.CacheReadPerMTok,
		UpdatedAt:         updatedAt,
		Source:            r.Source,
		Bands:             r.Bands,
	}
}

func (r ModelRates) CostForTokens(
	inputTokens, outputTokens, reasoningTokens, cacheWriteTokens, cacheReadTokens int,
) (money.Money, error) {
	return r.CostForTokensScoped(
		true,
		inputTokens,
		outputTokens,
		reasoningTokens,
		cacheWriteTokens,
		cacheReadTokens,
	)
}

func (r ModelRates) CostForTokensScoped(
	requestScoped bool,
	inputTokens, outputTokens, reasoningTokens, cacheWriteTokens, cacheReadTokens int,
) (money.Money, error) {
	if requestScoped {
		r = r.RatesForTokens(inputTokens, cacheWriteTokens, cacheReadTokens)
	}
	// reasoningTokens is a breakdown of outputTokens for current sources, not
	// additional billable output. Reasoning-only rows still bill at output rate.
	billableOutputTokens := outputTokens
	if billableOutputTokens == 0 {
		billableOutputTokens = reasoningTokens
	}
	return money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(inputTokens), Rate: r.InputPerMTok},
		{Tokens: int64(billableOutputTokens), Rate: r.OutputPerMTok},
		{Tokens: int64(cacheWriteTokens), Rate: r.CacheWritePerMTok},
		{Tokens: int64(cacheReadTokens), Rate: r.CacheReadPerMTok},
	})
}

func (r ModelRates) pricingBandForTokens(
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) (PricingBand, bool) {
	totalInput := int64(inputTokens) + int64(cacheWriteTokens) + int64(cacheReadTokens)
	var selected PricingBand
	var ok bool
	for _, band := range r.Bands {
		if totalInput > int64(band.AboveInputTokens) &&
			(!ok || band.AboveInputTokens > selected.AboveInputTokens) {
			selected = band
			ok = true
		}
	}
	return selected, ok
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
	recorded             map[string]map[string]*pricingRecord
	unattributedReported bool
}

type pricingRecord struct {
	lookup            PricingLookup
	computed          bool
	reported          bool
	baseRequestCount  int
	aggregateRowCount int
	bandRequestCounts map[int]int
}

func NewPricingResolver(rows []EffectivePricingRow) *PricingResolver {
	copied := make([]EffectivePricingRow, len(rows))
	byModel := make(map[string]ModelRates, len(rows))
	for i, row := range rows {
		row.Rates.Bands = append([]PricingBand(nil), row.Rates.Bands...)
		copied[i] = row
		if row.ModelPattern == "" {
			continue
		}
		byModel[row.ModelPattern] = row.Rates
	}
	return &PricingResolver{
		rows:        copied,
		byModel:     byModel,
		lookupCache: make(map[string]PricingLookup),
		recorded:    make(map[string]map[string]*pricingRecord),
	}
}

func (r *PricingResolver) Lookup(model string) PricingLookup {
	if r == nil {
		return PricingLookup{}
	}
	if lookup, ok := r.lookupCache[model]; ok {
		return clonePricingLookup(lookup)
	}
	match := pricingpkg.ResolveMatch(model, r.byModel)
	lookup := PricingLookup{
		Rates:   match.Value,
		Pattern: match.Pattern,
		OK:      match.OK,
	}
	r.lookupCache[model] = clonePricingLookup(lookup)
	return clonePricingLookup(lookup)
}

func clonePricingLookup(lookup PricingLookup) PricingLookup {
	lookup.Rates.Bands = append([]PricingBand(nil), lookup.Rates.Bands...)
	return lookup
}

// Resolve selects the effective priced model while preserving the model name
// reported by the source. An exact custom rate for the reported name takes
// precedence over caller-supplied canonicalization.
func (r *PricingResolver) Resolve(
	reportedModel, canonicalModel string,
) (string, PricingLookup) {
	if r == nil {
		return reportedModel, PricingLookup{}
	}
	if rates, ok := r.byModel[reportedModel]; ok &&
		rates.Source == PricingRowSourceCustom {
		return reportedModel, PricingLookup{
			Rates:   rates,
			Pattern: reportedModel,
			OK:      true,
		}
	}
	pricedModel := canonicalModel
	if pricedModel == "" {
		pricedModel = reportedModel
	}
	return pricedModel, r.Lookup(pricedModel)
}

func (r *PricingResolver) RecordComputed(model string, lookup PricingLookup) {
	r.RecordResolvedComputed(model, model, lookup)
}

func (r *PricingResolver) RecordComputedRequest(
	model string,
	lookup PricingLookup,
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) {
	r.RecordResolvedComputedRequest(
		model,
		model,
		lookup,
		inputTokens,
		cacheWriteTokens,
		cacheReadTokens,
	)
}

func (r *PricingResolver) RecordResolvedComputedRequest(
	reportedModel, pricedModel string,
	lookup PricingLookup,
	inputTokens, cacheWriteTokens, cacheReadTokens int,
) {
	if r == nil || reportedModel == "" || pricedModel == "" {
		return
	}
	rec := r.record(reportedModel, pricedModel, lookup)
	rec.computed = true
	if !lookup.OK {
		return
	}
	band, ok := lookup.Rates.pricingBandForTokens(
		inputTokens,
		cacheWriteTokens,
		cacheReadTokens,
	)
	if !ok {
		rec.baseRequestCount++
		return
	}
	if rec.bandRequestCounts == nil {
		rec.bandRequestCounts = make(map[int]int)
	}
	rec.bandRequestCounts[band.AboveInputTokens]++
}

func (r *PricingResolver) RecordComputedAggregate(model string, lookup PricingLookup) {
	r.RecordResolvedComputedAggregate(model, model, lookup)
}

func (r *PricingResolver) RecordResolvedComputedAggregate(
	reportedModel, pricedModel string, lookup PricingLookup,
) {
	if r == nil || reportedModel == "" || pricedModel == "" {
		return
	}
	rec := r.record(reportedModel, pricedModel, lookup)
	rec.computed = true
	if !lookup.OK {
		return
	}
	rec.aggregateRowCount++
}

func (r *PricingResolver) RecordReported(model string, lookup PricingLookup) {
	r.RecordResolvedReported(model, model, lookup)
}

func (r *PricingResolver) RecordResolvedComputed(
	reportedModel, pricedModel string, lookup PricingLookup,
) {
	if r == nil || reportedModel == "" || pricedModel == "" {
		return
	}
	rec := r.record(reportedModel, pricedModel, lookup)
	rec.computed = true
}

func (r *PricingResolver) RecordResolvedReported(
	reportedModel, pricedModel string, lookup PricingLookup,
) {
	if r == nil || reportedModel == "" || pricedModel == "" {
		return
	}
	rec := r.record(reportedModel, pricedModel, lookup)
	rec.reported = true
}

// RecordUnattributedReported records an authoritative aggregate cost that
// cannot be assigned to a model without inventing an allocation.
func (r *PricingResolver) RecordUnattributedReported() {
	if r != nil {
		r.unattributedReported = true
	}
}

func (r *PricingResolver) record(
	reportedModel, pricedModel string, lookup PricingLookup,
) *pricingRecord {
	byPricedModel := r.recorded[reportedModel]
	if byPricedModel == nil {
		byPricedModel = make(map[string]*pricingRecord)
		r.recorded[reportedModel] = byPricedModel
	}
	rec := byPricedModel[pricedModel]
	if rec == nil {
		rec = &pricingRecord{}
		byPricedModel[pricedModel] = rec
	}
	rec.lookup = clonePricingLookup(lookup)
	return rec
}

func (r *PricingResolver) BuildBlock() (PricingBlock, error) {
	if r == nil {
		return PricingBlock{}, nil
	}
	models := make(map[string]ModelPricingProvenance, len(r.recorded))
	fallbackSet := make(map[string]struct{})
	var hasComputed bool
	hasReported := r.unattributedReported
	modelNames := make([]string, 0, len(r.recorded))
	for model := range r.recorded {
		modelNames = append(modelNames, model)
	}
	sort.Strings(modelNames)
	for _, reportedModel := range modelNames {
		byPricedModel := r.recorded[reportedModel]
		if len(byPricedModel) == 0 {
			continue
		}
		pricedModels := make([]string, 0, len(byPricedModel))
		for pricedModel := range byPricedModel {
			pricedModels = append(pricedModels, pricedModel)
		}
		sort.Strings(pricedModels)

		provenance := ModelPricingProvenance{
			Resolutions: make([]EffectiveModelRate, 0, len(pricedModels)),
		}
		var modelComputed, modelReported bool
		for _, pricedModel := range pricedModels {
			rec := byPricedModel[pricedModel]
			if rec == nil {
				continue
			}
			source := recordCostSource(rec)
			modelComputed = modelComputed || rec.computed
			modelReported = modelReported || rec.reported
			rate := EffectiveModelRate{
				PricedModel:           pricedModel,
				InputCostPerMTok:      rec.lookup.Rates.InputPerMTok,
				OutputCostPerMTok:     rec.lookup.Rates.OutputPerMTok,
				CacheWriteCostPerMTok: rec.lookup.Rates.CacheWritePerMTok,
				CacheReadCostPerMTok:  rec.lookup.Rates.CacheReadPerMTok,
				CostSource:            source,
				Bands: append(
					[]PricingBand(nil), rec.lookup.Rates.Bands...),
				Application: pricingApplicationForRecord(rec),
			}
			if rec.lookup.OK {
				pattern := rec.lookup.Pattern
				rate.MatchedPattern = &pattern
				if rec.lookup.Rates.Source == PricingRowSourceEmbedded {
					fallbackSet[reportedModel] = struct{}{}
				}
			}
			provenance.Resolutions = append(provenance.Resolutions, rate)
		}
		provenance.CostSource = CombinedCostSource(
			modelComputed, modelReported)
		hasComputed = hasComputed || modelComputed
		hasReported = hasReported || modelReported
		models[reportedModel] = provenance
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

func pricingApplicationForRecord(rec *pricingRecord) PricingApplication {
	application := PricingApplication{
		BaseRequestCount:  rec.baseRequestCount,
		AggregateRowCount: rec.aggregateRowCount,
	}
	thresholds := make([]int, 0, len(rec.bandRequestCounts))
	for threshold, count := range rec.bandRequestCounts {
		if count > 0 {
			thresholds = append(thresholds, threshold)
		}
	}
	sort.Ints(thresholds)
	for _, threshold := range thresholds {
		application.Bands = append(application.Bands, AppliedPricingBand{
			AboveInputTokens: threshold,
			RequestCount:     rec.bandRequestCounts[threshold],
		})
	}
	return application
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
	allocated, err := AllocateCostByWeightContext(
		context.Background(), total, weights,
	)
	if err != nil {
		panic(err)
	}
	return allocated
}

// AllocateCostByWeightContext is AllocateCostByWeight with cancellation for
// bounded in-memory aggregation paths.
func AllocateCostByWeightContext(
	ctx context.Context,
	total money.Money,
	weights []money.Money,
) ([]money.Money, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	allocated := make([]money.Money, len(weights))
	if len(weights) == 0 || total.Microdollars == 0 {
		return allocated, nil
	}

	weightTotal := new(big.Int)
	remainderIndex := -1
	equalWeights := false
	for i, weight := range weights {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
		if err := ctx.Err(); err != nil {
			return nil, err
		}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	remainder := new(big.Int).Sub(totalInt, assigned)
	if !remainder.IsInt64() {
		panic(money.ErrOverflow)
	}
	allocated[remainderIndex] = money.Money{Microdollars: remainder.Int64()}
	return allocated, nil
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
		if row.Rates.UpdatedAt != nil {
			t := row.Rates.UpdatedAt.UTC()
			if latest == nil || t.After(*latest) {
				latest = &t
			}
		}
		for _, band := range row.Rates.Bands {
			if band.UpdatedAt == nil {
				continue
			}
			t := band.UpdatedAt.UTC()
			if latest == nil || t.After(*latest) {
				latest = &t
			}
		}
	}
	return latest
}
