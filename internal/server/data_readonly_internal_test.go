package server

import (
	"context"
	"net/http"
	"testing"

	"go.kenn.io/agentsview/internal/db"
)

// readOnlyDataSpy stubs the Store interface and returns
// db.ErrReadOnly from the three Data queries. It lets us
// verify the handlers map the sentinel error to 501 Not
// Implemented without spinning up a real PG instance.
type readOnlyDataSpy struct {
	db.Store
}

func (readOnlyDataSpy) GetProjectInventory(
	_ context.Context,
) (db.ProjectInventory, error) {
	return db.ProjectInventory{}, db.ErrReadOnly
}

func (readOnlyDataSpy) ListProjectRules(
	_ context.Context, _ string,
) (db.ProjectRules, error) {
	return db.ProjectRules{}, db.ErrReadOnly
}

func (readOnlyDataSpy) ListArchiveWorktreeCandidates(
	_ context.Context, _ db.ArchiveWorktreeCandidateRequest,
) ([]db.WorktreeReclassificationCandidate, error) {
	return nil, db.ErrReadOnly
}

// TestDataHandlers_ReturnNotImplementedOnReadOnlyStore locks
// in the Postgres-backend contract: when the underlying Store
// reports a Data query as unavailable (db.ErrReadOnly), all
// three Data HTTP endpoints must surface 501 Not Implemented
// rather than silently returning an empty body, which would
// look like "no data" to the user.
func TestDataHandlers_ReturnNotImplementedOnReadOnlyStore(
	t *testing.T,
) {
	s := newRoutedTestServerWithStore(t, readOnlyDataSpy{})

	cases := []struct {
		name string
		path string
	}{
		{
			name: "projects",
			path: "/api/v1/data/projects",
		},
		{
			name: "project-rules",
			path: "/api/v1/data/project-rules?machine=workstation",
		},
		{
			name: "candidates",
			path: "/api/v1/data/project-reclassification/candidates?" +
				"project_label=alpha&project_key=key-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveGet(t, s, tc.path)
			assertRecorderStatus(t, w, http.StatusNotImplemented)
		})
	}
}
