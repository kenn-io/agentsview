package export

import (
	"cmp"
	"context"
	"math/big"
	"slices"
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

// ModelRates prices one model. CacheWrite1hPerMTok bills the 1-hour-TTL
// subset of cache writes; zero means no separate 1h rate is published and
// 1h writes bill at CacheWritePerMTok.
type ModelRates struct {
	InputPerMTok        money.Money
	OutputPerMTok       money.Money
	CacheWritePerMTok   money.Money
	CacheWrite1hPerMTok money.Money
	CacheReadPerMTok    money.Money
	UpdatedAt           *time.Time
	Source              PricingRowSource
	Bands               []PricingBand
}

// EffectiveCacheWrite1hPerMTok returns the rate that 1h-TTL cache writes
// bill at: the published 1h rate, or the base write rate when none exists.
func (r ModelRates) EffectiveCacheWrite1hPerMTok() money.Money {
	if r.CacheWrite1hPerMTok.Microdollars != 0 {
		return r.CacheWrite1hPerMTok
	}
	return r.CacheWritePerMTok
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
		InputPerMTok:        band.InputPerMTok,
		OutputPerMTok:       band.OutputPerMTok,
		CacheWritePerMTok:   band.CacheWritePerMTok,
		CacheWrite1hPerMTok: band.CacheWrite1hPerMTok,
		CacheReadPerMTok:    band.CacheReadPerMTok,
		UpdatedAt:           updatedAt,
		Source:              r.Source,
		Bands:               r.Bands,
	}
}

func (r ModelRates) CostForTokens(
	inputTokens, outputTokens, reasoningTokens,
	cacheWriteTokens, cacheWrite1hTokens, cacheReadTokens int,
) (money.Money, error) {
	return r.CostForTokensScoped(
		true,
		inputTokens,
		outputTokens,
		reasoningTokens,
		cacheWriteTokens,
		cacheWrite1hTokens,
		cacheReadTokens,
	)
}

