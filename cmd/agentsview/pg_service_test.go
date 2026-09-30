package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/dbtest"
)

func TestBuildServiceSpec_RequiresURL(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "")
	_, err := buildServiceSpec(config.Config{}, pgServiceKind)
	require.Error(t, err, "expected error when pg.url is not configured")
	assert.Contains(t, err.Error(), "default_pg-selected [pg.NAME].url")
}

func TestBuildServiceSpec_PopulatesFields(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "")
	dataDir := t.TempDir()
	spec, err := buildServiceSpec(config.Config{
		DataDir: dataDir,
		PG: config.PGConfig{
			URL:         "postgres://u:p@localhost/db?sslmode=disable",
			MachineName: "box1",
		},
	}, pgServiceKind)
	require.NoError(t, err, "buildServiceSpec")
	assert.NotEmpty(t, spec.BinPath, "BinPath should be set")
	assert.Equal(t, dataDir, spec.DataDir)
	assert.Equal(t, filepath.Join(dataDir, "pg-watch.log"), spec.LogPath)
}

func TestBuildServiceSpec_UsesNamedDefaultTarget(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "")
	restoreUnsetEnv(t, "BROKEN_WORK_TARGET")
	dataDir := t.TempDir()
	spec, err := buildServiceSpec(config.Config{
		DataDir:   dataDir,
		DefaultPG: "archive",
		PGTargets: map[string]config.PGConfig{
			"work": {
				URL:         "${BROKEN_WORK_TARGET}",
				MachineName: "workbox",
			},
			"archive": {
				URL:         "postgres://u:p@localhost/archive?sslmode=disable",
				MachineName: "archivebox",
			},
		},
	}, pgServiceKind)
	require.NoError(t, err)
	assert.Equal(t, dataDir, spec.DataDir)
	assert.Equal(t, filepath.Join(dataDir, "pg-watch.log"), spec.LogPath)
	assert.NotEmpty(t, spec.BinPath)
}

func TestBuildServiceSpec_RejectsEnvPGURL(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "postgres://from-env")
	_, err := buildServiceSpec(config.Config{
		DataDir: t.TempDir(),
		PG: config.PGConfig{
			URL:         "postgres://from-env",
			MachineName: "box1",
		},
	}, pgServiceKind)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AGENTSVIEW_PG_URL")
	assert.Contains(t, err.Error(), "literal PostgreSQL URL")
	assert.Contains(t, err.Error(), "default_pg-selected [pg.NAME].url")
}

func TestBuildServiceSpec_RejectsExpandedPGURL(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "")
	t.Setenv("PGURL", "postgres://from-var")
	_, err := buildServiceSpec(config.Config{
		DataDir: t.TempDir(),
		PG: config.PGConfig{
			URL:         "${PGURL}",
			MachineName: "box1",
		},
	}, pgServiceKind)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "environment variable expansion")
	assert.Contains(t, err.Error(), "literal PostgreSQL URL")

	_, err = buildServiceSpec(config.Config{
		DataDir: t.TempDir(),
		PG: config.PGConfig{
			URL:         "$PGURL",
			MachineName: "box1",
		},
	}, pgServiceKind)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "environment variable expansion")
	assert.Contains(t, err.Error(), "default_pg-selected [pg.NAME].url")
}

func TestValidateServiceSpec_RejectsUnsafeChars(t *testing.T) {
	const clean = "/usr/local/bin/agentsview"
	cleanSpec := serviceSpec{
		BinPath: clean,
		DataDir: "/home/me/.agentsview",
		LogPath: "/home/me/.agentsview/pg-watch.log",
	}
	require.NoError(t, validateServiceSpec(cleanSpec),
		"clean spec should pass validation")

	// A path containing a space is unusual but legal and must not be
	// rejected (only control characters and double quotes are unsafe).
	spaced := cleanSpec
	spaced.DataDir = "/home/me/App Support/agentsview"
	require.NoError(t, validateServiceSpec(spaced),
		"a space in a path is legal and should pass")

	unsafe := []struct {
		name string
		bad  string
	}{
		{"newline", "/bin/x\nExecStart=/evil"},
		{"carriage return", "/bin/x\rExecStart=/evil"},
		{"double quote", `/bin/x" "/evil`},
		{"nul byte", "/bin/x\x00/evil"},
		{"control byte", "/bin/x\x07/evil"},
	}
	for _, u := range unsafe {
		// ExecStart is rendered from BinPath; Environment from DataDir.
		// Cover both interpolation sites.
		t.Run("execstart_"+u.name, func(t *testing.T) {
			s := cleanSpec
			s.BinPath = u.bad
			err := validateServiceSpec(s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "binary path")
			assert.Contains(t, err.Error(), "unsafe character")
		})
		t.Run("environment_"+u.name, func(t *testing.T) {
			s := cleanSpec
			s.DataDir = u.bad
			err := validateServiceSpec(s)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "data dir")
			assert.Contains(t, err.Error(), "unsafe character")
		})
	}
}

