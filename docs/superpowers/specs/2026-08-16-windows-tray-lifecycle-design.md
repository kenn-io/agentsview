# Windows Tray Lifecycle

## Goal

Give the Windows desktop app the same close-to-tray behavior as macOS. Closing
the main window keeps AgentsView available from the system tray, while the Quit
menu action remains the explicit way to exit. Linux behavior does not change.

## Design

Enable Tauri's `tray-icon` feature for Windows and macOS. Use one shared tray
setup path for both platforms, with the existing template image on macOS and the
packaged application icon on Windows. The tray menu continues to provide Show
AgentsView, Open Logs Folder, Check for Updates, and Quit.

Register the close handler on Windows and macOS. A close request is prevented
and the main window is hidden. Show AgentsView restores, unminimizes, and
focuses that same window. The existing desktop menu routing remains the single
action dispatcher for window restore and explicit exit.

Linux retains normal close behavior and does not gain Tauri's tray dependency.
This avoids adding Linux AppIndicator packaging requirements in this change.

## Error Handling

Close-to-tray setup keeps the existing non-fatal startup behavior: setup
failures are logged and do not abort the desktop wrapper. Window-lifecycle setup
only runs after tray creation succeeds, so a tray setup failure preserves normal
close behavior rather than hiding the window without a restore surface.
Platform-specific icon selection occurs during tray setup and propagates errors
through the same setup result.

## Verification

Extend the existing close-and-restore unit test to compile for Windows and
macOS, and keep the macOS template-icon assertions macOS-only. Run Rust
formatting, the desktop crate check, and its library tests on the available
host. Windows compilation and installer behavior remain covered by Windows CI
because the local host does not have the Windows Rust target installed.
