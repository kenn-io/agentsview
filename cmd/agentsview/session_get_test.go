package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

func TestPrintSessionDetailShowsSecretLeak(t *testing.T) {
	d := &service.SessionDetail{}
	d.ID = "s1"
	d.SecretLeakCount = 3
	var buf bytes.Buffer
	require.NoError(t, printSessionDetailHuman(&buf, d))
	out := buf.String()
	assert.Contains(t, out, "Secrets")
	assert.Contains(t, out, "3")
}

func TestPrintSessionDetailHidesZeroSecretLeak(t *testing.T) {
	d := &service.SessionDetail{}
	d.ID = "s1"
	d.SecretLeakCount = 0
	var buf bytes.Buffer
	require.NoError(t, printSessionDetailHuman(&buf, d))
	assert.NotContains(t, buf.String(), "Secrets",
		"clean session should not show a Secrets line")
}

// stubPartialService implements service.SessionService and records the
// arguments passed to FindSessionIDsByPartial. All other methods panic;
// tests that call resolveBareCodebuffIDRemote must not exercise them.
type stubPartialService struct {
	// findResult is returned by FindSessionIDsByPartial.
	findResult []string
	// findErr is returned by FindSessionIDsByPartial when non-nil.
	findErr error
	// findCalls records every (partial, limit) pair passed to
	// FindSessionIDsByPartial so tests can assert the limit.
	findCalls []stubPartialCall
}

type stubPartialCall struct {
	Partial string
	Limit   int
}

func (s *stubPartialService) FindSessionIDsByPartial(
	_ context.Context, partial string, limit int,
) ([]string, error) {
	s.findCalls = append(s.findCalls, stubPartialCall{
		Partial: partial,
		Limit:   limit,
	})
	return s.findResult, s.findErr
}

// Stubs for the remaining SessionService methods — never called by
// resolveBareCodebuffIDRemote so they panic to surface misuse.
func (s *stubPartialService) Get(context.Context, string) (*service.SessionDetail, error) {
	panic("Get not expected")
}
func (s *stubPartialService) List(context.Context, service.ListFilter) (*service.SessionList, error) {
	panic("List not expected")
}
func (s *stubPartialService) Messages(context.Context, string, service.MessageFilter) (*service.MessageList, error) {
	panic("Messages not expected")
}
func (s *stubPartialService) ToolCalls(context.Context, string) (*service.ToolCallList, error) {
	panic("ToolCalls not expected")
}
func (s *stubPartialService) Sync(context.Context, service.SyncInput) (*service.SessionDetail, error) {
	panic("Sync not expected")
}
func (s *stubPartialService) Watch(context.Context, string) (<-chan service.Event, error) {
	panic("Watch not expected")
}
func (s *stubPartialService) Stats(context.Context, service.StatsFilter) (*service.SessionStats, error) {
	panic("Stats not expected")
}
func (s *stubPartialService) Search(context.Context, service.SearchRequest) (*service.SessionSearchResult, error) {
	panic("Search not expected")
}
func (s *stubPartialService) SearchContent(context.Context, service.ContentSearchRequest) (*service.ContentSearchResult, error) {
	panic("SearchContent not expected")
}
func (s *stubPartialService) UsageSummary(context.Context, service.UsageRequest) (*service.UsageSummaryResult, error) {
	panic("UsageSummary not expected")
}
func (s *stubPartialService) UsagePairwiseComparison(context.Context, service.UsagePairwiseComparisonRequest) (*service.UsagePairwiseComparisonResponse, error) {
	panic("UsagePairwiseComparison not expected")
}
func (s *stubPartialService) ListRecallEntries(context.Context, service.RecallFilter) (*service.RecallList, error) {
	panic("ListRecallEntries not expected")
}
func (s *stubPartialService) GetRecallEntry(context.Context, string) (*db.RecallEntry, error) {
	panic("GetRecallEntry not expected")
}
func (s *stubPartialService) QueryRecallEntries(context.Context, service.RecallQuery) (*service.RecallQueryResult, error) {
	panic("QueryRecallEntries not expected")
}
func (s *stubPartialService) ImportRecallEntries(context.Context, io.Reader, db.RecallImportOptions) (*db.RecallImportResult, error) {
	panic("ImportRecallEntries not expected")
}
func (s *stubPartialService) ListSecrets(context.Context, service.SecretListFilter) (*service.SecretFindingList, error) {
	panic("ListSecrets not expected")
}
func (s *stubPartialService) ScanSecrets(context.Context, service.SecretScanInput, func(service.SecretScanProgress)) (*service.SecretScanSummary, error) {
	panic("ScanSecrets not expected")
}

func TestResolveBareCodebuffIDRemote_SingleMatch(t *testing.T) {
	t.Parallel()
	svc := &stubPartialService{
		findResult: []string{"codebuff:myproject:1704067200"},
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.NoError(t, err)
	assert.Equal(t, "codebuff:myproject:1704067200", got)
	require.Len(t, svc.findCalls, 1)
	assert.Equal(t, "1704067200", svc.findCalls[0].Partial)
	assert.Equal(t, codebuffRemoteLookupLimit, svc.findCalls[0].Limit)
}

func TestResolveBareCodebuffIDRemote_FreebuffMatch(t *testing.T) {
	t.Parallel()
	svc := &stubPartialService{
		findResult: []string{"freebuff:proj:1704067200"},
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.NoError(t, err)
	assert.Equal(t, "freebuff:proj:1704067200", got)
}

func TestResolveBareCodebuffIDRemote_NoMatch(t *testing.T) {
	t.Parallel()
	svc := &stubPartialService{
		findResult: []string{
			"codex:1704067200",       // different agent prefix
			"codebuff:proj:other",    // different suffix
		},
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.NoError(t, err)
	assert.Empty(t, got, "non-Codebuff/Freebuff matches must be ignored")
}

func TestResolveBareCodebuffIDRemote_EmptyResult(t *testing.T) {
	t.Parallel()
	svc := &stubPartialService{
		findResult: nil,
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestResolveBareCodebuffIDRemote_Ambiguous(t *testing.T) {
	t.Parallel()
	svc := &stubPartialService{
		findResult: []string{
			"codebuff:projA:1704067200",
			"freebuff:projB:1704067200",
		},
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "ambiguous")
	assert.Contains(t, err.Error(), "2")
	assert.Contains(t, err.Error(), "codebuff:projA:1704067200")
	assert.Contains(t, err.Error(), "freebuff:projB:1704067200")
}

func TestResolveBareCodebuffIDRemote_PropagatesError(t *testing.T) {
	t.Parallel()
	svc := &stubPartialService{
		findErr: errors.New("connection refused"),
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.Error(t, err)
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "connection refused")
}

func TestResolveBareCodebuffIDRemote_FiltersNonCodebuff(t *testing.T) {
	t.Parallel()
	// Matches that end with the raw ID but have wrong agent prefix.
	svc := &stubPartialService{
		findResult: []string{
			"codex:someproject:1704067200",
			"claude:1704067200",
			"codebuff:mine:1704067200",
		},
	}
	got, err := resolveBareCodebuffIDRemote(
		context.Background(), svc, "1704067200",
	)
	require.NoError(t, err)
	assert.Equal(t, "codebuff:mine:1704067200", got)
}