func TestBuildServiceSpec_RejectsUnsafeDataDir(t *testing.T) {
	t.Setenv("AGENTSVIEW_PG_URL", "")
	_, err := buildServiceSpec(config.Config{
		DataDir: "/home/me/.agentsview\nExecStart=/evil",
		PG: config.PGConfig{
			URL:         "postgres://u:p@localhost/db?sslmode=disable",
			MachineName: "box1",
		},
	}, pgServiceKind)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsafe character")
}

func TestSetEnvVarsAffectingService(t *testing.T) {
	// AGENTSVIEW_PG_URL is a hard error elsewhere, not a warning, so it
	// must never appear here even when set.
	env := map[string]string{
		"AGENTSVIEW_PG_SCHEMA":        "staging",
		"CLAUDE_CONFIG_DIR":           "/tmp/claude-root",
		"CLAUDE_PROJECTS_DIR":         "/tmp/claude",
		"PI_CODING_AGENT_DIR":         "/tmp/pi-home",
		"PI_CODING_AGENT_SESSION_DIR": "/tmp/pi-sessions",
		"AGENTSVIEW_PG_URL":           "postgres://from-env",
		"AGENTSVIEW_PG_MACHINE":       "", // set-but-empty should be ignored
	}
	lookup := func(name string) (string, bool) {
		v, ok := env[name]
		return v, ok
	}
	got := setEnvVarsAffectingService(pgServiceKind, lookup)
	assert.Contains(t, got, "AGENTSVIEW_PG_SCHEMA")
	assert.Contains(t, got, "CLAUDE_CONFIG_DIR")
	assert.Contains(t, got, "CLAUDE_PROJECTS_DIR")
	assert.Contains(t, got, "PI_CODING_AGENT_DIR")
	assert.Contains(t, got, "PI_CODING_AGENT_SESSION_DIR")
	assert.NotContains(t, got, "AGENTSVIEW_PG_URL")
	assert.NotContains(t, got, "AGENTSVIEW_PG_MACHINE",
		"set-but-empty env vars should not be reported")

	// Nothing set -> empty result.
	none := setEnvVarsAffectingService(pgServiceKind, func(string) (string, bool) {
		return "", false
	})
	assert.Empty(t, none)
}

func TestWarnUninheritedServiceEnv(t *testing.T) {
	var buf strings.Builder
	warnUninheritedServiceEnv(&buf, nil)
	assert.Empty(t, buf.String(), "no warning when nothing is set")

	buf.Reset()
	warnUninheritedServiceEnv(&buf, []string{
		"AGENTSVIEW_PG_SCHEMA",
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_PROJECTS_DIR",
	})
	out := buf.String()
	assert.Contains(t, out, "WARNING")
	assert.Contains(t, out, "AGENTSVIEW_PG_SCHEMA")
	assert.Contains(t, out, "CLAUDE_CONFIG_DIR")
	assert.Contains(t, out, "CLAUDE_PROJECTS_DIR")
	assert.Contains(t, out, "config.toml")
}

func TestReadServiceLastPush_ClickHouseRejectsInsecureRemote(t *testing.T) {
	local := dbtest.OpenTestDB(t)
	_, err := readServiceLastPush(t.Context(), clickHouseServiceKind, config.Config{
		ClickHouse: config.ClickHouseConfig{
			URL: "clickhouse://user:pw@ch.example.internal:9000/agentsview",
		},
	}, local)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not use TLS")
	assert.NotContains(t, err.Error(), "pw")
}

func TestReadServiceLastPush_UsesDefaultTargetScope(t *testing.T) {
	local := dbtest.OpenTestDB(t)

	require.NoError(t, local.SetSyncState(t.Context(),
		"last_push_at:work",
		"2026-03-11T12:34:56.123Z",
	))

	lastPush, err := readServiceLastPush(t.Context(), pgServiceKind, config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work": {URL: "postgres://work"},
		},
	}, local)
	require.NoError(t, err)
	assert.Equal(t, "2026-03-11T12:34:56.123Z", lastPush)
}

