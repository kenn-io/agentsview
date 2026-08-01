package parser

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resolveDirectoryPlan(
	t *testing.T, configured, requested []string,
) ReconciliationScopePlan {
	t.Helper()
	base := ProviderBase{Config: ProviderConfig{Roots: configured}}
	plan, err := base.ResolveReconciliationScopes(
		t.Context(), ReconciliationScopeRequest{Roots: requested},
	)
	require.NoError(t, err)
	return plan
}

func TestDirectoryPlanExactConfiguredRootIsOneAtomicScope(t *testing.T) {
	root := t.TempDir()
	plan := resolveDirectoryPlan(t, []string{root}, []string{root})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t, []string{root}, scope.TraversalRoots)
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: filepath.Clean(root)}},
		scope.PhysicalProofScopes)
	assert.Equal(t, []string{filepath.Clean(root)}, scope.CoverageIdentities)
	assert.Equal(t, []string{root}, scope.RetryRoots)
	assert.Equal(t, []string{filepath.Clean(root)},
		plan.RequiredCoverageIdentities)
}

func TestDirectoryPlanDescendantTraversesGatewayAndProvesDescendant(t *testing.T) {
	root := t.TempDir()
	descendant := filepath.Join(root, "project")
	plan := resolveDirectoryPlan(t, []string{root}, []string{descendant})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t, []string{root}, scope.TraversalRoots,
		"traversal must come from the configured gateway")
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: descendant}},
		scope.PhysicalProofScopes,
		"proof must stay bounded to the requested descendant")
	assert.Empty(t, scope.CoverageIdentities,
		"a descendant request cannot cover the configured root")
	assert.Equal(t, []string{descendant}, scope.RetryRoots)
}

func TestDirectoryPlanDescendantUsesDeepestConfiguredAncestor(t *testing.T) {
	outer := t.TempDir()
	inner := filepath.Join(outer, "inner")
	requested := filepath.Join(inner, "leaf")
	plan := resolveDirectoryPlan(t, []string{outer, inner}, []string{requested})

	require.Len(t, plan.Scopes, 1)
	assert.Equal(t, []string{inner}, plan.Scopes[0].TraversalRoots,
		"the deepest configured ancestor is the traversal gateway")
}

func TestDirectoryPlanAncestorClaimKeepsSiblingDescendantProof(t *testing.T) {
	outer := t.TempDir()
	requested := filepath.Join(outer, "mid")
	inner := filepath.Join(requested, "leaf")
	plan := resolveDirectoryPlan(t, []string{outer, inner}, []string{requested})

	require.Len(t, plan.Scopes, 2,
		"covering the inner root must not drop the descendant proof under the outer")
	full := plan.Scopes[0]
	assert.Equal(t, []string{inner}, full.CoverageIdentities,
		"the covered configured root is claimed fully")
	descendant := plan.Scopes[1]
	assert.Equal(t, []string{outer}, descendant.TraversalRoots)
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: requested}},
		descendant.PhysicalProofScopes,
		"the request still proves itself under its configured ancestor")
	assert.Empty(t, descendant.CoverageIdentities)
}

func TestDirectoryPlanAncestorSplitsIntoCoveredConfiguredRoots(t *testing.T) {
	parent := t.TempDir()
	first := filepath.Join(parent, "first")
	second := filepath.Join(parent, "second")
	plan := resolveDirectoryPlan(t, []string{first, second}, []string{parent})

	require.Len(t, plan.Scopes, 2)
	var identities []string
	for _, scope := range plan.Scopes {
		identities = append(identities, scope.CoverageIdentities...)
		assert.Equal(t, []string{parent}, scope.RetryRoots,
			"retry identities are the caller's own roots")
		require.Len(t, scope.PhysicalProofScopes, 1)
		assert.NotEqual(t, parent, scope.PhysicalProofScopes[0].Path,
			"an ancestor request must not claim unrelated paths beneath itself")
	}
	assert.ElementsMatch(t, []string{first, second}, identities)
}

func TestDirectoryPlanUnrelatedBlankAndRemoteRootsResolveNothing(t *testing.T) {
	root := t.TempDir()
	unrelated := t.TempDir()
	plan := resolveDirectoryPlan(t, []string{root}, []string{
		"", "   ", "s3://bucket/prefix", unrelated,
	})

	assert.Empty(t, plan.Scopes)
	assert.Equal(t, []string{filepath.Clean(root)},
		plan.RequiredCoverageIdentities)
}

