package sync

import (
	"context"
	"path/filepath"

	"go.kenn.io/agentsview/internal/db"
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
		// A stale row has an unfinished update, such as a cwd reconciled by
		// a parse that then failed, so it routes until a parse succeeds.
		stale := stored.DataVersion < db.CurrentDataVersion()
		cwdChanged := expectation.CwdKnown && stored.Cwd != expectation.Cwd
		parentChanged, err := e.summaryParentChanged(expectation, stored.ParserParentSessionID)
		if err != nil {
			return nil, err
		}
		if stale || cwdChanged || parentChanged {
			sources = append(sources, expectation.Source)
		}
	}
	return sources, nil
}

// summaryParentChanged reports whether a parse would store a parent other
// than storedParent. A source whose parent may come from elsewhere first is
// resolved only when the store disagrees with the archive, so the common
// path stays a comparison.
func (e *Engine) summaryParentChanged(
	expectation parser.SummaryStoreExpectation, storedParent string,
) (bool, error) {
	if !expectation.ParentKnown {
		return false, nil
	}
	want := expectation.ParentSessionID
	if storedParent == applyIDPrefixToID(e.idPrefix, want) {
		return false, nil
	}
	if expectation.ResolveParent != nil {
		resolved, err := expectation.ResolveParent()
		if err != nil {
			return false, err
		}
		want = resolved
	}
	return storedParent != applyIDPrefixToID(e.idPrefix, want), nil
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

type sessionSourceMetadataReader interface {
	GetSessionSourceMetadataByAgentPath(ctx context.Context, path, agent string) (db.SessionSourceMetadata, bool, error)
}

// archivedSessionHasSourceMetadata reports whether the live session archived
// from path holds a working directory, parser parent, or parser project, the
// values a provider declaring UnavailableCwdKeepsArchivedSession takes from
// the source it cannot read now.
func (e *Engine) archivedSessionHasSourceMetadata(
	ctx context.Context, path string, agent parser.AgentType,
) (bool, error) {
	// During a rebuild e.db is the empty replacement; the old archive holds
	// the values to protect.
	var reader sessionSourceMetadataReader = e.db
	if archived, ok := e.archiveStore.(sessionSourceMetadataReader); ok {
		reader = archived
	}
	meta, ok, err := reader.GetSessionSourceMetadataByAgentPath(ctx, path, string(agent))
	if err != nil || !ok {
		return false, err
	}
	return meta.Cwd != "" || meta.ParserParentSessionID != "" || meta.ParserProject != "", nil
}
