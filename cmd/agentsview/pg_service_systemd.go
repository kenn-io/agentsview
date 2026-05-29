package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Compile-time checks that systemdManager satisfies both interfaces.
var (
	_ serviceManager = (*systemdManager)(nil)
	_ lingerChecker  = (*systemdManager)(nil)
)

const systemdUnitName = "agentsview-pg-watch.service"

// systemdManager manages a per-user systemd unit (systemctl --user).
type systemdManager struct {
	user string
	home string
	run  cmdRunner
}

func (m *systemdManager) unitPath() string {
	return filepath.Join(
		m.home, ".config", "systemd", "user", systemdUnitName,
	)
}

// StandardOutput/StandardError append to LogPath so the daemon's stdout
// and stderr (notably the startup banner and fatal-exit messages, which
// bypass the log file) land where "pg service logs" tails. This mirrors
// the launchd plist's StandardOutPath/StandardErrorPath; without it those
// messages would go to journald and the tailed file would omit the very
// crash reason a user runs "logs" to find.
func (m *systemdManager) render(spec serviceSpec) string {
	return fmt.Sprintf(`[Unit]
Description=agentsview PostgreSQL auto-push
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart="%s" pg push --watch
Environment=AGENTSVIEW_DATA_DIR="%s"
StandardOutput=append:%s
StandardError=append:%s
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`, spec.BinPath, spec.DataDir, spec.LogPath, spec.LogPath)
}

func (m *systemdManager) lingerEnabled(ctx context.Context) bool {
	out, err := m.run(
		ctx, "loginctl", "show-user", m.user, "--property=Linger",
	)
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "Linger=yes"
}

func (m *systemdManager) enableLingerCmd() string {
	return fmt.Sprintf("loginctl enable-linger %s", m.user)
}

func (m *systemdManager) install(
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
	if out, err := m.run(
		ctx, "systemctl", "--user", "daemon-reload",
	); err != nil {
		return fmt.Errorf("systemctl daemon-reload: %v: %s", err, out)
	}
	if out, err := m.run(
		ctx, "systemctl", "--user", "enable", "--now", systemdUnitName,
	); err != nil {
		return fmt.Errorf("systemctl enable: %v: %s", err, out)
	}
	return nil
}

func (m *systemdManager) uninstall(ctx context.Context) error {
	_, _ = m.run(
		ctx, "systemctl", "--user", "disable", "--now", systemdUnitName,
	)
	if err := os.Remove(m.unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, _ = m.run(ctx, "systemctl", "--user", "daemon-reload")
	return nil
}

func (m *systemdManager) start(ctx context.Context) error {
	if out, err := m.run(
		ctx, "systemctl", "--user", "start", systemdUnitName,
	); err != nil {
		return fmt.Errorf("systemctl start: %v: %s", err, out)
	}
	return nil
}

func (m *systemdManager) stop(ctx context.Context) error {
	if out, err := m.run(
		ctx, "systemctl", "--user", "stop", systemdUnitName,
	); err != nil {
		return fmt.Errorf("systemctl stop: %v: %s", err, out)
	}
	return nil
}

func (m *systemdManager) status(ctx context.Context) (string, error) {
	out, _ := m.run(ctx, "systemctl", "--user", "status", systemdUnitName)
	return out, nil
}
