// Package pricingrefresh manages the SQLite model-pricing catalog lifecycle.
package pricingrefresh

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
)

const (
	fallbackVersionMetaKey = "_fallback_version"
	refreshAttemptMetaKey  = "_litellm_last_attempt"
	pricingStorageMetaKey  = "_pricing_storage_version"
	pricingStorageVersion  = "2"
)

type refreshGate struct {
	slot chan struct{}
	refs int
}

var currentRefreshGates = struct {
	sync.Mutex
	byDatabase map[*db.DB]*refreshGate
}{
	byDatabase: make(map[*db.DB]*refreshGate),
}

func retainRefreshGate(database *db.DB) *refreshGate {
	currentRefreshGates.Lock()
	defer currentRefreshGates.Unlock()

	gate := currentRefreshGates.byDatabase[database]
	if gate == nil {
		gate = &refreshGate{slot: make(chan struct{}, 1)}
		gate.slot <- struct{}{}
		currentRefreshGates.byDatabase[database] = gate
	}
	gate.refs++
	return gate
}

func releaseRefreshGateReference(database *db.DB, gate *refreshGate) {
	currentRefreshGates.Lock()
	defer currentRefreshGates.Unlock()

	gate.refs--
	if gate.refs == 0 {
		delete(currentRefreshGates.byDatabase, database)
	}
}

// RefreshCooldown is the minimum interval between upstream fetch attempts.
// Attempts are recorded before fetching, so failures observe the same cooldown.
const RefreshCooldown = time.Hour

// SeedFallback installs the embedded catalog (snapshot + supplemental
// aliases) when pricing.SeedVersion differs from the stored meta.
// On reseed it also deletes flat-rate rows for date-ambiguous Kimi
// aliases so they cannot shadow the date-based CanonicalModelForDate
// pricing path.
func SeedFallback(database *db.DB) error {
	return SeedFallbackContext(context.Background(), database)
}

// SeedFallbackContext is SeedFallback with cancellation checks between each
// catalog operation. It is intended for bounded, short-lived workflows.
func SeedFallbackContext(ctx context.Context, database *db.DB) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := seedEmbeddedGenAI(ctx, database); err != nil {
		return err
	}
	stored, err := database.GetPricingMetaContext(ctx, fallbackVersionMetaKey)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	storageVersion, err := database.GetPricingMetaContext(ctx, pricingStorageMetaKey)
	if err != nil {
		return err
	}
	if stored == pricing.SeedVersion && storageVersion == pricingStorageVersion {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storeFallbackContext(ctx, database); err != nil {
		return err
	}
	// Only delete while reseeding (version mismatch). A later LiteLLM
	// refresh that legitimately lists one of these names is not
	// clobbered on every startup.
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := database.DeleteModelPricingContext(ctx,
		pricing.DateAliasedModels(),
	); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := database.SetPricingMetaContext(ctx,
		fallbackVersionMetaKey, pricing.SeedVersion,
	); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return database.SetPricingMetaContext(ctx, pricingStorageMetaKey, pricingStorageVersion)
}

// RefreshIfStale refreshes when the last attempt is older than cooldown.
// It can refresh and return an error at once: a fetch that degraded to a
// LiteLLM-only snapshot (see pricing.FetchCatalog) still stores rows and
// reports true, and the error describes the degradation.
func RefreshIfStale(
	database *db.DB,
	fetch func() (pricing.Catalog, error),
	cooldown time.Duration,
	now time.Time,
) (bool, error) {
	stored, err := database.GetPricingMeta(refreshAttemptMetaKey)
	if err != nil {
		return false, fmt.Errorf("reading pricing refresh meta: %w", err)
	}
	if stored != "" {
		last, parseErr := time.Parse(time.RFC3339, stored)
		if parseErr == nil && now.Sub(last) < cooldown {
			return false, nil
		}
	}
	return refreshAt(database, fetch, now)
}

func refreshAt(
	database *db.DB,
	fetch func() (pricing.Catalog, error),
	now time.Time,
) (bool, error) {
	if err := database.SetPricingMeta(
		refreshAttemptMetaKey, now.UTC().Format(time.RFC3339),
	); err != nil {
		return false, fmt.Errorf(
			"recording pricing refresh attempt: %w", err,
		)
	}
	catalog, fetchErr := fetch()
	if fetchErr != nil && catalog.GenAI == nil && len(catalog.LiteLLM) == 0 {
		return false, fetchErr
	}
	if err := storeCatalog(database, catalog); err != nil {
		return false, err
	}
	return true, fetchErr
}

