package parser

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthoritativeAgentDefsDoNotExposeLegacyHooks(t *testing.T) {
	modes := ProviderMigrationModes()
	for _, def := range Registry {
		if modes[def.Type] != ProviderMigrationProviderAuthoritative {
			continue
		}
		assert.Nil(t, def.DiscoverFunc,
			"%s must discover through Provider.Discover", def.Type)
		assert.Nil(t, def.FindSourceFunc,
			"%s must resolve sources through Provider.FindSource", def.Type)
	}
}

func TestNoExportedProviderFacadeShims(t *testing.T) {
	// s3SyncPathSeams are exported parse/discover entrypoints retained solely
	// for the legacy S3 sync path (internal/sync), which buffers s3:// objects
	// to temp files and parses them outside the provider. They are a temporary
	// exception, removed once S3 support folds into the JSONL source sets.
	s3SyncPathSeams := map[string]bool{
		"ParseCodexSession":     true,
		"DiscoverCodexSessions": true,
	}

	files, err := filepath.Glob("*.go")
	require.NoError(t, err)

	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
		require.NoError(t, err, file)
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() {
				continue
			}
			if !isProviderFacadeShimName(fn.Name.Name) {
				continue
			}
			if s3SyncPathSeams[fn.Name.Name] {
				continue
			}
			offenders = append(offenders, file+":"+fn.Name.Name)
		}
	}

	assert.Empty(t, offenders,
		"provider-specific Discover/Find/Parse/Process/Classify facade functions must stay on provider methods or package-local helpers")
}

func isProviderFacadeShimName(name string) bool {
	if name == "Classify" {
		return false
	}
	for _, prefix := range []string{"Discover", "Find", "Process", "Classify"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	if !strings.HasPrefix(name, "Parse") {
		return false
	}
	for _, suffix := range []string{"Session", "Sessions", "DB", "SourceFile"} {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

func TestNoProviderFacadeShimNamePolicyDocumentsAllowedHelpers(t *testing.T) {
	assert.False(t, isProviderFacadeShimName("ParseVirtualSourcePath"))
	assert.False(t, isProviderFacadeShimName("ParseAiderVirtualPath"))
	assert.False(t, isProviderFacadeShimName("ParseCursorTranscriptRelPath"))
	assert.False(t, isProviderFacadeShimName("Classify"))
	assert.True(t, isProviderFacadeShimName("DiscoverExampleSessions"))
	assert.True(t, isProviderFacadeShimName("FindExampleSourceFile"))
	assert.True(t, isProviderFacadeShimName("ParseExampleSession"))
	assert.True(t, isProviderFacadeShimName("ProcessExampleSession"))
	assert.True(t, isProviderFacadeShimName("ClassifyExamplePath"))
}

func TestProviderAntiShimScanReadsExpectedPackage(t *testing.T) {
	_, err := os.Stat("provider.go")
	require.NoError(t, err, "anti-shim scan must run from internal/parser")
}
