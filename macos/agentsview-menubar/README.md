# AgentsView Menu Bar

Small macOS menu-bar controller for the existing `agentsview` CLI. It starts
the local web server with `serve --background --no-browser`, opens the detected
local URL, and leaves the detached server running when the menu-bar app quits.

## Build and run

The menu-bar app finds `agentsview` in Homebrew, `~/.local/bin`, or `PATH` and
starts it on an OS-assigned local port instead of the usual `8080`.
For a development binary, pass `AGENTSVIEW_BIN` when launching the app
executable directly:

```bash
./scripts/build-app.sh
AGENTSVIEW_BIN=/path/to/agentsview \
  ./dist/AgentsViewMenuBar.app/Contents/MacOS/AgentsViewMenuBar
```

From the repository root, use `make menubar-open` to open the built app with
an absolute path. The app is menu-bar-only, so it does not open a window or
appear in the Dock.

Run the package tests with:

```bash
swift test
```

Use **Stop Server** from the menu when the detached AgentsView process should
also exit. Quitting the menu-bar app alone intentionally does not stop it.