func TestDirectoryPlanBlankRootNeverResolvesToWorkingDirectory(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	// The working directory is a configured root, so a blank request that
	// leaked through filepath.Abs("") would claim it.
	plan := resolveDirectoryPlan(t, []string{cwd}, []string{""})
	assert.Empty(t, plan.Scopes,
		"a blank root must be discarded before normalization")
}

func TestDirectoryPlanRemoteConfiguredRootKeepsCoverageUnreachable(t *testing.T) {
	root := t.TempDir()
	remote := "s3://bucket/sessions"
	plan := resolveDirectoryPlan(t, []string{root, remote}, []string{root})

	require.Len(t, plan.Scopes, 1)
	assert.ElementsMatch(t,
		[]string{filepath.Clean(root), remote},
		plan.RequiredCoverageIdentities,
		"a remote configured root is never coverable by a local pass")
}

func TestDirectoryPlanDeduplicatesPathEquivalentRoots(t *testing.T) {
	root := t.TempDir()
	spelledDot := root + string(filepath.Separator) + "."
	plan := resolveDirectoryPlan(
		t, []string{root, spelledDot}, []string{root, spelledDot},
	)

	require.Len(t, plan.Scopes, 1)
	assert.Equal(t, []string{filepath.Clean(root)},
		plan.RequiredCoverageIdentities)
	assert.ElementsMatch(t, []string{root, spelledDot},
		plan.Scopes[0].RetryRoots,
		"merged spellings keep every caller retry identity")
}

func TestDirectoryPlanFullScopeSubsumesRequestedDescendant(t *testing.T) {
	root := t.TempDir()
	descendant := filepath.Join(root, "project")
	plan := resolveDirectoryPlan(t, []string{root}, []string{root, descendant})

	require.Len(t, plan.Scopes, 1)
	assert.Equal(t, []string{filepath.Clean(root)},
		plan.Scopes[0].CoverageIdentities)
	assert.ElementsMatch(t, []string{root, descendant},
		plan.Scopes[0].RetryRoots)
}

func TestDirectoryPlanEmptyConfigurationIsEmptyPlan(t *testing.T) {
	plan := resolveDirectoryPlan(t, nil, []string{t.TempDir()})
	assert.Empty(t, plan.Scopes)
	assert.Empty(t, plan.RequiredCoverageIdentities)
}

// writeHermesPlanArchive lays out one archive directory with a state.db file
// and a sessions directory; plan resolution only stats the layout, so the
// state.db content is irrelevant.
func writeHermesPlanArchive(t *testing.T, dir string) (stateDB, sessionsDir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	stateDB = filepath.Join(dir, "state.db")
	sessionsDir = filepath.Join(dir, "sessions")
	require.NoError(t, os.WriteFile(stateDB, []byte("db"), 0o644))
	require.NoError(t, os.MkdirAll(sessionsDir, 0o755))
	return stateDB, sessionsDir
}

func resolveHermesPlan(
	t *testing.T, configured, requested []string,
) ReconciliationScopePlan {
	t.Helper()
	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: configured})
	require.True(t, ok)
	plan, err := provider.ResolveReconciliationScopes(
		t.Context(), ReconciliationScopeRequest{Roots: requested},
	)
	require.NoError(t, err)
	return plan
}

func TestHermesReconciliationPlanAliasSpellingsResolveIdentically(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "archive")
	stateDB, sessionsDir := writeHermesPlanArchive(t, archive)

	base := resolveHermesPlan(t, []string{archive}, []string{archive})
	viaState := resolveHermesPlan(t, []string{archive}, []string{stateDB})
	viaSessions := resolveHermesPlan(t, []string{archive}, []string{sessionsDir})

	require.Len(t, base.Scopes, 1)
	for _, plan := range []ReconciliationScopePlan{viaState, viaSessions} {
		require.Len(t, plan.Scopes, 1)
		assert.Equal(t, base.Scopes[0].TraversalRoots,
			plan.Scopes[0].TraversalRoots)
		assert.Equal(t, base.Scopes[0].PhysicalProofScopes,
			plan.Scopes[0].PhysicalProofScopes,
			"no spelling may omit state.db members or sessions/ transcripts")
		assert.Equal(t, base.Scopes[0].CoverageIdentities,
			plan.Scopes[0].CoverageIdentities)
		assert.Equal(t, base.RequiredCoverageIdentities,
			plan.RequiredCoverageIdentities)
	}
	assert.Equal(t, []string{stateDB}, viaState.Scopes[0].RetryRoots)
	assert.Equal(t, []string{sessionsDir}, viaSessions.Scopes[0].RetryRoots)

	proofs := base.Scopes[0].PhysicalProofScopes
	require.Len(t, proofs, 2)
	assert.Equal(t, StoredSourceHintScope{
		Path: stateDB, IncludeVirtualMembers: true,
	}, proofs[0])
	assert.Equal(t, StoredSourceHintScope{Path: sessionsDir}, proofs[1])
}