func TestReadServiceLastPush_ReadsLegacyDefaultStateWithoutMigration(t *testing.T) {
	local := dbtest.OpenTestDB(t)

	require.NoError(t, local.SetSyncState(t.Context(),
		"last_push_at",
		"2026-03-11T12:34:56.123Z",
	))

	lastPush, err := readServiceLastPush(t.Context(), pgServiceKind, config.Config{
		DefaultPG: "work",
		PGTargets: map[string]config.PGConfig{
			"work": {URL: "postgres://work"},
		},
	}, local)
	require.NoError(t, err)
	assert.Equal(t, "2026-03-11T12:34:56.123Z", lastPush)

	legacyValue, err := local.GetSyncState(t.Context(), "last_push_at")
	require.NoError(t, err)
	assert.Equal(t, "2026-03-11T12:34:56.123Z", legacyValue)

	scopedValue, err := local.GetSyncState(t.Context(), "last_push_at:work")
	require.NoError(t, err)
	assert.Empty(t, scopedValue)
}

func TestWriteServiceStatus_AppendsScopedLastPush(t *testing.T) {
	var out strings.Builder
	writeServiceStatus(
		&out, "Service is active",
		"2026-03-11T12:34:56.123Z", true,
	)
	assert.Equal(t,
		"Service is active\nLast push: 2026-03-11T12:34:56.123Z\n",
		out.String(),
	)
}

func TestWriteServiceStatus_PrintsNeverWhenNoPushCompleted(t *testing.T) {
	var out strings.Builder
	writeServiceStatus(&out, "Service is active\n", "", true)
	assert.Equal(t, "Service is active\nLast push: never\n", out.String())
}

func TestWriteServiceStatus_SkipsLastPushWhenUnavailable(t *testing.T) {
	var out strings.Builder
	writeServiceStatus(&out, "Service is active\n", "", false)
	assert.Equal(t, "Service is active\n", out.String())
}

// recordingRunner captures shell-out calls for assertions.
type recordingRunner struct {
	calls   [][]string
	outputs map[string]string // keyed by joined args; optional
}

func (r *recordingRunner) run(
	_ context.Context, name string, args ...string,
) (string, error) {
	full := append([]string{name}, args...)
	r.calls = append(r.calls, full)
	if r.outputs != nil {
		if out, ok := r.outputs[strings.Join(full, " ")]; ok {
			return out, nil
		}
	}
	return "", nil
}

func (r *recordingRunner) sawContains(sub string) bool {
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), sub) {
			return true
		}
	}
	return false
}

func TestLaunchdRender_ClickHouseKind(t *testing.T) {
	m := &launchdManager{kind: clickHouseServiceKind, uid: 501, home: "/Users/me"}
	spec := serviceSpec{
		Kind:    clickHouseServiceKind,
		BinPath: "/usr/local/bin/agentsview",
		DataDir: "/Users/me/.agentsview",
		LogPath: "/Users/me/.agentsview/clickhouse-watch.log",
	}
	got := m.render(spec)
	assert.Contains(t, got, `<string>agentsview.clickhouse-watch</string>`)
	assert.Contains(t, got, `<string>clickhouse</string>`)
	assert.Contains(t, got, `<string>push</string>`)
	assert.Contains(t, got, `<string>--watch</string>`)
	assert.Contains(t, got, `<string>/Users/me/.agentsview/clickhouse-watch.log</string>`)
}

func TestSystemdRender_ClickHouseKind(t *testing.T) {
	m := &systemdManager{kind: clickHouseServiceKind, user: "me", home: "/home/me"}
	spec := serviceSpec{
		Kind:    clickHouseServiceKind,
		BinPath: "/usr/local/bin/agentsview",
		DataDir: "/home/me/.agentsview",
		LogPath: "/home/me/.agentsview/clickhouse-watch.log",
	}
	got := m.render(spec)
	assert.Contains(t, got, "Description=agentsview ClickHouse auto-push")
	assert.Contains(t, got, `ExecStart="/usr/local/bin/agentsview" clickhouse push --watch`)
	assert.Contains(t, got, "clickhouse-watch.log")
}

