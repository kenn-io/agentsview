package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureImports = `import ("testing"; "time"; "github.com/stretchr/testify/assert"; "github.com/stretchr/testify/require")`

func fixtureSource(imports, function, body string) string {
	return "package fixture\n" + imports + "\nfunc " + function + "(t *testing.T) {\n" + body + "\n}\n"
}

func fixtureDiagnostic(path string, line int, assertion, duration string) string {
	return fmt.Sprintf("%s:%d: github.com/stretchr/testify/%s budget %s is below 1s; synchronize in-process work or justify a retained integration budget in allowedBudgets\n", path, line, assertion, duration)
}

func writeTimingFixtures(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, source := range files {
		fullPath := filepath.Join(root, filepath.FromSlash(path))
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(source), 0o644))
	}
}

func assertTimingScan(t *testing.T, root string, files map[string]string, wantCode int, wantOutput string) {
	t.Helper()
	var stderr bytes.Buffer
	assert.Equal(t, wantCode, run([]string{root}, &stderr))
	assert.Equal(t, wantOutput, stderr.String())
	if stderr.Len() > 0 {
		t.Log(strings.TrimSpace(stderr.String()))
	}
	for path, source := range files {
		after, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		require.NoError(t, err)
		assert.Equal(t, []byte(source), after, "source bytes for %s", path)
	}
}