func TestHermesReconciliationPlanFlatRootDescendantProvesItself(t *testing.T) {
	// No state.db and no sessions/ subdirectory: transcripts live directly
	// under the configured root, so the unit has no archive topology.
	root := t.TempDir()
	requested := filepath.Join(root, "gone.jsonl")

	plan := resolveHermesPlan(t, []string{root}, []string{requested})

	require.Len(t, plan.Scopes, 1,
		"a flat root must resolve a descendant generically, not to nothing")
	scope := plan.Scopes[0]
	assert.Equal(t, []string{root}, scope.TraversalRoots)
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: requested}},
		scope.PhysicalProofScopes,
		"proof stays bounded to the requested transcript")
	assert.Empty(t, scope.CoverageIdentities,
		"a descendant request cannot cover the configured root")
	assert.Equal(t, []string{requested}, scope.RetryRoots)
}

func TestHermesReconciliationPlanProfileRequestIsolatesSiblings(t *testing.T) {
	home := t.TempDir()
	container := filepath.Join(home, ".hermes", "profiles")
	profileA := filepath.Join(container, "alpha")
	profileB := filepath.Join(container, "beta")
	_, sessionsA := writeHermesPlanArchive(t, profileA)
	writeHermesPlanArchive(t, profileB)

	plan := resolveHermesPlan(t, []string{container}, []string{
		filepath.Join(sessionsA, "one.jsonl"),
	})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t, []string{profileA}, scope.TraversalRoots)
	for _, proof := range scope.PhysicalProofScopes {
		assert.True(t,
			hermesPathWithinOrSame(absoluteHermesPath(proof.Path), profileA),
			"proof %q must stay inside the requested profile", proof.Path)
	}
	assert.Empty(t, scope.CoverageIdentities,
		"a single profile cannot cover the container identity")
	assert.Equal(t, []string{absoluteHermesPath(container)},
		plan.RequiredCoverageIdentities)
}

func TestHermesReconciliationPlanContainerRequestCoversContainer(t *testing.T) {
	home := t.TempDir()
	container := filepath.Join(home, ".hermes", "profiles")
	writeHermesPlanArchive(t, filepath.Join(container, "alpha"))

	plan := resolveHermesPlan(t, []string{container}, []string{container})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t, []string{container}, scope.TraversalRoots)
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: filepath.Clean(container)}},
		scope.PhysicalProofScopes)
	assert.Equal(t, []string{absoluteHermesPath(container)},
		scope.CoverageIdentities)
}

func TestHermesReconciliationPlanRejectsUnrelatedSiblingArchive(t *testing.T) {
	parent := t.TempDir()
	archive := filepath.Join(parent, "archive")
	sibling := filepath.Join(parent, "sibling")
	writeHermesPlanArchive(t, archive)
	writeHermesPlanArchive(t, sibling)

	plan := resolveHermesPlan(t, []string{archive}, []string{sibling})

	assert.Empty(t, plan.Scopes,
		"a sibling archive outside the configured topology resolves nothing")
}

