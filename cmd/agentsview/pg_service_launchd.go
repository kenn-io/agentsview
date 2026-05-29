package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Compile-time check that launchdManager implements serviceManager.
var _ serviceManager = (*launchdManager)(nil)

const launchdLabel = "agentsview.pg-watch"

// launchdManager manages a per-user launchd LaunchAgent.
type launchdManager struct {
	uid  int
	home string
	run  cmdRunner
}

func (m *launchdManager) unitPath() string {
	return filepath.Join(
		m.home, "Library", "LaunchAgents", launchdLabel+".plist",
	)
}

func (m *launchdManager) domain() string {
	return fmt.Sprintf("gui/%d", m.uid)
}

func (m *launchdManager) target() string {
	return fmt.Sprintf("gui/%d/%s", m.uid, launchdLabel)
}

func xmlEscape(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// KeepAlive restarts the daemon if it exits. install validates
// the DSN up front and the daemon does not fatal on a transiently
// unreachable database, so the narrow remaining fatal paths
// (config removed, lock contention) will be retried ~every 10s by
// launchd rather than staying down.
func (m *launchdManager) render(spec serviceSpec) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>pg</string>
		<string>push</string>
		<string>--watch</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>AGENTSVIEW_DATA_DIR</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`,
		xmlEscape(launchdLabel),
		xmlEscape(spec.BinPath),
		xmlEscape(spec.DataDir),
		xmlEscape(spec.LogPath),
		xmlEscape(spec.LogPath),
	)
}

func (m *launchdManager) install(
	ctx context.Context, spec serviceSpec,
) error {
	if err := os.MkdirAll(filepath.Dir(m.unitPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(
		m.unitPath(), []byte(m.render(spec)), 0o644,
	); err != nil {
		return err
	}
	// Best-effort unload of any prior instance so bootstrap succeeds.
	_, _ = m.run(ctx, "launchctl", "bootout", m.target())
	if out, err := m.run(
		ctx, "launchctl", "bootstrap", m.domain(), m.unitPath(),
	); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	return nil
}

func (m *launchdManager) uninstall(ctx context.Context) error {
	_, _ = m.run(ctx, "launchctl", "bootout", m.target())
	if err := os.Remove(m.unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (m *launchdManager) start(ctx context.Context) error {
	// bootout any prior instance so bootstrap reloads cleanly;
	// bootstrap fails if the job is already loaded.
	_, _ = m.run(ctx, "launchctl", "bootout", m.target())
	if out, err := m.run(
		ctx, "launchctl", "bootstrap", m.domain(), m.unitPath(),
	); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	return nil
}

func (m *launchdManager) stop(ctx context.Context) error {
	// bootout unloads the job so KeepAlive cannot restart it.
	// A non-zero exit means the job was not loaded (already
	// stopped), which we treat as success, so stop is idempotent.
	_, _ = m.run(ctx, "launchctl", "bootout", m.target())
	return nil
}

func (m *launchdManager) status(ctx context.Context) (string, error) {
	// Errors are intentionally ignored: launchctl print exits
	// non-zero when the job is not loaded, which is normal status
	// output rather than a failure. The combined output is shown
	// to the user verbatim. (The systemd manager swallows status
	// errors for the same reason.)
	out, _ := m.run(ctx, "launchctl", "print", m.target())
	return out, nil
}