func TestLaunchdRender(t *testing.T) {
	m := &launchdManager{kind: pgServiceKind, uid: 501, home: "/Users/me", run: nil}
	spec := serviceSpec{
		Kind:    pgServiceKind,
		BinPath: "/usr/local/bin/agentsview",
		DataDir: "/Users/me/.agentsview",
		LogPath: "/Users/me/.agentsview/pg-watch.log",
	}
	got := m.render(spec)
	// Substring assertions avoid brittleness over plist indentation.
	wants := []string{
		`<string>agentsview.pg-watch</string>`,
		`<string>/usr/local/bin/agentsview</string>`,
		`<string>pg</string>`,
		`<string>push</string>`,
		`<string>--watch</string>`,
		`<key>AGENTSVIEW_DATA_DIR</key>`,
		`<string>/Users/me/.agentsview</string>`,
		`<key>RunAtLoad</key>`,
		`<key>KeepAlive</key>`,
		`<string>/Users/me/.agentsview/pg-watch.log</string>`,
	}
	for _, w := range wants {
		assert.Contains(t, got, w)
	}
	assert.True(t, strings.HasPrefix(got, `<?xml version="1.0"`))
	assert.NotContains(t, got, "<false/>")
}

func TestLaunchdUnitPath(t *testing.T) {
	m := &launchdManager{kind: pgServiceKind, uid: 501, home: "/Users/me"}
	want := filepath.Join(
		"/Users/me", "Library", "LaunchAgents", "agentsview.pg-watch.plist",
	)
	assert.Equal(t, want, m.unitPath())
}

func TestLaunchdInstall_WritesAndBootstraps(t *testing.T) {
	home := t.TempDir()
	rr := &recordingRunner{}
	m := &launchdManager{kind: pgServiceKind, uid: 501, home: home, run: rr.run}
	spec := serviceSpec{
		BinPath: "/usr/local/bin/agentsview",
		DataDir: home,
		LogPath: filepath.Join(home, "pg-watch.log"),
	}
	require.NoError(t, m.install(t.Context(), spec), "install")
	_, err := os.Stat(m.unitPath())
	require.NoError(t, err, "plist written")
	assert.True(t, rr.sawContains("launchctl bootstrap gui/501 "+m.unitPath()))
}

func TestLaunchdStart_BootstrapsAfterBootout(t *testing.T) {
	rr := &recordingRunner{}
	m := &launchdManager{kind: pgServiceKind, uid: 501, home: t.TempDir(), run: rr.run}
	require.NoError(t, m.start(t.Context()), "start")
	assert.True(t, rr.sawContains("launchctl bootstrap gui/501 "+m.unitPath()))
}

func TestLaunchdStop_BootsOut(t *testing.T) {
	rr := &recordingRunner{}
	m := &launchdManager{kind: pgServiceKind, uid: 501, home: t.TempDir(), run: rr.run}
	require.NoError(t, m.stop(t.Context()), "stop")
	assert.True(t, rr.sawContains("launchctl bootout gui/501/agentsview.pg-watch"))
}

func TestLaunchdUninstall_RemovesPlist(t *testing.T) {
	home := t.TempDir()
	rr := &recordingRunner{}
	m := &launchdManager{kind: pgServiceKind, uid: 501, home: home, run: rr.run}
	spec := serviceSpec{
		BinPath: "/usr/local/bin/agentsview",
		DataDir: home,
		LogPath: filepath.Join(home, "pg-watch.log"),
	}
	require.NoError(t, m.install(t.Context(), spec), "install")
	require.NoError(t, m.uninstall(t.Context()), "uninstall")
	_, err := os.Stat(m.unitPath())
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.True(t, rr.sawContains("launchctl bootout gui/501/agentsview.pg-watch"))
}

func TestSystemdRender_Golden(t *testing.T) {
	m := &systemdManager{kind: pgServiceKind, user: "me", home: "/home/me"}
	spec := serviceSpec{
		BinPath: "/usr/local/bin/agentsview",
		DataDir: "/home/me/.agentsview",
		LogPath: "/home/me/.agentsview/pg-watch.log",
	}
	got := m.render(spec)
	want := `[Unit]
Description=agentsview PostgreSQL auto-push
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart="/usr/local/bin/agentsview" pg push --watch
Environment=AGENTSVIEW_DATA_DIR="/home/me/.agentsview"
StandardOutput=append:/home/me/.agentsview/pg-watch.log
StandardError=append:/home/me/.agentsview/pg-watch.log
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`
	assert.Equal(t, want, got)
}

