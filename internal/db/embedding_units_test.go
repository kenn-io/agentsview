package db

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbeddingUnitReducerCarriesRunAcrossPagesAndFilteredRows(t *testing.T) {
	var got []EmbeddableUnit
	r := NewEmbeddingUnitReducer(0, func(unit EmbeddableUnit) error {
		got = append(got, unit)
		return nil
	})

	require.NoError(t, r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "assistant", SourceUUID: "first-source",
		Ordinal: 1, Content: "hé",
	}))
	// A page boundary does not finish the logical input stream. A filtered
	// system row at ordinal 2 is likewise absent from the reducer's input.
	require.Empty(t, got)
	require.NoError(t, r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "assistant", Ordinal: 3, Content: "界",
	}))
	require.Empty(t, got)
	require.NoError(t, r.Finish())

	assert.Equal(t, []EmbeddableUnit{{
		SessionID: "session", Kind: "run", SourceUUID: "first-source",
		Ordinal: 1, OrdinalEnd: 3, Content: "hé\n\n界",
		Offsets: []UnitOffset{
			{Ordinal: 1, RuneStart: 0, ByteStart: 0},
			{Ordinal: 3, RuneStart: 4, ByteStart: 5},
		},
	}}, got)
}

func TestEmbeddingUnitReducerSplitsSidechainsAndUsers(t *testing.T) {
	var got []EmbeddableUnit
	r := NewEmbeddingUnitReducer(0, func(unit EmbeddableUnit) error {
		got = append(got, unit)
		return nil
	})

	require.NoError(t, r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "assistant", Ordinal: 4, Content: "main",
	}))
	require.NoError(t, r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "assistant", Ordinal: 5,
		Content: "branch", Sidechain: true,
	}))
	require.NoError(t, r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "user", SourceUUID: "question-source",
		Ordinal: 6, Content: "question", Sidechain: true,
	}))
	require.NoError(t, r.Finish())

	assert.Equal(t, []EmbeddableUnit{
		{
			SessionID: "session", Kind: "run", Ordinal: 4, OrdinalEnd: 4,
			Content: "main",
			Offsets: []UnitOffset{{Ordinal: 4, RuneStart: 0, ByteStart: 0}},
		},
		{
			SessionID: "session", Kind: "run", Ordinal: 5, OrdinalEnd: 5,
			Subordinate: true, Content: "branch",
			Offsets: []UnitOffset{{Ordinal: 5, RuneStart: 0, ByteStart: 0}},
		},
		{
			SessionID: "session", Kind: "user", SourceUUID: "question-source",
			Ordinal: 6, OrdinalEnd: 6, Subordinate: true, Content: "question",
		},
	}, got)
}

func TestEmbeddingUnitReducerEnforcesJoinedByteLimit(t *testing.T) {
	t.Run("ExactLimit", func(t *testing.T) {
		var got []EmbeddableUnit
		r := NewEmbeddingUnitReducer(8, func(unit EmbeddableUnit) error {
			got = append(got, unit)
			return nil
		})

		require.NoError(t, r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "assistant", Ordinal: 1, Content: "abc",
		}))
		require.NoError(t, r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "assistant", Ordinal: 2, Content: "def",
		}))
		require.NoError(t, r.Finish())

		require.Len(t, got, 1)
		assert.Equal(t, "abc\n\ndef", got[0].Content)
	})

	t.Run("ExceededBySeparator", func(t *testing.T) {
		var got []EmbeddableUnit
		r := NewEmbeddingUnitReducer(7, func(unit EmbeddableUnit) error {
			got = append(got, unit)
			return nil
		})
		require.NoError(t, r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "assistant", Ordinal: 1, Content: "abc",
		}))

		err := r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "assistant", Ordinal: 2, Content: "def",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEmbeddingUnitTooLarge)
		finishErr := r.Finish()
		require.Error(t, finishErr)
		assert.ErrorIs(t, finishErr, ErrEmbeddingUnitTooLarge)
		assert.Empty(t, got, "the retained prefix is not a complete unit")
	})

	t.Run("UserExceeded", func(t *testing.T) {
		r := NewEmbeddingUnitReducer(2, func(EmbeddableUnit) error { return nil })

		err := r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "user", Ordinal: 1, Content: "界",
		})
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrEmbeddingUnitTooLarge)
	})
}

func TestEmbeddingUnitReducerZeroLimitIsUnlimited(t *testing.T) {
	var got []EmbeddableUnit
	r := NewEmbeddingUnitReducer(0, func(unit EmbeddableUnit) error {
		got = append(got, unit)
		return nil
	})

	require.NoError(t, r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "user", Ordinal: 1,
		Content: "content larger than any test bound",
	}))
	require.NoError(t, r.Finish())

	require.Len(t, got, 1)
	assert.Equal(t, "content larger than any test bound", got[0].Content)
}

func TestEmbeddingUnitReducerPreservesEmitError(t *testing.T) {
	want := errors.New("stop emitting")
	r := NewEmbeddingUnitReducer(0, func(EmbeddableUnit) error { return want })

	err := r.Push(EmbeddingUnitRow{
		SessionID: "session", Role: "user", Ordinal: 1, Content: "question",
	})
	assert.ErrorIs(t, err, want)
}

// Both deferred assistant paths must return the exact callback error so callers
// retain its identity when classifying the failed unit publication.
func TestEmbeddingUnitReducerPreservesAssistantEmitError(t *testing.T) {
	want := errors.New("stop assistant emission")

	t.Run("finish", func(t *testing.T) {
		r := NewEmbeddingUnitReducer(0, func(EmbeddableUnit) error { return want })
		require.NoError(t, r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "assistant", Ordinal: 1, Content: "answer",
		}))

		assert.Same(t, want, r.Finish())
	})

	t.Run("semantic_boundary", func(t *testing.T) {
		r := NewEmbeddingUnitReducer(0, func(EmbeddableUnit) error { return want })
		require.NoError(t, r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "assistant", Ordinal: 1, Content: "answer",
		}))

		err := r.Push(EmbeddingUnitRow{
			SessionID: "session", Role: "user", Ordinal: 2, Content: "next question",
		})
		assert.Same(t, want, err)
	})
}
