package parser

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A CodeBuddy home can hold CLI/5.x transcripts in projects/ beside 4.x
// history. A watched-folder scan of that home walks and proves both layouts,
// so 4.x additions, changes, and removals are reconciled with the
// transcripts, and a scan of the home covers everything it configures.
func TestCodeBuddyReconciliationScopesKeepBothLayouts(t *testing.T) {
	home, _ := cbcHome(t)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "history", "ws"), 0o755))
	projects := filepath.Join(home, "projects")
	for _, tc := range []struct {
		name      string
		requested []string
	}{
		{"home", []string{home}},
		{"home and its transcript folder", []string{home, projects}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCodeBuddyProviderFactory(AgentDef{Type: AgentCodeBuddy, IDPrefix: "codebuddy:"}).
				NewProvider(ProviderConfig{Roots: []string{home}, Machine: "m"})
			plan, err := p.ResolveReconciliationScopes(t.Context(),
				ReconciliationScopeRequest{Roots: tc.requested})
			require.NoError(t, err)
			var traversal, proofs, covered []string
			for _, scope := range plan.Scopes {
				traversal = append(traversal, scope.TraversalRoots...)
				for _, proof := range scope.PhysicalProofScopes {
					proofs = append(proofs, proof.Path)
				}
				covered = append(covered, scope.CoverageIdentities...)
			}
			assert.ElementsMatch(t, []string{projects, home}, traversal, "the scan walks both layouts")
			assert.ElementsMatch(t, []string{projects, home}, proofs, "the scan proves both layouts")
			assert.Subset(t, covered, plan.RequiredCoverageIdentities,
				"a scan of the home covers every configured layout")
		})
	}
}
