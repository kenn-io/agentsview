package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
)

// serviceSpec is the resolved input for rendering a unit file.
type serviceSpec struct {
	BinPath string
	DataDir string
	LogPath string
}

func rejectEnvDependentServicePGURL(rawURL string) error {
	if os.Getenv("AGENTSVIEW_PG_URL") != "" {
		return fmt.Errorf(
			"AGENTSVIEW_PG_URL is set; pg service install requires a " +
				"literal PostgreSQL URL in config.toml, either " +
				"legacy [pg].url or the default_pg-selected [pg.NAME].url, because background " +
				"services do not inherit your shell environment",
		)
	}
	// Reuse config's expansion check so the rejection rule cannot drift
	// from how config.ResolvePG actually expands the URL at runtime.
	if config.IsEnvDependentURL(rawURL) {
		return fmt.Errorf(
			"pg.url uses environment variable expansion; pg service " +
				"install requires a literal PostgreSQL URL in config.toml, either " +
				"legacy [pg].url or the default_pg-selected [pg.NAME].url, because " +
				"background services do not inherit your shell environment",
		)
	}
	return nil
}

// serviceRuntimeEnvVars are environment variables that influence how the
// background pg push --watch service behaves at runtime but are NOT
// rendered into the service unit (only AGENTSVIEW_DATA_DIR is). When any
// are set at install time, the installed service will not see them and
// falls back to config.toml or built-in defaults, so install warns about
// the divergence. AGENTSVIEW_PG_URL is intentionally excluded: a
// service that cannot resolve a URL is broken rather than merely
// divergent, so it is a hard error (rejectEnvDependentServicePGURL), not
// a warning.
func serviceRuntimeEnvVars() []string {
	vars := []string{"AGENTSVIEW_PG_SCHEMA", "AGENTSVIEW_PG_MACHINE"}
	for _, def := range parser.Registry {
		if def.EnvVar != "" {
			vars = append(vars, def.EnvVar)
		}
		if def.DefaultRootEnvVar != "" {
			vars = append(vars, def.DefaultRootEnvVar)
		}
	}
	return vars
}

// setEnvVarsAffectingService returns, in declaration order, the names of
// serviceRuntimeEnvVars currently set to a non-empty value. lookup is
// injectable for testing; production callers pass os.LookupEnv.
func setEnvVarsAffectingService(lookup func(string) (string, bool)) []string {
	var set []string
	for _, name := range serviceRuntimeEnvVars() {
		if v, ok := lookup(name); ok && v != "" {
			set = append(set, name)
		}
	}
	return set
}

// validateServiceSpec rejects spec values containing characters that
// could break out of a quoted value in a rendered service unit. systemd
// unit files are not shell scripts and provide no escaping for the values
// we interpolate (ExecStart, Environment), so a newline or double quote
// in a path could terminate the directive and inject additional ones.
// launchd plists likewise cannot represent NUL or most control characters
// even when XML escaped. Rejecting these characters up front keeps both
// renderers safe regardless of how each escapes its output.
func validateServiceSpec(spec serviceSpec) error {
	for _, f := range []struct{ name, val string }{
		{"binary path", spec.BinPath},
		{"data dir", spec.DataDir},
		{"log path", spec.LogPath},
	} {
		if i := strings.IndexFunc(f.val, isUnsafeServiceRune); i >= 0 {
			return fmt.Errorf(
				"%s %q contains an unsafe character %q; refusing to write "+
					"a service unit that could be malformed or inject "+
					"directives",
				f.name, f.val, f.val[i],
			)
		}
	}
	return nil
}

// isUnsafeServiceRune reports whether r must not appear in a rendered
// service unit value. Control characters (newline, carriage return, NUL,
// and friends) and the double quote can terminate or escape a quoted
// value in a systemd unit.
func isUnsafeServiceRune(r rune) bool {
	return r == '"' || unicode.IsControl(r)
}

// buildServiceSpec validates PG config and resolves the binary path,
// data dir, and log path for the installed service. It refuses to
// build a spec when the PG URL is not resolvable so the service is
// only ever created in a working state.
func buildServiceSpec(appCfg config.Config) (serviceSpec, error) {
	rawPG, err := appCfg.RawPGTarget("")
	if err != nil {
		return serviceSpec{}, err
	}
	if err := rejectEnvDependentServicePGURL(rawPG.URL); err != nil {
		return serviceSpec{}, err
	}
	pgCfg, err := appCfg.ResolvePG()
	if err != nil {
		return serviceSpec{}, err
	}
	if pgCfg.URL == "" {
		return serviceSpec{}, fmt.Errorf(
			"pg url not configured; configure a legacy [pg].url or the default_pg-selected [pg.NAME].url before installing the service",
		)
	}
	exe, err := os.Executable()
	if err != nil {
		return serviceSpec{}, fmt.Errorf("resolving binary path: %w", err)
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	spec := serviceSpec{
		BinPath: exe,
		DataDir: appCfg.DataDir,
		LogPath: filepath.Join(appCfg.DataDir, "pg-watch.log"),
	}
	if err := validateServiceSpec(spec); err != nil {
		return serviceSpec{}, err
	}
	return spec, nil
}

// cmdRunner runs an external command and returns combined output.
// Injectable so managers can be tested without invoking launchctl /
// systemctl.
type cmdRunner func(ctx context.Context, name string, args ...string) (string, error)

func defaultRunner(
	ctx context.Context, name string, args ...string,
) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// serviceManager installs and controls the pg push --watch OS service.
type serviceManager interface {
	unitPath() string
	render(spec serviceSpec) string
	install(ctx context.Context, spec serviceSpec) error
	uninstall(ctx context.Context) error
	start(ctx context.Context) error
	stop(ctx context.Context) error
	status(ctx context.Context) (string, error)
}

// lingerChecker is implemented by managers (systemd) where the
// service needs an OS setting to run while logged out.
type lingerChecker interface {
	lingerEnabled(ctx context.Context) bool
	enableLingerCmd() string
}

// newServiceManager returns the manager for the current platform.
func newServiceManager() (serviceManager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return &launchdManager{
			uid: os.Getuid(), home: home, run: defaultRunner,
		}, nil
	case "linux":
		u, uerr := user.Current()
		if uerr != nil {
			return nil, fmt.Errorf("resolving current user: %w", uerr)
		}
		return &systemdManager{
			user: u.Username, home: home, run: defaultRunner,
		}, nil
	default:
		return nil, fmt.Errorf(
			"pg service: unsupported platform %q (supported: macOS, Linux)",
			runtime.GOOS,
		)
	}
}
