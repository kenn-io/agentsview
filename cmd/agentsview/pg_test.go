package main

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/postgres"
)

func loadPGServeConfigForTest(t *testing.T, args ...string) (config.Config, string, error) {
	t.Helper()
	cmd := newPGServeCommand()
	if err := cmd.Flags().Parse(args); err != nil {
		return config.Config{}, "", err
	}
	return loadPGServeConfig(cmd)
}

func restoreTestLogger(t *testing.T) {
	t.Helper()
	oldWriter := log.Writer()
	t.Cleanup(func() {
		if file, ok := log.Writer().(*os.File); ok && file != os.Stderr && file != os.Stdout {
			_ = file.Close()
		}
		log.SetOutput(oldWriter)
	})
}

func clearConfiguredAgentEnvVars(t *testing.T) {
	t.Helper()
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			t.Setenv(def.EnvVar, "")
		}
	}
}

func isolateDefaultAgentDirs(t *testing.T, root string) {
	t.Helper()
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("APPDATA", root)
	t.Setenv("LOCALAPPDATA", root)
	t.Setenv("HOMEDRIVE", filepath.VolumeName(root))
	t.Setenv("HOMEPATH", `\`)
}

func TestLoadPGServeConfigDoesNotInheritServeProxySettings(t *testing.T) {
	dataDir := testDataDir(t)

	err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
public_url = "https://viewer.example.test"
public_origins = ["https://app.example.test"]

[proxy]
mode = "caddy"
bind_host = "0.0.0.0"
public_port = 8443
tls_cert = "/tmp/viewer.crt"
tls_key = "/tmp/viewer.key"
allowed_subnets = ["10.0.0.0/16"]

[pg]
url = "postgres://user:pass@db.example.test:5432/agentsview?sslmode=require"
`), 0o600)
	require.NoError(t, err)

	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err, "loadPGServeConfigForTest")
	require.NotEmpty(t, cfg.PG.URL, "expected PG URL")
	assert.Empty(t, cfg.PublicURL, "PublicURL should be empty")
	assert.Empty(t, cfg.PublicOrigins, "PublicOrigins should be empty")
	assert.Empty(t, cfg.Proxy.Mode, "Proxy.Mode should be empty")
	assert.Equal(t, "127.0.0.1", cfg.Host)
	assert.Equal(t, 8080, cfg.Port)
}