// TestDirectoryPlanProvesARelativeRootInItsConfiguredSpelling pins the split
// between the two spellings a configured root carries. Comparison and coverage
// identity absolutize so a request naming the same directory differently still
// matches, but a proof scope is matched as a prefix against stored source
// paths, and discovery wrote those by joining the configured spelling.
// Absolutizing proof leaves every stored source unreachable: the pass pages
// zero ownership rows and still credits coverage.
func TestDirectoryPlanProvesARelativeRootInItsConfiguredSpelling(t *testing.T) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	absolute := filepath.Join(workingDir, "archive")
	require.NoError(t, os.MkdirAll(absolute, 0o755))

	plan := resolveDirectoryPlan(t, []string{"archive"}, []string{absolute})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: "archive"}},
		scope.PhysicalProofScopes,
		"proof must carry the spelling stored sources were written under")
	assert.Equal(t, []string{"archive"}, scope.TraversalRoots)
	assert.Equal(t, []string{absolute}, scope.CoverageIdentities,
		"coverage identity absolutizes so an absolute request matches it")
}

// TestDirectoryPlanProvesADescendantInItsGatewaySpelling extends the same
// split to a descendant request. Requests arrive absolute because they come
// from watch and polling roots, so the descendant has to be re-expressed under
// the gateway's configured spelling before it can prove anything.
func TestDirectoryPlanProvesADescendantInItsGatewaySpelling(t *testing.T) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	descendant := filepath.Join(workingDir, "archive", "project")
	require.NoError(t, os.MkdirAll(descendant, 0o755))

	plan := resolveDirectoryPlan(t, []string{"archive"}, []string{descendant})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: filepath.Join("archive", "project")}},
		scope.PhysicalProofScopes)
	assert.Equal(t, []string{"archive"}, scope.TraversalRoots)
	assert.Empty(t, scope.CoverageIdentities)
}

// TestReconciliationScopeRootsRejectTheWorkingDirectory covers the one
// spelling that cannot be bounded at all: discovery under a root that cleans
// to "." emits bare filenames, so no prefix matches them and the stored-hint
// normalizer discards the scope. Failing closed keeps a pass from reporting
// success over zero proven rows, and keeps the provider reachable instead of
// resolving an empty plan the engine would skip silently.
func TestReconciliationScopeRootsRejectTheWorkingDirectory(t *testing.T) {
	for _, root := range []string{".", filepath.Join("foo", "..")} {
		t.Run(root, func(t *testing.T) {
			base := ProviderBase{
				Def:    AgentDef{Type: AgentClaude},
				Config: ProviderConfig{Roots: []string{root}},
			}
			_, err := base.ResolveReconciliationScopes(
				t.Context(), ReconciliationScopeRequest{Roots: []string{root}},
			)
			require.Error(t, err,
				"an unprovable root must fail closed, not resolve no scope")
			assert.Contains(t, err.Error(), "working directory")
		})
	}
}

// TestHermesReconciliationPlanRejectsTheWorkingDirectory holds the Hermes
// override to the same contract; its archive proofs are built by cleaning the
// configured root, so it degenerates identically.
func TestHermesReconciliationPlanRejectsTheWorkingDirectory(t *testing.T) {
	provider, ok := NewProvider(AgentHermes, ProviderConfig{Roots: []string{"."}})
	require.True(t, ok)

	_, err := provider.ResolveReconciliationScopes(
		t.Context(), ReconciliationScopeRequest{Roots: []string{"."}},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "working directory")
}

// TestHermesReconciliationPlanProvesAProfileInItsContainerSpelling extends the
// generic two-spelling rule to the Hermes profiles fan-out. A request inside a
// profile arrives absolute, and matching it against the container has to be
// absolute too, but expandProfilesContainer builds each profile root by
// joining the configured container spelling, so a relative container's sources
// are stored relative. Proving such a profile with an absolute scope pages no
// ownership rows and leaves a removed member active.
func TestHermesReconciliationPlanProvesAProfileInItsContainerSpelling(
	t *testing.T,
) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	container := filepath.Join(".hermes", "profiles")
	archive := filepath.Join(container, "alpha")
	writeHermesPlanArchive(t, archive)
	absoluteArchive := filepath.Join(workingDir, archive)

	plan := resolveHermesPlan(t,
		[]string{container}, []string{absoluteArchive})

	require.Len(t, plan.Scopes, 1)
	scope := plan.Scopes[0]
	assert.Equal(t, []string{archive}, scope.TraversalRoots,
		"traversal must stay in the configured container spelling")
	assert.Equal(t, []StoredSourceHintScope{
		{Path: filepath.Join(archive, "state.db"), IncludeVirtualMembers: true},
		{Path: filepath.Join(archive, "sessions")},
	}, scope.PhysicalProofScopes,
		"proof must carry the spelling stored sources were written under")
	assert.Empty(t, scope.CoverageIdentities,
		"one profile cannot cover the container")
}

