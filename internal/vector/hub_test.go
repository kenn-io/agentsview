package vector

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

func TestHubVectorCodec(t *testing.T) {
	encoded, err := EncodeHubVector([]float32{1, -2})
	require.NoError(t, err)
	assert.Equal(t, "AACAPwAAAMA=", encoded)
	decoded, err := DecodeHubVector("AACAPwAAAMA=", 2)
	require.NoError(t, err)
	assert.Equal(t, []float32{1, -2}, decoded)
	hegel.Test(t, func(ht *hegel.T) {
		bits := hegel.Draw(ht, hegel.Integers[uint32](0, math.MaxUint32))
		value := math.Float32frombits(bits)
		var bytes [4]byte
		binary.LittleEndian.PutUint32(bytes[:], bits)
		raw := base64.StdEncoding.EncodeToString(bytes[:])
		encoded, encErr := EncodeHubVector([]float32{value})
		decoded, decErr := DecodeHubVector(raw, 1)
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			require.Error(ht, encErr)
			require.Error(ht, decErr)
			return
		}
		require.NoError(ht, encErr)
		require.NoError(ht, decErr)
		require.Equal(ht, raw, encoded)
		require.Equal(ht, bits, math.Float32bits(decoded[0]))
	}, hegel.WithTestCases(1000))
	for _, dimension := range []int{-1, 0, 1, 3, math.MaxInt} {
		_, err := DecodeHubVector("AACAPwAAAMA=", dimension)
		require.Error(t, err)
	}
	for _, raw := range []string{"", "!", "AA==", "AACAPw==trailing"} {
		_, err := DecodeHubVector(raw, 1)
		require.Error(t, err)
	}
	_, err = EncodeHubVector(nil)
	assert.Error(t, err)
}