// Ensure seeds fallback pricing and refreshes online catalogs when due.
func Ensure(
	database *db.DB,
	offline bool,
	fetch func() (pricing.Catalog, error),
	now time.Time,
) (bool, error) {
	if offline {
		if err := seedEmbeddedGenAI(context.Background(), database); err != nil {
			return false, err
		}
		return false, storeFallback(database)
	}
	if err := SeedFallback(database); err != nil {
		return false, err
	}
	return RefreshIfStale(database, fetch, RefreshCooldown, now)
}

// EnsureCurrent applies the standard online pricing lifecycle.
func EnsureCurrent(ctx context.Context, database *db.DB) error {
	return ensureCurrent(
		ctx, database, pricing.FetchCatalogContext, time.Now(),
	)
}

// RefreshCurrent applies the online pricing lifecycle immediately, regardless
// of the most recent refresh attempt.
func RefreshCurrent(ctx context.Context, database *db.DB) error {
	return refreshCurrent(
		ctx, database, pricing.FetchCatalogContext, time.Now(),
	)
}

func refreshCurrent(
	ctx context.Context,
	database *db.DB,
	fetch func(context.Context) (pricing.Catalog, error),
	now time.Time,
) error {
	return runCurrent(ctx, database, fetch, now, true)
}

func ensureCurrent(
	ctx context.Context,
	database *db.DB,
	fetch func(context.Context) (pricing.Catalog, error),
	now time.Time,
) error {
	return runCurrent(ctx, database, fetch, now, false)
}

func runCurrent(
	ctx context.Context,
	database *db.DB,
	fetch func(context.Context) (pricing.Catalog, error),
	now time.Time,
	force bool,
) error {
	gate := retainRefreshGate(database)
	if force {
		select {
		case <-gate.slot:
		default:
			releaseRefreshGateReference(database, gate)
			return nil
		}
	} else {
		select {
		case <-gate.slot:
		case <-ctx.Done():
			releaseRefreshGateReference(database, gate)
			return ctx.Err()
		}
	}
	defer func() {
		gate.slot <- struct{}{}
		releaseRefreshGateReference(database, gate)
	}()

	previousAttempt, err := database.GetPricingMeta(refreshAttemptMetaKey)
	if err != nil {
		return fmt.Errorf("reading pricing refresh meta: %w", err)
	}
	fetchCurrent := func() (pricing.Catalog, error) {
		return fetch(ctx)
	}
	if force {
		if err = SeedFallback(database); err == nil {
			_, err = refreshAt(database, fetchCurrent, now)
		}
	} else {
		_, err = Ensure(database, false, fetchCurrent, now)
	}
	if err == nil || ctx.Err() == nil {
		return err
	}
	if restoreErr := database.SetPricingMeta(
		refreshAttemptMetaKey, previousAttempt,
	); restoreErr != nil {
		return fmt.Errorf(
			"restoring pricing refresh attempt after cancellation: %v: %w",
			restoreErr, err,
		)
	}
	return err
}

