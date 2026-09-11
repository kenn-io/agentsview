package db

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrEmbeddingUnitTooLarge reports that one normalized embedding unit exceeds
// the reducer's configured byte limit.
var ErrEmbeddingUnitTooLarge = errors.New("embedding unit too large")

// EmbeddingUnitRow is one already-filtered source row in embedding stream
// order. Rows from one session must be ordered by ordinal, and sessions must
// be contiguous. Filtering system and otherwise ineligible rows before Push
// makes those rows invisible to conversation-unit boundaries.
type EmbeddingUnitRow struct {
	SessionID          string
	Role               string
	SourceUUID         string
	Ordinal            int
	Content            string
	Sidechain          bool
	SubordinateSession bool
}

// EmbeddingUnitReducer reduces ordered source rows into user messages and
// assistant runs. Callers may feed any number of transport pages through Push;
// Finish marks the logical end of the complete stream.
type EmbeddingUnitReducer struct {
	maxBytes int64
	emit     func(EmbeddableUnit) error

	haveSession bool
	sessionID   string
	run         *embeddingUnitRun
	failed      error
}

type embeddingUnitRun struct {
	sessionID   string
	sourceUUID  string
	ordinal     int
	ordinalEnd  int
	subordinate bool
	sidechain   bool
	content     strings.Builder
	runeCount   int
	offsets     []UnitOffset
}

// NewEmbeddingUnitReducer returns a streaming conversation-unit reducer.
// maxBytes bounds the UTF-8 byte length of normalized Content, including the
// separators between assistant messages. A zero limit preserves the unlimited
// local SQLite behavior.
func NewEmbeddingUnitReducer(
	maxBytes int64, emit func(EmbeddableUnit) error,
) *EmbeddingUnitReducer {
	return &EmbeddingUnitReducer{maxBytes: maxBytes, emit: emit}
}

// Push adds one already-filtered row. It emits a user unit immediately and
// retains at most one assistant run until a semantic boundary or Finish.
func (r *EmbeddingUnitReducer) Push(row EmbeddingUnitRow) error {
	if r.failed != nil {
		return r.failed
	}
	newSession := r.haveSession && row.SessionID != r.sessionID
	if err := r.closeRunIf(newSession); err != nil {
		return err
	}
	r.haveSession = true
	r.sessionID = row.SessionID

	if row.Role == "user" {
		if err := r.closeRun(); err != nil {
			return err
		}
		if err := r.checkSize(row, int64(len(row.Content))); err != nil {
			return err
		}
		return r.emit(EmbeddableUnit{
			SessionID:   row.SessionID,
			Kind:        "user",
			SourceUUID:  row.SourceUUID,
			Ordinal:     row.Ordinal,
			OrdinalEnd:  row.Ordinal,
			Subordinate: row.SubordinateSession || row.Sidechain,
			Content:     row.Content,
		})
	}

	sidechainFlip := r.run != nil && row.Sidechain != r.run.sidechain
	if err := r.closeRunIf(sidechainFlip); err != nil {
		return err
	}

	additionalBytes := int64(len(row.Content))
	if r.run != nil {
		additionalBytes += int64(len("\n\n"))
	}
	if err := r.checkAdditionalSize(row, additionalBytes); err != nil {
		return err
	}
	r.appendAssistant(row)
	return nil
}

// Finish flushes the assistant run left open at the logical end of the stream.
func (r *EmbeddingUnitReducer) Finish() error {
	if r.failed != nil {
		return r.failed
	}
	return r.closeRun()
}

func (r *EmbeddingUnitReducer) closeRunIf(condition bool) error {
	if !condition {
		return nil
	}
	return r.closeRun()
}

func (r *EmbeddingUnitReducer) closeRun() error {
	if r.run == nil {
		return nil
	}
	run := r.run
	r.run = nil
	return r.emit(EmbeddableUnit{
		SessionID:   run.sessionID,
		Kind:        "run",
		SourceUUID:  run.sourceUUID,
		Ordinal:     run.ordinal,
		OrdinalEnd:  run.ordinalEnd,
		Subordinate: run.subordinate,
		Content:     run.content.String(),
		Offsets:     run.offsets,
	})
}

func (r *EmbeddingUnitReducer) appendAssistant(row EmbeddingUnitRow) {
	if r.run == nil {
		r.run = &embeddingUnitRun{
			sessionID:   row.SessionID,
			sourceUUID:  row.SourceUUID,
			ordinal:     row.Ordinal,
			ordinalEnd:  row.Ordinal,
			subordinate: row.SubordinateSession || row.Sidechain,
			sidechain:   row.Sidechain,
		}
	}
	if len(r.run.offsets) > 0 {
		r.run.content.WriteString("\n\n")
		r.run.runeCount += 2
	}
	r.run.offsets = append(r.run.offsets, UnitOffset{
		Ordinal: row.Ordinal, RuneStart: r.run.runeCount,
		ByteStart: r.run.content.Len(),
	})
	r.run.content.WriteString(row.Content)
	r.run.runeCount += utf8.RuneCountInString(row.Content)
	r.run.ordinalEnd = row.Ordinal
}

func (r *EmbeddingUnitReducer) checkAdditionalSize(
	row EmbeddingUnitRow, additionalBytes int64,
) error {
	currentBytes := int64(0)
	if r.run != nil {
		currentBytes = int64(r.run.content.Len())
	}
	if r.maxBytes <= 0 || additionalBytes <= r.maxBytes-currentBytes {
		return nil
	}
	return r.tooLarge(row, currentBytes+additionalBytes)
}

func (r *EmbeddingUnitReducer) checkSize(
	row EmbeddingUnitRow, size int64,
) error {
	if r.maxBytes <= 0 || size <= r.maxBytes {
		return nil
	}
	return r.tooLarge(row, size)
}

func (r *EmbeddingUnitReducer) tooLarge(
	row EmbeddingUnitRow, size int64,
) error {
	r.failed = fmt.Errorf(
		"%w: session %q unit at ordinal %d is %d bytes (limit %d)",
		ErrEmbeddingUnitTooLarge, row.SessionID, row.Ordinal, size, r.maxBytes,
	)
	r.run = nil
	return r.failed
}
