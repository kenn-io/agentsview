package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/fileevidence"
	"go.kenn.io/agentsview/internal/integrations/docbank"
)

type referenceResolver struct {
	calls  int
	lookup func(context.Context, docbank.Lookup) (docbank.Page, error)
}

func (r *referenceResolver) Lookup(ctx context.Context, q docbank.Lookup) (docbank.Page, error) {
	r.calls++
	return r.lookup(ctx, q)
}
func (*referenceResolver) Close() error { return nil }
func referenceDB(t *testing.T, n int) *db.DB {
	t.Helper()
	d := dbtest.OpenTestDB(t)
	require.NoError(t, d.UpsertSession(t.Context(), db.Session{ID: "example", Agent: "pi", Project: "example"}))
	messages := make([]db.Message, n)
	for i := range messages {
		messages[i] = db.Message{SessionID: "example", Ordinal: i, Role: "assistant", Content: "write", FileEvidenceCoverage: fileevidence.Coverage{Version: 1, State: "complete"}, ToolCalls: []db.ToolCall{{ToolName: "write", Category: "Write", InputJSON: `{"path":"example.txt","content":"alpha\n"}`, ResultContent: "ok"}}, FileEvidenceCandidates: []fileevidence.Candidate{{Record: fileevidence.Record{Position: fileevidence.Position{Entry: strconv.Itoa(i), Call: 0, ResultEntry: "result", Result: 0, Block: 0}, Kind: "write", ProofMethod: fileevidence.ProofPiWriteUTF8V1, Retention: "source", Size: new(int64(6)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("alpha\n")))}, Payload: []byte("alpha\n"), Support: "input", SupportingText: `{"path":"example.txt","content":"alpha\n"}`, Call: 0}}}
	}
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "example", messages))
	return d
}

func TestFileReferencesMissingDatabaseIDPreservesTranscript(t *testing.T) {
	for _, query := range []string{"DELETE FROM archive_metadata WHERE key='database_id'", "UPDATE archive_metadata SET value='' WHERE key='database_id'"} {
		t.Run(query, func(t *testing.T) {
			d := referenceDB(t, 1)
			writer, err := sql.Open("sqlite3", d.Path())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, writer.Close()) })
			_, err = writer.ExecContext(t.Context(), query)
			require.NoError(t, err)
			svc := NewReadOnlyBackend(d)
			messages, err := svc.Messages(t.Context(), "example", MessageFilter{})
			require.NoError(t, err)
			require.Len(t, messages.Messages, 1)
			assert.Equal(t, "write", messages.Messages[0].Content)
			assert.Empty(t, messages.Binding)
			assert.Empty(t, messages.FileEvidence)
			_, err = d.ReadFileEvidenceSnapshot(t.Context(), "example", fileevidence.Window{})
			require.ErrorIs(t, err, db.ErrDatabaseIDMissing)
			_, err = svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
			require.ErrorIs(t, err, ErrFileReferencesUnavailable)
			assert.NotErrorIs(t, err, ErrFileReferencesNotFound)
		})
	}
}

func TestFileReferencesAdmissionAndLocalProjection(t *testing.T) {
	d := referenceDB(t, 2)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	resolver := &referenceResolver{lookup: func(_ context.Context, q docbank.Lookup) (docbank.Page, error) {
		return docbank.Page{State: "not_found", CheckedAt: &now, ReferenceTotal: new(0)}, nil
	}}
	svc := NewDirectBackend(d, nil, WithFileReferencesResolver(resolver)).(FileReferencesService)
	for _, in := range []FileReferencesInput{{SessionID: "missing"}, {SessionID: "example", Offset: 1}, {SessionID: "example", ReferenceOffset: 1}, {SessionID: "example", FirstOrdinal: new(0)}, {SessionID: "example", Limit: -1}, {SessionID: "example", Refresh: true, OccurrenceID: "x", Binding: "b", ReferenceOffset: 1}} {
		_, err := svc.FileReferences(t.Context(), in)
		require.Error(t, err)
	}
	assert.Zero(t, resolver.calls)
	result, err := svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.NoError(t, err)
	require.Len(t, result.Occurrences, 2)
	assert.Equal(t, "complete", result.Coverage)
	assert.Equal(t, "available", result.Capability)
	assert.Equal(t, "enabled", result.Integration)
	assert.Equal(t, 1, resolver.calls)
	assert.Equal(t, "not_found", result.Occurrences[0].State)
	assert.NotEqual(t, result.Occurrences[0].OccurrenceID, result.Occurrences[1].OccurrenceID)
	messages, err := NewReadOnlyBackend(d).Messages(t.Context(), "example", MessageFilter{})
	require.NoError(t, err)
	assert.Equal(t, result.Binding, messages.Binding)
	require.Len(t, messages.FileEvidence, 2)
	assert.Equal(t, 1, messages.FileEvidence[1].Ordinal)
	empty, err := NewReadOnlyBackend(d).Messages(t.Context(), "example", MessageFilter{From: new(99)})
	require.NoError(t, err)
	assert.Equal(t, result.Binding, empty.Binding)
	selected, err := svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example", Binding: result.Binding, OccurrenceID: result.Occurrences[1].OccurrenceID})
	require.NoError(t, err)
	require.Len(t, selected.Occurrences, 1)
	assert.Equal(t, 1, selected.Occurrences[0].MessageOrdinal)
	require.NoError(t, d.SetDatabaseIDForTest(t.Context(), "replacement"))
	_, err = svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example", Binding: result.Binding})
	assert.ErrorIs(t, err, ErrSourceChanged)
}

