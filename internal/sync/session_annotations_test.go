package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestSessionAnnotationWritesDuringSync(t *testing.T) {
	for _, condition := range []string{"held lock", "swapped handle"} {
		t.Run(condition, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				write   func(context.Context, *Engine) error
				labels  []string
				parent  *string
				project string
			}{
				{"set labels", func(ctx context.Context, engine *Engine) error {
					_, err := engine.SetSessionLabels(ctx, "worker", []string{"first"})
					return err
				}, []string{"first"}, nil, "proj"},
				{"update labels", func(ctx context.Context, engine *Engine) error {
					_, err := engine.UpdateSessionLabels(ctx, "worker", []string{"second"}, []string{"first"})
					return err
				}, []string{"second"}, nil, "proj"},
				{"set parent", func(ctx context.Context, engine *Engine) error {
					_, err := engine.SetSessionExternalParent(ctx, "worker", "manager")
					return err
				}, []string{"second"}, new("manager"), "proj"},
				{"clear parent", func(ctx context.Context, engine *Engine) error {
					_, err := engine.ClearSessionExternalParent(ctx, "worker")
					return err
				}, []string{"second"}, nil, "proj"},
				{"assign project", func(ctx context.Context, engine *Engine) error {
					_, err := engine.AssignSessionProject(ctx, "worker", "assigned")
					return err
				}, []string{"second"}, nil, "assigned"},
				{"clear project", func(ctx context.Context, engine *Engine) error {
					_, err := engine.ClearSessionProjectAssignment(ctx, "worker")
					return err
				}, []string{"second"}, nil, "proj"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					fx := newEngineFixture(t)
					databases := []*db.DB{fx.db}
					var replacement *db.DB
					release := make(chan struct{})
					releaseLock := sync.OnceFunc(func() { close(release) })
					if condition == "swapped handle" {
						replacement = openTestDB(t)
						databases = append(databases, replacement)
						// In-process rebuilds temporarily point the engine at the replacement.
						fx.engine.db = replacement
					} else {
						held := make(chan struct{})
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
					}
					for _, database := range databases {
						for _, id := range []string{"manager", "worker"} {
							dbtest.SeedSession(t, database, id, "proj")
						}
					}
					// Seed each edit's prerequisites independently of the other cases.
					if tc.name != "set labels" {
						labels := []string{"second"}
						if tc.name == "update labels" {
							labels = []string{"first"}
						}
						_, err := fx.db.SetSessionLabels(t.Context(), "worker", labels)
						require.NoError(t, err)
					}
					if tc.name == "clear parent" {
						_, err := fx.db.SetSessionExternalParent(t.Context(), "worker", "manager")
						require.NoError(t, err)
					}
					if tc.name == "clear project" {
						_, err := fx.db.AssignSessionProject(t.Context(), "worker", "assigned")
						require.NoError(t, err)
					}
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					done := make(chan error, 1)
					finished := make(chan struct{})
					waitForArchive := replacement != nil && tc.name == "set labels"
					if waitForArchive {
						require.NoError(t, fx.db.CloseWriter())
					}
					go func() {
						defer close(finished)
						done <- tc.write(ctx, fx.engine)
					}()
					t.Cleanup(func() {
						cancel()
						releaseLock()
						<-finished
					})
					if waitForArchive {
						select {
						case err := <-done:
							require.FailNow(t, "annotation returned while the archive writer was closed", "error: %v", err)
						case <-time.After(200 * time.Millisecond): //nolint:kennlint // deadlinetest: assert no return while the archive writer is closed.
						}
						require.NoError(t, fx.db.ReopenWriter())
					}
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
					assert.Equal(t, tc.parent, session.ParentSessionID)
					assert.Equal(t, tc.project, session.Project)

					if replacement != nil {
						session, err = replacement.GetSession(t.Context(), "worker")
						require.NoError(t, err)
						require.NotNil(t, session)
						assert.Empty(t, session.Labels)
						assert.Nil(t, session.ParentSessionID)
						assert.Equal(t, "proj", session.Project)
						assert.False(t, session.ProjectAssigned)
					}
				})
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
		dbtest.SeedSession(t, fx.db, id, "proj")
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
