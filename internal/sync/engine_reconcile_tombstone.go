package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
)

// tombstoneMissingWatchSourcesLocked adapts a raw changed-path root list to
// the typed scope authority by resolving it through each provider's own
// topology, exactly as a reconciliation pass would.
func (e *Engine) tombstoneMissingWatchSourcesLocked(
	ctx context.Context,
	roots []string,
	spool reconciliationSpoolStore,
) (deleted int, retErr error) {
	plans, _ := e.resolveReconciliationPlans(ctx, "", roots, false, false)
	var scopes []reconciliationProviderScope
	for _, plan := range plans {
		if plan.err != nil {
			return 0, fmt.Errorf(
				"%s provider reconciliation scopes: %w", plan.agent, plan.err,
			)
		}
		var provider parser.Provider
		if factory := e.sources().providerFactories[plan.agent]; factory != nil {
			provider = factory.NewProvider(parser.ProviderConfig{
				Roots:          e.sources().agentDirs[plan.agent],
				Machine:        e.machine,
				SourceMachines: e.sources().sourceMachines[plan.agent],
				PathRewriter:   e.pathRewriter,
			})
		}
		for _, scope := range plan.plan.Scopes {
			proofScopes := scope.PhysicalProofScopes
			if resolver, ok := provider.(parser.StoredSourceHintScopeProvider); ok {
				var hintScopes []parser.StoredSourceHintScope
				for _, retryRoot := range scope.RetryRoots {
					hintScopes = append(
						hintScopes,
						resolver.StoredSourceHintScopes(parser.ChangedPathRequest{
							Path: retryRoot,
						})...,
					)
				}
				if len(hintScopes) > 0 {
					proofScopes = deduplicateStoredSourceHintScopes(hintScopes)
				}
			}
			scopes = append(scopes, reconciliationProviderScope{
				agent:                      plan.agent,
				roots:                      scope.RetryRoots,
				proofScopes:                proofScopes,
				coverageIdentities:         scope.CoverageIdentities,
				requiredCoverageIdentities: plan.plan.RequiredCoverageIdentities,
			})
		}
	}
	return e.tombstoneMissingWatchSourceScopesLocked(ctx, scopes, spool)
}