// Hides the optional capability, as replica stores do.
type (
	referenceReplica struct{ db.Store }
	referenceGuard   struct {
		db.Store
		db.FileEvidenceReader
		accessError error
		slow        bool
		reads       int
	}
)

func (s *referenceGuard) GetSession(ctx context.Context, id string) (*db.Session, error) {
	if s.accessError != nil {
		return nil, s.accessError
	}
	return s.Store.GetSession(ctx, id)
}

func (s *referenceGuard) ReadFileEvidenceSnapshot(ctx context.Context, id string, w fileevidence.Window) (fileevidence.Snapshot, error) {
	s.reads++
	if s.slow {
		<-ctx.Done()
		return fileevidence.Snapshot{}, ctx.Err()
	}
	return s.FileEvidenceReader.ReadFileEvidenceSnapshot(ctx, id, w)
}

func TestFileReferencesCapabilityAccessAndBudget(t *testing.T) {
	d := referenceDB(t, 1)
	resolver := &referenceResolver{lookup: func(context.Context, docbank.Lookup) (docbank.Page, error) {
		return docbank.Page{State: "not_found", ReferenceTotal: new(0)}, nil
	}}
	denied := errors.New("access denied")
	guard := &referenceGuard{Store: d, FileEvidenceReader: d, accessError: denied}
	_, err := NewReadOnlyBackend(guard, WithFileReferencesResolver(resolver)).FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.ErrorIs(t, err, denied)
	assert.Zero(t, guard.reads)
	assert.Zero(t, resolver.calls)
	unavailable, err := NewReadOnlyBackend(referenceReplica{d}, WithFileReferencesResolver(resolver)).FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.NoError(t, err)
	assert.Equal(t, "unavailable", unavailable.Capability)
	assert.Zero(t, resolver.calls)
	disabled, err := NewDirectBackend(d, nil).FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.NoError(t, err)
	assert.Equal(t, "disabled", disabled.Integration)
	assert.Equal(t, "unavailable", disabled.Occurrences[0].State)
	readonly, err := db.OpenReadOnly(t.Context(), d.Path())
	require.NoError(t, err)
	defer readonly.Close()
	local, err := NewReadOnlyBackend(readonly).FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.NoError(t, err)
	assert.Equal(t, disabled.Binding, local.Binding)
	assert.Equal(t, "available", local.Capability)
	guard.accessError = nil
	guard.slow = true
	start := time.Now()
	out, err := NewReadOnlyBackend(guard, WithFileReferencesResolver(resolver)).FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.ErrorIs(t, err, ErrFileReferencesUnavailable)
	assert.Nil(t, out)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Zero(t, resolver.calls)
}

func TestFileReferencesRevalidateAfterRemote(t *testing.T) {
	for _, mutation := range []string{"generation", "policy"} {
		t.Run(mutation, func(t *testing.T) {
			d := referenceDB(t, 1)
			entered, release := make(chan struct{}), make(chan struct{})
			resolver := &referenceResolver{lookup: func(context.Context, docbank.Lookup) (docbank.Page, error) {
				close(entered)
				<-release
				return docbank.Page{State: "found", ReferenceTotal: new(1), MatchOffsets: []int{0}, Matches: []docbank.Match{{NodeID: 1, VersionID: "version", SHA256: "digest", Size: 6, State: "current"}}}, nil
			}}
			svc := NewDirectBackend(d, nil, WithFileReferencesResolver(resolver))
			done := make(chan error, 1)
			go func() {
				out, err := svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
				if out != nil {
					done <- errors.New("published stale response")
					return
				}
				done <- err
			}()
			<-entered
			if mutation == "generation" {
				require.NoError(t, d.SetDatabaseIDForTest(t.Context(), "replacement"))
			} else {
				messages, err := d.GetAllMessages(t.Context(), "example")
				require.NoError(t, err)
				d.SetArchiveContent(config.ArchiveContentUsage)
				require.NoError(t, d.ReplaceSessionMessages(t.Context(), "example", messages))
			}
			close(release)
			assert.ErrorIs(t, <-done, ErrSourceChanged)
		})
	}
}

