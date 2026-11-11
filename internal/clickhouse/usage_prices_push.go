package clickhouse

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

// usagePriceInsertBatch bounds retained pricing inputs and one insert block.
const usagePriceInsertBatch = 20000

// usagePricedDigestKey records the pricing digest the whole mirror was last
// priced under. It is mirror-wide, not per archive: a pricing pass covers
// every exporter's rows.
const usagePricedDigestKey = "usage_prices_digest"

// usagePricedCursorKey records what the last pricing pass over the Cursor
// rows covered, as cursorPricingParts.mark renders it. It is mirror-wide,
// like the Cursor rows themselves.
const usagePricedCursorKey = "usage_prices_cursor"

// usagePriceScope selects which mirrored usage rows a pricing pass reads.
// The zero value covers the whole mirror, whichever archive pushed it,
// including the Cursor admin usage rows.
type usagePriceScope struct {
	sessionIDs []string
	cursorOnly bool
}

// usagePriceRetention is how long a push keeps the records of a pricing
// digest that is neither current nor the one prepared_usage was built
// from, counted from the last record written under it. Exporters whose
// catalogs disagree on a priced model alternate between digests, and an
// exporter on an older build prices under its own; each keeps reusing its
// records instead of pricing the whole mirror again on every switch.
const usagePriceRetention = 24 * time.Hour

// usagePricer prices with one snapshot of the mirrored catalog. digest
// covers exactly the models in models. pending lists the models this
// push's session batches found and registered as pending; the push adopts
// them when its batches are done.
type usagePricer struct {
	digest   string
	resolver *export.PricingResolver
	models   []chUsagePriceModel
	pending  []chUsagePriceModel
}

func (s *Sync) newUsagePricer(ctx context.Context) (*usagePricer, error) {
	catalog, err := chLoadPricingCatalog(ctx, s.conn, nil)
	if err != nil {
		return nil, err
	}
	return &usagePricer{
		digest:   catalog.digest,
		resolver: export.NewPricingResolver(catalog.rows),
		models:   catalog.models,
	}, nil
}

// uncovered returns the models in candidates that p's digest does not
// cover, without repeats.
func (p *usagePricer) uncovered(candidates []chUsagePriceModel) []chUsagePriceModel {
	var out []chUsagePriceModel
	for _, m := range candidates {
		if !slices.Contains(p.models, m) && !slices.Contains(out, m) {
			out = append(out, m)
		}
	}
	return out
}

// withModels returns a pricer over the same catalog whose digest also
// covers added.
func (p *usagePricer) withModels(added []chUsagePriceModel) (*usagePricer, error) {
	models := slices.Concat(p.models, added)
	digest, err := chUsagePricingDigest(p.resolver, models)
	if err != nil {
		return nil, err
	}
	return &usagePricer{digest: digest, resolver: p.resolver, models: models}, nil
}

var chUsagePricingMessageEligibility = `
			` + chStoredMessageUsable("m.usage_present", "m.model")

// usagePricingRawSQL renders the raw usage rows of a scope. It reads what
// the mirror holds, not the local archive, so a price is always derived
// from the same normalized values the reader joins on. It reads no session
// rows: no pricing input comes from the session row, and a batch is priced
// before its session rows land. Message usage comes from usage_messages;
// see chUsageRawSQLFromWheres.
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
		true, messageWhere, messageArgs, eventWhere, eventArgs)
	if len(scope.sessionIDs) > 0 {
		return rawSQL, args
	}
	return rawSQL + "\n\t\tUNION ALL\n" + cursorSQL, append(args, cursorArgs...)
}

// usagePricingCTE renders the usage CTEs over the raw usage rows of a scope.
func usagePricingCTE(scope usagePriceScope) (string, []any) {
	rawSQL, args := usagePricingRawSQL(scope)
	return chUsageCTEFromRawSource(db.UsageFilter{Timezone: "UTC"}, rawSQL, args,
		false, chUsageStoredNormSource(), "")
}

