// Package pricingrefresh manages the SQLite model-pricing catalog lifecycle.
package pricingrefresh

import (
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

// SeedFallback installs the embedded catalog when its version changed.
func SeedFallback(database *db.DB) error {
	stored, err := database.GetPricingMeta(fallbackVersionMetaKey)
	if err != nil {
		return err
	}
	if stored == pricing.FallbackVersion {
		return nil
	}
	if err := upsert(database, pricing.FallbackPricing()); err != nil {
		return err
	}
	return database.SetPricingMeta(
		fallbackVersionMetaKey, pricing.FallbackVersion,
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
func EnsureCurrent(database *db.DB) error {
	_, err := Ensure(database, false, pricing.FetchLiteLLMPricing, time.Now())
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
