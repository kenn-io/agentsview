package server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/service"
)

type rawUsageStore struct {
	db.Store
	t                       *testing.T
	exactErr, suffixErr     error
	suffixCalls, usageCalls int
	matches                 []string
	retryErr                error
	retry                   bool
	rawID, canonicalID      string
	retryUsage              *db.SessionUsage
	wantBreakdown           bool
}

func (s *rawUsageStore) GetSessionUsage(ctx context.Context, id string, breakdown bool) (*db.SessionUsage, error) {
	s.usageCalls++
	require.LessOrEqual(s.t, s.usageCalls, 2)
	assert.Equal(s.t, s.wantBreakdown, breakdown)
	rawID, canonicalID := s.expectedIDs()
	if s.usageCalls == 2 {
		require.True(s.t, s.retry)
		require.Equal(s.t, canonicalID, id)
		return s.retryUsage, s.retryErr
	}
	require.Equal(s.t, rawID, id)
	return nil, s.exactErr
}

func (s *rawUsageStore) FindSessionIDsByRawSuffix(ctx context.Context, id string, limit int) ([]string, error) {
	s.suffixCalls++
	rawID, _ := s.expectedIDs()
	assert.Equal(s.t, rawID, id)
	assert.Equal(s.t, 5, limit)
	assert.Equal(s.t, "request", ctx.Value(rawUsageContextKey{}))
	return s.matches, s.suffixErr
}

func (s *rawUsageStore) expectedIDs() (string, string) {
	if s.rawID == "" {
		return "native", "codex:native"
	}
	return s.rawID, s.canonicalID
}

func FuzzSessionUsageRawFallback(f *testing.F) {
	f.Add([]byte{0x01, 0xab}, false)
	f.Add([]byte{0xff}, true)
	f.Fuzz(func(t *testing.T, native []byte, hosted bool) {
		native = native[:min(len(native), 32)]
		rawID := "native-" + hex.EncodeToString(native)
		canonicalID := "codex:" + rawID
		if hosted {
			canonicalID = "archive~" + canonicalID
		}
		store := &rawUsageStore{
			t: t, rawID: rawID, canonicalID: canonicalID,
			matches: []string{canonicalID}, retry: true, wantBreakdown: true,
			retryUsage: &db.SessionUsage{SessionID: canonicalID, Agent: "codex", TotalOutputTokens: 123, HasTokenData: true},
		}
		srv := &Server{db: store, sessions: service.NewReadOnlyBackend(store)}
		input := &sessionUsageInput{ID: rawID, Breakdown: true}
		response, err := srv.humaSessionUsage(context.WithValue(t.Context(), rawUsageContextKey{}, "request"), input)
		require.NoError(t, err)
		require.NotNil(t, response)
		assert.Equal(t, canonicalID, response.Body.SessionID)
		assert.Equal(t, 123, response.Body.TotalOutputTokens)
		assert.Equal(t, 1, store.suffixCalls)
		assert.Equal(t, 2, store.usageCalls)
		assert.Equal(t, rawID, input.ID)
	})
}

type rawUsageContextKey struct{}

func TestSessionUsageRawResolverErrors(t *testing.T) {
	for _, tc := range []struct {
		name                string
		exactErr, suffixErr error
		status, calls       int
	}{
		{"query", nil, errors.New("unavailable"), 500, 1},
		{"canceled", nil, fmt.Errorf("resolver: %w", context.Canceled), 500, 1},
		{"deadline", nil, fmt.Errorf("resolver: %w", context.DeadlineExceeded), 504, 1},
		{"exact query", errors.New("unavailable"), nil, 500, 0},
		{"exact canceled", fmt.Errorf("query: %w", context.Canceled), nil, 500, 0},
		{"exact deadline", fmt.Errorf("query: %w", context.DeadlineExceeded), nil, 504, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &rawUsageStore{t: t, exactErr: tc.exactErr, suffixErr: tc.suffixErr}
			srv := &Server{db: store, sessions: service.NewReadOnlyBackend(store)}
			_, err := srv.humaSessionUsage(context.WithValue(t.Context(), rawUsageContextKey{}, "request"), &sessionUsageInput{ID: "native"})
			var status huma.StatusError
			require.ErrorAs(t, err, &status)
			assert.Equal(t, tc.status, status.GetStatus())
			assert.Equal(t, tc.calls, store.suffixCalls)
			assert.Equal(t, 1, store.usageCalls)
			if tc.status == 500 {
				var usageErr *sessionUsageError
				require.ErrorAs(t, err, &usageErr)
				assert.Equal(t, "usage_query_failed", usageErr.Body.Code)
			}
		})
	}
}

type canonicalUsageStore struct {
	db.Store
	t        *testing.T
	id       string
	exactErr error
}

func (s canonicalUsageStore) GetSessionUsage(_ context.Context, id string, _ bool) (*db.SessionUsage, error) {
	assert.Equal(s.t, s.id, id)
	return nil, s.exactErr
}

func (s canonicalUsageStore) FindSessionIDsByRawSuffix(context.Context, string, int) ([]string, error) {
	require.FailNow(s.t, "canonical ID must not resolve")
	return nil, nil
}

func TestSessionUsageCanonicalMissSkipsResolver(t *testing.T) {
	for _, id := range []string{"codex:missing", "freebuff:missing", "host~missing"} {
		t.Run(id, func(t *testing.T) {
			store := canonicalUsageStore{t: t, id: id}
			srv := &Server{db: store, sessions: service.NewReadOnlyBackend(store)}
			_, err := srv.humaSessionUsage(t.Context(), &sessionUsageInput{ID: id})
			var usageErr *sessionUsageError
			require.ErrorAs(t, err, &usageErr)
			assert.Equal(t, 404, usageErr.Status)
		})
	}
}

func TestSessionUsageRawCanonicalRetryStops(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"missing", nil, 404}, {"failed", errors.New("unavailable"), 500}, {"deadline", context.DeadlineExceeded, 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &rawUsageStore{t: t, matches: []string{"codex:native"}, retry: true, retryErr: tc.err}
			srv := &Server{db: store, sessions: service.NewReadOnlyBackend(store)}
			input := &sessionUsageInput{ID: "native"}
			_, err := srv.humaSessionUsage(context.WithValue(t.Context(), rawUsageContextKey{}, "request"), input)
			var status huma.StatusError
			require.ErrorAs(t, err, &status)
			assert.Equal(t, tc.status, status.GetStatus())
			assert.Equal(t, 1, store.suffixCalls)
			assert.Equal(t, 2, store.usageCalls)
			assert.Equal(t, "native", input.ID)
		})
	}
}

func TestSessionUsageExactAmbiguityBoundsCandidates(t *testing.T) {
	candidates := []string{"a~native", "b~native", "c~native", "d~native", "e~native", "f~native"}
	store := canonicalUsageStore{t: t, id: "host~native", exactErr: fmt.Errorf("lookup: %w", &db.SessionIdentityError{State: "ambiguous", Variants: candidates})}
	srv := &Server{db: store, sessions: service.NewReadOnlyBackend(store)}
	_, err := srv.humaSessionUsage(t.Context(), &sessionUsageInput{ID: "host~native"})
	var usageErr *sessionUsageError
	require.ErrorAs(t, err, &usageErr)
	assert.Equal(t, 409, usageErr.Status)
	assert.Equal(t, "ambiguous_session_id", usageErr.Body.Code)
	assert.Equal(t, []string{"a~native", "b~native", "c~native", "d~native", "e~native"}, usageErr.Body.Candidates)
}
