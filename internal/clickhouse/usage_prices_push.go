package clickhouse

import (
	"context"
	"fmt"
	"sort"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// usagePriceInsertBatch bounds retained pricing inputs and one insert block.
const usagePriceInsertBatch = 20000

// usagePricedDigestKey records the pricing digest the whole mirror was last
// priced under. It is mirror-wide, not per archive: a pricing pass covers
// every exporter's rows.
const usagePricedDigestKey = "usage_prices_digest"

// usagePriceScope selects which mirrored usage rows a pricing pass reads.
// The zero value covers the whole mirror, whichever archive pushed it,
// including the Cursor admin usage rows.
type usagePriceScope struct {
	sessionIDs []string
	cursorOnly bool
}

// usagePricer prices with one snapshot of the mirrored catalog.
type usagePricer struct {
	digest   string
	resolver *export.PricingResolver
}

func (s *Sync) newUsagePricer(ctx context.Context) (*usagePricer, error) {
	catalog, err := chLoadPricingCatalog(ctx, s.conn, nil)
	if err != nil {
		return nil, err
	}
	return &usagePricer{
		digest:   catalog.digest,
		resolver: export.NewPricingResolver(catalog.rows),
	}, nil
}

const chUsagePricingMessageEligibility = `
			m.token_usage != ''
			AND m.model != ''
			AND m.model != '<synthetic>'`

// usagePricingRawSQL renders the raw usage rows of a scope. It reads what
// the mirror holds, not the local archive, so a price is always derived
// from the same normalized values the reader joins on. Session state is
// ignored: no pricing input comes from the session row, and a batch is
// priced before its session rows land.
func usagePricingRawSQL(scope usagePriceScope) (string, []any) {
	cursorSQL, cursorArgs, _ := chCursorUsageRowsSQLForBounds(
		db.UsageFilter{}, chUsageBounds{})
	if scope.cursorOnly {
		return cursorSQL, cursorArgs
	}
	messageWhere := chUsagePricingMessageEligibility
	eventWhere := chUsageEventSourceEligibility
	var messageArgs, eventArgs []any
	if len(scope.sessionIDs) > 0 {
		placeholders, args := inArgs(scope.sessionIDs)
		messageWhere += "\n\t\t\tAND m.session_id IN (" + placeholders + ")"
		eventWhere += "\n\t\t\tAND ue.session_id IN (" + placeholders + ")"
		messageArgs, eventArgs = args, args
	}
	rawSQL, args := chUsageRawSQLFromWheres(
		"LEFT JOIN", messageWhere, messageArgs, eventWhere, eventArgs)
	if len(scope.sessionIDs) > 0 {
		return rawSQL, args
	}
	return rawSQL + "\n\t\tUNION ALL\n" + cursorSQL, append(args, cursorArgs...)
}

// forEachUnpricedUsageBatch streams distinct missing pricing inputs in bounded
// batches. The callback must finish consuming a batch before returning.
func (s *Sync) forEachUnpricedUsageBatch(
	ctx context.Context, scope usagePriceScope, digest string,
	visit func([]chUsagePriceInput) error,
) error {
	rawSQL, args := usagePricingRawSQL(scope)
	cte, args := chUsageCTEFromRaw(
		db.UsageFilter{Timezone: "UTC"}, rawSQL, args, false)
	rows, err := s.conn.QueryContext(ctx, cte+`
		SELECT price_key, any(model), any(price_model), any(provider_id),
			any(pricing_ts), any(source), any(message_ordinal IS NOT NULL),
			any(input_tokens_norm), any(output_tokens_norm),
			any(reasoning_tokens_norm), any(cache_create_norm),
			any(cache_create_1h_norm), any(cache_read_norm),
			any(cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported')
		FROM usage_normalized
		WHERE price_key NOT IN (
			SELECT price_key FROM usage_event_prices WHERE pricing_digest = ?
		)
		GROUP BY price_key
		ORDER BY price_key`, append(args, digest)...)
	if err != nil {
		return fmt.Errorf("querying unpriced clickhouse usage: %w", err)
	}
	defer rows.Close()
	batch := make([]chUsagePriceInput, 0, usagePriceInsertBatch)
	for rows.Next() {
		var in chUsagePriceInput
		var pricingTS any
		if err := rows.Scan(
			&in.priceKey, &in.model, &in.priceModel, &in.providerID,
			&pricingTS, &in.source, &in.hasOrdinal,
			&in.inputTok, &in.outputTok, &in.reasoningTok, &in.cacheCr,
			&in.cacheCr1h, &in.cacheRd, &in.reported,
		); err != nil {
			return fmt.Errorf("scanning unpriced clickhouse usage: %w", err)
		}
		in.pricingTS = formatDBTime(pricingTS)
		batch = append(batch, in)
		if len(batch) == usagePriceInsertBatch {
			if err := visit(batch); err != nil {
				return err
			}
			clear(batch)
			batch = batch[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating unpriced clickhouse usage: %w", err)
	}
	if len(batch) > 0 {
		return visit(batch)
	}
	return nil
}

// priceUsage writes a price row for every unpriced input in scope. Contexts
// land before the prices that refer to them, so a reader never joins a price
// whose context is absent.
func (s *Sync) priceUsage(
	ctx context.Context, pricer *usagePricer, scope usagePriceScope,
) error {
	return s.forEachUnpricedUsageBatch(ctx, scope, pricer.digest, func(inputs []chUsagePriceInput) error {
		return s.insertUsagePrices(ctx, pricer, inputs)
	})
}

func (s *Sync) insertUsagePrices(
	ctx context.Context, pricer *usagePricer, inputs []chUsagePriceInput,
) error {
	version := newPushVersion()
	contexts := map[string]string{}
	priceRows := make([][]any, 0, len(inputs))
	for _, in := range inputs {
		rec, err := chPriceUsageInput(in, pricer.resolver, contexts)
		if err != nil {
			return err
		}
		priceRows = append(priceRows, []any{
			pricer.digest, in.priceKey,
			rec.tokenCost.Microdollars, rec.savings.Microdollars,
			rec.billedContextID, rec.unbilledContextID,
			rec.requestScoped, rec.bandAbove, rec.priceError,
			int64(1), version,
		})
	}
	contextIDs := make([]string, 0, len(contexts))
	for id := range contexts {
		contextIDs = append(contextIDs, id)
	}
	sort.Strings(contextIDs)
	contextRows := make([][]any, 0, len(contextIDs))
	for _, id := range contextIDs {
		contextRows = append(contextRows, []any{
			pricer.digest, id, contexts[id], version,
		})
	}
	if err := insertRows(ctx, s.conn, "usage_price_contexts", contextRows); err != nil {
		return err
	}
	return insertRows(ctx, s.conn, "usage_event_prices", priceRows)
}

// syncUsagePrices returns the pricer for this push's session batches after
// pricing what earlier pushes left unpriced under the current catalog. It
// runs after the catalog and Cursor syncs and before any session batch.
//
// The whole mirror is priced when the catalog digest changed, on a full
// push, and on a mirror no pass has covered yet, which is also the backfill
// for rows written before price records existed. Otherwise session rows were
// priced by the batch that wrote them, and only the Cursor rows need a pass.
// Rows this misses, such as those of an exporter that writes no prices, stay
// correct: the reader prices them per request until the next whole pass.
//
// Records of other digests are kept. Another exporter or an in-flight reader
// may still be using them, and a content-keyed record is never wrong for its
// own digest.
func (s *Sync) syncUsagePrices(ctx context.Context, full bool) (*usagePricer, error) {
	pricer, err := s.newUsagePricer(ctx)
	if err != nil {
		return nil, err
	}
	meta, err := readMetadata(ctx, s.conn, usagePricedDigestKey)
	if err != nil {
		return nil, err
	}
	whole := full || meta[usagePricedDigestKey] != pricer.digest
	if err := s.priceUsage(ctx, pricer, usagePriceScope{cursorOnly: !whole}); err != nil {
		return nil, err
	}
	if whole {
		if err := writeMetadata(ctx, s.conn, map[string]string{
			usagePricedDigestKey: pricer.digest,
		}); err != nil {
			return nil, err
		}
	}
	return pricer, nil
}