func deduplicateStoredSourceHintScopes(
	scopes []parser.StoredSourceHintScope,
) []parser.StoredSourceHintScope {
	if len(scopes) <= 1 {
		return scopes
	}
	seen := make(map[parser.StoredSourceHintScope]struct{}, len(scopes))
	out := make([]parser.StoredSourceHintScope, 0, len(scopes))
	for _, scope := range scopes {
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	return out
}

// reconciliationCoverageComplete reports whether the scopes completed for one
// provider cover every coverage identity its full configured scope requires.
// This is the provider-issued replacement for recomputing root geometry: a
// remote or unresolved configured root keeps its required identity uncovered,
// so full-coverage deletion authority stays unreachable.
func reconciliationCoverageComplete(scopes []reconciliationProviderScope) bool {
	covered := make(map[string]struct{})
	var required []string
	for _, scope := range scopes {
		if len(scope.requiredCoverageIdentities) > 0 {
			required = scope.requiredCoverageIdentities
		}
		for _, identity := range scope.coverageIdentities {
			covered[identity] = struct{}{}
		}
	}
	if len(required) == 0 {
		return false
	}
	for _, identity := range required {
		if _, ok := covered[identity]; !ok {
			return false
		}
	}
	return true
}

// tombstoneMissingWatchSourceScopesLocked pages ownership rows inside each
// completed scope's provider-issued physical proof and tombstones rows whose
// sources are provably gone.
func (e *Engine) tombstoneMissingWatchSourceScopesLocked(
	ctx context.Context,
	scopes []reconciliationProviderScope,
	spool reconciliationSpoolStore,
) (deleted int, retErr error) {
	if e.pathRewriter != nil {
		// Remote imports rewrite extraction paths to canonical stored paths in
		// one direction only. Without a stored-to-local inverse, stat and
		// provider lookup cannot authoritatively prove source loss.
		return 0, nil
	}
	var agents []parser.AgentType
	scopesByAgent := make(map[parser.AgentType][]reconciliationProviderScope)
	for _, scope := range scopes {
		if _, ok := scopesByAgent[scope.agent]; !ok {
			agents = append(agents, scope.agent)
		}
		scopesByAgent[scope.agent] = append(scopesByAgent[scope.agent], scope)
	}
	// Read once per pass, not per scope: the list is bounded by how many
	// machines the archive holds, which is independent of its session count.
	archiveMachines, err := e.db.GetMachines(ctx, false, false)
	if err != nil {
		return 0, fmt.Errorf("list archive machines for watch reconciliation: %w", err)
	}
	for _, agent := range agents {
		agentScopes := scopesByAgent[agent]
		var provider parser.Provider
		var replacementIndex reconciliationSpoolStore
		ownsReplacementIndex := false
		hasNonAuthoritativeScopes := false
		if spool != nil {
			hasNonAuthoritativeScopes, err = spool.HasNonAuthoritativeScopes(ctx, agent)
			if err != nil {
				return deleted, fmt.Errorf(
					"query %s non-authoritative reconciliation scopes: %w", agent, err,
				)
			}
		}
		allProviderRootsCovered := reconciliationCoverageComplete(agentScopes)
		if factory := e.sources().providerFactories[agent]; factory != nil {
			provider = factory.NewProvider(parser.ProviderConfig{
				Roots: e.sources().agentDirs[agent], Machine: e.machine,
				SourceMachines: e.sources().sourceMachines[agent],
				PathRewriter:   e.pathRewriter,
			})
		}
		if provider != nil &&
			provider.Capabilities().Source.ExplicitDeletionOnly ==
				parser.CapabilitySupported {
			continue
		}
		for _, scope := range agentScopes {
			ownershipScopes := storedSourceDBHintScopes(scope.proofScopes)
			if len(ownershipScopes) == 0 {
				continue
			}
			// A scope proves absence for its own roots, but a stored row carries
			// the machine it was admitted under. Query every machine those roots
			// can hold so a relabeled root still reconciles its older rows.
			ownershipMachines := e.reconciliationOwnershipMachines(
				agent, scope.roots, archiveMachines,
			)
			if len(ownershipMachines) == 0 {
				continue
			}
			// Freebuff shares the Codebuff provider. When processing
			// Codebuff, also query for Freebuff sessions so they can
			// be tombstoned.
			agentStrs := []string{string(agent)}
			if agent == parser.AgentCodebuff {
				agentStrs = append(agentStrs, string(parser.AgentFreebuff))
			}
			for _, agentStr := range agentStrs {
				queryIndex := 0
				var cursor db.SessionSourceCursor
				for queryIndex < len(ownershipMachines) {
					ownershipMachine := ownershipMachines[queryIndex]
					if err := ctx.Err(); err != nil {
						return deleted, err
					}
					page, err := e.db.ListActiveSessionSourceOwnershipScopesPage(
						ctx, ownershipMachine, agentStr,
						ownershipScopes, cursor,
					)
					if err != nil {
						return deleted, fmt.Errorf(
							"list %s watch source ownership: %w", agent, err,
						)
					}
					if len(page) == 0 {
						queryIndex++
						cursor = db.SessionSourceCursor{}
						continue
					}
					missingByPath := make(map[string]bool, len(page))
					for _, ownership := range page {
						if hasNonAuthoritativeScopes {
							withheld, err := reconciliationOwnershipWithinNonAuthoritativeContainer(
								ctx, spool, provider, scope, ownership,
							)
							if err != nil {
								return deleted, fmt.Errorf(
									"query %s non-authoritative source %q: %w",
									agent, ownership.FilePath, err,
								)
							}
							if withheld {
								continue
							}
						}
						if !db.StoredSourcePathHintScopesContain(
							ownership.FilePath, ownershipScopes,
						) {
							// The SQL LIKE prefilter is ASCII-case-insensitive
							// while this predicate is platform-exact; retain any
							// row the pass holds no proof over and leave the
							// keyset cursor untouched.
							continue
						}
						statPath := ownership.FilePath
						persistentMemberContainerExists := false
						missing, ok := missingByPath[statPath]
						if !ok {
							_, statErr := e.lstatSource(statPath)
							missing = os.IsNotExist(statErr)
							if !missing && agent == parser.AgentCline {
								dir := filepath.Dir(statPath)
								sessionID := filepath.Base(dir)
								metaPath := filepath.Join(dir, sessionID+".json")
								if metaPath != statPath {
									_, metaErr := e.lstatSource(metaPath)
									missing = os.IsNotExist(metaErr)
								}
							}
							missingByPath[statPath] = missing
						}
						if !missing {
							// An aggregate row stores its container path, so
							// full-root coverage OR a scope proof spanning that
							// container's whole membership grants member-absence
							// authority: either way the completed stream
							// enumerated every member the row could resolve to.
							gone, checkErr := aggregateOwnedMemberGone(
								ctx, spool, provider, agent, ownership,
								allProviderRootsCovered ||
									reconciliationProofCoversContainerMembership(
										scope.proofScopes, ownership.FilePath,
									),
							)
							if checkErr != nil {
								return deleted, checkErr
							}
							if !gone {
								continue
							}
							if !allProviderRootsCovered {
								// Membership authority came from the scope's own
								// container proof; a same-ID copy under another
								// configured root would make this a move, not a
								// deletion.
								relocated, err := e.reconciliationMemberRelocated(
									ctx, provider, ownership.ID, ownership.FilePath,
								)
								if err != nil {
									return deleted, err
								}
								if relocated {
									continue
								}
							}
							// The container still exists but the streamed pass no
							// longer yields this member; tombstone directly — the
							// guards below all assume a vanished stored path.
							changed, err := e.markSessionSourceMissing(
								ctx, ownership.Machine, ownership.Agent,
								ownership.ID, ownership.FilePath,
							)
							if err != nil {
								return deleted, fmt.Errorf(
									"tombstone %s session %s after watch reconciliation: %w",
									agent, ownership.ID, err,
								)
							}
							if changed {
								deleted++
							}
							continue
						}
						// A vanished tracked copy with a surviving same-identity
						// duplicate is a replacement, not a deletion; the next sync
						// re-points the session at the survivor. Claude keys
						// replacements by the session ID in the filename; a Codex
						// UUID can exist as both a live dated copy and a flat
						// archived copy sharing one discovery identity. Both resolve
						// through a bounded per-source index lookup: the streamed
						// pass's spool when it covers every configured root, else a
						// lazily built disk-backed index (at most one per pass).
						replacementIdentity := reconciliationReplacementIdentity(
							agent, ownership.FilePath,
						)
						if replacementIdentity != "" && provider != nil {
							if replacementIndex == nil {
								if spool != nil {
									if !allProviderRootsCovered {
										// A scoped pass cannot prove that a same-identity
										// replacement does not exist under another configured root.
										continue
									}
									replacementIndex = spool
								} else {
									// Deliberate bypass of the pass's narrowed proof:
									// the index must span the provider's full
									// configured scope so a replacement beyond the
									// narrowed pass stays resolvable.
									replacementIndex, err = e.buildReconciliationReplacementIndex(
										ctx, provider, e.sources().agentDirs[agent],
									)
									if err != nil {
										return deleted, fmt.Errorf(
											"index %s reconciliation replacements: %w", agent, err,
										)
									}
									if agent == parser.AgentCodex {
										reconciliationRuntimeMetricsFor(ctx).
											codexReplacementIndexBuild()
									}
									ownsReplacementIndex = true
									defer func() {
										if ownsReplacementIndex {
											retErr = errors.Join(
												retErr, replacementIndex.CloseAndRemove(ctx),
											)
										}
									}()
								}
							}
							replacement, found, lookupErr := replacementIndex.Candidate(
								ctx, agent, replacementIdentity,
							)
							if lookupErr != nil {
								return deleted, fmt.Errorf(
									"lookup %s reconciliation replacement: %w", agent, lookupErr,
								)
							}
							if found && !sameReconciliationSourcePath(
								replacement.Path, ownership.FilePath,
							) {
								continue
							}
						}
						persistentArchive := provider != nil && provider.Capabilities().Source.PersistentArchive == parser.CapabilitySupported
						if persistentArchive {
							resolver, ok := provider.(parser.PersistentArchiveSourceResolver)
							if ok {
								physicalPath, valid := resolver.PersistentArchiveSource(
									statPath, ownership.ID,
								)
								if valid {
									if !allProviderRootsCovered &&
										!reconciliationProofCoversContainerMembership(
											scope.proofScopes, physicalPath,
										) {
										// A scoped pass cannot prove that this member does not
										// exist in a persistent container under another root —
										// unless the completed scope's proof is this container's
										// whole virtual membership, in which case the admitted
										// stream enumerated exactly this container and spool
										// absence is authoritative for rows bound to it.
										continue
									}
									if _, statErr := e.lstatSource(physicalPath); statErr != nil {
										// A vanished or unreadable persistent container cannot
										// authoritatively prove that an archived member was deleted.
										continue
									}
									if spool == nil {
										continue
									}
									present, lookupErr := spool.ContainsSource(
										ctx, agent,
										canonicalReconciliationSourceIdentity(ownership.FilePath),
									)
									if lookupErr != nil {
										return deleted, fmt.Errorf(
											"lookup %s persistent member %s: %w",
											agent, ownership.FilePath, lookupErr,
										)
									}
									if !present {
										// Container-granular discovery spools the
										// still-present container itself rather than
										// each member. A discovered container accounts
										// for its members: deleting a member changes
										// the container's bytes, so the
										// fingerprint-gated complete-result parse
										// already tombstoned it.
										present, lookupErr = spool.ContainsSource(
											ctx, agent,
											canonicalReconciliationSourceIdentity(physicalPath),
										)
										if lookupErr != nil {
											return deleted, fmt.Errorf(
												"lookup %s persistent container %s: %w",
												agent, physicalPath, lookupErr,
											)
										}
									}
									if present {
										continue
									}
									persistentMemberContainerExists = true
								}
							}
						} else if resolver, ok := provider.(parser.ReconciliationSourceResolver); ok {
							source, found, err := resolver.SourceForReconciliation(
								ctx, statPath, "",
							)
							if err != nil {
								return deleted, fmt.Errorf(
									"validate %s source %s during watch reconciliation: %w",
									agent, statPath, err,
								)
							}
							if found {
								container, _, virtual := parser.ParseVirtualSourcePath(
									providerDiscoveredPath(source),
								)
								if virtual && !allProviderRootsCovered &&
									!reconciliationProofCoversContainerMembership(
										scope.proofScopes, container,
									) {
									// A scoped pass cannot prove that the same logical
									// member did not move to another configured root —
									// unless the completed scope's proof spans this
									// container's whole virtual membership; the
									// relocation guard before the tombstone below
									// then rules out a copy under another root.
									continue
								}
								if spool == nil || !virtual {
									continue
								}
								identity := ""
								identityResolver, resolvesIdentity := provider.(parser.ReconciliationMemberIdentityResolver)
								if resolvesIdentity {
									identity = identityResolver.ReconciliationMemberIdentity(
										ownership.ID,
									)
								}
								present, lookupErr := spool.ContainsSourceIdentity(
									ctx, agent,
									canonicalReconciliationSourceIdentity(ownership.FilePath),
									identity,
								)
								if lookupErr != nil {
									return deleted, fmt.Errorf(
										"lookup %s virtual member %s: %w",
										agent, ownership.FilePath, lookupErr,
									)
								}
								if present {
									continue
								}
							}
						}
						if !persistentMemberContainerExists {
							missing, ok = missingByPath[statPath]
							if !ok {
								_, statErr := e.lstatSource(statPath)
								missing = os.IsNotExist(statErr)
								missingByPath[statPath] = missing
							}
							if !missing {
								continue
							}
						}
						if !allProviderRootsCovered {
							// A scoped pass never streamed the other configured
							// roots, so before tombstoning a virtual member —
							// whose home container may itself be gone — ask the
							// provider across its full configured scope whether
							// the session still resolves anywhere: a same-ID copy
							// under another root is a move, not a deletion.
							if isProviderVirtualMemberPath(provider, ownership.FilePath, scope.proofScopes) {
								relocated, err := e.reconciliationMemberRelocated(
									ctx, provider, ownership.ID, ownership.FilePath,
								)
								if err != nil {
									return deleted, err
								}
								if relocated {
									continue
								}
							}
						}
						changed, err := e.markSessionSourceMissing(
							ctx, ownership.Machine, ownership.Agent,
							ownership.ID, ownership.FilePath,
						)
						if err != nil {
							return deleted, fmt.Errorf(
								"tombstone %s session %s after watch reconciliation: %w",
								agent, ownership.ID, err,
							)
						}
						if changed {
							deleted++
						}
					}
					cursor = page[len(page)-1].Cursor()
					if len(page) < db.WatchReconcileSourcePageSize {
						queryIndex++
						cursor = db.SessionSourceCursor{}
					}
				}
			}
		}
	}
	return deleted, nil
}

// canonicalProviderStatHashAgent maps an AgentType key to the canonical
// agent used by the provider_freshness side-table. The side-table is
// only ever written by recordProviderStatHash via the
// pendingProviderStatHash.agent field, which is set at the staging site
// from the discovered source's AgentType — always AgentCodebuff in the
// current registry because the Codebuff provider is the sole provider
// registered with MultiFileStatHash (Freebuff sessions surface with
// agent=AgentCodebuff per parser.AgentLabel routing, see
// buildProviderStatHashers). The storage-layer ownership rows, however,
// can carry agent=AgentFreebuff (the watcher/reconcile path may label a
// Freebuff-on-disk session with the AgentFreebuff literal, while the
// side-table provenance still rooted at the Codebuff probe). Reading
// the side-table at any site that takes ownership.Agent must therefore
// normalize Freebuff to Codebuff; without this, a tombstone on a
// Freebuff-tagged ownership row would silently miss the side-table row
// that the cold sync stamped under the Codebuff key, and a future
// byte-identical restore of the directory would falsely match the
// stale digest.
func canonicalProviderStatHashAgent(agent string) parser.AgentType {
	a := parser.AgentType(agent)
	if a == parser.AgentFreebuff {
		return parser.AgentCodebuff
	}
	return a
}

func (e *Engine) markSessionSourceMissing(
	ctx context.Context, machine, agent, id, filePath string,
) (bool, error) {
	// Clear durable freshness state first. If this fails, leave the session
	// unchanged so a later reconciliation can retry the cache invalidation and
	// source-state transition as one recoverable operation.
	if _, err := e.clearSkipPersistent(ctx, filePath); err != nil {
		return false, fmt.Errorf("clear source skip cache: %w", err)
	}
	if _, err := e.clearSkipPersistent(ctx, providerAgentSkipCacheKey(
		filePath, parser.AgentType(agent),
	)); err != nil {
		return false, fmt.Errorf("clear agent source skip cache: %w", err)
	}
	// Also drop the per-component provider_freshness row under the same
	// (agent, filePath) key. Freebuff sessions surface in storage with
	// agent=AgentFreebuff but the provider_freshness side-table is only
	// ever stamped under the canonical AgentCodebuff key (the sole
	// MultiFileStatHasher provider); canonicalProviderStatHashAgent
	// remaps Freebuff to Codebuff here so the delete matches the row the
	// cold-sync staging site wrote. If this row is left intact and the
	// same physical directory is later restored byte-for-byte, the stale
	// digest would short-circuit providerSourceFreshBeforeFingerprint on
	// the next warm pass and silently skip the source, preventing the
	// source-missing row from returning to sync eligibility.
	if err := e.db.DeleteProviderStatHash(
		ctx, canonicalProviderStatHashAgent(agent), filePath,
	); err != nil {
		return false, fmt.Errorf("clear provider_freshness: %w", err)
	}
	changed, err := e.db.MarkSessionSourceMissing(
		ctx, machine, agent, id, filePath,
	)
	if err != nil || !changed {
		return changed, err
	}
	// The skip family was removed before the database transition. Drop the
	// remaining source trust so a byte-identical return is reverified and can
	// clear the source-missing state.
	e.invalidateVerifiedSource(parser.AgentType(agent), filePath)
	return true, nil
}

// reconcileSourceMissingMembers applies the per-member CWD decision shared by
// batch and single-session writes. A member without source-absence proof cannot
// be marked source-missing yet, so baseline records only that exact admitted ownership; the
// caller persists those records at the correct point in its write ordering.
func (e *Engine) reconcileSourceMissingMembers(
	ctx context.Context,
	members []sourceMissingMember,
	baseline func(db.SessionSourceOwnership),
	reject func(db.SessionSourceOwnership),
) (int, []sourceMissingMember, error) {
	admitted := make([]bool, len(members))
	for i, member := range members {
		allowed, err := e.missingMemberTombstoneAllowed(
			ctx, member.sessionID,
		)
		if err != nil {
			return 0, nil, err
		}
		if allowed {
			admitted[i] = true
			continue
		}
		if reject != nil {
			reject(db.SessionSourceOwnership{
				Machine:  member.machine,
				Agent:    string(member.agent),
				ID:       member.sessionID,
				FilePath: member.filePath,
			})
		}
	}

	tombstoned := 0
	var deferred []sourceMissingMember
	for i, member := range members {
		if !admitted[i] {
			continue
		}
		ownership := db.SessionSourceOwnership{
			Machine:  member.machine,
			Agent:    string(member.agent),
			ID:       member.sessionID,
			FilePath: member.filePath,
		}
		if e.archiveStore != nil {
			replacement, err := e.db.GetSession(ctx, member.sessionID)
			if err != nil {
				return tombstoned, deferred, fmt.Errorf(
					"read replacement source-missing member %s: %w",
					member.sessionID, err,
				)
			}
			if replacement == nil {
				deferred = append(deferred, member)
				continue
			}
		}
		changed, err := e.markSessionSourceMissing(
			ctx, member.machine, string(member.agent),
			member.sessionID, member.filePath,
		)
		if err != nil {
			return tombstoned, deferred, err
		}
		if changed {
			tombstoned++
			continue
		}
		if baseline != nil {
			baseline(ownership)
		}
	}
	return tombstoned, deferred, nil
}

// reconcileCopiedSourceMissingMembers completes rebuild-time reconciliation
// after orphan copy has materialized archive-only rows in the replacement.
// Exact baselines copied from the archive authorize an immediate guarded
// source-missing transition.
// Baseline-free legacy rows remain available for this pass but gain exact proof,
// preserving the normal two-pass upgrade safety rule.
func (e *Engine) reconcileCopiedSourceMissingMembers(
	ctx context.Context,
	target *db.DB,
	archivePath string,
	members []sourceMissingMember,
) (int, error) {
	tombstoned := 0
	ownerships := make([]db.SessionSourceOwnership, 0, len(members))
	for _, member := range members {
		ownerships = append(ownerships, db.SessionSourceOwnership{
			ID:       member.sessionID,
			Machine:  member.machine,
			Agent:    string(member.agent),
			FilePath: member.filePath,
		})
	}
	if err := target.CopySessionSourceOwnershipBaselinesFrom(
		ctx, archivePath, ownerships,
	); err != nil {
		return 0, fmt.Errorf(
			"copy admitted source-missing baselines: %w", err,
		)
	}
	var needsBaseline []db.SessionSourceOwnership
	for i, member := range members {
		e.clearSkipInMemory(member.filePath)
		e.clearSkipInMemory(providerAgentSkipCacheKey(
			member.filePath, member.agent,
		))
		if err := target.DeleteProviderStatHash(
			ctx, member.agent, member.filePath,
		); err != nil {
			return tombstoned, fmt.Errorf(
				"clear copied source freshness for %s: %w",
				member.sessionID, err,
			)
		}
		changed, err := target.MarkSessionSourceMissing(
			ctx, member.machine, string(member.agent),
			member.sessionID, member.filePath,
		)
		if err != nil {
			return tombstoned, fmt.Errorf(
				"mark copied member %s source-missing: %w",
				member.sessionID, err,
			)
		}
		if changed {
			tombstoned++
			e.invalidateVerifiedSource(member.agent, member.filePath)
			continue
		}
		needsBaseline = append(needsBaseline, ownerships[i])
	}
	if err := target.BaselineActiveSessionSourceOwnerships(
		ctx, needsBaseline,
	); err != nil {
		return tombstoned, fmt.Errorf(
			"baseline copied source-missing members: %w", err,
		)
	}
	return tombstoned, nil
}

func (e *Engine) buildReconciliationReplacementIndex(
	ctx context.Context, provider parser.Provider, configuredRoots []string,
) (result reconciliationSpoolStore, retErr error) {
	discoverer, ok := provider.(parser.StreamingDiscoverer)
	if !ok || provider.Capabilities().Source.StreamingDiscovery != parser.CapabilitySupported {
		return nil, errors.New("provider does not support streaming discovery")
	}
	watchRoots, err := parser.ResolveWatchRoots(ctx, provider)
	if err != nil {
		return nil, err
	}
	spool, err := e.reconciliationSpoolFactory(ctx, e.db.Path())
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			retErr = errors.Join(retErr, spool.CloseAndRemove(ctx))
		}
	}()
	err = discoverer.DiscoverEach(ctx, func(source parser.SourceRef) error {
		candidate, admitted := e.reconciliationCandidate(ctx,
			provider, source, configuredRoots, watchRoots,
		)
		if !admitted {
			return nil
		}
		return spool.Add(ctx, candidate)
	})
	if err != nil {
		return nil, err
	}
	keep = true
	return spool, nil
}

func (e *Engine) lstatSource(path string) (os.FileInfo, error) {
	if e.lstat != nil {
		return e.lstat(path)
	}
	return os.Lstat(path)
}

func isRemoteReconciliationRoot(root string) bool {
	return strings.HasPrefix(strings.ToLower(root), "s3://")
}
