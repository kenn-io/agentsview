package config

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestApplyConfigTOMLMirrorParity guards the hand-maintained struct decoded by
// applyConfigTOML against drift from the exported Config struct.
//
// applyConfigTOML decodes config.toml into a local anonymous struct rather than
// into Config itself, and that is deliberate. Every field is then merged by
// hand: environment variables take precedence over the file, string values are
// trimmed, legacy keys are migrated (remote_access folds into RequireAuth), and
// some file keys land on unexported fields (session_sources). Decoding straight
// into Config would silently discard all of that per-field semantics, so the
// mirror is intentional and refactoring it away is not the remedy.
//
// The price of that choice is that nothing in the compiler links the two field
// lists, and the mirror has drifted before: Notifications was missing, so
// `[notifications]` was silently dropped on load and the user's setting reverted
// on every restart even though config.toml still said `enabled = true`.
//
// This test is the missing link: it fails when a Config toml key has no mirror
// counterpart, which is exactly the shape that silently dropped
// `[notifications]`.
//
// Scope, stated honestly: it compares key *sets*, so it does not cover the whole
// silent-drop space. A field declared in the mirror but never merged is dead
// code the compiler does not flag (the `pg` entry is one), and a field declared
// in both structs but never merged also passes here. This guard closes the
// omission direction — the one that bit — not every variant.
//
// Test-only: production code is untouched.
func TestApplyConfigTOMLMirrorParity(t *testing.T) {
	mirror := applyConfigTOMLMirrorKeys(t)
	config := configTOMLKeys()

	// A guard that finds nothing on either side would pass vacuously and stop
	// protecting anything, so fail loudly if extraction ever goes hollow.
	require.NotEmpty(t, mirror, "no toml keys extracted from the applyConfigTOML mirror")
	require.NotEmpty(t, config, "no toml keys extracted from the Config struct")

	var missingFromMirror []string
	for key := range config {
		if mirror[key] {
			continue
		}
		if _, allowed := configKeysAllowedOutsideMirror[key]; allowed {
			continue
		}
		missingFromMirror = append(missingFromMirror, key)
	}

	sort.Strings(missingFromMirror)

	assert.Empty(t, missingFromMirror,
		"Config toml keys absent from the applyConfigTOML mirror: config.toml "+
			"values for these are silently dropped on load. Add them to the "+
			"mirror struct, or to configKeysAllowedOutsideMirror with a reason. "+
			"Offending keys: %v", missingFromMirror)
}

// configKeysAllowedOutsideMirror lists Config toml keys that intentionally have
// no counterpart in the applyConfigTOML mirror. Every entry needs a reason;
// adding one without a reason defeats the purpose of this guard.
var configKeysAllowedOutsideMirror = map[string]string{
	"data_dir":             "owned by the AGENTSVIEW_DATA_DIR environment variable; never set from config.toml",
	"no_browser":           "env/CLI-owned; the tag exists but applyConfigTOML never reads it from the file (pre-existing gap, tracked in OPEN_ISSUE.md)",
	"custom_model_pricing": "decoded separately by decodeCustomModelPricing rather than through the mirror struct",
}

// applyConfigTOMLMirrorKeys parses config.go and returns the set of toml keys
// declared by the `var file struct { ... }` mirror inside applyConfigTOML. The
// real parser is used rather than a regex so the result tracks the actual
// declaration, including any future reshuffling of fields or comments.
func applyConfigTOMLMirrorKeys(t *testing.T) map[string]bool {
	t.Helper()

	const funcName = "applyConfigTOML"

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, "config.go", nil, 0)
	require.NoError(t, err, "parsing config.go")

	var fn *ast.FuncDecl
	for _, decl := range parsed.Decls {
		declared, ok := decl.(*ast.FuncDecl)
		if ok && declared.Name.Name == funcName {
			fn = declared
			break
		}
	}
	require.NotNil(t, fn, "config.go must declare %s", funcName)
	require.NotNil(t, fn.Body, "%s must have a body", funcName)

	var mirror *ast.StructType
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if mirror != nil {
			return false
		}
		decl, ok := node.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR {
			return true
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			if structType, ok := value.Type.(*ast.StructType); ok {
				mirror = structType
				return false
			}
		}
		return true
	})
	require.NotNil(t, mirror,
		"%s must declare a `var file struct { ... }` toml mirror; if it was "+
			"renamed or restructured, update this guard", funcName)

	keys := make(map[string]bool)
	for _, field := range mirror.Fields.List {
		if field.Tag == nil {
			continue
		}
		raw, err := strconv.Unquote(field.Tag.Value)
		require.NoError(t, err, "unquoting struct tag %s", field.Tag.Value)
		if key := tomlTagKey(reflect.StructTag(raw).Get("toml")); key != "" {
			keys[key] = true
		}
	}
	return keys
}

// configTOMLKeys returns the set of toml keys the exported Config struct
// accepts. Fields tagged `toml:"-"`, and fields with no toml tag at all, are not
// file-settable and are skipped.
func configTOMLKeys() map[string]bool {
	typ := reflect.TypeFor[Config]()
	keys := make(map[string]bool, typ.NumField())
	for field := range typ.Fields() {
		if key := tomlTagKey(field.Tag.Get("toml")); key != "" {
			keys[key] = true
		}
	}
	return keys
}

// tomlTagKey reduces a `toml:"name,opt1,opt2"` tag to its key, and returns ""
// for a missing tag or for the `toml:"-"` opt-out.
func tomlTagKey(tag string) string {
	name, _, _ := strings.Cut(tag, ",")
	if name == "" || name == "-" {
		return ""
	}
	return name
}
