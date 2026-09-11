package main

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type budgetKey struct {
	path      string
	function  string
	assertion string
	budget    time.Duration
}

type budgetAllowance struct {
	count  int
	reason string
}

var allowedBudgets = map[budgetKey]budgetAllowance{
	{"cmd/agentsview/embed_scheduler_test.go", "TestRecallSchedulerRequiresExplicitOptInForAutomaticBuilds", "github.com/stretchr/testify/assert.Never", 100 * time.Millisecond}:      {2, "Observe automatic-build absence around real loopback embedding and HTTP import servers."},
	{"cmd/agentsview/pricing_schedule_test.go", "TestStartPeriodicPricingRefreshWaitsForResyncSwap", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:               {1, "Observe exclusion while resync holds the engine lock and replaces the SQLite database."},
	{"cmd/agentsview/pricing_schedule_test.go", "TestSeedPricingWaitsForResyncSwap", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:                               {1, "Observe seed exclusion during the real SQLite replacement path."},
	{"internal/fsevents/fsevents_darwin_test.go", "TestStreamCloseWaitsForCallback", "github.com/stretchr/testify/assert.Never", 200 * time.Millisecond}:                              {1, "Observe native stream close while an FSEvents callback remains blocked."},
	{"internal/server/huma_routes_sync_internal_test.go", "TestForegroundSyncReleasesDeferredStartupMaintenance", "github.com/stretchr/testify/assert.Never", 100 * time.Millisecond}: {1, "Retain the sync/resync integration observation before foreground work releases maintenance."},
	{"internal/sync/engine_test.go", "TestStartupMaintenanceWaitsForForegroundSyncAndSerializesLaterSyncs", "github.com/stretchr/testify/assert.Never", 100 * time.Millisecond}:       {3, "Retain observations around foreground completion and the engine's exclusive mutex."},
	{"internal/sync/engine_test.go", "TestStartupSyncFallbackRechecksAfterInFlightForegroundSync", "github.com/stretchr/testify/assert.Never", 100 * time.Millisecond}:                {1, "Observe fallback blocking behind the foreground operation's mutex."},
	{"internal/sync/signal_schedule_test.go", "TestDeferredSignalRecomputeSerializesWithSync", "github.com/stretchr/testify/assert.Never", 100 * time.Millisecond}:                    {1, "Observe a flush blocked on the explicitly held syncMu."},
	{"internal/sync/watch_backend_fsnotify_test.go", "TestFSNotifyBackendOrdinaryErrorRemainsAnError", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:             {1, "Observe absence of recovery events with the real fsnotify backend running."},
	{"internal/sync/watcher_test.go", "TestWatcherLifecycleCollectsBeforeDispatchOpens", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:                           {1, "Retain the fake-backend watcher observation that collection emits no callback before dispatch opens."},
	{"internal/sync/watcher_test.go", "TestWatcherQueueRetryBatchDispatchesQueuedRoots", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:                           {1, "Retain the fake-backend watcher observation for queued roots before dispatch opens."},
	{"internal/sync/watcher_test.go", "TestWatcherSchedulerContinuesIntakeWithOnePendingAccumulator", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:              {1, "Retain the fake-backend watcher observation while another callback holds the consumer."},
	{"internal/sync/watcher_test.go", "TestWatcherDoesNotReplayOrdinaryBatchOnCallbackError", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:                      {1, "Retain the fake-backend watcher observation for unwanted retries."},
	{"internal/sync/watcher_test.go", "TestWatcherDoesNotReplayKnownFileRenameOnCallbackError", "github.com/stretchr/testify/assert.Never", 50 * time.Millisecond}:                    {1, "Retain the fake-backend watcher observation for unwanted rename retries."},
	{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherNativeSinkCollapsesBlockedConsumerOverflow", "github.com/stretchr/testify/assert.Never", 100 * time.Millisecond}:       {1, "Retain the direct-test-backend Darwin watcher observation for extra delivery after overflow."},
	{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherColdArchiveCardinalityUsesOneRecursiveStream", "github.com/stretchr/testify/assert.Never", 250 * time.Millisecond}:     {1, "Observe absence of reconciliation after real filesystem replacement."},
	{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherExcludesRecursiveEvents", "github.com/stretchr/testify/assert.Never", 400 * time.Millisecond}:                          {1, "Observe excluded writes through a running native watcher."},
	{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherExcludedDirectoryRenameIsFiltered", "github.com/stretchr/testify/assert.Never", 200 * time.Millisecond}:                {1, "Observe excluded rename delivery through a running native watcher."},
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	if len(args) > 1 {
		fmt.Fprintln(stderr, "usage: check-timing-budgets [directory]")
		return 2
	}
	root := "."
	if len(args) == 1 {
		root = args[0]
	}
	violations, err := check(root, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "check-timing-budgets: %v\n", err)
		return 2
	}
	if violations > 0 {
		return 1
	}
	return 0
}

func check(root string, stderr io.Writer) (int, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		return 0, fmt.Errorf("%s: expected a directory", root)
	}
	walkRoot, err := filepath.Abs(root)
	if err != nil {
		return 0, err
	}
	allowanceRoot := findModuleRoot(walkRoot)
	if allowanceRoot == "" {
		allowanceRoot = walkRoot
	}
	used := make(map[budgetKey]int)
	violations := 0
	err = filepath.WalkDir(walkRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != walkRoot && (name == "vendor" || name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !strings.HasSuffix(name, "_test.go") {
			return nil
		}
		relativePath, err := filepath.Rel(allowanceRoot, path)
		if err != nil {
			return err
		}
		count, err := checkFile(path, filepath.ToSlash(relativePath), used, stderr)
		violations += count
		return err
	})
	return violations, err
}

func findModuleRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func checkFile(path, relativePath string, used map[budgetKey]int, stderr io.Writer) (int, error) {
	source, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	fset := token.NewFileSet()
	// Parser object resolution distinguishes local declarations from import qualifiers.
	file, err := parser.ParseFile(fset, relativePath, source, 0)
	if err != nil {
		return 0, err
	}
	imports := make(map[string]string)
	timeImport := ""
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return 0, err
		}
		name := filepath.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." || name == "_" {
			continue
		}
		switch importPath {
		case "github.com/stretchr/testify/assert", "github.com/stretchr/testify/require":
			imports[name] = importPath
		case "time":
			timeImport = name
		}
	}
	violations := 0
	for _, decl := range file.Decls {
		function := ""
		if fn, ok := decl.(*ast.FuncDecl); ok {
			function = fn.Name.Name
		}
		ast.Inspect(decl, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || len(call.Args) < 4 {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "Eventually" && selector.Sel.Name != "Never") {
				return true
			}
			qualifier, ok := selector.X.(*ast.Ident)
			if !ok || qualifier.Obj != nil || imports[qualifier.Name] == "" {
				return true
			}
			budget, known := literalDuration(call.Args[2], timeImport)
			if !known || budget >= time.Second {
				return true
			}
			assertion := imports[qualifier.Name] + "." + selector.Sel.Name
			key := budgetKey{relativePath, function, assertion, budget}
			used[key]++
			if used[key] <= allowedBudgets[key].count {
				return true
			}
			violations++
			fmt.Fprintf(stderr, "%s:%d: %s budget %s is below 1s; synchronize in-process work or justify a retained integration budget in allowedBudgets\n", relativePath, fset.Position(call.Pos()).Line, assertion, budget)
			return true
		})
	}
	return violations, nil
}

func literalDuration(expr ast.Expr, timeImport string) (time.Duration, bool) {
	expression, ok := literalExpression(expr, timeImport)
	if !ok {
		return 0, false
	}
	value, err := types.Eval(token.NewFileSet(), nil, token.NoPos, expression)
	if err != nil || value.Value == nil {
		return 0, false
	}
	nanoseconds, ok := constant.Int64Val(constant.ToInt(value.Value))
	return time.Duration(nanoseconds), ok
}

func literalExpression(expr ast.Expr, timeImport string) (string, bool) {
	switch expr := expr.(type) {
	case *ast.BasicLit:
		return expr.Value, expr.Kind == token.INT || expr.Kind == token.FLOAT
	case *ast.ParenExpr:
		inner, ok := literalExpression(expr.X, timeImport)
		return "(" + inner + ")", ok
	case *ast.UnaryExpr:
		if expr.Op != token.ADD && expr.Op != token.SUB && expr.Op != token.XOR {
			return "", false
		}
		inner, ok := literalExpression(expr.X, timeImport)
		return "(" + expr.Op.String() + inner + ")", ok
	case *ast.BinaryExpr:
		switch expr.Op {
		case token.ADD, token.SUB, token.MUL, token.QUO, token.REM, token.AND, token.OR, token.XOR, token.SHL, token.SHR, token.AND_NOT:
			left, leftOK := literalExpression(expr.X, timeImport)
			right, rightOK := literalExpression(expr.Y, timeImport)
			return "(" + left + " " + expr.Op.String() + " " + right + ")", leftOK && rightOK
		}
	case *ast.SelectorExpr:
		qualifier, ok := expr.X.(*ast.Ident)
		if !ok || qualifier.Obj != nil || qualifier.Name != timeImport {
			return "", false
		}
		units := map[string]time.Duration{"Nanosecond": time.Nanosecond, "Microsecond": time.Microsecond, "Millisecond": time.Millisecond, "Second": time.Second, "Minute": time.Minute, "Hour": time.Hour}
		unit, ok := units[expr.Sel.Name]
		return strconv.FormatInt(int64(unit), 10), ok
	case *ast.CallExpr:
		selector, ok := expr.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "Duration" || len(expr.Args) != 1 || expr.Ellipsis.IsValid() {
			return "", false
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || qualifier.Obj != nil || qualifier.Name != timeImport {
			return "", false
		}
		inner, ok := literalExpression(expr.Args[0], timeImport)
		return "int64(" + inner + ")", ok
	}
	return "", false
}