func TestRunTimingBudgetFixtures(t *testing.T) {
	t.Run("repository-relative subdirectory keeps allowances", func(t *testing.T) {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module fixture\n"), 0o644))
		files := map[string]string{
			"internal/sync/engine_test.go": fixtureSource(fixtureImports, "TestStartupMaintenanceWaitsForForegroundSyncAndSerializesLaterSyncs", "assert.Never(t, nil, 100*time.Millisecond, time.Millisecond)"),
		}
		writeTimingFixtures(t, root, files)
		var stderr bytes.Buffer
		assert.Equal(t, 0, run([]string{filepath.Join(root, "internal", "sync")}, &stderr))
		assert.Empty(t, stderr.String())
		after, err := os.ReadFile(filepath.Join(root, "internal", "sync", "engine_test.go"))
		require.NoError(t, err)
		assert.Equal(t, []byte(files["internal/sync/engine_test.go"]), after)
	})

	for _, assertion := range []string{"assert.Eventually", "require.Eventually", "assert.Never", "require.Never"} {
		t.Run(assertion+" rejects 50ms", func(t *testing.T) {
			files := map[string]string{"fixture_test.go": fixtureSource(fixtureImports, "TestFixture", assertion+"(t, func() bool { panic(\"must never execute\") }, 50*time.Millisecond, time.Millisecond)")}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			assertTimingScan(t, root, files, 1, fixtureDiagnostic("fixture_test.go", 4, assertion, "50ms"))
		})
	}
	for _, tt := range []struct {
		name       string
		expression string
		wantCode   int
		duration   string
	}{
		{"reject 999ms", "999*time.Millisecond", 1, "999ms"},
		{"reject time.Second divided by 2", "time.Second/2", 1, "500ms"},
		{"reject raw 500_000_000 nanoseconds", "500_000_000", 1, "500ms"},
		{"reject integral fractional 0.5 times time.Second", "0.5*time.Second", 1, "500ms"},
		{"reject zero", "0", 1, "0s"},
		{"reject negative 50ms", "-50*time.Millisecond", 1, "-50ms"},
		{"reject parenthesized arithmetic 50ms", "(time.Second - 900*time.Millisecond)/2", 1, "50ms"},
		{"reject converted 50ms", "time.Duration(50)*time.Millisecond", 1, "50ms"},
		{"reject integer division 999999999ns", "time.Second/3*3", 1, "999.999999ms"},
		{"reject shift 500ms", "(1 << 9 - 12)*time.Millisecond", 1, "500ms"},
		{"reject remainder 50ms", "(150 % 100)*time.Millisecond", 1, "50ms"},
		{"reject bitwise arithmetic 50ms", "((+51 & 63 | 0) ^ 1 &^ 0)*time.Millisecond", 1, "50ms"},
		{"reject unary complement negative 1ns", "^0", 1, "-1ns"},
		{"reject microseconds and nanoseconds", "49999*time.Microsecond + 1000*time.Nanosecond", 1, "50ms"},
		{"accept exactly 1s with 1ms tick", "time.Second", 0, ""},
		{"accept 2s", "2*time.Second", 0, ""},
		{"accept minute", "time.Minute", 0, ""},
		{"accept hour", "time.Hour", 0, ""},
		{"accept named 50ms constant", "short", 0, ""},
		{"accept expression containing named constant", "short+time.Millisecond", 0, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"fixture_test.go": fixtureSource(fixtureImports, "TestFixture", "const short = 50*time.Millisecond\nassert.Eventually(t, func() bool { panic(\"must never execute\") }, "+tt.expression+", time.Millisecond)")}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			wantOutput := ""
			if tt.wantCode == 1 {
				wantOutput = fixtureDiagnostic("fixture_test.go", 5, "assert.Eventually", tt.duration)
			}
			assertTimingScan(t, root, files, tt.wantCode, wantOutput)
		})
	}
	for _, tt := range []struct {
		name    string
		imports string
		body    string
		want    string
	}{
		{"aliased assert and time", `import ("testing"; clock "time"; check "github.com/stretchr/testify/assert")`, `check.Never(t, nil, clock.Duration(50)*clock.Millisecond, 1)`, fixtureDiagnostic("fixture_test.go", 4, "assert.Never", "50ms")},
		{"aliased require", `import ("testing"; clock "time"; check "github.com/stretchr/testify/require")`, `check.Eventually(t, nil, clock.Second/2, 1)`, fixtureDiagnostic("fixture_test.go", 4, "require.Eventually", "500ms")},
		{"unrelated assert import", `import ("testing"; "time"; assert "example.org/assert")`, `assert.Never(t, nil, 50*time.Millisecond, 1)`, ""},
		{"unrelated time import", `import ("testing"; time "example.org/clock"; "github.com/stretchr/testify/assert")`, `assert.Never(t, nil, 50*time.Millisecond, 1)`, ""},
		{"shadowed assert", fixtureImports, `assert := fake(); assert.Never(t, nil, 50000000, 1)`, ""},
		{"shadowed require parameter", fixtureImports, `func(require fake) { require.Eventually(t, nil, 50000000, 1) }(fake{})`, ""},
		{"shadowed time variable", fixtureImports, `time := fake(); assert.Never(t, nil, 50*time.Millisecond, 1)`, ""},
		{"shadowed time conversion", fixtureImports, `time := fake(); assert.Never(t, nil, time.Duration(50000000), 1)`, ""},
		{"shadow ends with block", fixtureImports, "{ assert := fake(); assert.Never(t, nil, 50000000, 1) }\nassert.Never(t, nil, 50*time.Millisecond, 1)", fixtureDiagnostic("fixture_test.go", 5, "assert.Never", "50ms")},
		{"declaration RHS uses import", fixtureImports, `assert := assert.Never(t, nil, 50*time.Millisecond, 1); _ = assert`, fixtureDiagnostic("fixture_test.go", 4, "assert.Never", "50ms")},
		{"dot imports", `import ("testing"; . "time"; . "github.com/stretchr/testify/assert")`, `Never(t, nil, 50*Millisecond, 1)`, ""},
		{"excluded call forms", fixtureImports, "assert.Neverf(t, nil, 50000000, 1, \"message\")\nrequire.Eventuallyf(t, nil, 50000000, 1, \"message\")\nassert.EventuallyWithT(t, nil, 50000000, 1)\nassert.New(t).Never(nil, 50000000, 1)\ntime.Sleep(50*time.Millisecond)", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"fixture_test.go": fixtureSource(tt.imports, "TestFixture", tt.body)}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			wantCode := 0
			if tt.want != "" {
				wantCode = 1
			}
			assertTimingScan(t, root, files, wantCode, tt.want)
		})
	}
	for _, path := range []string{"nested/fixture_darwin_test.go", "fixture.go", "vendor/fixture_test.go", "node_modules/fixture_test.go", "testdata/fixture_test.go", ".hidden/fixture_test.go", "_fixtures/fixture_test.go", "nested/testdata/fixture_test.go"} {
		t.Run("file selection "+path, func(t *testing.T) {
			files := map[string]string{path: "//go:build darwin\n\n" + fixtureSource(fixtureImports, "TestFixture", "assert.Never(t, nil, 50*time.Millisecond, 1)")}
			root := t.TempDir()
			writeTimingFixtures(t, root, files)
			wantCode, wantOutput := 0, ""
			if path == "nested/fixture_darwin_test.go" {
				wantCode, wantOutput = 1, fixtureDiagnostic(path, 6, "assert.Never", "50ms")
			}
			assertTimingScan(t, root, files, wantCode, wantOutput)
		})
	}
	t.Run("explicit excluded-name root still scans", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "_root")
		files := map[string]string{"fixture_test.go": fixtureSource(fixtureImports, "TestFixture", "assert.Never(t, nil, 50000000, 1)")}
		writeTimingFixtures(t, root, files)
		assertTimingScan(t, root, files, 1, fixtureDiagnostic("fixture_test.go", 4, "assert.Never", "50ms"))
	})
	t.Run("default root and deterministic diagnostic order", func(t *testing.T) {
		root := t.TempDir()
		source := fixtureSource(fixtureImports, "TestFixture", "assert.Never(t, nil, 50000000, 1)")
		files := map[string]string{"z_test.go": source, "a_test.go": source}
		writeTimingFixtures(t, root, files)
		t.Chdir(root)
		var stderr bytes.Buffer
		assert.Equal(t, 1, run(nil, &stderr))
		assert.Equal(t, fixtureDiagnostic("a_test.go", 4, "assert.Never", "50ms")+fixtureDiagnostic("z_test.go", 4, "assert.Never", "50ms"), stderr.String())
		assertTimingScan(t, root, files, 1, stderr.String())
	})
}

