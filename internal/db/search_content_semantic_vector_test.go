// ABOUTME: end-to-end semantic search tests wiring a real internal/vector
// ABOUTME: index into db.SearchContent, pinning anchor-local snippet centering.
package db_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/vector"
	kitvec "go.kenn.io/kit/vector"
)

// vectorIndexSearcher adapts a real *vector.Index to db.VectorSearcher for
// tests, mirroring the production searcherAdapter in cmd/agentsview without
// its staleness gate.
type vectorIndexSearcher struct {
	ix  *vector.Index
	enc kitvec.EncodeFunc
}

func (s vectorIndexSearcher) SemanticSearch(
	ctx context.Context, query string, limit int,
) ([]db.VectorHit, error) {
	hits, err := s.ix.Search(ctx, s.enc, query, limit)
	if err != nil {
		return nil, err
	}
	out := make([]db.VectorHit, len(hits))
	for i, h := range hits {
		out[i] = db.VectorHit{
			SessionID:    h.SessionID,
			Ordinal:      h.Ordinal,
			OrdinalStart: h.OrdinalStart,
			OrdinalEnd:   h.OrdinalEnd,
			Subordinate:  h.Subordinate,
			Score:        h.Score,
			Snippet:      h.Snippet,
		}
	}
	return out, nil
}

func (s vectorIndexSearcher) ResolveMessageUnits(
	ctx context.Context, refs []db.MessageRef,
) ([]db.UnitRef, error) {
	return s.ix.ResolveMessageUnits(ctx, refs)
}

// TestSearchContentSemanticCrossMemberChunkCentersOnAnchorMessage is the
// end-to-end regression test for run-chunk snippet mislocation: a run whose
// matched chunk spans two assistant messages must produce a ContentMatch
// whose snippet centers on the ANCHOR message's content. Before the fix, the
// vector layer returned the whole cross-member chunk as the snippet; the db
// layer could not locate that text inside the anchor message's content and
// fell back to centering on the query pattern (absent here), i.e. the start
// of the message — losing the matched region entirely.
func TestSearchContentSemanticCrossMemberChunkCentersOnAnchorMessage(t *testing.T) {
	ctx := context.Background()
	d := dbtest.OpenTestDB(t)

	memberA := "a short first assistant step"
	// The distinctive matched text sits past the snippet window's 60-byte
	// radius from the start of the anchor message, so a start-of-content
	// fallback cannot accidentally include it.
	memberB := strings.Repeat("background context sentence. ", 4) +
		"the particles remain entangled across any distance"
	msgs := []db.Message{
		dbtest.UserMsg("s1", 0, "please explain the experiment results"),
		dbtest.AsstMsg("s1", 1, memberA),
		dbtest.AsstMsg("s1", 2, memberB),
	}
	dbtest.SeedSessionWithMessages(t, d, "s1", "proj", msgs,
		dbtest.WithMessageCounts(3, 2))

	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		out := make([][]float32, len(texts))
		for i, text := range texts {
			if strings.Contains(text, "entangled") || strings.Contains(text, "quantum") {
				out[i] = []float32{1, 0, 0}
			} else {
				out[i] = []float32{0, 1, 0}
			}
		}
		return out, nil
	}

	ix, err := vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
	require.NoError(t, err)
	defer func() { require.NoError(t, ix.Close()) }()
	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	_, err = ix.Build(ctx, d, enc, gen, vector.BuildOptions{})
	require.NoError(t, err)

	d.SetVectorSearcher(vectorIndexSearcher{ix: ix, enc: enc})

	// The query shares no literal token with the anchor message, so a
	// pattern-based fallback cannot rescue a mislocated snippet.
	page, err := d.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "quantum superposition", Mode: "semantic", Limit: 10,
	})
	require.NoError(t, err)
	require.NotEmpty(t, page.Matches)

	m := page.Matches[0]
	assert.Equal(t, "s1", m.SessionID)
	assert.Equal(t, 2, m.Ordinal,
		"anchor: the member containing the matched chunk's center")
	assert.Contains(t, m.Snippet, "entangled",
		"snippet must center on the anchor message's matched content")
	assert.NotContains(t, m.Snippet, memberA,
		"snippet must not carry text from a different run member")
}
