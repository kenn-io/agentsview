# AgentsView Desktop (Tauri)

This directory contains an experimental Tauri desktop wrapper for AgentsView.

The wrapper does not reimplement the web app. Instead, it:

1. Builds the existing Go `agentsview` binary.
1. Packages it as a Tauri sidecar.
1. Starts it with `serve --background --host 127.0.0.1` on a local port.
1. Loads the local URL in a native webview.

On macOS and Windows, the same desktop process also provides a system tray item
for showing the AgentsView window, opening logs, checking for updates, and
quitting.

## Requirements

- Rust toolchain (`rustc`, `cargo`)
- Node.js and npm
- Go (with CGO enabled; same requirements as the main project)

## Usage

```bash
npm ci
npm run tauri:dev
npm run tauri:build
npm run tauri:build:macos-app
npm run tauri:build:windows
```

The `prepare-sidecar` step runs automatically for `tauri:dev` and `tauri:build`.
It builds `agentsview` and copies it to
`src-tauri/binaries/agentsview-<target-triple>`.

## Deep Links

The desktop app registers the `agentsview://` URL scheme. External tools can
open a specific session in the app:

```bash
open "agentsview://sessions/<session-id>"
```

An optional `?msg=<ordinal>` or `?msg=last` query scrolls to that message in the
session; other query parameters are dropped.

If the app is already running, the existing window is focused and navigated to
the session. If it is not running, the app starts, waits for the backend, and
then opens the session. URLs outside `agentsview://sessions/<session-id>` are
ignored.

On Windows and Linux a second launch forwards its URL to the running instance
and exits, so only one app instance runs at a time.

If a link appears to do nothing, deep link handling is recorded in
`agentsview-desktop.log` (File > Open Logs Folder, or the tray menu). On macOS
the scheme is registered through the bundled app's `Info.plist`, so dev builds
(`tauri:dev`) do not receive deep links; use a bundled build.

## Environment Notes (Desktop)

When launched from Finder/Explorer, desktop apps usually do not inherit your
shell profile (`.zshrc`, `.bashrc`), which can hide CLIs like `claude`, `codex`,
and `gemini` from `PATH`.

On macOS/Linux, the Tauri wrapper loads login-shell env (`$SHELL -lic 'env -0'`)
for the sidecar (with a short timeout to avoid startup hangs). On Windows this
probing is skipped by default.

Optional escape hatch:

- Add overrides in `~/.agentsview/desktop.env`:
  - Example: `PATH=/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin`
  - Example: `ANTHROPIC_API_KEY=...`
- Windows WSL bridge example:
  - `CODEX_SESSIONS_DIR=wsl:Ubuntu:/home/me/.codex/sessions` (becomes
    `\\wsl.localhost\Ubuntu\home\me\.codex\sessions` in desktop wrapper env)
- On Windows, this file resolves to `%USERPROFILE%\\.agentsview\\desktop.env`.
- Force a custom PATH with `AGENTSVIEW_DESKTOP_PATH`.
- Skip login-shell env loading with `AGENTSVIEW_DESKTOP_SKIP_LOGIN_SHELL_ENV=1`.
