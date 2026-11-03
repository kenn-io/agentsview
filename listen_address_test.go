package agentsview_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// listenFuncNames are the listener constructors whose literal address
// arguments this guard inspects.
var listenFuncNames = map[string]bool{
	"Listen":             true,
	"ListenTCP":          true,
	"ListenUDP":          true,
	"ListenIP":           true,
	"ListenUnix":         true,
	"ListenPacket":       true,
	"ListenAndServe":     true,
	"ListenAndServeTLS":  true,
	"ListenMulticastUDP": true,
}

// wildcardIdents are the net package's wildcard IP values, used when a
// listener takes a *net.TCPAddr instead of an address string.
var wildcardIdents = map[string]bool{
	"IPv4zero":        true,
	"IPv6zero":        true,
	"IPv6unspecified": true,
}

// TestTestsBindLoopbackListeners keeps `go test ./...` free of host-firewall
// prompts. A listener bound to a wildcard address (0.0.0.0, [::], or a bare
// ":port") makes Windows Defender Firewall ask whether the test binary may
// communicate on public and private networks; because `go test` relinks
// <pkg>.test.exe into a fresh temporary directory on every run and the
// firewall records its decision per program path, the prompt returns on every
// run. Loopback listeners never prompt.
//
// A test that genuinely needs wildcard bind semantics opts in by calling
// testutil.RequireWildcardListen, which skips it where the prompt would
// appear.
func TestTestsBindLoopbackListeners(t *testing.T) {
	root, err := filepath.Abs(".")
	require.NoError(t, err)

	fset := token.NewFileSet()
	var findings []string

	require.NoError(t, filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".worktrees", "node_modules", "frontend", "vendor", "docs", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			if callsWildcardOptIn(fn) {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if !isListenCall(call) {
					return true
				}
				for _, arg := range call.Args {
					if reason, bad := wildcardArg(arg); bad {
						findings = append(findings, fmt.Sprintf(
							"%s: %s binds %s",
							fset.Position(call.Pos()), fn.Name.Name, reason,
						))
					}
				}
				return true
			})
		}
		return nil
	}))

	require.Empty(t, findings, strings.Join(append([]string{
		"tests must bind loopback (127.0.0.1 / [::1]) so the host firewall " +
			"does not prompt for the test binary; call " +
			"testutil.RequireWildcardListen(t) if a test truly needs a " +
			"wildcard listener:",
	}, findings...), "\n"))
}

// callsWildcardOptIn reports whether fn (anywhere in its body, including
// subtest closures) calls testutil.RequireWildcardListen.
func callsWildcardOptIn(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == "RequireWildcardListen" {
			found = true
		}
		return !found
	})
	return found
}

func isListenCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		return listenFuncNames[fn.Sel.Name]
	case *ast.Ident:
		return listenFuncNames[fn.Name]
	}
	return false
}

// wildcardArg reports whether arg is a literal wildcard listen address or a
// wildcard net.IP value.
func wildcardArg(arg ast.Expr) (string, bool) {
	switch v := arg.(type) {
	case *ast.BasicLit:
		if v.Kind != token.STRING {
			return "", false
		}
		addr, err := strconv.Unquote(v.Value)
		if err != nil || !strings.Contains(addr, ":") {
			return "", false
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return "", false
		}
		switch host {
		case "", "0.0.0.0", "::":
			return strconv.Quote(addr), true
		}
		return "", false
	case *ast.SelectorExpr:
		if wildcardIdents[v.Sel.Name] {
			return "net." + v.Sel.Name, true
		}
	case *ast.UnaryExpr:
		return wildcardArg(v.X)
	case *ast.CompositeLit:
		for _, elt := range v.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if reason, bad := wildcardArg(kv.Value); bad {
				return reason, true
			}
		}
	}
	return "", false
}
