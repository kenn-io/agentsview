package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

type deadlineSearchStore struct {
	db.Store
	phase            string
	semanticBounded  bool
	searched         chan struct{}
	readinessEntered chan struct{}
	readinessRelease chan struct{}
	readinessOnce    sync.Once
}

func (s *deadlineSearchStore) ReadOnly() bool    { return false }
func (s *deadlineSearchStore) HasSemantic() bool { return false }

func (s *deadlineSearchStore) SemanticReadiness(ctx context.Context) (db.SemanticReadiness, error) {
	if s.readinessEntered != nil {
		s.readinessOnce.Do(func() { close(s.readinessEntered) })
		select {
		case <-s.readinessRelease:
		case <-ctx.Done():
			return db.SemanticReadiness{}, ctx.Err()
		}
	}
	return db.SemanticReadiness{}, nil
}

func (s *deadlineSearchStore) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("machine lookup escaped the deadline")
}

func (s *deadlineSearchStore) GetMachineAliases(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (s *deadlineSearchStore) SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error) {
	_, s.semanticBounded = ctx.Deadline()
	if s.phase == "mutex" {
		close(s.searched)
		return db.ContentSearchPage{}, nil
	}
	db.SetContentScanDeadline(ctx, time.Now().Add(-time.Second))
	if s.phase == "context" {
		return db.ContentSearchPage{Matches: []db.ContentMatch{{SessionID: "s1", Ordinal: 1}}}, nil
	}
	return db.ContentSearchPage{}, nil
}

func (s *deadlineSearchStore) GetMessagesWindow(ctx context.Context, _ string, _ db.MessageWindow) ([]db.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("context read escaped the deadline")
}

func TestLocalContentSearchDeadlineCoversReads(t *testing.T) {
	for _, tc := range []struct{ name, mode, phase string }{
		{"machine lookup", "substring", "machine"},
		{"message context", "substring", "context"},
		{"hybrid message context", "hybrid", "context"},
		{"readiness", "substring", "readiness"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := dbtest.OpenTestDB(t)
			store := &deadlineSearchStore{phase: tc.phase}
			backend := &directBackend{db: store, local: d}
			ctx := db.WithContentScanBudget(t.Context())
			if tc.phase == "machine" {
				db.SetContentScanDeadline(ctx, time.Now().Add(-time.Second))
			}
			req := ContentSearchRequest{Pattern: "needle", Mode: tc.mode}
			if tc.phase == "machine" {
				req.Machine = "machine-a"
			}
			if tc.phase == "context" {
				req.Context = 1
			}
			_, err := backend.SearchContent(ctx, req)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			if tc.mode == "hybrid" {
				assert.False(t, store.semanticBounded)
			}
		})
	}
}

func TestLocalContentSearchDeadlineWhileReadinessBusy(t *testing.T) {
	d := dbtest.OpenTestDB(t)
	store := &deadlineSearchStore{phase: "mutex", searched: make(chan struct{}), readinessEntered: make(chan struct{}), readinessRelease: make(chan struct{})}
	backend := &directBackend{db: store, local: d}
	readinessDone := make(chan struct{})
	go func() {
		_, _ = backend.memoryStatusForSearch(t.Context())
		close(readinessDone)
	}()
	<-store.readinessEntered
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := backend.SearchContent(ctx, ContentSearchRequest{Pattern: "needle", Mode: "substring"})
		done <- err
	}()
	<-store.searched
	cancel()
	select {
	case err := <-done:
		close(store.readinessRelease)
		<-readinessDone
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		close(store.readinessRelease)
		<-readinessDone
		<-done
		require.FailNow(t, "search waited for a busy readiness cache after its deadline")
	}
}
