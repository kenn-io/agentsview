package vector

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kitvec "go.kenn.io/kit/vector"
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

func TestMessageBuildPolicyPrecedesDocumentEncoder(t *testing.T) {
	for _, mode := range []string{"ordinary", "full", "backstop", "repair", "async"} {
		t.Run(mode, func(t *testing.T) {
			ix := openTestIndex(t)
			var encoded, pulled atomic.Int32
			m := NewManager(ix, twoDocSource(), EncoderSet{Default: "local", ByName: map[string]ManagedEncoder{"local": {Encode: func(context.Context, []string) ([][]float32, error) {
				encoded.Add(1)
				return nil, errors.New("document encoder must not run")
			}}}}, fakeGeneration("local"))
			defer m.Shutdown()
			m.documentBuild = func(_ context.Context, req BuildRequest, progress func(BuildProgress)) (BuildResult, error) {
				pulled.Add(1)
				progress(BuildProgress{Phase: "pulling", Done: 1, Total: 1})
				return BuildResult{Fingerprint: "hub"}, nil
			}
			req := BuildRequest{FullRebuild: mode == "full", Backstop: mode == "backstop", RepairInvalid: mode == "repair", Using: "local"}
			if mode == "async" {
				require.NoError(t, m.StartBuild(req))
				m.Wait()
				require.Empty(t, m.Status().LastError)
			} else {
				ran, err := m.TryBuild(t.Context(), req)
				require.NoError(t, err)
				require.True(t, ran)
			}
			require.EqualValues(t, 0, encoded.Load())
			require.EqualValues(t, 1, pulled.Load())
			require.Equal(t, "hub", m.Status().LastResult.Fingerprint)
		})
	}
}

func TestDocumentBuildPolicyLeavesRecallLocal(t *testing.T) {
	ix, err := OpenSpec(t.Context(), t.TempDir()+"/vectors.db", RecallIndexSpec(), false, 4000)
	require.NoError(t, err)
	defer ix.Close()
	var encoded, pulled int
	enc := kitvec.EncodeFunc(func(ctx context.Context, texts []string) ([][]float32, error) {
		encoded++
		return fakeBuildEncoder()(ctx, texts)
	})
	m := NewManager(ix, twoDocSource(), EncoderSet{Default: "local", ByName: map[string]ManagedEncoder{"local": {Encode: enc}}}, fakeGeneration("recall"))
	defer m.Shutdown()
	m.documentBuild = func(context.Context, BuildRequest, func(BuildProgress)) (BuildResult, error) {
		pulled++
		return BuildResult{}, errors.New("recall must stay local")
	}
	ran, err := m.TryBuild(t.Context(), BuildRequest{})
	require.NoError(t, err)
	require.True(t, ran)
	require.Positive(t, encoded)
	require.Zero(t, pulled)
}

func TestDocumentPolicyRejectsExplicitUnknownServer(t *testing.T) {
	ix := openTestIndex(t)
	m := NewManager(ix, twoDocSource(), EncoderSet{}, fakeGeneration("local"))
	defer m.Shutdown()
	m.documentBuild = func(context.Context, BuildRequest, func(BuildProgress)) (BuildResult, error) {
		require.FailNow(t, "invalid explicit server started a pull")
		return BuildResult{}, nil
	}
	for _, localProducer := range []bool{false, true} {
		req := BuildRequest{Using: "typo", LocalProducer: localProducer}
		require.ErrorIs(t, m.StartBuild(req), ErrUnknownServer)
		ran, err := m.TryBuild(t.Context(), req)
		require.ErrorIs(t, err, ErrUnknownServer)
		require.False(t, ran)
	}
}