// TestHermesReconciliationPlanCanonicalizesProfileCase keeps a request's own
// casing out of the scope it resolves. On Windows "alpha" and "ALPHA" select
// the same physical profile, so carrying the requested spelling into traversal,
// proof, and identity would mint a second coverage identity for one archive and
// let reconciliation persist the request-cased path.
func TestHermesReconciliationPlanCanonicalizesProfileCase(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive path selection is Windows behavior")
	}
	home := t.TempDir()
	container := filepath.Join(home, ".hermes", "profiles")
	archive := filepath.Join(container, "alpha")
	writeHermesPlanArchive(t, archive)
	shouted := filepath.Join(container, "ALPHA")

	plan := resolveHermesPlan(t, []string{container}, []string{archive, shouted})

	require.Len(t, plan.Scopes, 1,
		"two spellings of one profile must resolve one scope")
	scope := plan.Scopes[0]
	assert.Equal(t, []string{archive}, scope.TraversalRoots,
		"traversal must use the on-disk profile spelling")
	assert.Equal(t, []StoredSourceHintScope{
		{Path: filepath.Join(archive, "state.db"), IncludeVirtualMembers: true},
		{Path: filepath.Join(archive, "sessions")},
	}, scope.PhysicalProofScopes)
	assert.Equal(t, []string{archive, shouted}, scope.RetryRoots,
		"both request spellings must retry against the one scope")
}

// TestHermesReconciliationPlanWidensAnUnownedChildToItsContainer covers every
// request the container cannot resolve to an owned profile. Reconstructing a
// root from the request is what four earlier rounds each got wrong, since it
// puts the caller's casing into proof and lets traversal follow a symlink out
// of the container, and resolving nothing loses the removal entirely. The
// container is the nearest scope spelled entirely from configuration, and it
// proves without covering, so a deleted profile's sources page and a live
// sibling's are stat'd and kept.
func TestHermesReconciliationPlanWidensAnUnownedChildToItsContainer(t *testing.T) {
	home := t.TempDir()
	container := filepath.Join(home, ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(container, 0o755))
	outside := filepath.Join(home, "elsewhere")
	writeHermesPlanArchive(t, outside)
	require.NoError(t, os.WriteFile(
		filepath.Join(container, "notes.txt"), []byte("x"), 0o644))

	requests := map[string]string{
		"removed":         filepath.Join(container, "gone"),
		"not a directory": filepath.Join(container, "notes.txt"),
	}
	link := filepath.Join(container, "alpha")
	if err := os.Symlink(outside, link); err == nil {
		requests["symlink"] = link
	} else {
		t.Logf("symlink creation unavailable, case skipped: %v", err)
	}

	for name, requested := range requests {
		t.Run(name, func(t *testing.T) {
			plan := resolveHermesPlan(t, []string{container}, []string{requested})

			require.Len(t, plan.Scopes, 1)
			scope := plan.Scopes[0]
			assert.Equal(t, []string{container}, scope.TraversalRoots,
				"traversal must be the configured container, never the request")
			assert.Equal(t,
				[]StoredSourceHintScope{{Path: container}},
				scope.PhysicalProofScopes)
			assert.Empty(t, scope.CoverageIdentities,
				"a child request cannot cover the container")
			assert.Equal(t, []string{requested}, scope.RetryRoots)
		})
	}
}

// TestHermesReconciliationPlanKeepsContainerCoverageIndependentOfAnUnownedChild
// pins the two container scopes apart. They share an identity, so keying them
// together would let whichever request arrived first decide whether the batch
// credits coverage.
func TestHermesReconciliationPlanKeepsContainerCoverageIndependentOfAnUnownedChild(
	t *testing.T,
) {
	home := t.TempDir()
	container := filepath.Join(home, ".hermes", "profiles")
	require.NoError(t, os.MkdirAll(container, 0o755))
	removed := filepath.Join(container, "gone")

	plan := resolveHermesPlan(t,
		[]string{container}, []string{removed, container})

	require.Len(t, plan.Scopes, 2)
	var covering int
	for _, scope := range plan.Scopes {
		if len(scope.CoverageIdentities) > 0 {
			covering++
			assert.Equal(t, []string{container}, scope.RetryRoots)
		}
	}
	assert.Equal(t, 1, covering,
		"the container request still credits coverage regardless of order")
}

