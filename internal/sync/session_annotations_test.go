package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

func TestSessionAnnotationWritesDuringSync(t *testing.T) {
	fx := newEngineFixture(t)
	for _, id := range []string{"manager", "worker"} {
		require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{
			ID: id, Project: "proj", Machine: "local",
			Agent: string(parser.AgentClaude), MessageCount: 1,
		}))
	}

	held := make(chan struct{})
	release := make(chan struct{})
	releaseLock := sync.OnceFunc(func() { close(release) })
	exclusive := make(chan error, 1)
	go func() {
		exclusive <- fx.engine.RunExclusive(func() error {
			close(held)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() {
		releaseLock()
		require.NoError(t, <-exclusive)
	})
	<-held

	for _, tc := range []struct {
		name    string
		write   func(context.Context) error
		labels  []string
		parent  string
		project string
	}{
		{"set labels", func(ctx context.Context) error {
			_, err := fx.engine.SetSessionLabels(ctx, "worker", []string{"first"})
			return err
		}, []string{"first"}, "", "proj"},
		{"update labels", func(ctx context.Context) error {
			_, err := fx.engine.UpdateSessionLabels(ctx, "worker", []string{"second"}, []string{"first"})
			return err
		}, []string{"second"}, "", "proj"},
		{"set parent", func(ctx context.Context) error {
			_, err := fx.engine.SetSessionExternalParent(ctx, "worker", "manager")
			return err
		}, []string{"second"}, "manager", "proj"},
		{"clear parent", func(ctx context.Context) error {
			_, err := fx.engine.ClearSessionExternalParent(ctx, "worker")
			return err
		}, []string{"second"}, "", "proj"},
		{"assign project", func(ctx context.Context) error {
			_, err := fx.engine.AssignSessionProject(ctx, "worker", "assigned")
			return err
		}, []string{"second"}, "", "assigned"},
		{"clear project", func(ctx context.Context) error {
			_, err := fx.engine.ClearSessionProjectAssignment(ctx, "worker")
			return err
		}, []string{"second"}, "", "proj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- tc.write(ctx) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				cancel()
				releaseLock()
				<-done
				require.FailNow(t, "annotation waited for the sync pass")
			}
			session, err := fx.db.GetSession(t.Context(), "worker")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, tc.labels, session.Labels)
			assert.Equal(t, tc.project, session.Project)
			if tc.parent == "" {
				assert.Nil(t, session.ParentSessionID)
			} else {
				require.NotNil(t, session.ParentSessionID)
				assert.Equal(t, tc.parent, *session.ParentSessionID)
			}
		})
	}
}

func TestSessionAnnotationWritesIgnoreRebuildSwap(t *testing.T) {
	fx := newEngineFixture(t)
	replacement := openTestDB(t)
	for _, database := range []*db.DB{fx.db, replacement} {
		for _, id := range []string{"manager", "worker"} {
			require.NoError(t, database.UpsertSession(t.Context(), db.Session{
				ID: id, Project: "proj", Machine: "local",
				Agent: string(parser.AgentClaude), MessageCount: 1,
			}))
		}
	}
	// In-process rebuilds temporarily point the engine at the replacement.
	fx.engine.db = replacement
	for _, tc := range []struct {
		name    string
		write   func(context.Context) error
		labels  []string
		parent  *string
		project string
	}{
		{"set labels", func(ctx context.Context) error {
			_, err := fx.engine.SetSessionLabels(ctx, "worker", []string{"first"})
			return err
		}, []string{"first"}, nil, "proj"},
		{"update labels", func(ctx context.Context) error {
			_, err := fx.engine.UpdateSessionLabels(ctx, "worker", []string{"second"}, []string{"first"})
			return err
		}, []string{"second"}, nil, "proj"},
		{"set parent", func(ctx context.Context) error {
			_, err := fx.engine.SetSessionExternalParent(ctx, "worker", "manager")
			return err
		}, []string{"second"}, new("manager"), "proj"},
		{"clear parent", func(ctx context.Context) error {
			_, err := fx.engine.ClearSessionExternalParent(ctx, "worker")
			return err
		}, []string{"second"}, nil, "proj"},
		{"assign project", func(ctx context.Context) error {
			_, err := fx.engine.AssignSessionProject(ctx, "worker", "assigned")
			return err
		}, []string{"second"}, nil, "assigned"},
		{"clear project", func(ctx context.Context) error {
			_, err := fx.engine.ClearSessionProjectAssignment(ctx, "worker")
			return err
		}, []string{"second"}, nil, "proj"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, tc.write(t.Context()))
			session, err := fx.db.GetSession(t.Context(), "worker")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, tc.labels, session.Labels)
			assert.Equal(t, tc.parent, session.ParentSessionID)
			assert.Equal(t, tc.project, session.Project)

			session, err = replacement.GetSession(t.Context(), "worker")
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Empty(t, session.Labels)
			assert.Nil(t, session.ParentSessionID)
			assert.Equal(t, "proj", session.Project)
			assert.False(t, session.ProjectAssigned)
		})
	}

	t.Run("wait for archive writer", func(t *testing.T) {
		require.NoError(t, fx.db.CloseWriter())
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := fx.engine.SetSessionLabels(ctx, "worker", []string{"queued"})
			done <- err
		}()
		select {
		case err := <-done:
			require.FailNow(t, "annotation followed the open replacement writer", "error: %v", err)
		case <-time.After(200 * time.Millisecond): //nolint:kennlint // deadlinetest: assert no return while the archive writer is closed.
		}
		err := fx.db.ReopenWriter()
		if err != nil {
			cancel()
		}
		select {
		case writeErr := <-done:
			require.NoError(t, err)
			require.NoError(t, writeErr)
		case <-time.After(5 * time.Second):
			cancel()
			<-done
			require.FailNow(t, "annotation kept waiting after the archive writer reopened")
		}
		session, err := fx.db.GetSession(t.Context(), "worker")
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, []string{"queued"}, session.Labels)
		session, err = replacement.GetSession(t.Context(), "worker")
		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Empty(t, session.Labels)
	})
}

