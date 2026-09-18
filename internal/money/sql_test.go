package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoneySQLUsesIntegersOnly(t *testing.T) {
	var value Money
	require.NoError(t, value.Scan(int64(420_000)))
	assert.Equal(t, Money{Microdollars: 420_000}, value)

	driverValue, err := value.Value()
	require.NoError(t, err)
	assert.Equal(t, int64(420_000), driverValue)

	err = value.Scan(float64(0.42))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "integer required")
}