func resolveOpenCodePlan(
	t *testing.T, agent AgentType, configured, requested []string,
) ReconciliationScopePlan {
	t.Helper()
	provider, ok := NewProvider(agent, ProviderConfig{Roots: configured})
	require.True(t, ok)
	plan, err := provider.ResolveReconciliationScopes(
		t.Context(), ReconciliationScopeRequest{Roots: requested},
	)
	require.NoError(t, err)
	return plan
}

// TestOpenCodePlanWidensAContainerAliasToItsVirtualMembers pins the atomic
// container: any spelling that lands on the database proves the container's
// whole membership. A proof of the bare database path admits no member row,
// and a proof of one member would let a completed pass promote
// container-state trust over siblings it never verified.
func TestOpenCodePlanWidensAContainerAliasToItsVirtualMembers(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	for name, requested := range map[string]string{
		"database":       dbPath,
		"wal sidecar":    dbPath + "-wal",
		"shm sidecar":    dbPath + "-shm",
		"virtual member": OpenCodeSQLiteVirtualPath(dbPath, "ses-1"),
	} {
		t.Run(name, func(t *testing.T) {
			plan := resolveOpenCodePlan(
				t, AgentOpenCode, []string{root}, []string{requested},
			)
			require.Len(t, plan.Scopes, 1)
			scope := plan.Scopes[0]
			assert.Equal(t, []string{root}, scope.TraversalRoots,
				"traversal must come from the configured gateway")
			assert.Equal(t,
				[]StoredSourceHintScope{{
					Path: dbPath, IncludeVirtualMembers: true,
				}},
				scope.PhysicalProofScopes,
				"every container alias proves the container's whole membership")
			assert.Empty(t, scope.CoverageIdentities,
				"a container request cannot cover the configured root")
			assert.Equal(t, []string{requested}, scope.RetryRoots)
		})
	}
}

// TestOpenCodePlanWidensACaseVariantContainerAliasOnWindows pins the widening
// against respelled aliases: stored-path admission is case-insensitive on
// Windows, so a case variant that escaped widening would prove exactly one
// member and reopen the partial-membership trust hole.
func TestOpenCodePlanWidensACaseVariantContainerAliasOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("case-insensitive path selection is Windows behavior")
	}
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	variant := filepath.Join(root, "OPENCODE.DB")
	for name, requested := range map[string]string{
		"database":       variant,
		"wal sidecar":    variant + "-wal",
		"virtual member": OpenCodeSQLiteVirtualPath(variant, "ses-1"),
	} {
		t.Run(name, func(t *testing.T) {
			plan := resolveOpenCodePlan(
				t, AgentOpenCode, []string{root}, []string{requested},
			)
			require.Len(t, plan.Scopes, 1)
			assert.Equal(t,
				[]StoredSourceHintScope{{
					Path: dbPath, IncludeVirtualMembers: true,
				}},
				plan.Scopes[0].PhysicalProofScopes,
				"a respelled alias still proves the whole membership")
			assert.Equal(t, []string{requested}, plan.Scopes[0].RetryRoots)
		})
	}
}

func TestOpenCodePlanCoalescesContainerAliasesIntoOneScope(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "opencode.db")
	aliases := []string{
		OpenCodeSQLiteVirtualPath(dbPath, "ses-a"),
		dbPath + "-wal",
		OpenCodeSQLiteVirtualPath(dbPath, "ses-b"),
		dbPath,
	}
	plan := resolveOpenCodePlan(t, AgentOpenCode, []string{root}, aliases)

	require.Len(t, plan.Scopes, 1,
		"aliases of one container are one atomic scope")
	assert.ElementsMatch(t, aliases, plan.Scopes[0].RetryRoots,
		"every caller spelling stays a retry identity")
}

func TestOpenCodePlanKeepsAStorageDescendantProofExact(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, "storage", "session")
	plan := resolveOpenCodePlan(
		t, AgentOpenCode, []string{root}, []string{storage},
	)

	require.Len(t, plan.Scopes, 1)
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: storage}},
		plan.Scopes[0].PhysicalProofScopes,
		"a plain directory descendant owns no virtual members")
}

