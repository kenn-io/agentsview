package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/money"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

// chUsagePriceFormatVersion is part of the pricing digest. Bump it whenever
// chUsagePriceKeySQL, chPriceUsageInput, the context shape, or model
// canonicalization change. Records of the old format stay in place for
// binaries that still read them; the next push prices the mirror under the
// new digest. Provider billing policies carry their own version in the
// digest and need no bump here.
const chUsagePriceFormatVersion = 2

// Persist error identity with its diagnostic text. The format version in the
// digest keeps readers from decoding records written in the old string format.
type chUsagePriceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e chUsagePriceError) Error() string { return e.Message }

func (e chUsagePriceError) Unwrap() error {
	switch e.Code {
	case "overflow":
		return money.ErrOverflow
	case "negative":
		return money.ErrNegative
	case "invalid_decimal":
		return money.ErrInvalidDecimal
	default:
		return nil
	}
}

func encodeUsagePriceError(err error) (string, error) {
	record := chUsagePriceError{Message: err.Error()}
	switch {
	case errors.Is(err, money.ErrOverflow):
		record.Code = "overflow"
	case errors.Is(err, money.ErrNegative):
		record.Code = "negative"
	case errors.Is(err, money.ErrInvalidDecimal):
		record.Code = "invalid_decimal"
	}
	data, encodeErr := json.Marshal(record)
	return string(data), encodeErr
}

func decodeUsagePriceError(data string) error {
	var record chUsagePriceError
	if err := json.Unmarshal([]byte(data), &record); err != nil {
		return fmt.Errorf("decoding clickhouse usage price error: %w", err)
	}
	return record
}

// chUsagePriceKeySQL identifies one distinct set of pricing inputs over the
// usage_normalized columns. It covers every value chPriceUsageInput reads
// and nothing that varies with filters, windows, or deduplication: web
// search requests and reported costs are summed by the reader instead. Both
// push and the reader evaluate this one expression in ClickHouse, so a key
// is never computed in Go.
const chUsagePriceKeySQL = `hex(sipHash128(
				model, price_model, provider_id,
				ifNull(toUnixTimestamp64Micro(pricing_ts), toInt64(0)),
				pricing_ts IS NULL,
				source, message_ordinal IS NOT NULL,
				input_tokens_norm, output_tokens_norm, reasoning_tokens_norm,
				cache_create_norm, cache_create_1h_norm, cache_read_norm,
				cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported'
			))`

const (
	chUsagePriceKindReported  = "reported"
	chUsagePriceKindZero      = "zero"
	chUsagePriceKindRequest   = "request"
	chUsagePriceKindAggregate = "aggregate"
)

// chPricingCatalog is the mirrored catalog a request or push prices with.
// digest identifies the catalog alone, without reader custom rates, so
// every reader and exporter of one mirror agrees on it.
type chPricingCatalog struct {
	rows   []export.EffectivePricingRow
	digest string
}

func chLoadPricingCatalog(
	ctx context.Context, conn *sql.DB,
	customPricing map[string]config.CustomModelRate,
) (chPricingCatalog, error) {
	pricing, err := chLoadPricing(ctx, conn, nil)
	if err != nil {
		return chPricingCatalog{}, err
	}
	document, err := loadGenAIPricing(ctx, conn)
	if err != nil {
		return chPricingCatalog{}, err
	}
	genAI, err := genAIEffectivePricingRow(document)
	if err != nil {
		return chPricingCatalog{}, err
	}
	shared := append(chPricingRows(pricing), genAI)
	digest, err := chUsagePricingDigest(shared, document)
	if err != nil {
		return chPricingCatalog{}, err
	}
	if len(customPricing) == 0 {
		return chPricingCatalog{rows: shared, digest: digest}, nil
	}
	chApplyCustomPricing(pricing, customPricing)
	return chPricingCatalog{rows: append(chPricingRows(pricing), genAI), digest: digest}, nil
}