// unpricedUsageSQL renders the distinct pricing inputs of a scope that have
// no price record under digest.
//
// usage_event_prices is sorted by (pricing_digest, price_key). A Cursor or
// session-batch scope looks up only its own price keys, so the lookup reads
// the granules that can hold them, and its key set stays small. Price keys
// are hashes spread evenly over the digest's granules, so a scope with more
// than a few hundred keys still reads nearly all of them: 885 keys of 100
// sessions read 192 of 194 granules on a production mirror. The lookup
// evaluates usage_normalized a second time, which costs little for these
// scopes, but for the whole mirror it doubles the cost of the pass, so the
// whole mirror filters on the digest's whole key set.
//
// Records are only ever inserted, and dropped a digest at a time, so a key
// has a record whenever any part holds one: the lookup skips FINAL.
func unpricedUsageSQL(scope usagePriceScope, digest string) (string, []any) {
	cte, args := usagePricingCTE(scope)
	candidates := ""
	if scope.cursorOnly || len(scope.sessionIDs) > 0 {
		candidates = `
				AND price_key IN (SELECT price_key FROM usage_normalized)`
	}
	return cte + `
		SELECT price_key, any(model), any(price_model), any(provider_id),
			any(pricing_ts), any(source), any(message_ordinal IS NOT NULL),
			any(input_tokens_norm), any(output_tokens_norm),
			any(reasoning_tokens_norm), any(cache_create_norm),
			any(cache_create_1h_norm), any(cache_read_norm),
			any(cost_microdollars IS NOT NULL AND cost_source != 'copilot-reported')
		FROM usage_normalized
		WHERE price_key NOT IN (
			SELECT price_key FROM usage_event_prices
			WHERE pricing_digest = ?` + candidates + `
			SETTINGS final = 0
		)
		GROUP BY price_key
		ORDER BY price_key`, append(args, digest)
}

