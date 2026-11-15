package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/agentsview/internal/export"
)

func TestPGActivityInputBytesIncludesPrices(t *testing.T) {
	raw := pgActivityInputs{}
	withPrices := pgActivityInputs{priced: &pgActivityPriced{rows: make([]pgActivityPricedRow, 2), block: &export.PricingBlock{}}}
	assert.Greater(t, pgActivityInputBytes(withPrices), pgActivityInputBytes(raw))
}
