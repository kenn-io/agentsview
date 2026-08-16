# Windows Tray Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> to implement this plan directly in the current agent, task-by-task. Never use
> subagent-driven development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Windows the existing macOS close-to-tray lifecycle while leaving
Linux behavior unchanged.

**Approved spec/design:**
`docs/superpowers/specs/2026-08-16-windows-tray-lifecycle-design.md`

**Architecture:** Compile Tauri's tray support on macOS and Windows, then share
one status-item setup path and one close-handler path across those targets. Keep
icon selection platform-specific: the monochrome template asset on macOS and
Tauri's packaged default window icon on Windows.

**Tech Stack:** Rust 2021, Tauri 2, Cargo target dependencies, Rust unit tests.

## Global Constraints

- Linux keeps normal close behavior and does not gain the `tray-icon` feature.
- The tray menu remains Show AgentsView, Open Logs Folder, Check for Updates,
  and Quit.
- Closing hides the main window on macOS and Windows; Quit explicitly exits.
- Preserve non-fatal logging for setup errors, but only register close
  interception after tray creation succeeds.
- Do not add dependencies beyond Tauri's existing `tray-icon` feature.

______________________________________________________________________

### Task 1: Share the Tray Lifecycle with Windows

**Files:**

- Modify: `desktop/src-tauri/Cargo.toml`
- Modify: `desktop/src-tauri/src/lib.rs`
- Test: `desktop/src-tauri/src/lib.rs`
- Modify: `desktop/README.md`

**Interfaces:**

- Consumes: Tauri `App::default_window_icon()`, `TrayIconBuilder`, the existing
  `desktop_menu_action`, `show_main_window`, and `MainWindowVisibility` APIs.

- Produces: `setup_status_item(app: &mut App) -> Result<(), DynError>` and
  `setup_window_lifecycle(app: &App) -> Result<(), DynError>`, compiled only
  for macOS and Windows.

- [ ] **Step 1: Extend the lifecycle test target to Windows**

    Change the status-item routing test, `FakeMainWindow`, its
    `MainWindowVisibility` implementation, and the close-and-restore test from
    `#[cfg(target_os = "macos")]` to:

    ```rust
    #[cfg(any(target_os = "macos", target_os = "windows"))]
    ```

    Rename the tests to describe desktop status-item and close behavior rather
    than macOS-only behavior:

    ```rust
    fn status_item_actions_share_desktop_menu_routing()
    fn close_hides_the_existing_window_and_show_restores_it()
    ```

- [ ] **Step 2: Verify the Windows-facing test fails before implementation**

    Run on a Windows Rust host or CI runner:

    ```powershell
    cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml --lib
    ```

    Expected before implementation: compilation fails because the hide method and
    close helper are still macOS-only. On the available macOS host, run the same
    command to ensure the expanded test preserves existing behavior; it should
    pass there.

- [ ] **Step 3: Enable Windows tray support**

    Add the Windows-only target dependency without changing Linux:

    ```toml
    [target.'cfg(target_os = "windows")'.dependencies]
    tauri = { version = "2", features = ["tray-icon"] }
    ```

- [ ] **Step 4: Share tray setup across macOS and Windows**

    Compile the import, setup calls, hide method, hide helper, and lifecycle setup
    on both supported targets:

    ```rust
    #[cfg(any(target_os = "macos", target_os = "windows"))]
    use tauri::tray::TrayIconBuilder;
    ```

    Rename the setup functions and use the same menu on both platforms:

    ```rust
    #[cfg(any(target_os = "macos", target_os = "windows"))]
    fn setup_status_item(app: &mut App) -> Result<(), DynError> {
        let show = MenuItemBuilder::with_id(SHOW_MAIN_WINDOW_MENU_ID, "Show AgentsView")
            .build(app)?;
        let open_logs =
            MenuItemBuilder::with_id(OPEN_LOGS_FOLDER_MENU_ID, "Open Logs Folder").build(app)?;
        let check_updates = MenuItemBuilder::with_id(
            CHECK_UPDATES_MENU_ID,
            "Check for Updates...",
        )
        .build(app)?;
        let quit = MenuItemBuilder::with_id(QUIT_FROM_STATUS_ITEM_MENU_ID, "Quit AgentsView")
            .build(app)?;
        let menu = MenuBuilder::new(app)
            .item(&show)
            .separator()
            .item(&open_logs)
            .item(&check_updates)
            .separator()
            .item(&quit)
            .build()?;

        let builder = TrayIconBuilder::with_id("agentsview")
            .tooltip("AgentsView")
            .menu(&menu);

        #[cfg(target_os = "macos")]
        let builder = builder
            .icon(macos_status_item_icon()?)
            .icon_as_template(true);

        #[cfg(target_os = "windows")]
        let builder = builder.icon(
            app.default_window_icon()
                .cloned()
                .ok_or_else(|| io::Error::other("default window icon is unavailable"))?,
        );

        builder.build(app)?;
        Ok(())
    }
    ```

    Rename `setup_macos_window_lifecycle` to `setup_window_lifecycle`, and keep
    `macos_status_item_icon` macOS-only. Sequence tray and lifecycle setup so a
    tray error skips close interception, preserving normal close behavior. Use a
    platform-neutral startup log message for the combined setup path.

- [ ] **Step 5: Update the desktop documentation**

    Change the introductory platform sentence in `desktop/README.md` to state that
    macOS and Windows provide a system tray item for showing the window, opening
    logs, checking for updates, and quitting.

- [ ] **Step 6: Format and run focused verification**

    Use an isolated Cargo target directory and a temporary ignored sidecar
    placeholder, then run:

    ```bash
    cargo fmt --manifest-path desktop/src-tauri/Cargo.toml -- --check
    cargo check --locked --manifest-path desktop/src-tauri/Cargo.toml
    cargo test --locked --manifest-path desktop/src-tauri/Cargo.toml --lib
    ```

    Expected: formatting and check succeed with no warnings; all desktop library
    tests pass. Remove the temporary placeholder and isolated target directory.
    Windows compilation remains an explicit CI limitation on the macOS host.

- [ ] **Step 7: Review and commit the implementation**

    Review `git diff --check` and the complete diff. Stage only
    `desktop/src-tauri/Cargo.toml`, `desktop/src-tauri/src/lib.rs`, and
    `desktop/README.md`, then commit with the repository's mandatory commit
    workflow and hooks.