func TestSystemdUnitPath(t *testing.T) {
	m := &systemdManager{kind: pgServiceKind, user: "me", home: "/home/me"}
	want := filepath.Join(
		"/home/me", ".config", "systemd", "user", "agentsview-pg-watch.service",
	)
	assert.Equal(t, want, m.unitPath())
}

func TestSystemdLingerDetection(t *testing.T) {
	yes := &recordingRunner{outputs: map[string]string{
		"loginctl show-user me --property=Linger": "Linger=yes\n",
	}}
	m := &systemdManager{kind: pgServiceKind, user: "me", home: "/home/me", run: yes.run}
	assert.True(t, m.lingerEnabled(t.Context()))
	no := &recordingRunner{outputs: map[string]string{
		"loginctl show-user me --property=Linger": "Linger=no\n",
	}}
	m2 := &systemdManager{kind: pgServiceKind, user: "me", home: "/home/me", run: no.run}
	assert.False(t, m2.lingerEnabled(t.Context()))
}

func TestSystemdInstall_ReloadsAndEnables(t *testing.T) {
	home := t.TempDir()
	rr := &recordingRunner{}
	m := &systemdManager{kind: pgServiceKind, user: "me", home: home, run: rr.run}
	spec := serviceSpec{
		BinPath: "/usr/local/bin/agentsview",
		DataDir: home,
		LogPath: filepath.Join(home, "pg-watch.log"),
	}
	require.NoError(t, m.install(t.Context(), spec), "install")
	_, err := os.Stat(m.unitPath())
	require.NoError(t, err, "unit written")
	assert.True(t, rr.sawContains("systemctl --user daemon-reload"))
	assert.True(t, rr.sawContains("systemctl --user enable --now agentsview-pg-watch.service"))
}

func TestSystemdStart_CallsStart(t *testing.T) {
	rr := &recordingRunner{}
	m := &systemdManager{kind: pgServiceKind, user: "me", home: t.TempDir(), run: rr.run}
	require.NoError(t, m.start(t.Context()), "start")
	assert.True(t, rr.sawContains("systemctl --user start agentsview-pg-watch.service"))
}

func TestSystemdStop_CallsStop(t *testing.T) {
	rr := &recordingRunner{}
	m := &systemdManager{kind: pgServiceKind, user: "me", home: t.TempDir(), run: rr.run}
	require.NoError(t, m.stop(t.Context()), "stop")
	assert.True(t, rr.sawContains("systemctl --user stop agentsview-pg-watch.service"))
}

func TestSystemdUninstall_DisablesAndRemoves(t *testing.T) {
	home := t.TempDir()
	rr := &recordingRunner{}
	m := &systemdManager{kind: pgServiceKind, user: "me", home: home, run: rr.run}
	spec := serviceSpec{
		BinPath: "/usr/local/bin/agentsview",
		DataDir: home,
		LogPath: filepath.Join(home, "pg-watch.log"),
	}
	require.NoError(t, m.install(t.Context(), spec), "install")
	require.NoError(t, m.uninstall(t.Context()), "uninstall")
	_, err := os.Stat(m.unitPath())
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.True(t, rr.sawContains("systemctl --user disable --now agentsview-pg-watch.service"))
}

func TestPGServiceCommandTree(t *testing.T) {
	cmd := newPGServiceCommand()
	want := map[string]bool{
		"install": false, "uninstall": false, "status": false,
		"start": false, "stop": false, "logs": false,
	}
	for _, c := range cmd.Commands() {
		want[c.Name()] = true
	}
	for name, found := range want {
		assert.True(t, found, "missing subcommand %q", name)
	}
}

func TestTailFile_ReadsContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pg-watch.log")
	require.NoError(t, os.WriteFile(path, []byte("hello\nworld\n"), 0o644))
	var buf strings.Builder
	require.NoError(t, tailFile(&buf, path, false), "tailFile")
	assert.Equal(t, "hello\nworld\n", buf.String())
}

func TestTailFile_MissingFileErrors(t *testing.T) {
	var buf strings.Builder
	err := tailFile(&buf, filepath.Join(t.TempDir(), "nope.log"), false)
	assert.Error(t, err)
}

func TestPromptYesNo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"Y\n", true},
		{"n\n", false},
		{"\n", false},
		{"", false}, // EOF
		{"garbage\n", false},
	}
	for _, c := range cases {
		got := promptYesNo(strings.NewReader(c.in), "Continue?")
		assert.Equal(t, c.want, got, "promptYesNo(%q)", c.in)
	}
}
