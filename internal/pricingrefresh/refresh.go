// Package pricingrefresh manages the SQLite model-pricing catalog lifecycle.
package pricingrefresh

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/pricing"
)

const (
	fallbackVersionMetaKey = "_fallback_version"
	refreshAttemptMetaKey  = "_litellm_last_attempt"
)

// RefreshCooldown is the minimum interval between upstream fetch attempts.
// Attempts are recorded before fetching, so failures observe the same cooldown.
const RefreshCooldown = time.Hour

// SeedFallback installs the embedded catalog (snapshot + supplemental
// aliases) when pricing.SeedVersion differs from the stored meta.
// On reseed it also deletes flat-rate rows for date-ambiguous Kimi
// aliases so they cannot shadow the date-based CanonicalModelForDate
// pricing path.
func SeedFallback(database *db.DB) error {
	stored, err := database.GetPricingMeta(fallbackVersionMetaKey)
	if err != nil {
		return err
	}
	if stored == pricing.SeedVersion {
		return nil
	}
	if err := upsert(database, pricing.FallbackPricing()); err != nil {
		return err
	}
	// Only delete while reseeding (version mismatch). A later LiteLLM
	// refresh that legitimately lists one of these names is not
	// clobbered on every startup.
	if err := database.DeleteModelPricing(
		pricing.DateAliasedModels(),
	); err != nil {
		return err
	}
	return database.SetPricingMeta(
		fallbackVersionMetaKey, pricing.SeedVersion,
	)
}

// Refresh fetches and stores the upstream pricing catalog immediately.
func Refresh(
	database *db.DB,
	fetch func() ([]pricing.ModelPricing, error),
) error {
	prices, err := fetch()
	if err != nil {
		return fmt.Errorf("litellm fetch failed: %w", err)
	}
	if err := upsert(database, prices); err != nil {
		return fmt.Errorf("upsert failed: %w", err)
	}
	return nil
}

// RefreshIfStale refreshes when the last attempt is older than cooldown.
func RefreshIfStale(
	database *db.DB,
	fetch func() ([]pricing.ModelPricing, error),
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
	if err := database.SetPricingMeta(
		refreshAttemptMetaKey, now.UTC().Format(time.RFC3339),
	); err != nil {
		return false, fmt.Errorf(
			"recording pricing refresh attempt: %w", err,
		)
	}
	prices, err := fetch()
	if err != nil {
		return false, err
	}
	if err := upsert(database, prices); err != nil {
		return false, err
	}
	return true, nil
}

// Ensure seeds fallback pricing and refreshes online catalogs when due.
func Ensure(
	database *db.DB,
	offline bool,
	fetch func() ([]pricing.ModelPricing, error),
	now time.Time,
) (bool, error) {
	if offline {
		return false, upsert(database, pricing.FallbackPricing())
	}
	if err := SeedFallback(database); err != nil {
		return false, err
	}
	return RefreshIfStale(database, fetch, RefreshCooldown, now)
}

// EnsureCurrent applies the standard online pricing lifecycle.
func EnsureCurrent(ctx context.Context, database *db.DB) error {
	return ensureCurrent(
		ctx, database, pricing.FetchLiteLLMPricingContext, time.Now(),
	)
}

func ensureCurrent(
	ctx context.Context,
	database *db.DB,
	fetch func(context.Context) ([]pricing.ModelPricing, error),
	now time.Time,
) error {
	previousAttempt, err := database.GetPricingMeta(refreshAttemptMetaKey)
	if err != nil {
		return fmt.Errorf("reading pricing refresh meta: %w", err)
	}
	fetchCurrent := func() ([]pricing.ModelPricing, error) {
		return fetch(ctx)
	}
	_, err = Ensure(database, false, fetchCurrent, now)
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

func upsert(database *db.DB, prices []pricing.ModelPricing) error {
	dbPrices := make([]db.ModelPricing, len(prices))
	for i, price := range prices {
		dbPrices[i] = db.ModelPricing{
			ModelPattern:         price.ModelPattern,
			InputPerMTok:         price.InputPerMTok,
			OutputPerMTok:        price.OutputPerMTok,
			CacheCreationPerMTok: price.CacheCreationPerMTok,
			CacheReadPerMTok:     price.CacheReadPerMTok,
		}
	}
	return database.UpsertModelPricing(dbPrices)
}