// forEachUnpricedUsageBatch streams distinct missing pricing inputs in bounded
// batches. The callback must finish consuming a batch before returning.
func (s *Sync) forEachUnpricedUsageBatch(
	ctx context.Context, scope usagePriceScope, digest string,
	visit func([]chUsagePriceInput) error,
) error {
	query, args := unpricedUsageSQL(scope, digest)
	rows, err := s.conn.QueryContext(ctx, query, args...)
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

// priceUsageBatch writes a price record for every unpriced input of the
// sessions in scope, as each session batch lands.
//
// An input whose model the digest does not cover is not priced under it: a
// catalog that resolves that model differently would map to the same
// digest. Such a model is registered as pending instead, and the push
// adopts every pending model in one pass when its batches are done. Until
// then readers price that model's rows per request.
func (s *Sync) priceUsageBatch(
	ctx context.Context, pricer *usagePricer, scope usagePriceScope,
) error {
	added, err := s.priceUsageScope(ctx, pricer, scope)
	if err != nil {
		return err
	}
	var fresh []chUsagePriceModel
	for _, m := range added {
		if !slices.Contains(pricer.pending, m) {
			fresh = append(fresh, m)
		}
	}
	if err := s.registerUsagePriceModels(ctx, fresh, true); err != nil {
		return err
	}
	pricer.pending = append(pricer.pending, fresh...)
	return nil
}

// adoptPendingUsageModels adopts the models this push's batches found.
func (s *Sync) adoptPendingUsageModels(ctx context.Context) error {
	if s.pricer == nil || len(s.pricer.pending) == 0 {
		return nil
	}
	return s.adoptUsageModels(ctx, s.pricer, s.pricer.pending)
}

// adoptUsageModels prices the whole mirror under the digest that also
// covers added, registers the models, and makes that digest current.
// Readers derive the digest from the registered models, so they move to it
// only once its records are complete. pricer then prices under the new
// digest.
func (s *Sync) adoptUsageModels(
	ctx context.Context, pricer *usagePricer, added []chUsagePriceModel,
) error {
	next := pricer
	for added = pricer.uncovered(added); len(added) > 0; {
		var err error
		if next, err = next.withModels(added); err != nil {
			return err
		}
		// Usage pushed meanwhile can name yet another new model.
		if added, err = s.priceUsageScope(ctx, next, usagePriceScope{}); err != nil {
			return err
		}
	}
	if err := s.registerUsagePriceModels(ctx, next.models[len(pricer.models):], false); err != nil {
		return err
	}
	*pricer = *next
	s.usageWritten = true
	if err := writeMetadata(ctx, s.conn, map[string]string{
		usagePricedDigestKey: pricer.digest,
	}); err != nil {
		return err
	}
	return s.pruneUsagePrices(ctx, pricer.digest)
}

// priceUsageScope prices the unpriced inputs in scope whose model pricer
// covers, and returns the models it does not cover.
func (s *Sync) priceUsageScope(
	ctx context.Context, pricer *usagePricer, scope usagePriceScope,
) ([]chUsagePriceModel, error) {
	known := make(map[chUsagePriceModel]bool, len(pricer.models))
	for _, m := range pricer.models {
		known[m] = true
	}
	var added []chUsagePriceModel
	err := s.forEachUnpricedUsageBatch(ctx, scope, pricer.digest, func(inputs []chUsagePriceInput) error {
		covered := inputs[:0:0]
		for _, in := range inputs {
			m := chUsagePriceModel{providerID: in.providerID, model: in.model, priceModel: in.priceModel}
			if known[m] {
				covered = append(covered, in)
				continue
			}
			if !slices.Contains(added, m) {
				added = append(added, m)
			}
		}
		return s.insertUsagePrices(ctx, pricer, covered)
	})
	return added, err
}

// registerUsagePriceModels records models as covered by the pricing
// digest, or as pending: found in pushed usage, but not yet priced under a
// digest that covers them.
func (s *Sync) registerUsagePriceModels(
	ctx context.Context, models []chUsagePriceModel, pending bool,
) error {
	version := newPushVersion()
	rows := make([][]any, 0, len(models))
	for _, m := range models {
		rows = append(rows, []any{m.providerID, m.model, m.priceModel, pending, version})
	}
	return insertRows(ctx, s.conn, "usage_price_models", rows)
}

func (s *Sync) insertUsagePrices(
	ctx context.Context, pricer *usagePricer, inputs []chUsagePriceInput,
) error {
	if len(inputs) == 0 {
		return nil
	}
	s.usageWritten = true
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
// The whole mirror is priced when the digest changed, on a full push, and on
// a mirror no pass has covered yet, which is also the backfill for rows
// written before price records existed. Otherwise session rows were priced
// by the batch that wrote them, and only the Cursor rows can need a pass:
// when there are Cursor rows and the last pass that covered them did not
// see the same rows, digest, and price records; see cursorPricingParts.
// Rows this misses, such as those of an exporter that writes no prices,
// stay correct: the reader prices them per request until the next whole
// pass.
//
// Models that pass finds uncovered, and models an earlier push left
// pending, are adopted before any session batch. Every push then drops the
// records of digests that pruneUsagePrices finds unused.
func (s *Sync) syncUsagePrices(ctx context.Context, full bool) (*usagePricer, error) {
	pricer, err := s.newUsagePricer(ctx)
	if err != nil {
		return nil, err
	}
	// Read before pricing, so a mark never claims rows the pass missed.
	parts, err := s.readCursorPricingParts(ctx)
	if err != nil {
		return nil, err
	}
	meta, err := readMetadata(ctx, s.conn, usagePricedDigestKey, usagePricedCursorKey)
	if err != nil {
		return nil, err
	}
	pending, err := loadUsagePriceModels(ctx, s.conn, true)
	if err != nil {
		return nil, err
	}
	whole := full || meta[usagePricedDigestKey] != pricer.digest
	cursorDue := parts.cursor != "" && meta[usagePricedCursorKey] != parts.mark(pricer.digest)
	var added []chUsagePriceModel
	if whole || cursorDue {
		if added, err = s.priceUsageScope(ctx, pricer, usagePriceScope{cursorOnly: !whole}); err != nil {
			return nil, err
		}
	}
	if added = pricer.uncovered(slices.Concat(added, pending)); len(added) > 0 {
		// Adoption prices the whole mirror, Cursor rows included, under the
		// digest it moves to.
		if err := s.adoptUsageModels(ctx, pricer, added); err != nil {
			return nil, err
		}
		if parts.cursor == "" {
			return pricer, nil
		}
		if err := writeMetadata(ctx, s.conn, map[string]string{
			usagePricedCursorKey: parts.mark(pricer.digest),
		}); err != nil {
			return nil, err
		}
		return pricer, nil
	}
	covered := map[string]string{}
	if whole {
		s.usageWritten = true
		covered[usagePricedDigestKey] = pricer.digest
	}
	if parts.cursor != "" && (whole || cursorDue) {
		covered[usagePricedCursorKey] = parts.mark(pricer.digest)
	}
	if err := writeMetadata(ctx, s.conn, covered); err != nil {
		return nil, err
	}
	if err := s.pruneUsagePrices(ctx, pricer.digest); err != nil {
		return nil, err
	}
	return pricer, nil
}

// cursorPricingParts identifies, from their active parts, what a pass over
// the Cursor rows reads and what it leaves behind.
//
// cursor names every active part of cursor_usage_events with a hash of its
// files, so it changes when an insert, merge, or mutation does, and two
// equal values mean the same rows. It is empty when the table has no
// active parts, and then there is nothing to price.
//
// firstBlock maps each digest to the lowest block number among its active
// usage_event_prices parts. Merges and inserts keep it; dropping the
// digest's partition, as pruneUsagePrices does, raises it, because every
// part written afterwards has a higher block number. A pass that read the
// value before writing records therefore sees it change if a drop
// removed any of them.
type cursorPricingParts struct {
	cursor     string
	firstBlock map[string]int64
}

func (s *Sync) readCursorPricingParts(ctx context.Context) (cursorPricingParts, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT table, partition, name, hash_of_all_files, min_block_number
		FROM system.parts
		WHERE database = currentDatabase() AND active
			AND table IN ('cursor_usage_events', 'usage_event_prices')
		ORDER BY table, name`)
	if err != nil {
		return cursorPricingParts{}, fmt.Errorf("reading clickhouse cursor pricing parts: %w", err)
	}
	defer rows.Close()
	out := cursorPricingParts{firstBlock: map[string]int64{}}
	cursor := sha256.New()
	cursorParts := 0
	for rows.Next() {
		var table, partition, name, files string
		var minBlock int64
		if err := rows.Scan(&table, &partition, &name, &files, &minBlock); err != nil {
			return cursorPricingParts{}, fmt.Errorf("scanning clickhouse cursor pricing parts: %w", err)
		}
		if table == "cursor_usage_events" {
			_, _ = fmt.Fprintf(cursor, "%d:%s%d:%s", len(name), name, len(files), files)
			cursorParts++
			continue
		}
		if first, ok := out.firstBlock[partition]; !ok || minBlock < first {
			out.firstBlock[partition] = minBlock
		}
	}
	if err := rows.Err(); err != nil {
		return cursorPricingParts{}, fmt.Errorf("iterating clickhouse cursor pricing parts: %w", err)
	}
	if cursorParts > 0 {
		out.cursor = hex.EncodeToString(cursor.Sum(nil))
	}
	return out, nil
}

// mark renders what a pass over the Cursor rows covers under digest: the
// Cursor parts, the digest's first price record block, and the query that
// derives their price keys, which a build can change without changing the
// digest. A pass records its mark after it finishes, so a push that reads
// the same mark has nothing to price.
func (p cursorPricingParts) mark(digest string) string {
	cte, args := usagePricingCTE(usagePriceScope{cursorOnly: true})
	h := sha256.New()
	for _, field := range []string{
		digest, p.cursor, strconv.FormatInt(p.firstBlock[digest], 10), cte, fmt.Sprint(args),
	} {
		_, _ = fmt.Fprintf(h, "%d:%s", len(field), field)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// pruneUsagePrices drops the records and contexts of every pricing digest
// that is not current, is not the one prepared_usage was built from, and
// has had nothing written under it for usagePriceRetention. current is the
// digest this push prices under; the mirror's current digest can differ
// when another exporter pushed since. A refresh or rebuild already reading
// a dropped digest keeps the parts it started with, and a read that finds
// the records gone prices its rows per request.
func (s *Sync) pruneUsagePrices(ctx context.Context, current string) error {
	keep := map[string]bool{current: true}
	meta, err := readMetadata(ctx, s.conn, usagePricedDigestKey)
	if err != nil {
		return err
	}
	keep[meta[usagePricedDigestKey]] = true
	var stamp string
	err = s.conn.QueryRowContext(ctx,
		`SELECT pricing_digest FROM prepared_usage LIMIT 1 SETTINGS final=0`).Scan(&stamp)
	switch {
	case err == nil:
		keep[stamp] = true
	case errors.Is(err, sql.ErrNoRows), isMissingTableError(err):
	default:
		return fmt.Errorf("reading prepared usage pricing digest: %w", err)
	}

	idle, err := s.idleUsagePriceDigests(ctx)
	if err != nil {
		return err
	}
	var stale []string
	for _, digest := range idle {
		if !keep[digest] {
			stale = append(stale, digest)
		}
	}
	if len(stale) == 0 {
		return nil
	}
	s.usageWritten = true
	for _, table := range []string{"usage_event_prices", "usage_price_contexts"} {
		for batch := range slices.Chunk(stale, pricingUpsertBatch) {
			drops := make([]string, len(batch))
			args := make([]any, len(batch))
			for i, digest := range batch {
				drops[i], args[i] = "DROP PARTITION ?", digest
			}
			if _, err := s.conn.ExecContext(ctx,
				"ALTER TABLE "+table+" "+strings.Join(drops, ", "), args...); err != nil {
				return fmt.Errorf("dropping unused clickhouse %s digests: %w", table, err)
			}
		}
	}
	log.Printf("clickhouse push: dropped price records of %d unused pricing digests", len(stale))
	return nil
}

// idleUsagePriceDigests lists the digests nothing was written under for
// usagePriceRetention.
func (s *Sync) idleUsagePriceDigests(ctx context.Context) ([]string, error) {
	cutoff := newPushVersion() - uint64(usagePriceRetention.Nanoseconds())
	rows, err := s.conn.QueryContext(ctx, `
		SELECT pricing_digest FROM (
			SELECT pricing_digest, push_version FROM usage_event_prices
			UNION ALL
			SELECT pricing_digest, push_version FROM usage_price_contexts
		)
		GROUP BY pricing_digest
		HAVING max(push_version) < ?
		SETTINGS final=0`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse pricing digests: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, fmt.Errorf("scanning clickhouse pricing digest: %w", err)
		}
		out = append(out, digest)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating clickhouse pricing digests: %w", err)
	}
	return out, nil
}
