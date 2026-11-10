package vector

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildConcurrentDocumentWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.db")
	ix, err := OpenSpec(t.Context(), path, MessageIndexSpec(), false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ix.Close()) })
	recall, err := OpenSpec(t.Context(), path, RecallIndexSpec(), false, 4000)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, recall.Close()) })
	src := &fakeUnitSource{}
	for i := range 128 {
		src.rows = append(src.rows, fakeUnit{
			unit:    userDoc("session", fmt.Sprintf("user-%d", i), i, "searchable content"),
			endedAt: "2024-01-01T00:00:00Z",
		})
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		result, err := recall.Build(t.Context(), &fakeUnitSource{rows: src.rows}, fakeBuildEncoder(), fakeGeneration("fake-model"), BuildOptions{Concurrency: 8, BatchSize: 1})
		if assert.NoError(t, err) {
			assert.True(t, result.Activated)
			assert.Equal(t, 128, result.Fill.Documents)
		}
	})
	defer wg.Wait()
	result, err := ix.Build(t.Context(), src, fakeBuildEncoder(), fakeGeneration("fake-model"),
		BuildOptions{Concurrency: 8, BatchSize: 1})
	require.NoError(t, err)
	require.True(t, result.Activated)
	require.Equal(t, 128, result.Fill.Documents)
}