func TestFileReferencesDeferredRetryAndWindows(t *testing.T) {
	d := referenceDB(t, 3)
	attempts := 0
	resolver := &referenceResolver{lookup: func(_ context.Context, q docbank.Lookup) (docbank.Page, error) {
		assert.Equal(t, int64(6), q.Size)
		assert.Equal(t, "b6a98d9ce9a2d9149288fa3df42d377c3e42737afdcdaf714e33c0a100b51060", q.SHA256)
		attempts++
		if attempts == 1 {
			return docbank.Page{State: "deferred"}, nil
		}
		return docbank.Page{State: "not_found", ReferenceTotal: new(0)}, nil
	}}
	svc := NewDirectBackend(d, nil, WithFileReferencesResolver(resolver))
	input := FileReferencesInput{SessionID: "example", Limit: 1, FirstOrdinal: new(1), LastOrdinal: new(2)}
	first, err := svc.FileReferences(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, first.Occurrences, 1)
	assert.Equal(t, 1, first.Occurrences[0].MessageOrdinal)
	assert.Equal(t, "deferred", first.Occurrences[0].State)
	assert.Nil(t, first.Occurrences[0].ReferenceTotal)
	assert.Equal(t, 1, *first.NextOffset)
	input.Binding = first.Binding
	retry, err := svc.FileReferences(t.Context(), input)
	require.NoError(t, err)
	assert.Equal(t, first.Occurrences[0].OccurrenceID, retry.Occurrences[0].OccurrenceID)
	assert.Equal(t, "not_found", retry.Occurrences[0].State)
	input.Offset = *retry.NextOffset
	last, err := svc.FileReferences(t.Context(), input)
	require.NoError(t, err)
	require.Len(t, last.Occurrences, 1)
	assert.Equal(t, 2, last.Occurrences[0].MessageOrdinal)
	assert.Nil(t, last.NextOffset)
}

func FuzzFileReferencesPaging(f *testing.F) {
	f.Add(0, 0, 0, 0, 1)
	f.Add(-1, 100, 1, 5, 2)
	f.Fuzz(func(t *testing.T, offset, limit, reference, first, last int) {
		in, err := NormalizeFileReferencesInput(FileReferencesInput{SessionID: "example", Binding: "binding", OccurrenceID: "selected", Offset: offset, Limit: limit, ReferenceOffset: reference, FirstOrdinal: &first, LastOrdinal: &last})
		if err != nil {
			return
		}
		assert.GreaterOrEqual(t, in.Offset, 0)
		assert.Positive(t, in.Limit)
		assert.LessOrEqual(t, in.Limit, 50)
		assert.GreaterOrEqual(t, in.ReferenceOffset, 0)
		assert.GreaterOrEqual(t, *in.FirstOrdinal, 0)
		assert.GreaterOrEqual(t, *in.LastOrdinal, *in.FirstOrdinal)
	})
}

func TestFileReferencesEscapedPositionContinuation(t *testing.T) {
	d := referenceDB(t, 1)
	messages, err := d.GetAllMessages(t.Context(), "example")
	require.NoError(t, err)
	// Quotes maximize nested JSON escaping while remaining a valid 1024-byte slot.
	position := fileevidence.Position{Entry: strings.Repeat(`"`, 256), Call: 0, ResultEntry: strings.Repeat(`"`, 248) + "a", Result: 0, Block: 0}
	slot, err := position.Slot()
	require.NoError(t, err)
	require.Len(t, slot, 1024)
	messages[0].FileEvidence[0].Position = position
	require.NoError(t, d.ReplaceSessionMessages(t.Context(), "example", messages))
	svc := NewDirectBackend(d, nil)
	first, err := svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example"})
	require.NoError(t, err)
	require.Len(t, first.Occurrences, 1)
	selector := first.Occurrences[0].OccurrenceID
	require.Greater(t, len(selector), 2048)
	t.Run("database", func(t *testing.T) {
		selected, err := d.ReadFileEvidenceSnapshot(t.Context(), "example", fileevidence.Window{Binding: first.Binding, OccurrenceID: selector})
		require.NoError(t, err)
		require.Len(t, selected.Rows, 1)
		assert.Equal(t, position, selected.Rows[0].Record.Position)
	})
	t.Run("service", func(t *testing.T) {
		selected, err := svc.FileReferences(t.Context(), FileReferencesInput{SessionID: "example", Binding: first.Binding, OccurrenceID: selector})
		require.NoError(t, err)
		assert.Equal(t, first, selected)
	})
}

func TestFileReferencesSelectorLimit(t *testing.T) {
	for _, size := range []int{4096, 4097} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			_, err := NormalizeFileReferencesInput(FileReferencesInput{SessionID: "example", Binding: "bound", OccurrenceID: strings.Repeat("a", size)})
			if size == 4096 {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, ErrFileReferencesInput)
			}
		})
	}
}