func TestLoadPGServeConfigIgnoresInvalidPersistedServeSettings(t *testing.T) {
	dataDir := testDataDir(t)

	err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
public_url = "not a url"

[proxy]
mode = "bogus"

[pg]
url = "postgres://user:pass@db.example.test:5432/agentsview?sslmode=require"
`), 0o600)
	require.NoError(t, err)

	cfg, _, err := loadPGServeConfigForTest(t)
	require.NoError(t, err, "loadPGServeConfigForTest")
	require.NotEmpty(t, cfg.PG.URL, "expected PG URL")
	assert.Empty(t, cfg.PublicURL, "PublicURL should be empty")
	assert.Empty(t, cfg.Proxy.Mode, "Proxy.Mode should be empty")
}

func TestPGServeConfigAcceptsManagedCaddyFlags(t *testing.T) {
	testDataDir(t)

	cfg, basePath, err := loadPGServeConfigForTest(t,
		"--host", "127.0.0.1",
		"--port", "8081",
		"--public-url", "https://viewer.example.test",
		"--public-origin", "https://app.example.test/",
		"--proxy", "caddy",
		"--caddy-bin", "/usr/local/bin/caddy",
		"--proxy-bind-host", "0.0.0.0",
		"--public-port", "8443",
		"--tls-cert", "/tmp/viewer.crt",
		"--tls-key", "/tmp/viewer.key",
		"--allowed-subnet", "10.0.0.0/16",
	)
	require.NoError(t, err, "loadPGServeConfigForTest")
	assert.Equal(t, "caddy", cfg.Proxy.Mode)
	assert.Equal(t, "https://viewer.example.test:8443", cfg.PublicURL)
	assert.Equal(t,
		"https://app.example.test,https://viewer.example.test:8443",
		strings.Join(cfg.PublicOrigins, ","))
	assert.Equal(t, "/usr/local/bin/caddy", cfg.Proxy.Bin)
	assert.Equal(t, "0.0.0.0", cfg.Proxy.BindHost)
	assert.Equal(t, 8443, cfg.Proxy.PublicPort)
	assert.Equal(t, "/tmp/viewer.crt", cfg.Proxy.TLSCert)
	assert.Equal(t, "/tmp/viewer.key", cfg.Proxy.TLSKey)
	assert.Equal(t, "10.0.0.0/16",
		strings.Join(cfg.Proxy.AllowedSubnets, ","))
	assert.Empty(t, basePath, "basePath should be empty")
}

func TestRunPGPush_IgnoresBrokenUnselectedTarget(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	writeTestConfig(t, dataDir, `
default_pg = "archive"

[pg.work]
url = "${BROKEN_WORK_TARGET}"
machine_name = "workbox"

[pg.archive]
url = "postgres://archive"
`)

	var err error
	out := captureStdout(t, func() {
		err = runPGPush(PGPushConfig{}, "archive")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg connection to archive permits plaintext")
	assert.Contains(t, err.Error(), "allow_insecure = true under [pg] or [pg.NAME]")
	assert.NotContains(t, err.Error(), "BROKEN_WORK_TARGET")
	assert.Contains(t, out, "Target: archive")
}

func TestRunPGStatus_IgnoresBrokenUnselectedTarget(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	writeTestConfig(t, dataDir, `
default_pg = "archive"

[pg.work]
url = "${BROKEN_WORK_TARGET}"
machine_name = "workbox"

[pg.archive]
url = "postgres://archive"
`)

	err := runPGStatus("archive", PGStatusConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg connection to archive permits plaintext")
	assert.Contains(t, err.Error(), "allow_insecure = true under [pg] or [pg.NAME]")
	assert.NotContains(t, err.Error(), "BROKEN_WORK_TARGET")
	assert.NoFileExists(t, filepath.Join(dataDir, "sessions.db"))
}

func TestRunPGStatus_IgnoresUnreadableLocalWatermark(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	writeTestConfig(t, dataDir, `
default_pg = "archive"

[pg.archive]
url = "postgres://archive"
`)
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "sessions.db"),
		nil,
		0o600,
	))

	err := runPGStatus("archive", PGStatusConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg connection to archive permits plaintext")
	assert.Contains(t, err.Error(), "allow_insecure = true under [pg] or [pg.NAME]")
	assert.NotContains(t, err.Error(), "opening database")
	assert.NotContains(t, err.Error(), "sessions.db is empty")
}

func TestRunPGPushAll_AggregatesTargetFailures(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	writeTestConfig(t, dataDir, `
default_pg = "work"

[pg.work]
url = "${BROKEN_WORK_TARGET}"
machine_name = "workbox"

[pg.archive]
url = "postgres://archive"
`)

	err := runPGPush(PGPushConfig{AllTargets: true}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 pg target(s) failed")
	assert.Contains(t, err.Error(), "work (default): expanding url: environment variable(s) not set: BROKEN_WORK_TARGET")
	assert.Contains(t, err.Error(), "archive: pg connection to archive permits plaintext")
	assert.Contains(t, err.Error(), "allow_insecure = true under [pg] or [pg.NAME]")
}

func TestRunPGStatusAll_AggregatesTargetFailures(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	writeTestConfig(t, dataDir, `
default_pg = "work"

[pg.work]
url = "${BROKEN_WORK_TARGET}"
machine_name = "workbox"

[pg.archive]
url = "postgres://archive"
`)

	err := runPGStatus("", PGStatusConfig{AllTargets: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2 pg target(s) failed")
	assert.Contains(t, err.Error(), "work (default): expanding url: environment variable(s) not set: BROKEN_WORK_TARGET")
	assert.Contains(t, err.Error(), "archive: pg connection to archive permits plaintext")
	assert.Contains(t, err.Error(), "allow_insecure = true under [pg] or [pg.NAME]")
}

func TestPGPushCommandPrefixesErrors(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	t.Setenv("AGENTSVIEW_NO_DAEMON", "1")
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	writeTestConfig(t, dataDir, `
default_pg = "archive"

[pg.archive]
url = "postgres://archive"
`)

	_, err := executeCommand(newRootCommand(), "pg", "push", "archive")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg push: pg connection to archive permits plaintext")
}

func TestPGPushWatchCommandPrefixesErrors(t *testing.T) {
	_, err := executeCommand(newRootCommand(), "pg", "push", "--watch", "--all")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg push --watch: --all cannot be combined with --watch")
}

func TestPGStatusCommandPrefixesErrors(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	clearConfiguredAgentEnvVars(t)
	isolateDefaultAgentDirs(t, dataDir)
	restoreTestLogger(t)
	writeTestConfig(t, dataDir, `
default_pg = "archive"

[pg.archive]
url = "postgres://archive"
`)

	_, err := executeCommand(newRootCommand(), "pg", "status", "archive")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pg status: pg connection to archive permits plaintext")
}

func TestRunPGServeRejectsInvalidManagedCaddyConfigBeforePGSetup(t *testing.T) {
	dataDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestRunPGServeHelperProcess", "--",
		"--host", "0.0.0.0",
		"--public-url", "https://viewer.example.test",
		"--proxy", "caddy",
		"--caddy-bin", os.Args[0],
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_RUN_PG_SERVE_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "runPGServe unexpectedly succeeded")
	assert.Contains(t, string(out), "loopback backend host")
}

func TestRunPGServeNonLoopbackWithoutProxyFallsThroughToPGConfig(t *testing.T) {
	dataDir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestRunPGServeHelperProcess", "--",
		"--host", "0.0.0.0",
		"--port", "8081",
	)
	cmd.Env = append(
		os.Environ(),
		"AGENTSVIEW_RUN_PG_SERVE_HELPER=1",
		"AGENTSVIEW_DATA_DIR="+dataDir,
	)
	out, err := cmd.CombinedOutput()
	require.Error(t, err, "runPGServe unexpectedly succeeded")
	output := string(out)
	assert.NotContains(t, output, "invalid serve config",
		"unexpected serve validation failure")
	assert.Contains(t, output, "pg serve: url not configured")
}

func TestRunPGServeHelperProcess(t *testing.T) {
	if os.Getenv("AGENTSVIEW_RUN_PG_SERVE_HELPER") != "1" {
		return
	}

	args := os.Args
	sep := -1
	for i, arg := range args {
		if arg == "--" {
			sep = i
			break
		}
	}
	require.NotEqual(t, -1, sep, "missing argument separator")

	cmd := newPGServeCommand()
	require.NoError(t, cmd.Flags().Parse(args[sep+1:]))
	cfg, basePath, err := loadPGServeConfig(cmd)
	require.NoError(t, err)
	runPGServe(cfg, basePath)
}

func TestWritePGPushSummaryIncludesSkippedConflicts(t *testing.T) {
	var out bytes.Buffer

	writePGPushSummary(&out, postgres.PushResult{
		SessionsPushed:   3,
		MessagesPushed:   9,
		SkippedConflicts: 2,
		Duration:         1500 * time.Millisecond,
	})

	got := out.String()
	assert.Contains(t, got,
		"Pushed 3 sessions, 9 messages, skipped 2 ownership conflict(s) in 1.5s")
	assert.Contains(t, got,
		"Warning: skipped 2 session(s) owned by another PostgreSQL push marker")
}

func TestWritePGPushSummaryVectorPhase(t *testing.T) {
	tests := []struct {
		name        string
		vectors     postgres.VectorPushResult
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "counters",
			vectors: postgres.VectorPushResult{
				SessionsPushed:    2,
				SessionsUnchanged: 5,
				DocsPushed:        7,
				ChunksPushed:      9,
			},
			wantContain: []string{
				"Vectors: 2 session(s) pushed, 5 unchanged, 7 docs, 9 chunks",
			},
			wantAbsent: []string{"skipped", "Warning: skipped"},
		},
		{
			name: "skipped with reason",
			vectors: postgres.VectorPushResult{
				Skipped:       true,
				SkippedReason: "pgvector extension unavailable",
			},
			wantContain: []string{"Vectors: skipped (pgvector extension unavailable)"},
		},
		{
			name:       "skipped without reason prints nothing",
			vectors:    postgres.VectorPushResult{Skipped: true},
			wantAbsent: []string{"Vectors:"},
		},
		{
			name: "conflicts warning",
			vectors: postgres.VectorPushResult{
				SessionsPushed: 1,
				Conflicts:      3,
			},
			wantContain: []string{
				"Vectors: 1 session(s) pushed, 0 unchanged, 0 docs, 0 chunks",
				"Warning: skipped 3 vector session(s) owned by another PostgreSQL push marker",
			},
		},
		{
			name: "deferred warning",
			vectors: postgres.VectorPushResult{
				SessionsPushed:   1,
				SessionsDeferred: 2,
			},
			wantContain: []string{
				"Vectors: 1 session(s) pushed, 0 unchanged, 0 docs, 0 chunks",
				"Warning: deferred vectors for 2 session(s) whose session push failed; the next generation-wide reconciliation sends them",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			writePGPushSummary(&out, postgres.PushResult{
				SessionsPushed: 1,
				MessagesPushed: 1,
				Duration:       time.Second,
				Vectors:        tt.vectors,
			})
			got := out.String()
			for _, want := range tt.wantContain {
				assert.Contains(t, got, want)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

func TestWritePGPushSummaryReportsErrorCount(t *testing.T) {
	tests := []struct {
		name        string
		result      postgres.PushResult
		wantContain []string
		wantAbsent  []string
	}{
		{
			name: "conflicts with errors",
			result: postgres.PushResult{
				SessionsPushed:   3,
				MessagesPushed:   9,
				SkippedConflicts: 2,
				Errors:           4,
				Duration:         1500 * time.Millisecond,
			},
			wantContain: []string{
				"Pushed 3 sessions, 9 messages, skipped 2 ownership conflict(s), 4 error(s) in 1.5s",
				"Warning: skipped 2 session(s) owned by another PostgreSQL push marker",
			},
		},
		{
			name: "conflicts without errors omits error count",
			result: postgres.PushResult{
				SessionsPushed:   3,
				MessagesPushed:   9,
				SkippedConflicts: 2,
				Duration:         1500 * time.Millisecond,
			},
			wantContain: []string{
				"Pushed 3 sessions, 9 messages, skipped 2 ownership conflict(s) in 1.5s",
			},
			wantAbsent: []string{"error(s)"},
		},
		{
			name: "errors without conflicts",
			result: postgres.PushResult{
				SessionsPushed: 5,
				MessagesPushed: 12,
				Errors:         1,
				Duration:       2 * time.Second,
			},
			wantContain: []string{"Pushed 5 sessions, 12 messages, 1 error(s) in 2s"},
			wantAbsent:  []string{"ownership conflict"},
		},
		{
			name: "clean run omits error count",
			result: postgres.PushResult{
				SessionsPushed: 5,
				MessagesPushed: 12,
				Duration:       2 * time.Second,
			},
			wantContain: []string{"Pushed 5 sessions, 12 messages in 2s"},
			wantAbsent:  []string{"error(s)", "ownership conflict"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			writePGPushSummary(&out, tt.result)
			got := out.String()
			for _, want := range tt.wantContain {
				assert.Contains(t, got, want)
			}
			for _, absent := range tt.wantAbsent {
				assert.NotContains(t, got, absent)
			}
		})
	}
}

// TestPGPushProgressPrinterKeepsCompletedStages pins the stage-aware
// renderer: reports within a stage rerender one line in place, and a stage
// transition finishes the line with a newline so completed stages stay in
// the scrollback instead of being overwritten by the next stage.
func TestPGPushProgressPrinterKeepsCompletedStages(t *testing.T) {
	out := captureStdout(t, func() {
		print := newPGPushProgressPrinter()
		print(postgres.PushProgress{Phase: "preparing"})
		print(postgres.PushProgress{Phase: "preparing"})
		print(postgres.PushProgress{
			Phase: "preparing", SessionsDone: 500, SessionsTotal: 1000,
		})
		print(postgres.PushProgress{
			Phase: "preparing", SessionsDone: 1000, SessionsTotal: 1000,
		})
		print(postgres.PushProgress{
			SessionsDone: 1, SessionsTotal: 9, MessagesDone: 5,
		})
		print(postgres.PushProgress{
			Phase: "vectors", VectorSessionsDone: 1,
			VectorSessionsTotal: 2, VectorChunksPushed: 3,
		})
	})

	lines := strings.Split(out, "\n")
	require.Len(t, lines, 4, "one line per stage: %q", out)

	// Within a line, in-place rerenders are separated by carriage returns;
	// the text a terminal leaves visible is the segment after the last one.
	// Every render carries a wall-clock elapsed suffix, so assert on the
	// stable prefix and the suffix shape rather than the exact duration.
	visible := func(line string) string {
		parts := strings.Split(line, "\r")
		return strings.TrimSuffix(parts[len(parts)-1], "\x1b[K")
	}
	wantPrefixes := []string{
		"Preparing push (sync state, metadata, fingerprints)...",
		"Preparing... 1000/1000 sessions fingerprinted",
		"Pushing... 1/9 sessions, 5 messages",
		"Pushing vectors... 1/2 sessions scanned, 3 chunks",
	}
	for i, want := range wantPrefixes {
		got := visible(lines[i])
		assert.True(t, strings.HasPrefix(got, want),
			"line %d: %q must start with %q", i, got, want)
		assert.Regexp(t, ` \([0-9a-z.]+ elapsed\)$`, got,
			"line %d must end with an elapsed suffix", i)
	}
}
