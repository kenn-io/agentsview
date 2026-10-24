package vector

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHubVectorCodec(t *testing.T) {
	encoded, err := EncodeHubVector([]float32{1, -2})
	require.NoError(t, err)
	assert.Equal(t, "AACAPwAAAMA=", encoded)
	decoded, err := DecodeHubVector("AACAPwAAAMA=", 2)
	require.NoError(t, err)
	assert.Equal(t, []float32{1, -2}, decoded)
	rng := rand.New(rand.NewPCG(2416, 1))
	bitsToTest := []uint32{0, 0x80000000, 1, 0x7f7fffff, 0x7f800000, 0xff800000, 0x7fc00000}
	for range 1000 {
		bitsToTest = append(bitsToTest, rng.Uint32())
	}
	for _, bits := range bitsToTest {
		value := math.Float32frombits(bits)
		var bytes [4]byte
		binary.LittleEndian.PutUint32(bytes[:], bits)
		raw := base64.StdEncoding.EncodeToString(bytes[:])
		encoded, encErr := EncodeHubVector([]float32{value})
		decoded, decErr := DecodeHubVector(raw, 1)
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			require.Error(t, encErr)
			require.Error(t, decErr)
			continue
		}
		require.NoError(t, encErr)
		require.NoError(t, decErr)
		require.Equal(t, raw, encoded)
		require.Equal(t, bits, math.Float32bits(decoded[0]))
	}
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
