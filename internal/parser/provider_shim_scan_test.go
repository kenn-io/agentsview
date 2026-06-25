package parser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyEntrypointVerb matches the provider-specific legacy parser entrypoint
// naming convention this facade migration deletes: package-level
// Discover*/Find*/Parse*/Process*/Classify* free functions that encode one
// provider's source shape. A migrated provider owns that behavior on receiver
// methods or provider-neutral source-set helpers; it must not reach back into a
// legacy free function as a compatibility shim.
var legacyEntrypointVerb = regexp.MustCompile(`^(Discover|Find|Parse|Process|Classify)[A-Z]`)

// providerNeutralEntrypoints are package-level helpers whose names match the
// legacy verb pattern but are genuinely provider-neutral shared utilities.
// Provider files may reference these; they are not provider-specific legacy
// entrypoints. Keep this list small and add to it only when a new shared,
// provider-agnostic helper is introduced.
var providerNeutralEntrypoints = map[string]bool{
	"ParseVirtualSourcePath":        true,
	"ParseVirtualSourcePathForBase": true,
	// ParseCursorTranscriptRelPath is a pure rel-path shape validator with no
	// filesystem or provider state. It is shared by the engine's path
	// classification/enrichment and the Cursor provider's source set, so it
	// stays a free helper rather than moving onto the provider.
	"ParseCursorTranscriptRelPath": true,
}

// pendingShimProviderFiles are provider files whose behavior has not yet been
// folded onto the provider. They still reference legacy free functions and are
// temporarily exempt from the anti-shim gate so intermediate branches in the
// facade migration stay green while providers are folded one branch at a time.
//
// Each entry is a standing migration TODO: when a provider's behavior moves
// onto receiver methods or a provider-owned source set, delete its legacy free
// functions and remove the file from this list on the same branch. The stack
// tip (the zero-legacy gate) asserts this list is empty, so a provider cannot
// remain a permanent shim.
var pendingShimProviderFiles = map[string]bool{
	"antigravity_cli_provider.go":      true,
	"antigravity_provider.go":          true,
	"codex_provider.go":                true,
	"copilot_provider.go":              true,
	"db_backed_provider.go":            true,
	"gemini_provider.go":               true,
	"kiro_ide_provider.go":             true,
	"kiro_provider.go":                 true,
	"positron_provider.go":             true,
	"shelley_provider.go":              true,
	"visualstudio_copilot_provider.go": true,
	"vscode_copilot_provider.go":       true,
	"zed_provider.go":                  true,
}

// collectLegacyFreeFuncs returns the set of package-level free functions in the
// parser package whose names match the legacy entrypoint pattern, excluding the
// provider-neutral helpers. Tying detection to functions that actually exist
// (rather than to the name pattern alone) avoids false positives on types and
// values such as ParseResult or ParseRequest, and naturally shrinks as legacy
// functions are deleted.
func collectLegacyFreeFuncs(t *testing.T) (map[string]bool, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	goFiles, err := filepath.Glob("*.go")
	require.NoError(t, err)

	legacy := make(map[string]bool)
	for _, file := range goFiles {
		if isTestGoFile(file) {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoErrorf(t, err, "parse %s", file)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue // methods are provider-owned, not free entrypoints
			}
			name := fn.Name.Name
			if legacyEntrypointVerb.MatchString(name) &&
				!providerNeutralEntrypoints[name] {
				legacy[name] = true
			}
		}
	}
	return legacy, fset
}

func isTestGoFile(name string) bool {
	return len(name) > len("_test.go") &&
		name[len(name)-len("_test.go"):] == "_test.go"
}

// TestProviderFilesDoNotReferenceLegacyEntrypoints is the migration anti-shim
// gate. A *_provider.go that references a provider-specific legacy free
// function (whether by calling it or passing it as a value) is a shim, not a
// migration, so this scan fails for it unless the file is an explicitly tracked
// pending shim. The test is vacuous at the root (no provider files yet) and
// keeps the migrated providers honest as the stack folds each one.
func TestProviderFilesDoNotReferenceLegacyEntrypoints(t *testing.T) {
	legacy, fset := collectLegacyFreeFuncs(t)

	providerFiles, err := filepath.Glob("*_provider.go")
	require.NoError(t, err)

	for _, file := range providerFiles {
		t.Run(file, func(t *testing.T) {
			parsed, err := parser.ParseFile(fset, file, nil, 0)
			require.NoErrorf(t, err, "parse %s", file)

			offenders := legacyReferencesInProviderFile(parsed, legacy)
			if pendingShimProviderFiles[file] {
				assert.NotEmptyf(
					t,
					offenders,
					"%s is listed in pendingShimProviderFiles but no "+
						"longer references provider-specific legacy "+
						"entrypoints; remove it from the pending list",
					file,
				)
				return
			}
			assert.Emptyf(
				t,
				offenders,
				"%s references provider-specific legacy entrypoints %v; "+
					"fold that behavior onto the provider or a "+
					"provider-neutral source-set helper instead of shimming",
				file,
				offenders,
			)
		})
	}
}

func legacyReferencesInProviderFile(
	parsed *ast.File,
	legacy map[string]bool,
) []string {
	// A package cannot redeclare a free function name, so any direct ident in
	// a provider file that matches a legacy free function is a reference to it.
	// Method declarations and selector method names are provider-owned receiver
	// surface, so they are not legacy free-function references.
	declNames := make(map[*ast.Ident]struct{})
	selectorNames := make(map[*ast.Ident]struct{})
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			declNames[fn.Name] = struct{}{}
		}
	}
	ast.Inspect(parsed, func(n ast.Node) bool {
		if selector, ok := n.(*ast.SelectorExpr); ok {
			selectorNames[selector.Sel] = struct{}{}
		}
		return true
	})

	seen := make(map[string]struct{})
	var offenders []string
	ast.Inspect(parsed, func(n ast.Node) bool {
		ident, ok := n.(*ast.Ident)
		if !ok {
			return true
		}
		if _, isDecl := declNames[ident]; isDecl {
			return true
		}
		if _, isSelector := selectorNames[ident]; isSelector {
			return true
		}
		if !legacy[ident.Name] {
			return true
		}
		if _, dup := seen[ident.Name]; dup {
			return true
		}
		seen[ident.Name] = struct{}{}
		offenders = append(offenders, ident.Name)
		return true
	})

	sort.Strings(offenders)
	return offenders
}
