package sync

import (
	"context"

	"go.kenn.io/agentsview/internal/parser"
)

// summaryStoreChangedSources returns the sources whose archived session no
// longer matches what the Antigravity summary store at path says: a different
// working directory, or a parent the archive lacks. Comparing with the archive
// instead of with an earlier read keeps routing correct after a restart, on a
// retry after failed processing, and for remote imports that plan without
// discovery. A session not archived yet is created by its own source events.
func (e *Engine) summaryStoreChangedSources(
	ctx context.Context, provider parser.Provider, path string,
) ([]parser.SourceRef, error) {
	lister, ok := provider.(parser.SummaryStoreExpectationLister)
	if !ok {
		return nil, nil
	}
	expectations, err := lister.SummaryStoreExpectations(ctx, path)
	if err != nil || len(expectations) == 0 {
		return nil, err
	}
	ids := make([]string, len(expectations))
	for i, expectation := range expectations {
		ids[i] = applyIDPrefixToID(e.idPrefix, expectation.SessionID)
	}
	archived, err := e.db.ListSessionWriteIdentitiesByID(ctx, ids)
	if err != nil {
		return nil, err
	}
	var sources []parser.SourceRef
	for i, expectation := range expectations {
		stored, ok := archived[ids[i]]
		if !ok {
			continue
		}
		cwdChanged := expectation.CwdKnown && stored.Cwd != expectation.Cwd
		parentChanged := expectation.ParentSessionID != "" &&
			stored.ParserParentSessionID != applyIDPrefixToID(e.idPrefix, expectation.ParentSessionID)
		if cwdChanged || parentChanged {
			sources = append(sources, expectation.Source)
		}
	}
	return sources, nil
}
