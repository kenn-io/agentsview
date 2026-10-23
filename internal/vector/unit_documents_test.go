package vector

import (
	"crypto/sha256"
	"fmt"
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/agentsview/internal/db"
)

func TestUnitDocumentsOccurrenceKeys(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		size := hegel.Draw(ht, hegel.Integers(0, math.MaxInt)) % 65
		units := make([]db.EmbeddableUnit, size)
		for i := range units {
			units[i] = db.EmbeddableUnit{
				SessionID: hegel.Draw(ht, hegel.SampledFrom([]string{"session", "other"})),
				Kind:      hegel.Draw(ht, hegel.SampledFrom([]string{"user", "run"})), Content: hegel.Draw(ht, hegel.Text()), Ordinal: i,
				SourceUUID: hegel.Draw(ht, hegel.SampledFrom([]string{"", "dup", "dup#2", "a:b", "%"})),
			}
		}
		docs := slices.Collect(UnitDocuments(slices.Values(units)))
		require.Len(ht, docs, len(units))
		seen := make(map[string]bool)
		for i, d := range docs {
			assert.False(ht, seen[d.DocKey])
			seen[d.DocKey] = true
			if units[i].SourceUUID == "dup" {
				n := 0
				for _, u := range units[:i+1] {
					if u.SourceUUID == "dup" && u.SessionID == units[i].SessionID {
						n++
					}
				}
				suffix := ""
				if n > 1 {
					suffix = "#" + strconv.Itoa(n)
				}
				prefix := "u:"
				if units[i].Kind == "run" {
					prefix = "r:"
				}
				assert.Equal(ht, prefix+units[i].SessionID+":dup"+suffix, d.DocKey)
			}
		}
	}, hegel.WithTestCases(1000))
}

func TestUnitDocumentsMetadata(t *testing.T) {
	units := []db.EmbeddableUnit{
		{
			SessionID: "s", Kind: "run", SourceUUID: "id", Ordinal: 2,
			OrdinalEnd: 4, Subordinate: true, Content: "abc", Offsets: []db.UnitOffset{{Ordinal: 2}, {Ordinal: 4, RuneStart: 2, ByteStart: 2}},
		},
		{SessionID: "s", Kind: "user", SourceUUID: "id", Ordinal: 5, Deleted: true},
	}
	docs := slices.Collect(UnitDocuments(slices.Values(units)))
	require.Equal(t, []UnitDocument{
		{
			DocKey: "r:s:id", SessionID: "s", SourceUUID: "id", Ordinal: 2, OrdinalEnd: 4, Subordinate: true,
			Content: "abc", ContentHash: "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			Offsets: []db.UnitOffset{{Ordinal: 2}, {Ordinal: 4, RuneStart: 2, ByteStart: 2}},
		},
		{
			DocKey: "u:s:id#2", SessionID: "s", SourceUUID: "id", Ordinal: 5, Deleted: true,
			ContentHash: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}, docs)
}

func TestUnitDocumentsIteration(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		size := 1 + hegel.Draw(ht, hegel.Integers(0, math.MaxInt))%65
		stop := hegel.Draw(ht, hegel.Integers(0, math.MaxInt)) % size
		read := 0
		docs := UnitDocuments(func(yield func(db.EmbeddableUnit) bool) {
			for i := range size {
				read++
				if !yield(db.EmbeddableUnit{SessionID: "s", Kind: "user", SourceUUID: "dup", Ordinal: i}) {
					return
				}
			}
		})
		for range docs {
			if read == stop+1 {
				break
			}
		}
		assert.Equal(ht, stop+1, read)
		first, second := slices.Collect(docs), slices.Collect(docs)
		assert.Equal(ht, first, second)
	}, hegel.WithTestCases(1000))
}

func TestUnitDocumentsContentRevision(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		content := hegel.Draw(ht, hegel.Text())
		docs := slices.Collect(UnitDocuments(slices.Values([]db.EmbeddableUnit{{Content: content}})))
		require.Len(ht, docs, 1)
		assert.Equal(ht, content, docs[0].Content)
		assert.Equal(ht, fmt.Sprintf("%x", sha256.Sum256([]byte(content))), docs[0].ContentHash)
	}, hegel.WithTestCases(1000))
}

func TestUnitDocumentsEscapedKeys(t *testing.T) {
	for _, tc := range []struct{ kind, session, uuid, want string }{
		{"user", "s:#%", "dup#2:%", "u:s%3A%23%25:dup%232%3A%25"},
	} {
		docs := slices.Collect(UnitDocuments(slices.Values([]db.EmbeddableUnit{{Kind: tc.kind, SessionID: tc.session, SourceUUID: tc.uuid, Ordinal: 2}})))
		require.Len(t, docs, 1)
		assert.Equal(t, tc.want, docs[0].DocKey)
	}
}
