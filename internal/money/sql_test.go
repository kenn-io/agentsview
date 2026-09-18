package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMoneySQLUsesIntegersOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var value Money
	require.NoError(value.Scan(int64(420_000)))
	assert.Equal(Money{Microdollars: 420_000}, value)

	driverValue, err := value.Value()
	require.NoError(err)
	assert.Equal(int64(420_000), driverValue)

	err = value.Scan(float64(0.42))
	require.Error(err)
	assert.Contains(err.Error(), "integer required")
}
