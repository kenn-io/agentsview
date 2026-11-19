package sync

import (
	"context"
	"slices"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

type reconciliationAuthority struct {
	providers  map[parser.AgentType]struct{}
	sources    map[parser.AgentType]parser.Provider
	proofs     map[parser.AgentType][]parser.StoredSourceHintScope
	spool      reconciliationSpoolStore
	exclusions map[parser.AgentType]bool
}

func newReconciliationAuthority(
	providers map[parser.AgentType]struct{}, sources map[parser.AgentType]parser.Provider,
	scopes []reconciliationProviderScope, spool reconciliationSpoolStore,
) *reconciliationAuthority {
	proofs := make(map[parser.AgentType][]parser.StoredSourceHintScope)
	for _, scope := range scopes {
		proofs[scope.agent] = append(proofs[scope.agent], scope.proofScopes...)
	}
	return &reconciliationAuthority{
		providers: providers, sources: sources, proofs: proofs,
		spool: spool, exclusions: make(map[parser.AgentType]bool),
	}
}

func (a *reconciliationAuthority) physicalPath(agent parser.AgentType, path string) string {
	return reconciliationPhysicalSourcePath(a.sources[agent], path, a.proofs[agent])
}

func (a *reconciliationAuthority) allows(ctx context.Context, agent parser.AgentType, path string) (bool, error) {
	if _, eligible := a.providers[agent]; !eligible {
		return false, nil
	}
	if a.spool == nil {
		return true, nil
	}
	hasScopes, queried := a.exclusions[agent]
	if !queried {
		var err error
		hasScopes, err = a.spool.HasNonAuthoritativeScopes(ctx, agent)
		if err != nil {
			return false, err
		}
		a.exclusions[agent] = hasScopes
	}
	if !hasScopes {
		return true, nil
	}
	withheld, err := a.spool.ContainsNonAuthoritativeScope(ctx, agent, a.physicalPath(agent, path))
	return !withheld, err
}

func (a *reconciliationAuthority) ownerships(ctx context.Context, ownerships []db.SessionSourceOwnership) ([]db.SessionSourceOwnership, error) {
	var eligible []db.SessionSourceOwnership
	for _, ownership := range ownerships {
		allowed, err := a.allows(ctx, parser.AgentType(ownership.Agent), ownership.FilePath)
		if err != nil {
			return nil, err
		}
		if allowed {
			eligible = append(eligible, ownership)
		}
	}
	return eligible, nil
}

type recoveryCachePreparationKey struct{}

type recoveryCachePreparation struct {
	prepared bool
}

func localisedReconciliationFailures(
	agent parser.AgentType, group reconciliationTraversalGroup, err error,
) ([]reconciliationSourceScope, []string, bool) {
	failures, localised := parser.IncompleteDiscoveryScopes(err)
	if !localised {
		return nil, nil, false
	}
	var excluded []reconciliationSourceScope
	var retryRoots []string
	for _, failure := range failures {
		if !slices.ContainsFunc(group.roots, func(root string) bool {
			return sameReconciliationSourcePath(root, failure.Root)
		}) {
			return nil, nil, false
		}
		for _, scope := range group.scopes {
			for _, proof := range scope.PhysicalProofScopes {
				path := failure.Path
				if !db.StoredSourcePathHintScopesContain(path, storedSourceDBHintScopes([]parser.StoredSourceHintScope{proof})) {
					if !failure.Subtree || !samePathOrDescendant(cleanRootPath(proof.Path), cleanRootPath(path)) {
						continue
					}
					path = proof.Path
				}
				if slices.Contains(retryRoots, path) {
					continue
				}
				if len(retryRoots) == 256 {
					return nil, nil, false
				}
				retryRoots = append(retryRoots, path)
				excluded = append(excluded, reconciliationSourceScope{
					Provider: agent, Path: path, Subtree: failure.Subtree,
				})
			}
		}
	}
	return excluded, retryRoots, true
}