func TestRunTimingBudgetAllowances(t *testing.T) {
	t.Run("18 allowance records retain 21 calls across repeated scans", func(t *testing.T) {
		files := make(map[string]string)
		for _, tt := range []struct {
			path     string
			function string
			budget   string
			count    int
		}{
			{"cmd/agentsview/embed_scheduler_test.go", "TestRecallSchedulerRequiresExplicitOptInForAutomaticBuilds", "100*time.Millisecond", 2},
			{"cmd/agentsview/pricing_schedule_test.go", "TestStartPeriodicPricingRefreshWaitsForResyncSwap", "50*time.Millisecond", 1},
			{"cmd/agentsview/pricing_schedule_test.go", "TestSeedPricingWaitsForResyncSwap", "50*time.Millisecond", 1},
			{"internal/fsevents/fsevents_darwin_test.go", "TestStreamCloseWaitsForCallback", "200*time.Millisecond", 1},
			{"internal/server/huma_routes_sync_internal_test.go", "TestForegroundSyncReleasesDeferredStartupMaintenance", "100*time.Millisecond", 1},
			{"internal/sync/engine_test.go", "TestStartupMaintenanceWaitsForForegroundSyncAndSerializesLaterSyncs", "100*time.Millisecond", 3},
			{"internal/sync/engine_test.go", "TestStartupSyncFallbackRechecksAfterInFlightForegroundSync", "100*time.Millisecond", 1},
			{"internal/sync/signal_schedule_test.go", "TestDeferredSignalRecomputeSerializesWithSync", "100*time.Millisecond", 1},
			{"internal/sync/watch_backend_fsnotify_test.go", "TestFSNotifyBackendOrdinaryErrorRemainsAnError", "50*time.Millisecond", 1},
			{"internal/sync/watcher_test.go", "TestWatcherLifecycleCollectsBeforeDispatchOpens", "50*time.Millisecond", 1},
			{"internal/sync/watcher_test.go", "TestWatcherQueueRetryBatchDispatchesQueuedRoots", "50*time.Millisecond", 1},
			{"internal/sync/watcher_test.go", "TestWatcherSchedulerContinuesIntakeWithOnePendingAccumulator", "50*time.Millisecond", 1},
			{"internal/sync/watcher_test.go", "TestWatcherDoesNotReplayOrdinaryBatchOnCallbackError", "50*time.Millisecond", 1},
			{"internal/sync/watcher_test.go", "TestWatcherDoesNotReplayKnownFileRenameOnCallbackError", "50*time.Millisecond", 1},
			{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherNativeSinkCollapsesBlockedConsumerOverflow", "100*time.Millisecond", 1},
			{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherColdArchiveCardinalityUsesOneRecursiveStream", "250*time.Millisecond", 1},
			{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherExcludesRecursiveEvents", "400*time.Millisecond", 1},
			{"internal/sync/watcher_darwin_test.go", "TestDarwinWatcherExcludedDirectoryRenameIsFiltered", "200*time.Millisecond", 1},
		} {
			if files[tt.path] == "" {
				files[tt.path] = "package fixture\n" + fixtureImports + "\n"
			}
			body := strings.Repeat("func() { assert.Never(t, nil, "+tt.budget+", time.Millisecond) }()\n", tt.count)
			files[tt.path] += "func " + tt.function + "(t *testing.T) {\n" + body + "}\n"
		}
		root := t.TempDir()
		writeTimingFixtures(t, root, files)
		assertTimingScan(t, root, files, 0, "")
		assertTimingScan(t, root, files, 0, "")
	})
	const allowedPath = "cmd/agentsview/embed_scheduler_test.go"
	const allowedFunction = "TestRecallSchedulerRequiresExplicitOptInForAutomaticBuilds"
	for _, tt := range []struct {
		name     string
		path     string
		function string
		body     string
		want     string
	}{
		{"extra third occurrence", allowedPath, allowedFunction, "assert.Never(t, nil, 100*time.Millisecond, 1)\nassert.Never(t, nil, 100*time.Millisecond, 1)\nassert.Never(t, nil, 100*time.Millisecond, 1)", fixtureDiagnostic(allowedPath, 6, "assert.Never", "100ms")},
		{"changed duration 99ms", allowedPath, allowedFunction, "assert.Never(t, nil, 99*time.Millisecond, 1)", fixtureDiagnostic(allowedPath, 4, "assert.Never", "99ms")},
		{"changed function", allowedPath, "TestDifferent", "assert.Never(t, nil, 100*time.Millisecond, 1)", fixtureDiagnostic(allowedPath, 4, "assert.Never", "100ms")},
		{"changed file", "other_test.go", allowedFunction, "assert.Never(t, nil, 100*time.Millisecond, 1)", fixtureDiagnostic("other_test.go", 4, "assert.Never", "100ms")},
		{"Eventually at Never identity", allowedPath, allowedFunction, "assert.Eventually(t, nil, 100*time.Millisecond, 1)", fixtureDiagnostic(allowedPath, 4, "assert.Eventually", "100ms")},
		{"require at assert identity", allowedPath, allowedFunction, "require.Never(t, nil, 100*time.Millisecond, 1)", fixtureDiagnostic(allowedPath, 4, "require.Never", "100ms")},
		{"deleted wait", allowedPath, allowedFunction, "", ""},
		{"replacement at same identity and duration", allowedPath, allowedFunction, "assert.Never(t, func() bool { panic(\"replacement\") }, time.Second/10, time.Microsecond)", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{tt.path: fixtureSource(fixtureImports, tt.function, tt.body)}
			writeTimingFixtures(t, root, files)
			wantCode := 0
			if tt.want != "" {
				wantCode = 1
			}
			assertTimingScan(t, root, files, wantCode, tt.want)
		})
	}
	t.Run("package initializer has no allowance", func(t *testing.T) {
		files := map[string]string{allowedPath: "package fixture\n" + fixtureImports + "\nvar result = assert.Never(nil, nil, 100*time.Millisecond, 1)\n"}
		root := t.TempDir()
		writeTimingFixtures(t, root, files)
		assertTimingScan(t, root, files, 1, fixtureDiagnostic(allowedPath, 3, "assert.Never", "100ms"))
	})
}

func TestRunTimingBudgetErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
	}{
		{"short calls", "assert.Never()\nrequire.Eventually(t, nil, 50000000)"},
		{"unrelated selector", "other.Never(t, nil, 50000000, 1)"},
		{"string containing call text", `_ = "assert.Never(t, nil, 50000000, 1)"`},
		{"named function result", "assert.Never(t, nil, budget(), 1)"},
		{"division by zero", "assert.Never(t, nil, time.Second/0, 1)"},
		{"overflowing expression", "assert.Never(t, nil, 1<<100, 1)"},
		{"nonintegral duration", "assert.Never(t, nil, 0.5, 1)"},
		{"invalid conversion", "assert.Never(t, nil, time.Duration(0.5), 1)"},
		{"overflowing conversion", "assert.Never(t, nil, time.Duration(1<<100), 1)"},
		{"conversion argument count", "assert.Never(t, nil, time.Duration(1, 2), 1)"},
		{"negative shift", "assert.Never(t, nil, 1<<-1, 1)"},
		{"unsupported string and comparison", "assert.Never(t, nil, \"short\", 1)\nassert.Never(t, nil, 1<2, 1)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			files := map[string]string{"fixture_test.go": fixtureSource(fixtureImports, "TestFixture", tt.body)}
			writeTimingFixtures(t, root, files)
			assertTimingScan(t, root, files, 0, "")
		})
	}
	t.Run("invalid syntax returns 2", func(t *testing.T) {
		root := t.TempDir()
		files := map[string]string{"broken_test.go": "package fixture\nfunc (\n"}
		writeTimingFixtures(t, root, files)
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{root}, &stderr))
		assert.Contains(t, stderr.String(), "check-timing-budgets: broken_test.go:2:")
		t.Log(strings.TrimSpace(stderr.String()))
		after, err := os.ReadFile(filepath.Join(root, "broken_test.go"))
		require.NoError(t, err)
		assert.Equal(t, []byte(files["broken_test.go"]), after)
	})
	t.Run("missing root returns 2", func(t *testing.T) {
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{filepath.Join(t.TempDir(), "missing")}, &stderr))
		assert.Contains(t, stderr.String(), "check-timing-budgets:")
		assert.Contains(t, stderr.String(), "missing")
	})
	t.Run("file root returns 2", func(t *testing.T) {
		root := t.TempDir()
		writeTimingFixtures(t, root, map[string]string{"fixture_test.go": "package fixture\n"})
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{filepath.Join(root, "fixture_test.go")}, &stderr))
		assert.Contains(t, stderr.String(), "expected a directory")
	})
	t.Run("excess arguments returns 2", func(t *testing.T) {
		var stderr bytes.Buffer
		assert.Equal(t, 2, run([]string{"one", "two"}, &stderr))
		assert.Equal(t, "usage: check-timing-budgets [directory]\n", stderr.String())
	})
}
