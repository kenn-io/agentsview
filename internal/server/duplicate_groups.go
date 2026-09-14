package server

import (
	"context"
	"net/http"

	"go.kenn.io/agentsview/internal/db"
)

type duplicateRebuildInput struct {
	Body struct{} `json:"body"`
}

type duplicateRebuildOutput struct {
	Body db.DuplicateGroupsResult
}

type duplicateGroupsOutput struct {
	Body []db.DuplicateGroupInfo
}

func (s *Server) duplicateGroupsDB() (*db.DB, error) {
	localDB, ok := s.db.(*db.DB)
	if !ok || localDB == nil || localDB.ReadOnly() || s.engine == nil {
		return nil, apiError(
			http.StatusNotImplemented, "not available in remote mode",
		)
	}
	return localDB, nil
}

// humaRebuildDuplicateGroups recomputes duplicate-session group membership
// on demand (Data-page review card).
func (s *Server) humaRebuildDuplicateGroups(
	ctx context.Context, _ *duplicateRebuildInput,
) (*duplicateRebuildOutput, error) {
	if _, err := s.duplicateGroupsDB(); err != nil {
		return nil, err
	}
	result, err := s.engine.RebuildDuplicateGroups(ctx)
	if err != nil {
		return nil, internalError("rebuild duplicate groups", err)
	}
	return &duplicateRebuildOutput{Body: result}, nil
}

// humaListDuplicateGroups returns the stored duplicate groups with member
// session summaries for the Data-page review list.
func (s *Server) humaListDuplicateGroups(
	ctx context.Context, _ *emptyInput,
) (*duplicateGroupsOutput, error) {
	localDB, err := s.duplicateGroupsDB()
	if err != nil {
		return nil, err
	}
	groups, err := localDB.ListDuplicateGroups(ctx)
	if err != nil {
		return nil, internalError("list duplicate groups", err)
	}
	if groups == nil {
		groups = []db.DuplicateGroupInfo{}
	}
	return &duplicateGroupsOutput{Body: groups}, nil
}