func (r ModelRates) CostForTokensScoped(
	requestScoped bool,
	inputTokens, outputTokens, reasoningTokens,
	cacheWriteTokens, cacheWrite1hTokens, cacheReadTokens int,
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
	// cacheWrite1hTokens is a TTL breakdown of cacheWriteTokens, never
	// additional billable volume.
	if cacheWrite1hTokens > cacheWriteTokens {
		cacheWrite1hTokens = cacheWriteTokens
	}
	return money.CostPerMillion([]money.RatedTokens{
		{Tokens: int64(inputTokens), Rate: r.InputPerMTok},
		{Tokens: int64(billableOutputTokens), Rate: r.OutputPerMTok},
		{Tokens: int64(cacheWriteTokens - cacheWrite1hTokens), Rate: r.CacheWritePerMTok},
		{Tokens: int64(cacheWrite1hTokens), Rate: r.EffectiveCacheWrite1hPerMTok()},
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
	ModelPattern   string
	Rates          ModelRates
	GenAI          *pricingpkg.GenAIPrices
	GenAIVersion   string
	GenAISource    PricingRowSource
	GenAIUpdatedAt *time.Time
}

type PricingLookup struct {
	Rates      ModelRates
	Pattern    string
	OK         bool
	Adjustment string
}

type PricingResolver struct {
	rows                 []EffectivePricingRow
	byModel              map[string]ModelRates
	lookupCache          map[string]PricingLookup
	genAI                *pricingpkg.GenAIPrices
	genAISource          PricingRowSource
	genAIUpdatedAt       *time.Time
	recordedBandKeys     []pricingBandKeyCacheEntry
	recorded             map[string]map[pricingRecordKey]*pricingRecord
	unattributedReported bool
}

type pricingBandKeyCacheEntry struct {
	bands []PricingBand
	key   string
}

type pricingRecord struct {
	lookup            PricingLookup
	computed          bool
	reported          bool
	baseRequestCount  int
	aggregateRowCount int
	bandRequestCounts map[int]int
}

type pricingRecordKey struct {
	pricedModel                         string
	pattern                             string
	ok                                  bool
	source                              PricingRowSource
	input, output                       int64
	cacheWrite, cacheWrite1h, cacheRead int64
	bands                               string
	adjustment                          string
}

func NewPricingResolver(rows []EffectivePricingRow) *PricingResolver {
	copied := make([]EffectivePricingRow, len(rows))
	byModel := make(map[string]ModelRates, len(rows))
	for i, row := range rows {
		row.Rates.Bands = append([]PricingBand(nil), row.Rates.Bands...)
		copied[i] = row
		if row.GenAI != nil {
			if copied[i].GenAIUpdatedAt != nil {
				updatedAt := copied[i].GenAIUpdatedAt.UTC()
				copied[i].GenAIUpdatedAt = &updatedAt
			}
			continue
		}
		if row.ModelPattern == "" {
			continue
		}
		byModel[row.ModelPattern] = row.Rates
	}
	resolver := &PricingResolver{
		rows:        copied,
		byModel:     byModel,
		lookupCache: make(map[string]PricingLookup),
		recorded:    make(map[string]map[pricingRecordKey]*pricingRecord),
	}
	for _, row := range copied {
		if row.GenAI == nil {
			continue
		}
		resolver.genAI = row.GenAI
		resolver.genAISource = row.GenAISource
		resolver.genAIUpdatedAt = row.GenAIUpdatedAt
		break
	}
	return resolver
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

// ResolveBilled resolves model pricing without a usage timestamp, then applies
// one exact provider adjustment.
func (r *PricingResolver) ResolveBilled(providerID, reportedModel, canonicalModel string) (string, PricingLookup, error) {
	return r.ResolveBilledAt(providerID, reportedModel, canonicalModel, time.Time{})
}

// ResolveBilledAt selects model, historical, band, and cache rates at
// timestamp before applying one exact provider adjustment.
func (r *PricingResolver) ResolveBilledAt(providerID, reportedModel, canonicalModel string, timestamp time.Time) (string, PricingLookup, error) {
	pricedModel, lookup := r.ResolveAt(reportedModel, canonicalModel, timestamp)
	policy, ok := pricingpkg.BillingPolicyFor(providerID)
	if !ok || !lookup.OK || lookup.Rates.Source == PricingRowSourceCustom {
		return pricedModel, lookup, nil
	}
	rates, err := scaleModelRates(lookup.Rates, policy.Numerator, policy.Denominator)
	if err != nil {
		return "", PricingLookup{}, err
	}
	lookup.Rates = rates
	lookup.Adjustment = policy.Service + "@" + policy.Version
	return pricedModel, lookup, nil
}

func scaleModelRates(r ModelRates, numerator, denominator int64) (ModelRates, error) {
	scale := func(v money.Money) (money.Money, error) { return money.ScaleRatio(v, numerator, denominator) }
	var err error
	if r.InputPerMTok, err = scale(r.InputPerMTok); err != nil {
		return ModelRates{}, err
	}
	if r.OutputPerMTok, err = scale(r.OutputPerMTok); err != nil {
		return ModelRates{}, err
	}
	if r.CacheWritePerMTok, err = scale(r.CacheWritePerMTok); err != nil {
		return ModelRates{}, err
	}
	if r.CacheWrite1hPerMTok, err = scale(r.CacheWrite1hPerMTok); err != nil {
		return ModelRates{}, err
	}
	if r.CacheReadPerMTok, err = scale(r.CacheReadPerMTok); err != nil {
		return ModelRates{}, err
	}
	r.Bands = append([]PricingBand(nil), r.Bands...)
	for i := range r.Bands {
		if r.Bands[i].InputPerMTok, err = scale(r.Bands[i].InputPerMTok); err != nil {
			return ModelRates{}, err
		}
		if r.Bands[i].OutputPerMTok, err = scale(r.Bands[i].OutputPerMTok); err != nil {
			return ModelRates{}, err
		}
		if r.Bands[i].CacheWritePerMTok, err = scale(r.Bands[i].CacheWritePerMTok); err != nil {
			return ModelRates{}, err
		}
		if r.Bands[i].CacheWrite1hPerMTok, err = scale(r.Bands[i].CacheWrite1hPerMTok); err != nil {
			return ModelRates{}, err
		}
		if r.Bands[i].CacheReadPerMTok, err = scale(r.Bands[i].CacheReadPerMTok); err != nil {
			return ModelRates{}, err
		}
	}
	return r, nil
}

// Resolve selects the effective priced model while preserving the model name
// reported by the source. An exact custom rate for the reported name takes
// precedence over caller-supplied canonicalization.
func (r *PricingResolver) Resolve(
	reportedModel, canonicalModel string,
) (string, PricingLookup) {
	return r.ResolveAt(reportedModel, canonicalModel, time.Time{})
}

// ResolveAt applies custom pricing matched from the reported or canonical
// model, then the timestamp-aware GenAI Prices document, then the existing
// LiteLLM/OpenRouter rows. An exact custom rate for the reported model still
// takes precedence over caller-supplied canonicalization.
func (r *PricingResolver) ResolveAt(
	reportedModel, canonicalModel string, timestamp time.Time,
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
	flatLookup := r.Lookup(pricedModel)
	if flatLookup.OK && flatLookup.Rates.Source == PricingRowSourceCustom {
		return pricedModel, flatLookup
	}
	if !timestamp.IsZero() {
		var genAIFallbackModel string
		if flatLookup.OK &&
			pricingpkg.EffortTierBaseModel(pricedModel) != pricedModel {
			genAIFallbackModel = flatLookup.Pattern
		}
		if pricedModel, rates, ok := r.resolveGenAI(
			reportedModel, canonicalModel, genAIFallbackModel, timestamp,
		); ok {
			return pricedModel, rates
		}
	}
	return pricedModel, flatLookup
}

func (r *PricingResolver) resolveGenAI(
	reportedModel, canonicalModel, fallbackModel string, timestamp time.Time,
) (string, PricingLookup, bool) {
	if r.genAI == nil {
		return "", PricingLookup{}, false
	}
	type modelAlias struct {
		lookup string
		priced string
	}
	type modelCandidate struct {
		provider string
		model    string
		priced   string
	}
	models := []modelAlias{{lookup: reportedModel, priced: reportedModel}}
	if canonicalModel != "" && canonicalModel != reportedModel {
		models = []modelAlias{
			{lookup: canonicalModel, priced: canonicalModel},
			{lookup: reportedModel, priced: reportedModel},
		}
	}
	pricedModel := canonicalModel
	if pricedModel == "" {
		pricedModel = reportedModel
	}
	baseModel := pricingpkg.EffortTierBaseModel(pricedModel)
	if baseModel != pricedModel && baseModel != reportedModel &&
		baseModel != canonicalModel {
		models = append(models, modelAlias{
			lookup: baseModel,
			priced: pricedModel,
		})
	}
	if fallbackModel != "" && fallbackModel != reportedModel &&
		fallbackModel != canonicalModel && fallbackModel != baseModel {
		models = append(models, modelAlias{
			lookup: fallbackModel,
			priced: pricedModel,
		})
	}
	for _, model := range models {
		provider, unqualified := genAIProviderAndModel(model.lookup)
		candidates := []modelCandidate{{provider, unqualified, model.priced}}
		if provider != "" || unqualified != model.lookup {
			candidates = append(candidates, modelCandidate{
				model:  model.lookup,
				priced: model.priced,
			})
		}
		for _, candidate := range candidates {
			if candidate.model == "" {
				continue
			}
			resolved, ok := r.genAI.Resolve(
				candidate.provider, candidate.model, timestamp,
			)
			if !ok {
				continue
			}
			return candidate.priced, PricingLookup{
				Rates: ModelRates{
					InputPerMTok:        resolved.InputPerMTok,
					OutputPerMTok:       resolved.OutputPerMTok,
					CacheWritePerMTok:   resolved.CacheCreationPerMTok,
					CacheWrite1hPerMTok: resolved.CacheCreation1hPerMTok,
					CacheReadPerMTok:    resolved.CacheReadPerMTok,
					UpdatedAt:           r.genAIUpdatedAt,
					Source:              r.genAISource,
					Bands:               genAIPricingBands(resolved.Bands),
				},
				Pattern: resolved.ModelPattern,
				OK:      true,
			}, true
		}
	}
	return "", PricingLookup{}, false
}

func genAIProviderAndModel(model string) (string, string) {
	provider, unqualified, ok := strings.Cut(model, "/")
	if !ok {
		return "", model
	}
	return provider, unqualified
}

func genAIPricingBands(bands []pricingpkg.PricingBand) []PricingBand {
	out := make([]PricingBand, len(bands))
	for i, band := range bands {
		out[i] = PricingBand{
			AboveInputTokens:    band.AboveInputTokens,
			InputPerMTok:        band.InputPerMTok,
			OutputPerMTok:       band.OutputPerMTok,
			CacheWritePerMTok:   band.CacheCreationPerMTok,
			CacheWrite1hPerMTok: band.CacheCreation1hPerMTok,
			CacheReadPerMTok:    band.CacheReadPerMTok,
		}
	}
	return out
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
		byPricedModel = make(map[pricingRecordKey]*pricingRecord)
		r.recorded[reportedModel] = byPricedModel
	}
	key := pricingRecordKey{
		pricedModel:  pricedModel,
		pattern:      lookup.Pattern,
		ok:           lookup.OK,
		source:       lookup.Rates.Source,
		input:        lookup.Rates.InputPerMTok.Microdollars,
		output:       lookup.Rates.OutputPerMTok.Microdollars,
		cacheWrite:   lookup.Rates.CacheWritePerMTok.Microdollars,
		cacheWrite1h: lookup.Rates.CacheWrite1hPerMTok.Microdollars,
		cacheRead:    lookup.Rates.CacheReadPerMTok.Microdollars,
		bands:        r.recordedBandsKey(lookup.Rates.Bands),
		adjustment:   lookup.Adjustment,
	}
	rec := byPricedModel[key]
	if rec == nil {
		rec = &pricingRecord{lookup: clonePricingLookup(lookup)}
		byPricedModel[key] = rec
	} else if !pricingLookupEqual(rec.lookup, lookup) {
		// Preserve the existing last-observation behavior when two lookups
		// share a canonical record key but differ in non-key representation.
		rec.lookup = clonePricingLookup(lookup)
	}
	return rec
}

func (r *PricingResolver) recordedBandsKey(bands []PricingBand) string {
	if len(bands) == 0 {
		return ""
	}
	for _, cached := range r.recordedBandKeys {
		if pricingBandsCanonicalEqual(cached.bands, bands) {
			return cached.key
		}
	}
	key := canonicalPricingBandsSortKey(bands)
	r.recordedBandKeys = append(r.recordedBandKeys, pricingBandKeyCacheEntry{
		bands: append([]PricingBand(nil), bands...),
		key:   key,
	})
	return key
}

func pricingBandsCanonicalEqual(a, b []PricingBand) bool {
	if len(a) != len(b) {
		return false
	}
	for position := range a {
		if !pricingBandEqual(
			canonicalPricingBandAt(a, position),
			canonicalPricingBandAt(b, position),
		) {
			return false
		}
	}
	return true
}

// canonicalPricingBandsSortKey sorts bands by threshold with a stable sort.
// Select the band at the same logical position without allocating a copy.
func canonicalPricingBandAt(bands []PricingBand, position int) PricingBand {
	for i, band := range bands {
		rank := 0
		for j, other := range bands {
			if other.AboveInputTokens < band.AboveInputTokens ||
				(other.AboveInputTokens == band.AboveInputTokens && j < i) {
				rank++
			}
		}
		if rank == position {
			return band
		}
	}
	return PricingBand{}
}

func pricingBandEqual(a, b PricingBand) bool {
	if a.AboveInputTokens != b.AboveInputTokens ||
		a.InputPerMTok != b.InputPerMTok ||
		a.OutputPerMTok != b.OutputPerMTok ||
		a.CacheWritePerMTok != b.CacheWritePerMTok ||
		a.CacheWrite1hPerMTok != b.CacheWrite1hPerMTok ||
		a.CacheReadPerMTok != b.CacheReadPerMTok {
		return false
	}
	return pricingTimeEqual(a.UpdatedAt, b.UpdatedAt)
}

func pricingLookupEqual(a, b PricingLookup) bool {
	if a.Pattern != b.Pattern || a.OK != b.OK || a.Adjustment != b.Adjustment ||
		a.Rates.InputPerMTok != b.Rates.InputPerMTok ||
		a.Rates.OutputPerMTok != b.Rates.OutputPerMTok ||
		a.Rates.CacheWritePerMTok != b.Rates.CacheWritePerMTok ||
		a.Rates.CacheWrite1hPerMTok != b.Rates.CacheWrite1hPerMTok ||
		a.Rates.CacheReadPerMTok != b.Rates.CacheReadPerMTok ||
		a.Rates.Source != b.Rates.Source ||
		!pricingTimeEqual(a.Rates.UpdatedAt, b.Rates.UpdatedAt) ||
		len(a.Rates.Bands) != len(b.Rates.Bands) {
		return false
	}
	for i := range a.Rates.Bands {
		if !pricingBandEqual(a.Rates.Bands[i], b.Rates.Bands[i]) {
			return false
		}
	}
	return true
}

func pricingTimeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
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
		resolutions := make([]pricingRecordKey, 0, len(byPricedModel))
		for key := range byPricedModel {
			resolutions = append(resolutions, key)
		}
		slices.SortFunc(resolutions, comparePricingRecordKey)

		provenance := ModelPricingProvenance{
			Resolutions: make([]EffectiveModelRate, 0, len(resolutions)),
		}
		var modelComputed, modelReported bool
		for _, key := range resolutions {
			rec := byPricedModel[key]
			if rec == nil {
				continue
			}
			source := recordCostSource(rec)
			modelComputed = modelComputed || rec.computed
			modelReported = modelReported || rec.reported
			rate := EffectiveModelRate{
				PricedModel:             key.pricedModel,
				InputCostPerMTok:        rec.lookup.Rates.InputPerMTok,
				OutputCostPerMTok:       rec.lookup.Rates.OutputPerMTok,
				CacheWriteCostPerMTok:   rec.lookup.Rates.CacheWritePerMTok,
				CacheWrite1hCostPerMTok: rec.lookup.Rates.CacheWrite1hPerMTok,
				CacheReadCostPerMTok:    rec.lookup.Rates.CacheReadPerMTok,
				CostSource:              source,
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

func comparePricingRecordKey(a, b pricingRecordKey) int {
	for _, comparison := range []int{
		strings.Compare(a.pricedModel, b.pricedModel),
		strings.Compare(a.adjustment, b.adjustment),
		strings.Compare(a.pattern, b.pattern),
		strings.Compare(string(a.source), string(b.source)),
		cmp.Compare(a.input, b.input),
		cmp.Compare(a.output, b.output),
		cmp.Compare(a.cacheWrite, b.cacheWrite),
		cmp.Compare(a.cacheWrite1h, b.cacheWrite1h),
		cmp.Compare(a.cacheRead, b.cacheRead),
		strings.Compare(a.bands, b.bands),
	} {
		if comparison != 0 {
			return comparison
		}
	}
	if a.ok != b.ok {
		if !a.ok {
			return -1
		}
		return 1
	}
	return 0
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
		source := row.Rates.Source
		if row.GenAI != nil {
			source = row.GenAISource
		}
		switch source {
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
		if row.GenAIUpdatedAt != nil {
			t := row.GenAIUpdatedAt.UTC()
			if latest == nil || t.After(*latest) {
				latest = &t
			}
		}
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