// storeCatalog reconciles a fetched catalog with the stored table (see
// pricing.Catalog.Reconcile) and writes rows, retirements, and the
// OpenRouter ownership sentinel in one transaction, so a crash or a
// concurrent push never sees rows without the metadata that retires them.
//
// The sentinel and stored patterns are read before that transaction, and
// the in-process refresh gate does not cover a second process on the
// same archive (server loop beside a CLI refresh), so racing refreshes
// can commit a plan built from a one-snapshot-stale read. The next
// refresh recomputes ownership from the stored table and converges:
// retiring an already-deleted row is a no-op and a re-listed pattern is
// re-adopted.
func storeCatalog(database *db.DB, catalog pricing.Catalog) error {
	if catalog.GenAI != nil {
		if err := database.UpsertGenAIPricing(
			context.Background(), db.GenAIPricingDocument{
				Version: catalog.GenAI.Version, SourceRef: catalog.GenAI.SourceRef,
				Source: db.GenAIPricingSourceFetched,
				Data:   catalog.GenAI.RawJSON(),
			},
		); err != nil {
			return err
		}
	}
	if len(catalog.LiteLLM) == 0 && len(catalog.OpenRouter) == 0 {
		return nil
	}
	value, err := database.GetPricingMeta(pricing.OpenRouterModelsMetaKey)
	if err != nil {
		return err
	}
	previous, err := pricing.DecodeOpenRouterModels(value)
	if err != nil {
		return err
	}
	existing, err := database.ListModelPricing(context.Background())
	if err != nil {
		return fmt.Errorf("listing stored pricing: %w", err)
	}
	stored := make([]string, 0, len(existing))
	for _, row := range existing {
		if !strings.HasPrefix(row.ModelPattern, "_") {
			stored = append(stored, row.ModelPattern)
		}
	}
	prices, owned, retired := catalog.Reconcile(stored, previous)
	return database.ReconcileModelPricing(
		dbModelPricing(prices), retired,
		db.PricingMeta{
			Key:   pricing.OpenRouterModelsMetaKey,
			Value: pricing.EncodeOpenRouterModels(owned),
		},
	)
}

func seedEmbeddedGenAI(ctx context.Context, database *db.DB) error {
	embedded := pricing.EmbeddedGenAIDocument()
	return database.InsertMissingGenAIPricing(ctx, db.GenAIPricingDocument{
		Version: embedded.Version, SourceRef: embedded.SourceRef,
		Source: db.GenAIPricingSourceEmbedded, Data: embedded.RawJSON(),
	})
}

// storeFallback writes the embedded catalog over the stored table. Like
// any non-OpenRouter source it outranks OpenRouter, so OpenRouter-owned
// rows it covers under another spelling are retired and rows it lists
// exactly pass to its ownership, in the same transaction, so a reseed
// never leaves an OpenRouter row beside or under an embedded one while
// the sentinel still claims it. Databases that never stored the sentinel
// keep none.
func storeFallback(database *db.DB) error {
	return storeFallbackContext(context.Background(), database)
}

func storeFallbackContext(ctx context.Context, database *db.DB) error {
	value, err := database.GetPricingMetaContext(
		ctx, pricing.OpenRouterModelsMetaKey,
	)
	if err != nil {
		return err
	}
	previous, err := pricing.DecodeOpenRouterModels(value)
	if err != nil {
		return err
	}
	fallback := pricing.FallbackPricing()
	patterns := make([]string, len(fallback))
	for i, p := range fallback {
		patterns[i] = p.ModelPattern
	}
	retired := pricing.ShadowedPatterns(patterns, previous)
	var owned []string
	for _, pattern := range previous {
		if !slices.Contains(patterns, pattern) &&
			!slices.Contains(retired, pattern) {
			owned = append(owned, pattern)
		}
	}
	var meta db.PricingMeta
	if value != "" {
		meta = db.PricingMeta{
			Key:   pricing.OpenRouterModelsMetaKey,
			Value: pricing.EncodeOpenRouterModels(owned),
		}
	}
	return database.ReconcileModelPricingContext(
		ctx, dbModelPricing(fallback), retired, meta,
	)
}

func dbModelPricing(prices []pricing.ModelPricing) []db.ModelPricing {
	dbPrices := make([]db.ModelPricing, len(prices))
	for i, price := range prices {
		bands := make([]db.PricingBand, len(price.Bands))
		for j, band := range price.Bands {
			bands[j] = db.PricingBand{
				AboveInputTokens:     band.AboveInputTokens,
				InputPerMTok:         band.InputPerMTok,
				OutputPerMTok:        band.OutputPerMTok,
				CacheCreationPerMTok: band.CacheCreationPerMTok,
				CacheReadPerMTok:     band.CacheReadPerMTok,
			}
		}
		dbPrices[i] = db.ModelPricing{
			ModelPattern:         price.ModelPattern,
			InputPerMTok:         price.InputPerMTok,
			OutputPerMTok:        price.OutputPerMTok,
			CacheCreationPerMTok: price.CacheCreationPerMTok,
			CacheReadPerMTok:     price.CacheReadPerMTok,
			Bands:                bands,
		}
	}
	return dbPrices
}