func TestOpenCodeFamilyPlansWidenTheirOwnContainers(t *testing.T) {
	for _, agent := range []AgentType{
		AgentKilo, AgentMiMoCode, AgentIcodemate,
	} {
		t.Run(string(agent), func(t *testing.T) {
			root := t.TempDir()
			dbPath := filepath.Join(
				root, openCodeProviderSpecForAgent(agent).dbName,
			)
			plan := resolveOpenCodePlan(
				t, agent, []string{root},
				[]string{OpenCodeSQLiteVirtualPath(dbPath, "ses-1")},
			)
			require.Len(t, plan.Scopes, 1)
			assert.Equal(t,
				[]StoredSourceHintScope{{
					Path: dbPath, IncludeVirtualMembers: true,
				}},
				plan.Scopes[0].PhysicalProofScopes)
		})
	}
}

// TestOpenCodePlanProvesAContainerInItsConfiguredSpelling extends the
// two-spelling split to the widened container: comparison absolutizes so the
// absolute request finds the relative root's database, and proof re-expresses
// the container under the configured spelling stored sources carry.
func TestOpenCodePlanProvesAContainerInItsConfiguredSpelling(t *testing.T) {
	workingDir := t.TempDir()
	t.Chdir(workingDir)
	require.NoError(t, os.MkdirAll(filepath.Join(workingDir, "ocroot"), 0o755))
	absoluteDB := filepath.Join(workingDir, "ocroot", "opencode.db")

	plan := resolveOpenCodePlan(
		t, AgentOpenCode, []string{"ocroot"}, []string{absoluteDB},
	)

	require.Len(t, plan.Scopes, 1)
	assert.Equal(t,
		[]StoredSourceHintScope{{
			Path:                  filepath.Join("ocroot", "opencode.db"),
			IncludeVirtualMembers: true,
		}},
		plan.Scopes[0].PhysicalProofScopes,
		"proof must carry the spelling stored sources were written under")
	assert.Equal(t, []string{"ocroot"}, plan.Scopes[0].TraversalRoots)
}

// TestMultiSessionPlanWidensContainerAliasesToVirtualMembership pins the
// container topology for the shared multi-session base (Zed here, standing in
// for Shelley, Trae, Aider, Visual Studio Copilot, and Omnigent): a request
// naming the physical container, one virtual member, or a WAL sidecar widens
// to the container's whole virtual membership. The generic descendant proof
// would name only the bare path, admitting no "<container>#<member>" source
// and paging no member row. The container deliberately does not exist on
// disk: a deleted container must still resolve so its members stay
// reclaimable.
func TestMultiSessionPlanWidensContainerAliasesToVirtualMembership(
	t *testing.T,
) {
	root := t.TempDir()
	container := filepath.Join(root, "threads", "threads.db")
	provider, ok := NewProvider(AgentZed, ProviderConfig{Roots: []string{root}})
	require.True(t, ok)

	for _, requested := range []string{
		container,
		container + "#thread-1",
		container + "-wal",
	} {
		plan, err := provider.ResolveReconciliationScopes(
			t.Context(), ReconciliationScopeRequest{Roots: []string{requested}},
		)
		require.NoError(t, err, requested)
		require.Len(t, plan.Scopes, 1, requested)
		scope := plan.Scopes[0]
		assert.Equal(t, []string{root}, scope.TraversalRoots, requested)
		assert.Equal(t,
			[]StoredSourceHintScope{{
				Path: container, IncludeVirtualMembers: true,
			}},
			scope.PhysicalProofScopes,
			"request %q must prove the container's whole membership", requested)
		assert.Empty(t, scope.CoverageIdentities,
			"a container request cannot cover the configured root")
	}

	unrelated, err := provider.ResolveReconciliationScopes(
		t.Context(), ReconciliationScopeRequest{
			Roots: []string{filepath.Join(root, "settings.json")},
		},
	)
	require.NoError(t, err)
	require.Len(t, unrelated.Scopes, 1)
	assert.Equal(t,
		[]StoredSourceHintScope{{Path: filepath.Join(root, "settings.json")}},
		unrelated.Scopes[0].PhysicalProofScopes,
		"a non-container descendant keeps the exact generic proof")
}