// chUsagePricingDigest changes when any rate, band, source classification,
// GenAI document content, or provider billing policy changes. Row timestamps
// are excluded: a catalog refresh that republishes the same rates must not
// reprice the mirror.
func chUsagePricingDigest(
	rows []export.EffectivePricingRow, document *db.GenAIPricingDocument,
) (string, error) {
	stripped := make([]export.EffectivePricingRow, len(rows))
	for i, row := range rows {
		row.GenAIUpdatedAt = nil
		row.Rates.UpdatedAt = nil
		row.Rates.Bands = append([]export.PricingBand(nil), row.Rates.Bands...)
		for j := range row.Rates.Bands {
			row.Rates.Bands[j].UpdatedAt = nil
		}
		stripped[i] = row
	}
	catalog, err := export.EffectivePricingDigest(stripped)
	if err != nil {
		return "", fmt.Errorf("digesting clickhouse usage pricing: %w", err)
	}
	h := sha256.New()
	h.Write([]byte("agentsview-clickhouse-usage-prices/"))
	h.Write([]byte(strconv.Itoa(chUsagePriceFormatVersion)))
	h.Write([]byte{0})
	h.Write([]byte(pricingpkg.BillingPolicyVersion()))
	h.Write([]byte{0})
	h.Write([]byte(catalog))
	h.Write([]byte{0})
	if document != nil {
		data := sha256.Sum256(document.Data)
		h.Write(data[:])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type chUsagePriceContextBand struct {
	AboveInputTokens int   `json:"above_input_tokens"`
	Input            int64 `json:"input"`
	Output           int64 `json:"output"`
	CacheWrite       int64 `json:"cache_write"`
	CacheWrite1h     int64 `json:"cache_write_1h"`
	CacheRead        int64 `json:"cache_read"`
}

// chUsagePriceContext is one pricing resolution: which reported model
// resolved to which priced model and rates. The reader replays it into the
// pricing block instead of resolving every event again.
type chUsagePriceContext struct {
	ReportedModel  string                    `json:"reported_model"`
	CanonicalModel string                    `json:"canonical_model"`
	PricedModel    string                    `json:"priced_model"`
	Pattern        string                    `json:"pattern"`
	OK             bool                      `json:"ok"`
	Adjustment     string                    `json:"adjustment"`
	Source         string                    `json:"source"`
	Input          int64                     `json:"input"`
	Output         int64                     `json:"output"`
	CacheWrite     int64                     `json:"cache_write"`
	CacheWrite1h   int64                     `json:"cache_write_1h"`
	CacheRead      int64                     `json:"cache_read"`
	Bands          []chUsagePriceContextBand `json:"bands"`
}

func newChUsagePriceContext(
	reportedModel, canonicalModel, pricedModel string,
	lookup export.PricingLookup,
) chUsagePriceContext {
	c := chUsagePriceContext{
		ReportedModel:  reportedModel,
		CanonicalModel: canonicalModel,
		PricedModel:    pricedModel,
		Pattern:        lookup.Pattern,
		OK:             lookup.OK,
		Adjustment:     lookup.Adjustment,
		Source:         string(lookup.Rates.Source),
		Input:          lookup.Rates.InputPerMTok.Microdollars,
		Output:         lookup.Rates.OutputPerMTok.Microdollars,
		CacheWrite:     lookup.Rates.CacheWritePerMTok.Microdollars,
		CacheWrite1h:   lookup.Rates.CacheWrite1hPerMTok.Microdollars,
		CacheRead:      lookup.Rates.CacheReadPerMTok.Microdollars,
		Bands:          make([]chUsagePriceContextBand, 0, len(lookup.Rates.Bands)),
	}
	for _, band := range lookup.Rates.Bands {
		c.Bands = append(c.Bands, chUsagePriceContextBand{
			AboveInputTokens: band.AboveInputTokens,
			Input:            band.InputPerMTok.Microdollars,
			Output:           band.OutputPerMTok.Microdollars,
			CacheWrite:       band.CacheWritePerMTok.Microdollars,
			CacheWrite1h:     band.CacheWrite1hPerMTok.Microdollars,
			CacheRead:        band.CacheReadPerMTok.Microdollars,
		})
	}
	return c
}

// encode returns the context's content-derived ID and its stored JSON.
func (c chUsagePriceContext) encode() (string, string, error) {
	data, err := json.Marshal(c)
	if err != nil {
		return "", "", fmt.Errorf("encoding clickhouse usage price context: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16]), string(data), nil
}

func (c chUsagePriceContext) lookup() export.PricingLookup {
	bands := make([]export.PricingBand, 0, len(c.Bands))
	for _, band := range c.Bands {
		bands = append(bands, export.PricingBand{
			AboveInputTokens:    band.AboveInputTokens,
			InputPerMTok:        money.Money{Microdollars: band.Input},
			OutputPerMTok:       money.Money{Microdollars: band.Output},
			CacheWritePerMTok:   money.Money{Microdollars: band.CacheWrite},
			CacheWrite1hPerMTok: money.Money{Microdollars: band.CacheWrite1h},
			CacheReadPerMTok:    money.Money{Microdollars: band.CacheRead},
		})
	}
	return export.PricingLookup{
		Rates: export.ModelRates{
			InputPerMTok:        money.Money{Microdollars: c.Input},
			OutputPerMTok:       money.Money{Microdollars: c.Output},
			CacheWritePerMTok:   money.Money{Microdollars: c.CacheWrite},
			CacheWrite1hPerMTok: money.Money{Microdollars: c.CacheWrite1h},
			CacheReadPerMTok:    money.Money{Microdollars: c.CacheRead},
			Source:              export.PricingRowSource(c.Source),
			Bands:               bands,
		},
		Pattern:    c.Pattern,
		OK:         c.OK,
		Adjustment: c.Adjustment,
	}
}

// record replays events priced under this context into the pricing block,
// one call per event so the application counts match per-event pricing.
func (c chUsagePriceContext) record(
	pricing *export.PricingResolver, kind string, bandAbove int64, events int,
) error {
	lookup := c.lookup()
	switch kind {
	case chUsagePriceKindReported:
		pricing.RecordResolvedReported(c.ReportedModel, c.PricedModel, lookup)
	case chUsagePriceKindZero:
		pricing.RecordResolvedComputed(c.ReportedModel, c.PricedModel, lookup)
	case chUsagePriceKindAggregate:
		for range events {
			pricing.RecordResolvedComputedAggregate(
				c.ReportedModel, c.PricedModel, lookup)
		}
	case chUsagePriceKindRequest:
		// One token above the threshold selects exactly that band; zero
		// tokens select the base rates.
		bandTokens := 0
		if bandAbove >= 0 {
			bandTokens = int(bandAbove) + 1
		}
		for range events {
			pricing.RecordResolvedComputedRequest(
				c.ReportedModel, c.PricedModel, lookup, bandTokens, 0, 0)
		}
	default:
		return fmt.Errorf("unknown clickhouse usage price kind %q", kind)
	}
	return nil
}

// customPriced reports whether the reader's own custom rates can apply to
// this context's model. It is deliberately broader than the resolver's
// ladder: a false positive only sends events through request-time pricing,
// while a false negative would serve a shared price the reader overrode.
func (c chUsagePriceContext) customPriced(pricing *export.PricingResolver) bool {
	priced := c.CanonicalModel
	if priced == "" {
		priced = c.ReportedModel
	}
	for _, model := range []string{
		c.ReportedModel, priced,
		pricingpkg.OllamaCloudBaseModel(c.ReportedModel),
		pricingpkg.OllamaCloudBaseModel(priced),
	} {
		if pricing.Lookup(model).Rates.Source == export.PricingRowSourceCustom {
			return true
		}
	}
	return false
}

// chUsagePriceInput is one distinct set of normalized pricing inputs, as
// selected from usage_normalized.
type chUsagePriceInput struct {
	priceKey     string
	model        string
	priceModel   string
	providerID   string
	pricingTS    string
	source       string
	hasOrdinal   bool
	inputTok     int
	outputTok    int
	reasoningTok int
	cacheCr      int
	cacheCr1h    int
	cacheRd      int
	reported     bool
}

type chUsagePriceRecord struct {
	tokenCost         money.Money
	savings           money.Money
	billedContextID   string
	unbilledContextID string
	requestScoped     bool
	bandAbove         int64
	priceError        string
}

// chPriceUsageInput prices one input exactly as request-time pricing would,
// minus the web search fee and reported cost the reader adds. contexts
// collects the JSON of every context the record refers to, by ID.
func chPriceUsageInput(
	in chUsagePriceInput, pricing *export.PricingResolver,
	contexts map[string]string,
) (chUsagePriceRecord, error) {
	ts := chUsagePricingTimestamp(in.pricingTS)
	rec := chUsagePriceRecord{
		requestScoped: db.UsageSourceIsRequestScoped(in.source) || in.hasOrdinal,
		bandAbove:     -1,
	}
	addContext := func(pricedModel string, lookup export.PricingLookup) (string, error) {
		id, data, err := newChUsagePriceContext(
			in.model, in.priceModel, pricedModel, lookup).encode()
		if err != nil {
			return "", err
		}
		contexts[id] = data
		return id, nil
	}
	pricedModel, unbilled := pricing.ResolveAt(in.model, in.priceModel, ts)
	var err error
	if rec.unbilledContextID, err = addContext(pricedModel, unbilled); err != nil {
		return rec, err
	}
	billedModel, billed, billedErr := pricing.ResolveBilledAt(
		in.providerID, in.model, in.priceModel, ts)
	if billedErr == nil {
		if rec.billedContextID, err = addContext(billedModel, billed); err != nil {
			return rec, err
		}
	}

	// Mirrors chUsageBillableSelect.
	var billableInput, billableOutput, billableCacheCr, billableCacheCr1h, billableCacheRd int
	if !in.reported {
		billableInput = in.inputTok
		billableOutput = in.outputTok
		if billableOutput == 0 {
			billableOutput = in.reasoningTok
		}
		billableCacheCr = in.cacheCr
		billableCacheCr1h = in.cacheCr1h
		billableCacheRd = in.cacheRd
	}
	cost, savings, _, _, priceErr := chUsageAggregateResolvedCost(
		in.model, in.priceModel, in.providerID, ts,
		in.inputTok, in.outputTok, in.cacheCr, in.cacheCr1h, in.cacheRd,
		billableInput, billableOutput, 0,
		billableCacheCr, billableCacheCr1h, billableCacheRd,
		0, 0, in.reported, rec.requestScoped, pricing,
	)
	switch {
	case priceErr != nil:
		// Reads selecting this event report the failure. Unrelated sessions
		// can still be exported.
		rec.priceError, err = encodeUsagePriceError(priceErr)
	case billedErr != nil && !in.reported:
		// This pass omits web searches, which the reader adds. Preserve the
		// billing failure even when no token was billed here.
		rec.priceError, err = encodeUsagePriceError(billedErr)
	default:
		rec.tokenCost, rec.savings = cost, savings
		if rec.requestScoped && !in.reported && billedErr == nil && billed.OK {
			total := int64(billableInput) + int64(billableCacheCr) + int64(billableCacheRd)
			for _, band := range billed.Rates.Bands {
				if total > int64(band.AboveInputTokens) &&
					int64(band.AboveInputTokens) > rec.bandAbove {
					rec.bandAbove = int64(band.AboveInputTokens)
				}
			}
		}
	}
	return rec, err
}

func decodeUsagePriceContext(data string) (chUsagePriceContext, error) {
	var c chUsagePriceContext
	err := json.Unmarshal([]byte(data), &c)
	return c, err
}

// loadUsagePriceContexts reads every context persisted under digest.
func loadUsagePriceContexts(
	ctx context.Context, conn *sql.DB, digest string,
) (map[string]chUsagePriceContext, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT context_id, context_json
		FROM usage_price_contexts
		WHERE pricing_digest = ?`, digest)
	if isMissingTableError(err) {
		// The reader may hold SELECT-only credentials, so only push upgrades
		// the schema.
		return nil, fmt.Errorf(
			"clickhouse mirror predates schema version %d; run "+
				"`agentsview clickhouse push` to upgrade it: %w", SchemaVersion, err)
	}
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse usage price contexts: %w", err)
	}
	defer rows.Close()
	out := map[string]chUsagePriceContext{}
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, fmt.Errorf("scanning clickhouse usage price context: %w", err)
		}
		c, err := decodeUsagePriceContext(data)
		if err != nil {
			return nil, fmt.Errorf("decoding clickhouse usage price context %s: %w", id, err)
		}
		out[id] = c
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse usage price contexts: %w", err)
	}
	return out, nil
}
