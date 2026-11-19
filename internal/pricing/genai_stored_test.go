package pricing

import (
	"crypto/sha256"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The stored-document memo is process-wide, so this test does not run in
// parallel with anything else that parses a stored document.
func TestParseStoredGenAIDocumentReusesParseOfSameBytes(t *testing.T) {
	data := EmbeddedGenAIDocument().RawJSON()
	first, err := ParseStoredGenAIDocument(slices.Clone(data), "", "ref-a")
	require.NoError(t, err)

	again, err := ParseStoredGenAIDocument(slices.Clone(data), "", "ref-a")
	require.NoError(t, err)
	assert.Same(t, first.Prices, again.Prices)

	cached, ok := CachedGenAIDocument(sha256.Sum256(data), "", "ref-a")
	require.True(t, ok)
	assert.Same(t, first.Prices, cached.Prices)
	_, ok = CachedGenAIDocument(sha256.Sum256(data), "", "ref-b")
	assert.False(t, ok, "a different source ref must not reuse the parse")

	changed := append(slices.Clone(data), '\n')
	other, err := ParseStoredGenAIDocument(changed, "", "ref-a")
	require.NoError(t, err)
	assert.NotSame(t, first.Prices, other.Prices)
	_, ok = CachedGenAIDocument(sha256.Sum256(data), "", "ref-a")
	assert.False(t, ok, "the memo must forget a document whose bytes changed")
}
