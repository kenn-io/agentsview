// ABOUTME: End-to-end semantic and hybrid search tests using a real vector index.
// ABOUTME: Covers snippet centering and retained soft-deleted session vectors.
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

func TestSearchContentSemanticAndHybridRetainSoftDeletedVectors(t *testing.T) {
	ctx := context.Background()
	d := dbtest.OpenTestDB(t)
	dbtest.SeedSessionWithMessages(t, d, "deleted-session", "proj", []db.Message{
		dbtest.UserMsg("deleted-session", 0, "archived vector content"),
	}, dbtest.WithMessageCounts(2, 2))

	var encoded []string
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
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
	active, err := d.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "unrelated query", Mode: "semantic", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, active.Matches, 1)
	assert.Equal(t, "deleted-session", active.Matches[0].SessionID)

	require.NoError(t, d.SoftDeleteSession("deleted-session"))
	encoded = nil
	_, err = ix.Build(ctx, d, enc, gen, vector.BuildOptions{Backstop: true})
	require.NoError(t, err)
	assert.Empty(t, encoded, "a backstop must reuse the existing deleted-session vector")

	for _, mode := range []string{"semantic", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "hybrid" && !d.HasFTS() {
				t.Skip("FTS5 is unavailable")
			}

			withoutDeleted, err := d.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: "unrelated query", Mode: mode, Limit: 10,
			})
			require.NoError(t, err)
			assert.Empty(t, withoutDeleted.Matches)

			withDeleted, err := d.SearchContent(ctx, db.ContentSearchFilter{
				Pattern: "unrelated query", Mode: mode,
				IncludeDeleted: true, Limit: 10,
			})
			require.NoError(t, err)
			require.Len(t, withDeleted.Matches, 1)
			assert.Equal(t, "deleted-session", withDeleted.Matches[0].SessionID)
		})
	}

	dbtest.SeedSessionWithMessages(t, d, "active-session", "proj", []db.Message{
		dbtest.UserMsg("active-session", 0, "new active vector content"),
	}, dbtest.WithMessageCounts(2, 2))
	encoded = nil
	nextGen := kitvec.Generation{Model: "next-fake-model", Dimensions: 3}
	result, err := ix.Build(ctx, d, enc, nextGen, vector.BuildOptions{Backstop: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"new active vector content"}, encoded,
		"a new generation must not re-embed deleted transcript content")
	assert.Equal(t, 1, result.Fill.Documents)
}

func TestVectorBuildDoesNotEncodeAlreadyDeletedSessions(t *testing.T) {
	ctx := context.Background()
	d := dbtest.OpenTestDB(t)
	for _, session := range []struct {
		id, content string
	}{
		{id: "active-session", content: "active vector content"},
		{id: "deleted-session", content: "deleted vector content"},
	} {
		dbtest.SeedSessionWithMessages(t, d, session.id, "proj", []db.Message{
			dbtest.UserMsg(session.id, 0, session.content),
		}, dbtest.WithMessageCounts(2, 2))
	}
	require.NoError(t, d.SoftDeleteSession("deleted-session"))

	var encoded []string
	enc := func(_ context.Context, texts []string) ([][]float32, error) {
		encoded = append(encoded, texts...)
		out := make([][]float32, len(texts))
		for i := range texts {
			out[i] = []float32{1, 0, 0}
		}
		return out, nil
	}

	ix, err := vector.Open(ctx, filepath.Join(t.TempDir(), "vectors.db"), false, 4000)
	require.NoError(t, err)
	defer func() { require.NoError(t, ix.Close()) }()
	gen := kitvec.Generation{Model: "fake-model", Dimensions: 3}
	result, err := ix.Build(ctx, d, enc, gen, vector.BuildOptions{})
	require.NoError(t, err)
	assert.Equal(t, []string{"active vector content"}, encoded,
		"the encoder must receive active content and never deleted content")
	assert.Equal(t, 1, result.Fill.Documents)

	d.SetVectorSearcher(vectorIndexSearcher{ix: ix, enc: enc})
	page, err := d.SearchContent(ctx, db.ContentSearchFilter{
		Pattern: "unrelated query", Mode: "semantic",
		IncludeDeleted: true, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, page.Matches, 1)
	assert.Equal(t, "active-session", page.Matches[0].SessionID)
}
