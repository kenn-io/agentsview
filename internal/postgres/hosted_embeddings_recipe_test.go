package postgres

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostedEmbeddingRecipeIdentity(t *testing.T) {
	base := HostedEmbeddingRecipe{ProfileName: "test", Model: "synthetic", Dimensions: 3, BuilderVersion: "conversation-v1", ChunkerVersion: "rune-v1", MaxInputChars: 64, DocumentPrefix: "doc: ", QueryPrefix: "query: ", EncodingType: HostedEmbeddingEncoding, CorpusScope: "conversation-v1"}
	canonical, e := CanonicalHostedEmbeddingRecipe(base)
	require.NoError(t, e)
	assert.Equal(t, "97b10c0dbbdc5831b7a24980b88cd90e3828552623374f4d10991146c364f2a9", canonical.Fingerprint)
	otherProfile := canonical
	otherProfile.ProfileName = "same-vectors"
	other, e := CanonicalHostedEmbeddingRecipe(otherProfile)
	require.NoError(t, e)
	assert.Equal(t, canonical.Fingerprint, other.Fingerprint)
	changed := canonical
	changed.QueryPrefix = "different: "
	_, e = CanonicalHostedEmbeddingRecipe(changed)
	assert.Error(t, e)
	changed.Fingerprint = ""
	other, e = CanonicalHostedEmbeddingRecipe(changed)
	require.NoError(t, e)
	assert.NotEqual(t, canonical.Fingerprint, other.Fingerprint)
	invalid := base
	invalid.Dimensions = 4001
	_, e = CanonicalHostedEmbeddingRecipe(invalid)
	assert.Error(t, e)
}
