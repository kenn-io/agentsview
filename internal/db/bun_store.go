package db

import (
	"context"
	"sync"

	"github.com/uptrace/bun"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/export"
)

type pricingState struct {
	custom       map[string]config.CustomModelRate
	effective    map[string]export.ModelRates
	emptyCatalog map[string]export.ModelRates
}

// BunStore is the common query runtime embedded by every concrete store.
type BunStore struct {
	backend BunBackend

	cursorMu     sync.RWMutex
	cursorSecret []byte

	pricingMu sync.RWMutex
	pricing   pricingState
}

// NewBunStore creates a shared store over one guarded backend.
func NewBunStore(backend BunBackend) *BunStore {
	return &BunStore{backend: backend}
}

// SetCursorSecret updates the shared cursor signing key.
func (s *BunStore) SetCursorSecret(secret []byte) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	s.cursorSecret = append([]byte(nil), secret...)
}

// SetCustomPricing updates the shared custom pricing overrides.
func (s *BunStore) SetCustomPricing(pricing map[string]config.CustomModelRate) {
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	s.pricing.custom = pricing
	s.pricing.effective = nil
}

// SetEffectivePricing updates the shared effective pricing catalogue.
func (s *BunStore) SetEffectivePricing(pricing map[string]export.ModelRates) {
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	s.pricing.custom = nil
	s.pricing.effective = cloneModelRates(pricing)
}

// SetEmptyCatalogPricing updates the shared empty-catalog fallback rates.
func (s *BunStore) SetEmptyCatalogPricing(pricing map[string]export.ModelRates) {
	s.pricingMu.Lock()
	defer s.pricingMu.Unlock()
	s.pricing.emptyCatalog = cloneModelRates(pricing)
}

func cloneModelRates(pricing map[string]export.ModelRates) map[string]export.ModelRates {
	clone := make(map[string]export.ModelRates, len(pricing))
	for model, rates := range pricing {
		rates.Bands = append([]export.PricingBand(nil), rates.Bands...)
		clone[model] = rates
	}
	return clone
}

func (s *BunStore) view(
	ctx context.Context, fn func(bun.IDB) error,
) error {
	return s.backend.View(ctx, fn)
}

func (s *BunStore) update(
	ctx context.Context,
	operation WriteOperation,
	fn func(bun.IDB) error,
) error {
	if !s.backend.Capabilities().AllowsWrite(operation) {
		return ErrReadOnly
	}
	return s.backend.Update(ctx, fn)
}