func TestSessionAnnotationWaitsForWriter(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cancelWrite    bool
		closeAfterWait bool
	}{
		{name: "reopen"},
		{name: "cancel", cancelWrite: true},
		{name: "close after wait/reopen", closeAfterWait: true},
		{name: "close after wait/cancel", closeAfterWait: true, cancelWrite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newEngineFixture(t)
			if !tc.closeAfterWait {
				require.NoError(t, fx.db.CloseWriter())
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			closed := make(chan error, 1)
			done := make(chan error, 1)
			go func() {
				closeAfterWait := tc.closeAfterWait
				write := func() (db.SessionLabels, error) {
					// Close the real writer once, after WaitForWriter returned,
					// to force a handoff before the transaction begins.
					if closeAfterWait {
						closeAfterWait = false
						err := fx.db.CloseWriter()
						closed <- err
						if err != nil {
							return db.SessionLabels{}, err
						}
					}
					return fx.db.UpdateSessionLabels(ctx, "worker", []string{"queued"}, nil)
				}
				_, err := runSessionAnnotationWrite(ctx, fx.engine, write, sessionLabelsChanged)
				done <- err
			}()
			if tc.closeAfterWait {
				select {
				case err := <-closed:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					cancel()
					<-done
					require.FailNow(t, "annotation did not reach the writer handoff")
				}
			}
			select {
			case err := <-done:
				require.FailNow(t, "annotation returned before the writer reopened", "error: %v", err)
			case <-time.After(200 * time.Millisecond): //nolint:kennlint // deadlinetest: assert no return while the writer is closed.
			}
			if tc.cancelWrite {
				cancel()
			} else {
				require.NoError(t, fx.db.ReopenWriter())
			}
			select {
			case err := <-done:
				if tc.cancelWrite {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(5 * time.Second):
				cancel()
				<-done
				require.FailNow(t, "annotation kept waiting after reopen or cancellation")
			}
			if tc.cancelWrite {
				require.NoError(t, fx.db.ReopenWriter())
			}
			require.NoError(t, fx.db.UpsertSession(t.Context(), db.Session{
				ID: "worker", Project: "proj", Machine: "local",
				Agent: string(parser.AgentClaude), MessageCount: 1,
			}))
			session, err := fx.db.GetSession(t.Context(), "worker")
			require.NoError(t, err)
			require.NotNil(t, session)
			if tc.cancelWrite {
				assert.Empty(t, session.Labels, "canceled edits must stay unwritten after reopen")
			} else {
				assert.Equal(t, []string{"queued"}, session.Labels)
			}
		})
	}
}

func TestSessionAnnotationWritesEmitOnlyOnChange(t *testing.T) {
	fx := newEngineFixture(t)
	emitter := &fakeEmitter{}
	fx.engineWithEmitter(t.Context(), emitter)
	ctx := t.Context()
	for _, id := range []string{"manager", "worker"} {
		require.NoError(t, fx.db.UpsertSession(ctx, db.Session{
			ID: id, Project: "proj", Machine: "local",
			Agent: string(parser.AgentClaude), MessageCount: 1,
		}))
	}
	expectEmits := func(t *testing.T, want int, step string) {
		t.Helper()
		got := emitter.got()
		assert.Len(t, got, want, step)
		emitter.mu.Lock()
		emitter.scopes = nil
		emitter.mu.Unlock()
	}

	_, err := fx.engine.UpdateSessionLabels(ctx, "worker", []string{"ticket=A"}, nil)
	require.NoError(t, err)
	expectEmits(t, 1, "adding a label")
	_, err = fx.engine.UpdateSessionLabels(ctx, "worker", []string{"ticket=A"}, nil)
	require.NoError(t, err)
	expectEmits(t, 0, "re-adding the same label")
	_, err = fx.engine.SetSessionLabels(ctx, "worker", []string{"ticket=A"})
	require.NoError(t, err)
	expectEmits(t, 0, "replacing labels with the same set")
	_, err = fx.engine.UpdateSessionLabels(ctx, "worker", nil, []string{"missing"})
	require.NoError(t, err)
	expectEmits(t, 0, "removing an absent label")
	_, err = fx.engine.SetSessionLabels(ctx, "worker", nil)
	require.NoError(t, err)
	expectEmits(t, 1, "clearing the last label")
	_, err = fx.engine.SetSessionLabels(ctx, "not-synced", []string{"ticket=A"})
	require.NoError(t, err)
	expectEmits(t, 0, "labeling a session that has not synced")

	_, err = fx.engine.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	expectEmits(t, 1, "applying a parent link")
	_, err = fx.engine.SetSessionExternalParent(ctx, "worker", "manager")
	require.NoError(t, err)
	expectEmits(t, 0, "re-recording the same parent link")
	_, err = fx.engine.SetSessionExternalParent(ctx, "pending", "manager")
	require.NoError(t, err)
	expectEmits(t, 0, "linking a session that has not synced")
	_, err = fx.engine.ClearSessionExternalParent(ctx, "worker")
	require.NoError(t, err)
	expectEmits(t, 1, "clearing an applied parent link")
}
