// ABOUTME: static AST scan that prevents new commands from
// ABOUTME: opening a store without first wiring the
// ABOUTME: user-prefix classifier singleton.
package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// triggerCalls names the qualified function calls that read
// the classifier singleton (directly or indirectly via
// backfill). Every function or function literal in
// cmd/agentsview/ that contains one of these calls must
// contain an EARLIER call to applyClassifierConfig in the
// same enclosing body.
var triggerCalls = map[string]struct{}{
	"db.Open":               {},
	"postgres.Open":         {},
	"postgres.NewStore":     {},
	"postgres.New":          {},
	"postgres.EnsureSchema": {},
}

const wiringHelper = "applyClassifierConfig"

var inheritedWiringFuncs = map[string]struct{}{
	"runPGPushTarget":   {},
	"runPGStatusTarget": {},
}

// TestEveryStoreOpenPathIsWired enforces the rule documented
// in the design spec: every code path in cmd/agentsview that
// opens or initializes a store must first call
// applyClassifierConfig so user-defined prefixes reach the
// db package singleton.
func TestEveryStoreOpenPathIsWired(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err, "listing cmd/agentsview")

	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(
			fset, filepath.Join(".", name), nil,
			parser.ParseComments,
		)
		require.NoError(t, err, "parsing %s", name)
		violations = append(
			violations, scanFile(fset, f)...,
		)
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf(
			"functions or closures missing %s before "+
				"opening a store:\n  %s",
			wiringHelper,
			strings.Join(violations, "\n  "),
		)
	}
}

// scanFile walks every function declaration and function
// literal in f, returning a violation string for each body
// that contains a trigger call without an earlier
// applyClassifierConfig call.
func scanFile(
	fset *token.FileSet, f *ast.File,
) []string {
	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch fn := n.(type) {
		case *ast.FuncDecl:
			if fn.Body == nil {
				return true
			}
			if _, ok := inheritedWiringFuncs[fn.Name.Name]; ok {
				return true
			}
			if v := checkBody(
				fset, fn.Body, funcLabel(fset, fn),
			); v != "" {
				violations = append(violations, v)
			}
		case *ast.FuncLit:
			if v := checkBody(
				fset, fn.Body, litLabel(fset, fn),
			); v != "" {
				violations = append(violations, v)
			}
		}
		return true
	})
	return violations
}

// checkBody walks body's statements in source order. If a
// trigger call appears before the helper call (or the helper
// call never appears), it returns a violation string. Helper
// and trigger searches descend into nested expressions but
// stop at nested function literals — those have their own
// scope and are checked separately by ast.Inspect.
func checkBody(
	fset *token.FileSet,
	body *ast.BlockStmt,
	label string,
) string {
	var (
		seenHelper  bool
		earlyTrig   string
		earlyTrigAt token.Pos
	)
	ast.Inspect(body, func(n ast.Node) bool {
		// Don't descend into nested func literals — they
		// carry their own scope and are visited by the
		// outer ast.Inspect in scanFile.
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			if fn.Name == wiringHelper {
				seenHelper = true
			}
		case *ast.SelectorExpr:
			pkg, ok := fn.X.(*ast.Ident)
			if !ok {
				return true
			}
			qname := pkg.Name + "." + fn.Sel.Name
			if _, isTrigger := triggerCalls[qname]; isTrigger {
				if !seenHelper && earlyTrig == "" {
					earlyTrig = qname
					earlyTrigAt = call.Pos()
				}
			}
		}
		return true
	})
	if earlyTrig == "" {
		return ""
	}
	pos := fset.Position(earlyTrigAt)
	return label + ": calls " + earlyTrig +
		" at " + pos.Filename + ":" +
		itoa(pos.Line) + " without earlier " +
		wiringHelper
}

func funcLabel(fset *token.FileSet, fn *ast.FuncDecl) string {
	pos := fset.Position(fn.Pos())
	return fn.Name.Name + " (" + pos.Filename + ":" +
		itoa(pos.Line) + ")"
}

func litLabel(fset *token.FileSet, fn *ast.FuncLit) string {
	pos := fset.Position(fn.Pos())
	return "anonymous func at " + pos.Filename + ":" +
		itoa(pos.Line)
}

// itoa avoids importing strconv just for line numbers.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
