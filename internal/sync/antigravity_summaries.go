package sync

import (
	"context"
	"path/filepath"

	"go.kenn.io/agentsview/internal/parser"
)

// summaryStoreChangedSources returns the sources whose archived session no
// longer matches what the Antigravity summary store at path says: a different
// working directory or parent. Comparing with the archive instead of with an
// earlier read keeps routing correct after a restart, on a retry after failed
// processing, and for remote imports that plan without discovery.
//
// A session archived from another root that shares the conversation ID is
// left alone. A session not archived yet is routed only when a cwd allow-list
// accepts the workspace the store now gives it: an earlier parse may have
// been filtered out before the store named that workspace, and its own source
// events already cover every other new session.
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
	agent := provider.Definition().Type
	root := filepath.Dir(path)
	var sources []parser.SourceRef
	for i, expectation := range expectations {
		stored, ok := archived[ids[i]]
		if !ok {
			if !e.cwdFilter.empty() && expectation.CwdKnown && e.cwdFilter.allows(expectation.Cwd) {
				sources = append(sources, expectation.Source)
			}
			continue
		}
		if !e.summaryRootOwns(agent, root, stored.FilePath) {
			continue
		}
		cwdChanged := expectation.CwdKnown && stored.Cwd != expectation.Cwd
		parentChanged := expectation.ParentKnown &&
			stored.ParserParentSessionID != applyIDPrefixToID(e.idPrefix, expectation.ParentSessionID)
		if cwdChanged || parentChanged {
			sources = append(sources, expectation.Source)
		}
	}
	return sources, nil
}

// summaryRootOwns reports whether the session archived from storedPath belongs
// to root, resolving a remote import's stored path as the title refresh does.
func (e *Engine) summaryRootOwns(agent parser.AgentType, root, storedPath string) bool {
	if e.pathRewriter != nil {
		if e.storedPathResolver == nil {
			return false
		}
		resolved, ok := e.storedPathResolver(storedPath)
		if !ok {
			return false
		}
		storedPath = resolved
	}
	owner, ok := parser.AntigravitySummarySessionRoot(agent, storedPath)
	return ok && sameReconciliationSourcePath(owner, root)
}

// archivedSessionHasSourceMetadata reports whether the live session archived
// from path holds a working directory, parser parent, or parser project, the
// values a provider declaring UnavailableCwdKeepsArchivedSession takes from
// the source it cannot read now.
func (e *Engine) archivedSessionHasSourceMetadata(
	ctx context.Context, path string, agent parser.AgentType,
) (bool, error) {
	meta, ok, err := e.db.GetSessionSourceMetadataByAgentPath(ctx, path, string(agent))
	if err != nil || !ok {
		return false, err
	}
	return meta.Cwd != "" || meta.ParserParentSessionID != "" || meta.ParserProject != "", nil
}
