use std::collections::BTreeMap;
use std::error::Error;
use std::ffi::OsString;
use std::fs;
use std::io;
use std::io::{Read, Seek, SeekFrom, Write};
use std::net::{Ipv4Addr, SocketAddrV4, TcpStream};
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::atomic::{AtomicBool, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread;
use std::time::{Duration, Instant};

use tauri::async_runtime::Receiver;
use tauri::menu::{MenuBuilder, MenuItemBuilder, SubmenuBuilder};
use tauri::plugin::Builder as PluginBuilder;
use tauri::{App, AppHandle, Emitter, Manager, RunEvent, Url, WebviewWindow};
use tauri_plugin_dialog::{DialogExt, MessageDialogButtons};
use tauri_plugin_opener::OpenerExt;
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;
use tauri_plugin_updater::UpdaterExt;

const HOST: &str = "127.0.0.1";
const PREFERRED_PORT: u16 = 8080;
const READY_TIMEOUT: Duration = Duration::from_secs(30);
const READY_POLL_INTERVAL: Duration = Duration::from_millis(125);
const LOGIN_SHELL_ENV_TIMEOUT: Duration = Duration::from_secs(3);
const UPDATE_SIDECAR_STOP_TIMEOUT: Duration = Duration::from_secs(10);
// Delay after navigating to the backend before probing whether the
// Linux WebKitGTK web content process is actually alive. Gives the
// process time to spawn so we don't false-positive on slow startup.
#[cfg(target_os = "linux")]
const WEBVIEW_HEALTH_PROBE_DELAY: Duration = Duration::from_secs(5);

type DynError = Box<dyn Error>;
type CommandRx = Receiver<CommandEvent>;

#[derive(Default)]
struct SidecarState {
    child: Mutex<Option<SidecarProcess>>,
    backend_port: Mutex<Option<u16>>,
    active_generation: Mutex<Option<u64>>,
    stopping_generation: Mutex<Option<u64>>,
    restart_after_stop_timeout_generation: Mutex<Option<u64>>,
    active_update_stop_waiters: AtomicUsize,
    terminated_generation: Mutex<u64>,
    termination: Condvar,
    next_generation: AtomicU64,
}

struct SidecarProcess {
    child: CommandChild,
    generation: u64,
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    // WebKitGTK 2.40+ DMABUF renderer aborts on some Linux EGL
    // setups (NVIDIA, headless, certain Wayland sessions); fall
    // back to the legacy compositing path unless the user opted
    // out by setting the variable explicitly.
    #[cfg(target_os = "linux")]
    if std::env::var_os("WEBKIT_DISABLE_DMABUF_RENDERER").is_none() {
        std::env::set_var("WEBKIT_DISABLE_DMABUF_RENDERER", "1");
    }

    let mut updater_builder = tauri_plugin_updater::Builder::new();
    // Override the placeholder pubkey from tauri.conf.json with
    // the real key when baked in at compile time via env var.
    if let Some(pubkey) = option_env!("AGENTSVIEW_UPDATER_PUBKEY") {
        if !pubkey.is_empty() {
            updater_builder = updater_builder.pubkey(pubkey.to_string());
        }
    }

    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_opener::init())
        .plugin(updater_builder.build())
        .plugin(tauri_plugin_dialog::init())
        .plugin(init_navigation_guard_plugin())
        .manage(SidecarState::default())
        .setup(|app| {
            launch_backend(app)?;
            setup_menu(app)?;
            schedule_auto_update_check(app.handle().clone());
            Ok(())
        })
        .build(tauri::generate_context!())
        .expect("failed to build tauri app")
        .run(|app_handle, event| {
            if let RunEvent::MenuEvent(event) = &event {
                if event.id().0 == "about" {
                    if let Some(window) = app_handle.get_webview_window("main") {
                        let _ = window.eval("window.dispatchEvent(new CustomEvent('show-about'));");
                    }
                }
                if event.id().0 == "check_updates" {
                    let handle = app_handle.clone();
                    tauri::async_runtime::spawn(async move {
                        check_for_updates(&handle, false).await;
                    });
                }
            }
            if matches!(event, RunEvent::ExitRequested { .. } | RunEvent::Exit) {
                stop_backend(app_handle);
            }
        });
}

fn launch_backend(app: &mut App) -> Result<(), DynError> {
    let window = main_window(app)?;
    let handle = app.handle().clone();
    let (rx, child) = spawn_sidecar(&handle)?;

    let generation = save_sidecar(&handle, child)?;

    let focus_window = window.clone();
    let focus_handle = app.handle().clone();
    window.on_window_event(move |event| {
        if let tauri::WindowEvent::Focused(true) = event {
            let port = focus_handle
                .state::<SidecarState>()
                .backend_port
                .lock()
                .ok()
                .and_then(|g| *g);
            if let Some(port) = port {
                recover_webview(&focus_window, port);
            }
        }
    });

    forward_sidecar_logs(rx, window, generation);

    Ok(())
}

fn launch_backend_from_handle(handle: &AppHandle) -> Result<(), DynError> {
    let window = main_window_from_handle(handle)?;
    let (rx, child) = spawn_sidecar(handle)?;
    let generation = save_sidecar(handle, child)?;
    forward_sidecar_logs(rx, window, generation);
    Ok(())
}

fn spawn_sidecar(app: &AppHandle) -> Result<(CommandRx, CommandChild), DynError> {
    let port_arg = PREFERRED_PORT.to_string();
    let mut command = app.shell().sidecar("agentsview")?;
    for (key, value) in sidecar_env() {
        command = command.env(key, value);
    }

    Ok(command.args(sidecar_args(port_arg.as_str())).spawn()?)
}

fn sidecar_args(port: &str) -> Vec<String> {
    vec![
        "serve".to_string(),
        "--host".to_string(),
        HOST.to_string(),
        "--port".to_string(),
        port.to_string(),
    ]
}

fn init_navigation_guard_plugin<R: tauri::Runtime>() -> tauri::plugin::TauriPlugin<R> {
    PluginBuilder::new("navigation-guard")
        .on_navigation(|webview, url| {
            let backend_port = webview
                .app_handle()
                .try_state::<SidecarState>()
                .and_then(|state| state.backend_port.lock().ok().and_then(|g| *g));
            if is_allowed_navigation_url(url, backend_port) {
                return true;
            }
            if is_allowed_external_open_url(url) {
                if let Err(err) = webview
                    .app_handle()
                    .opener()
                    .open_url(url.as_str(), Option::<&str>::None)
                {
                    eprintln!("[agentsview] failed to open external URL in system browser: {err}");
                }
            } else {
                eprintln!(
                    "[agentsview] blocked disallowed external URL scheme: {}",
                    url.as_str()
                );
            }
            false
        })
        .build()
}

fn is_allowed_navigation_url(url: &Url, backend_port: Option<u16>) -> bool {
    // macOS/Linux: tauri://localhost
    if url.scheme() == "tauri" && url.host_str() == Some("localhost") {
        return true;
    }
    // Windows (WebView2): http://tauri.localhost or https://tauri.localhost.
    // WebView2 uses http by default for the custom localhost origin.
    // Reject explicit ports to prevent spoofing via other local services.
    if matches!(url.scheme(), "http" | "https")
        && url.host_str() == Some("tauri.localhost")
        && url.port().is_none()
    {
        return true;
    }
    // Only allow navigation to the known sidecar port on
    // localhost. Rejects all localhost URLs when the sidecar
    // port is not yet known.
    if let Some(port) = backend_port {
        return url.scheme() == "http" && url.host_str() == Some(HOST) && url.port() == Some(port);
    }
    false
}

fn is_allowed_external_open_url(url: &Url) -> bool {
    matches!(url.scheme(), "http" | "https" | "mailto")
}

// sidecar_env returns the environment passed to the backend
// sidecar process. It merges the app environment with
// login-shell variables so desktop launches inherit zshrc/bash
// exports. An optional ~/.agentsview/desktop.env file can
// override specific keys as an escape hatch.
fn sidecar_env() -> Vec<(OsString, OsString)> {
    let skip_login_shell = std::env::var_os("AGENTSVIEW_DESKTOP_SKIP_LOGIN_SHELL_ENV");
    let should_probe =
        should_probe_login_shell(skip_login_shell.as_ref(), cfg!(target_os = "windows"));

    build_sidecar_env(
        std::env::vars_os().collect(),
        if should_probe {
            read_login_shell_env().unwrap_or_default()
        } else {
            Vec::new()
        },
        read_desktop_env_file(),
        std::env::var_os("AGENTSVIEW_DESKTOP_PATH"),
        cfg!(target_os = "windows"),
    )
}

// read_login_shell_env invokes the user's login shell and
// parses NUL-delimited env output (`env -0`).
fn read_login_shell_env() -> Option<Vec<(OsString, OsString)>> {
    let default_shell = default_login_shell();
    let shell = std::env::var("SHELL")
        .ok()
        .filter(|s| !s.trim().is_empty())
        .unwrap_or(default_shell);

    let stdout = run_login_shell_env(shell.as_str(), LOGIN_SHELL_ENV_TIMEOUT)?;
    Some(parse_nul_env(stdout.as_slice()))
}

fn default_login_shell() -> String {
    if cfg!(target_os = "macos") {
        return "/bin/zsh".to_string();
    }
    if Path::new("/bin/bash").exists() {
        return "/bin/bash".to_string();
    }
    "/bin/sh".to_string()
}

// read_desktop_env_file parses ~/.agentsview/desktop.env as
// KEY=VALUE lines. This provides a manual override path before
// desktop settings UI exists.
fn read_desktop_env_file() -> Vec<(OsString, OsString)> {
    let Some(home) = resolve_home_dir() else {
        return Vec::new();
    };
    let path = home.join(".agentsview").join("desktop.env");
    let Ok(content) = fs::read_to_string(path) else {
        return Vec::new();
    };

    parse_desktop_env_content(content.as_str())
}

fn resolve_home_dir() -> Option<PathBuf> {
    resolve_home_dir_from_lookup(|key| std::env::var_os(key), cfg!(target_os = "windows"))
}

fn should_probe_login_shell(skip: Option<&OsString>, is_windows: bool) -> bool {
    !is_windows && skip.is_none()
}

fn build_sidecar_env(
    inherited: Vec<(OsString, OsString)>,
    login_shell: Vec<(OsString, OsString)>,
    desktop_file: Vec<(OsString, OsString)>,
    forced_path: Option<OsString>,
    case_insensitive_keys: bool,
) -> Vec<(OsString, OsString)> {
    let mut merged = BTreeMap::new();
    merge_env_pairs(&mut merged, inherited, case_insensitive_keys);
    merge_env_pairs(&mut merged, login_shell, case_insensitive_keys);
    merge_env_pairs(&mut merged, desktop_file, case_insensitive_keys);

    if let Some(path) = forced_path {
        merged.insert(
            normalize_env_key(std::ffi::OsStr::new("PATH"), case_insensitive_keys),
            path,
        );
    }

    merged.into_iter().collect()
}

fn merge_env_pairs(
    dest: &mut BTreeMap<OsString, OsString>,
    pairs: Vec<(OsString, OsString)>,
    case_insensitive_keys: bool,
) {
    for (k, v) in pairs {
        dest.insert(normalize_env_key(k.as_os_str(), case_insensitive_keys), v);
    }
}

fn normalize_env_key(key: &std::ffi::OsStr, case_insensitive_keys: bool) -> OsString {
    if case_insensitive_keys {
        return OsString::from(key.to_string_lossy().to_ascii_uppercase());
    }
    key.to_os_string()
}

/// LoginShellEnvError captures every way try_run_login_shell_env
/// can fail so tests can print an actionable reason when the
/// probe returns nothing. Production callers flatten this into
/// `Option` via `.ok()` since they already fall back to parent
/// env on any failure.
#[derive(Debug)]
enum LoginShellEnvError {
    TempFile(io::Error),
    Spawn(io::Error),
    Wait(io::Error),
    Timeout {
        elapsed: Duration,
    },
    NonZero {
        code: Option<i32>,
        stdout_len: usize,
        stderr: Vec<u8>,
    },
    ReadStdout(io::Error),
}

impl std::fmt::Display for LoginShellEnvError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::TempFile(e) => write!(f, "tempfile create/clone failed: {e}"),
            Self::Spawn(e) => write!(f, "spawn failed: {e}"),
            Self::Wait(e) => write!(f, "try_wait failed: {e}"),
            Self::Timeout { elapsed } => write!(f, "timed out after {elapsed:?}"),
            Self::NonZero {
                code,
                stdout_len,
                stderr,
            } => {
                let stderr_str = String::from_utf8_lossy(stderr);
                write!(
                    f,
                    "child exited non-zero code={code:?} stdout_len={stdout_len} \
                     stderr={stderr_str:?}"
                )
            }
            Self::ReadStdout(e) => write!(f, "reading stdout tempfile failed: {e}"),
        }
    }
}

/// try_run_login_shell_env spawns `shell -<login-flag> "env -0"` and
/// returns the captured stdout, or a structured error explaining why
/// it couldn't. stdout is captured to a tempfile (not a pipe) so a
/// child that emits more than a pipe buffer's worth of bytes never
/// deadlocks. stderr is captured the same way so test failures can
/// surface the shell's error output.
fn try_run_login_shell_env(shell: &str, timeout: Duration) -> Result<Vec<u8>, LoginShellEnvError> {
    let shell_arg = shell_login_env_flag(shell);
    let mut stdout_capture = tempfile::tempfile().map_err(LoginShellEnvError::TempFile)?;
    let stdout_writer = stdout_capture
        .try_clone()
        .map_err(LoginShellEnvError::TempFile)?;
    let mut stderr_capture = tempfile::tempfile().map_err(LoginShellEnvError::TempFile)?;
    let stderr_writer = stderr_capture
        .try_clone()
        .map_err(LoginShellEnvError::TempFile)?;
    let mut child = std::process::Command::new(shell)
        .args([shell_arg, "env -0"])
        .stdin(Stdio::null())
        .stderr(Stdio::from(stderr_writer))
        .stdout(Stdio::from(stdout_writer))
        .spawn()
        .map_err(LoginShellEnvError::Spawn)?;

    let started = Instant::now();
    let deadline = started + timeout;
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) => {
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(LoginShellEnvError::Timeout {
                        elapsed: started.elapsed(),
                    });
                }
                thread::sleep(Duration::from_millis(25));
            }
            Err(err) => {
                let _ = child.kill();
                let _ = child.wait();
                return Err(LoginShellEnvError::Wait(err));
            }
        }
    };

    let mut output = Vec::new();
    if let Err(e) = stdout_capture.seek(SeekFrom::Start(0)) {
        return Err(LoginShellEnvError::ReadStdout(e));
    }
    if let Err(e) = stdout_capture.read_to_end(&mut output) {
        return Err(LoginShellEnvError::ReadStdout(e));
    }

    if !status.success() {
        let mut stderr_bytes = Vec::new();
        let _ = stderr_capture.seek(SeekFrom::Start(0));
        let _ = stderr_capture.read_to_end(&mut stderr_bytes);
        return Err(LoginShellEnvError::NonZero {
            code: status.code(),
            stdout_len: output.len(),
            stderr: stderr_bytes,
        });
    }

    Ok(output)
}

/// run_login_shell_env is the Option-returning facade used by
/// production code, which treats any probe failure as "no login
/// shell env available" and falls back to the parent environment.
/// Tests that need a failure reason should call
/// try_run_login_shell_env directly.
fn run_login_shell_env(shell: &str, timeout: Duration) -> Option<Vec<u8>> {
    match try_run_login_shell_env(shell, timeout) {
        Ok(bytes) => Some(bytes),
        Err(err) => {
            eprintln!("[agentsview] login shell env probe failed: {err}");
            None
        }
    }
}

fn shell_login_env_flag(shell: &str) -> &'static str {
    let name = Path::new(shell)
        .file_name()
        .and_then(|n| n.to_str())
        .unwrap_or_default();
    match name {
        "sh" | "dash" | "busybox" => "-c",
        "fish" => "-lc",
        _ => "-lic",
    }
}

fn parse_nul_env(content: &[u8]) -> Vec<(OsString, OsString)> {
    let mut vars = Vec::new();
    for entry in content.split(|b| *b == 0) {
        if entry.is_empty() {
            continue;
        }
        let Some(eq) = entry.iter().position(|b| *b == b'=') else {
            continue;
        };
        if eq == 0 {
            continue;
        }
        vars.push((
            os_string_from_bytes(&entry[..eq]),
            os_string_from_bytes(&entry[eq + 1..]),
        ));
    }
    vars
}

#[cfg(unix)]
fn os_string_from_bytes(bytes: &[u8]) -> OsString {
    use std::os::unix::ffi::OsStringExt;
    OsString::from_vec(bytes.to_vec())
}

#[cfg(not(unix))]
fn os_string_from_bytes(bytes: &[u8]) -> OsString {
    OsString::from(String::from_utf8_lossy(bytes).into_owned())
}

fn parse_desktop_env_content(content: &str) -> Vec<(OsString, OsString)> {
    let mut vars = Vec::new();
    for line in content.lines() {
        let line = line.trim();
        if line.is_empty() || line.starts_with('#') {
            continue;
        }
        let Some((k, v)) = line.split_once('=') else {
            continue;
        };
        let key = k.trim();
        if key.is_empty() {
            continue;
        }
        vars.push((OsString::from(key), OsString::from(v.trim())));
    }
    vars
}

fn resolve_home_dir_from_lookup<F>(mut lookup: F, prefer_userprofile: bool) -> Option<PathBuf>
where
    F: FnMut(&str) -> Option<OsString>,
{
    let get = |key: &str, lookup: &mut F| lookup(key).filter(|v| !v.is_empty());

    if prefer_userprofile {
        if let Some(profile) = get("USERPROFILE", &mut lookup) {
            return Some(PathBuf::from(profile));
        }
        if let Some(home) = get("HOME", &mut lookup) {
            return Some(PathBuf::from(home));
        }
    } else {
        if let Some(home) = get("HOME", &mut lookup) {
            return Some(PathBuf::from(home));
        }
        if let Some(profile) = get("USERPROFILE", &mut lookup) {
            return Some(PathBuf::from(profile));
        }
    }

    let drive = get("HOMEDRIVE", &mut lookup)?;
    let path = get("HOMEPATH", &mut lookup)?;
    let mut combined = drive;
    combined.push(path);
    Some(PathBuf::from(combined))
}

fn save_sidecar(app: &AppHandle, child: CommandChild) -> Result<u64, DynError> {
    let state = app.state::<SidecarState>();
    let generation = state.next_generation.fetch_add(1, Ordering::SeqCst) + 1;
    let mut guard = state
        .child
        .lock()
        .map_err(|_| io::Error::other("sidecar state lock poisoned"))?;
    *guard = Some(SidecarProcess { child, generation });
    if let Ok(mut active_generation) = state.active_generation.lock() {
        *active_generation = Some(generation);
    }
    if let Ok(mut stopping_generation) = state.stopping_generation.lock() {
        *stopping_generation = None;
    }
    if let Ok(mut restart_generation) = state.restart_after_stop_timeout_generation.lock() {
        *restart_generation = None;
    }
    Ok(generation)
}

fn save_sidecar_port(app: &AppHandle, port: u16) {
    let state = app.state::<SidecarState>();
    set_sidecar_port(&state, Some(port));
}

fn clear_sidecar_port(app: &AppHandle) {
    let state = app.state::<SidecarState>();
    set_sidecar_port(&state, None);
}

fn set_sidecar_port(state: &SidecarState, port: Option<u16>) {
    if let Ok(mut guard) = state.backend_port.lock() {
        *guard = port;
    }
}

fn handle_sidecar_terminated(
    state: &SidecarState,
    startup_handled: &AtomicBool,
    generation: u64,
) -> bool {
    if mark_sidecar_inactive_if_current(state, generation) {
        set_sidecar_port(state, None);
    }
    clear_sidecar_child_if_current(state, generation);
    clear_stopping_generation_if_current(state, generation);
    record_sidecar_terminated(state, generation);
    !startup_handled.swap(true, Ordering::SeqCst)
}

fn mark_sidecar_inactive_if_current(state: &SidecarState, generation: u64) -> bool {
    let Ok(mut guard) = state.active_generation.lock() else {
        return false;
    };
    if *guard == Some(generation) {
        *guard = None;
        return true;
    }
    false
}

fn clear_sidecar_child_if_current(state: &SidecarState, generation: u64) {
    let Ok(mut guard) = state.child.lock() else {
        return;
    };
    if guard
        .as_ref()
        .map(|process| process.generation)
        .is_some_and(|active_generation| active_generation == generation)
    {
        *guard = None;
    }
}

fn mark_sidecar_stopping(state: &SidecarState, generation: u64) {
    if let Ok(mut guard) = state.stopping_generation.lock() {
        *guard = Some(generation);
    }
}

fn current_stopping_generation(state: &SidecarState) -> Option<u64> {
    state
        .stopping_generation
        .lock()
        .ok()
        .and_then(|guard| *guard)
}

fn clear_stopping_generation_if_current(state: &SidecarState, generation: u64) {
    let Ok(mut guard) = state.stopping_generation.lock() else {
        return;
    };
    if *guard == Some(generation) {
        *guard = None;
    }
}

fn mark_restart_after_stop_timeout(state: &SidecarState, generation: u64) {
    if let Ok(mut guard) = state.restart_after_stop_timeout_generation.lock() {
        *guard = Some(generation);
    }
}

fn clear_restart_after_stop_timeout_if_current(state: &SidecarState, generation: u64) {
    if let Ok(mut guard) = state.restart_after_stop_timeout_generation.lock() {
        if *guard == Some(generation) {
            *guard = None;
        }
    }
}

fn take_restart_after_stop_timeout_if_current(state: &SidecarState, generation: u64) -> bool {
    let Ok(mut guard) = state.restart_after_stop_timeout_generation.lock() else {
        return false;
    };
    if *guard == Some(generation) {
        *guard = None;
        return true;
    }
    false
}

fn begin_update_stop_wait(state: &SidecarState) {
    state
        .active_update_stop_waiters
        .fetch_add(1, Ordering::SeqCst);
}

fn end_update_stop_wait(state: &SidecarState) {
    let previous = state
        .active_update_stop_waiters
        .fetch_sub(1, Ordering::SeqCst);
    debug_assert!(previous > 0);
}

fn has_active_update_stop_waiter(state: &SidecarState) -> bool {
    state.active_update_stop_waiters.load(Ordering::SeqCst) > 0
}

fn take_restart_after_stop_timeout_for_terminated_sidecar(
    state: &SidecarState,
    generation: u64,
) -> bool {
    if has_active_update_stop_waiter(state) {
        return false;
    }
    take_restart_after_stop_timeout_if_current(state, generation)
}

fn restart_backend_after_stop_timeout_if_terminated(
    app: &AppHandle,
    state: &SidecarState,
    generation: u64,
) {
    if !sidecar_generation_terminated(state, generation) {
        return;
    }
    if take_restart_after_stop_timeout_for_terminated_sidecar(state, generation) {
        restart_backend_after_update(app.clone());
    }
}

fn record_sidecar_terminated(state: &SidecarState, generation: u64) {
    if let Ok(mut guard) = state.terminated_generation.lock() {
        if *guard < generation {
            *guard = generation;
        }
        state.termination.notify_all();
    }
}

fn sidecar_generation_terminated(state: &SidecarState, generation: u64) -> bool {
    state
        .terminated_generation
        .lock()
        .is_ok_and(|guard| *guard >= generation)
}

fn wait_for_sidecar_termination(state: &SidecarState, generation: u64, timeout: Duration) -> bool {
    let Ok(mut guard) = state.terminated_generation.lock() else {
        return false;
    };
    let deadline = Instant::now() + timeout;
    loop {
        if *guard >= generation {
            return true;
        }

        let now = Instant::now();
        if now >= deadline {
            return false;
        }
        let remaining = deadline.saturating_duration_since(now);
        match state.termination.wait_timeout(guard, remaining) {
            Ok((next_guard, result)) => {
                guard = next_guard;
                if result.timed_out() && *guard < generation {
                    return false;
                }
            }
            Err(_) => return false,
        }
    }
}

fn forward_sidecar_logs(mut rx: CommandRx, window: WebviewWindow, generation: u64) {
    let startup_handled = Arc::new(AtomicBool::new(false));
    let first_output = Arc::new(AtomicBool::new(false));
    let timeout_window = window.clone();
    let timeout_state = startup_handled.clone();
    thread::spawn(move || {
        thread::sleep(READY_TIMEOUT);
        if !timeout_state.load(Ordering::SeqCst) {
            let _ = timeout_window
                .eval("window.__setStatus('AgentsView backend did not become ready in time.');");
        }
    });

    tauri::async_runtime::spawn(async move {
        let mut stdout_buffer = String::new();
        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(chunk_bytes) => {
                    let chunk = String::from_utf8_lossy(&chunk_bytes);
                    eprintln!("[agentsview] {}", chunk.trim_end());
                    if !startup_handled.load(Ordering::SeqCst) {
                        if !first_output.swap(true, Ordering::SeqCst) {
                            let _ = window.eval(
                                "window.__setStage(1); \
                                 window.__setStatus('Starting database and syncing sessions...');",
                            );
                        }
                        if let Some(status) = extract_startup_status(chunk.as_ref()) {
                            let escaped = status.replace('\\', "\\\\").replace('\'', "\\'");
                            let _ =
                                window.eval(format!("window.__setStatus('{escaped}');").as_str());
                        }
                        if let Some(port) = parse_listening_port_from_stdout_buffer(
                            &mut stdout_buffer,
                            chunk.as_ref(),
                        ) {
                            save_sidecar_port(window.app_handle(), port);
                            startup_handled.store(true, Ordering::SeqCst);
                            let _ = window.eval(
                                "window.__setStage(2); \
                                 window.__setStatus('Connecting to interface...');",
                            );
                            redirect_when_ready(window.clone(), port);
                        }
                    }
                }
                CommandEvent::Stderr(line_bytes) => {
                    let line = String::from_utf8_lossy(&line_bytes);
                    eprintln!("[agentsview:stderr] {}", line.trim_end());
                }
                CommandEvent::Terminated(payload) => {
                    eprintln!(
                        "[agentsview] sidecar terminated (code: {:?}, signal: {:?})",
                        payload.code, payload.signal
                    );
                    let handle = window.app_handle().clone();
                    let state = handle.state::<SidecarState>();
                    if handle_sidecar_terminated(&state, startup_handled.as_ref(), generation) {
                        let _ = window.eval(
                            "window.__setStatus(\
                             'AgentsView backend exited before startup completed.');",
                        );
                    }
                    let restart_after_stop_timeout =
                        take_restart_after_stop_timeout_for_terminated_sidecar(&state, generation);
                    if restart_after_stop_timeout {
                        restart_backend_after_update(handle);
                    }
                    break;
                }
                CommandEvent::Error(err) => {
                    eprintln!("[agentsview:error] {err}");
                }
                _ => {}
            }
        }
    });
}

fn main_window(app: &App) -> Result<WebviewWindow, DynError> {
    app.get_webview_window("main")
        .ok_or_else(|| io::Error::other("missing main window").into())
}

fn main_window_from_handle(handle: &AppHandle) -> Result<WebviewWindow, DynError> {
    handle
        .get_webview_window("main")
        .ok_or_else(|| io::Error::other("missing main window").into())
}

fn desktop_redirect_url(port: u16) -> String {
    format!("http://{HOST}:{port}?desktop=1")
}

/// Recover a dead or stale WebView on window focus.
///
/// Layer 1: try eval — if WKWebView content process was killed by
/// macOS (sleep/wake, memory pressure), eval returns Err and we
/// navigate to the backend URL which spawns a fresh content process.
///
/// Layer 2: if eval succeeds (content process alive), the injected
/// JS pings the backend and reloads on failure — covers
/// alive-but-disconnected WebViews.
fn recover_webview(window: &WebviewWindow, port: u16) {
    // Probe the sidecar at its absolute URL (not relative) so we
    // always hit the correct port even if the WebView is still on
    // a stale origin from a previous sidecar instance. No auth
    // header — the local sidecar doesn't require it, and sending
    // one to a random service on the old port would leak the token.
    //
    // Uses AbortController+setTimeout instead of AbortSignal.timeout
    // for compatibility with older WebKit (macOS 12 / Safari 15).
    let probe = format!("http://{HOST}:{port}/api/v1/version");
    let target = desktop_redirect_url(port);
    let health_js = format!(
        "(function(){{\
        var c=new AbortController();\
        setTimeout(function(){{c.abort()}},3000);\
        fetch('{probe}',{{signal:c.signal}})\
        .then(function(r){{if(r.status>=500)throw r}})\
        .catch(function(){{location.href='{target}'}})\
        }})()"
    );
    match window.eval(health_js) {
        Ok(()) => {}
        Err(err) => {
            eprintln!("[agentsview] WebView eval failed, recovering: {err}");
            let url = desktop_redirect_url(port);
            if let Ok(parsed) = Url::parse(url.as_str()) {
                let _ = window.navigate(parsed);
            }
        }
    }
}

fn redirect_when_ready(window: WebviewWindow, port: u16) {
    let target_url = desktop_redirect_url(port);

    thread::spawn(move || {
        if wait_for_server(port, READY_TIMEOUT) {
            match Url::parse(target_url.as_str()) {
                Ok(url) => {
                    if let Err(err) = window.navigate(url) {
                        eprintln!("[agentsview] navigate failed: {err}");
                    }
                    // On Linux a failed WebKitGTK GPU/EGL init aborts the
                    // web content process, leaving a blank window while the
                    // backend keeps serving. Detect that and fall back to
                    // the system browser. See
                    // https://github.com/kenn-io/agentsview/issues/635
                    #[cfg(target_os = "linux")]
                    spawn_webview_health_fallback(window.clone(), port);
                }
                Err(err) => {
                    eprintln!("[agentsview] invalid redirect URL: {err}");
                }
            }
            return;
        }

        let _ = window.eval(
            "document.getElementById('status').textContent = \
             'AgentsView backend did not start within 30 seconds.';",
        );
    });
}

/// Fall back to the system browser when the Linux WebView fails to render.
///
/// On some Linux GPU/driver/compositor combinations (e.g. Wayland + AMD,
/// headless, certain NVIDIA setups) WebKitGTK cannot initialize EGL and
/// aborts the web content process with
/// `Could not create default EGL display: EGL_BAD_PARAMETER. Aborting...`.
/// The Tauri UI process survives, so the user is left staring at a blank
/// window even though the backend is up and the web UI is fully usable.
///
/// We reuse the same signal `recover_webview` relies on: when the content
/// process is gone, `eval` returns `Err`. This check is purely additive —
/// if the WebView is healthy (`eval` succeeds) it does nothing, so it can
/// never disrupt the working path. When the WebView is dead we open the
/// backend URL in the system browser, hide the blank window, and tell the
/// user where the UI went. If no browser can be opened the window stays
/// visible and the dialog shows the URL to open manually.
#[cfg(target_os = "linux")]
fn spawn_webview_health_fallback(window: WebviewWindow, port: u16) {
    // One-shot guard so focus/navigation retries can't open many tabs.
    static FALLBACK_TRIGGERED: AtomicBool = AtomicBool::new(false);

    thread::spawn(move || {
        thread::sleep(WEBVIEW_HEALTH_PROBE_DELAY);

        // A trivial eval round-trips through the web content process; an
        // error means it is gone (e.g. the EGL_BAD_PARAMETER abort above).
        if window.eval("void 0").is_ok() {
            return;
        }
        if FALLBACK_TRIGGERED.swap(true, Ordering::SeqCst) {
            return;
        }

        let url = format!("http://{HOST}:{port}");
        eprintln!(
            "[agentsview] WebView content process is not responding \
             (likely a GPU/EGL initialization failure); opening {url} \
             in the system browser instead"
        );

        let handle = window.app_handle().clone();
        match handle.opener().open_url(url.as_str(), Option::<&str>::None) {
            Ok(()) => {
                let _ = window.hide();
                handle
                    .dialog()
                    .message(format!(
                        "AgentsView could not render its window, likely due to a \
                         graphics driver (EGL) issue. It has been opened in your \
                         web browser instead:\n\n{url}"
                    ))
                    .title("AgentsView")
                    .show(|_| {});
            }
            Err(err) => {
                eprintln!("[agentsview] failed to open system browser fallback: {err}");
                // Keep the window up so the app stays visible and quittable.
                handle
                    .dialog()
                    .message(format!(
                        "AgentsView could not render its window, likely due to a \
                         graphics driver (EGL) issue, and no web browser could be \
                         opened automatically. Open this URL in a browser to use \
                         AgentsView:\n\n{url}"
                    ))
                    .title("AgentsView")
                    .show(|_| {});
            }
        }
    });
}

/// Extracts the latest human-readable status text from a stdout
/// chunk during startup. The Go server uses `\r` for in-place
/// progress updates and `\n` for line breaks.
fn extract_startup_status(chunk: &str) -> Option<String> {
    // Split on \r or \n, take the last non-empty segment.
    let segment = chunk
        .rsplit(['\r', '\n'])
        .map(|s| s.trim())
        .find(|s| !s.is_empty())?;
    // Only forward lines that look like sync output, not
    // arbitrary log noise.
    if segment.contains("sessions") || segment.contains("ync") || segment.contains("atching") {
        return Some(segment.to_string());
    }
    None
}

fn parse_listening_port(line: &str) -> Option<u16> {
    let marker = format!("listening at http://{HOST}:");
    let idx = line.find(marker.as_str())?;
    let after = &line[(idx + marker.len())..];
    let digits: String = after.chars().take_while(|ch| ch.is_ascii_digit()).collect();
    if digits.is_empty() {
        return None;
    }
    digits.parse::<u16>().ok()
}

fn parse_listening_port_from_stdout_buffer(buffer: &mut String, chunk: &str) -> Option<u16> {
    buffer.push_str(chunk);

    let mut consumed = 0;
    while let Some(rel_idx) = buffer[consumed..].find('\n') {
        let end = consumed + rel_idx;
        let line = buffer[consumed..end].trim_end_matches('\r');
        if let Some(port) = parse_listening_port(line) {
            return Some(port);
        }
        consumed = end + 1;
    }

    if consumed > 0 {
        buffer.drain(..consumed);
    }

    None
}

fn setup_menu(app: &mut App) -> Result<(), DynError> {
    let about = MenuItemBuilder::with_id("about", "About AgentsView").build(app)?;
    let check_updates =
        MenuItemBuilder::with_id("check_updates", "Check for Updates...").build(app)?;

    let builder = SubmenuBuilder::new(app, "File")
        .item(&about)
        .separator()
        .item(&check_updates)
        .separator();

    #[cfg(target_os = "macos")]
    let builder = builder.hide().hide_others().separator();

    let app_submenu = builder.quit().build()?;

    let edit_submenu = SubmenuBuilder::new(app, "Edit")
        .undo()
        .redo()
        .separator()
        .cut()
        .copy()
        .paste()
        .select_all()
        .build()?;

    let menu = MenuBuilder::new(app)
        .item(&app_submenu)
        .item(&edit_submenu)
        .build()?;
    app.set_menu(menu)?;
    Ok(())
}

/// Restore input focus to the main webview after a native GTK dialog
/// is dismissed. On Linux/WebKitGTK, native dialogs can leave the
/// webview in a frozen state where it renders but does not process
/// input events.
fn restore_webview_focus(handle: &AppHandle) {
    let handle = handle.clone();
    // Delay focus restoration so the native GTK dialog has time to
    // fully close and release window focus. Without this, set_focus()
    // fires while the dialog still owns focus and the webview stays
    // unresponsive.
    std::thread::spawn(move || {
        std::thread::sleep(Duration::from_millis(100));
        if let Some(window) = handle.get_webview_window("main") {
            let _ = window.set_focus();
        }
    });
}

static UPDATE_CHECK_ACTIVE: AtomicBool = AtomicBool::new(false);

// Guard that clears UPDATE_CHECK_ACTIVE on drop, ensuring the
// flag is reset regardless of which return path is taken.
struct UpdateGuard;

impl Drop for UpdateGuard {
    fn drop(&mut self) {
        UPDATE_CHECK_ACTIVE.store(false, Ordering::SeqCst);
    }
}

fn schedule_auto_update_check(handle: AppHandle) {
    let disabled = std::env::var("AGENTSVIEW_DESKTOP_AUTOUPDATE")
        .map(|v| v == "0")
        .unwrap_or(false);
    if disabled {
        return;
    }

    tauri::async_runtime::spawn(async move {
        tokio::time::sleep(Duration::from_secs(5)).await;
        check_for_updates(&handle, true).await;
    });
}

async fn check_for_updates(handle: &AppHandle, silent: bool) {
    if UPDATE_CHECK_ACTIVE
        .compare_exchange(false, true, Ordering::SeqCst, Ordering::SeqCst)
        .is_err()
    {
        if !silent {
            let h = handle.clone();
            handle
                .dialog()
                .message("An update check is already in progress.")
                .title("Update Check")
                .show(move |_| restore_webview_focus(&h));
        }
        return;
    }
    let _guard = UpdateGuard;

    let updater = match handle.updater() {
        Ok(updater) => updater,
        Err(err) => {
            eprintln!("[agentsview] updater unavailable: {err}");
            if !silent {
                let h = handle.clone();
                handle
                    .dialog()
                    .message("Could not check for updates. The updater is not configured.")
                    .title("Update Check")
                    .show(move |_| restore_webview_focus(&h));
            }
            return;
        }
    };

    let update = match updater.check().await {
        Ok(update) => update,
        Err(err) => {
            eprintln!("[agentsview] update check failed: {err}");
            if !silent {
                let h = handle.clone();
                handle
                    .dialog()
                    .message("Could not check for updates. Please try again later.")
                    .title("Update Check")
                    .show(move |_| restore_webview_focus(&h));
            }
            return;
        }
    };

    let Some(update) = update else {
        if !silent {
            let h = handle.clone();
            handle
                .dialog()
                .message("You're running the latest version.")
                .title("No Updates Available")
                .show(move |_| restore_webview_focus(&h));
        }
        return;
    };

    let version = update.version.clone();
    let confirmed = dialog_confirm(
        handle,
        "Update Available",
        &format!(
            "Version {version} is available. \
             Would you like to download and install it?"
        ),
    )
    .await;

    if !confirmed {
        return;
    }

    let update_bytes = match update.download(|_, _| {}, || {}).await {
        Ok(bytes) => bytes,
        Err(err) => {
            eprintln!("[agentsview] update download failed: {err}");
            let h = handle.clone();
            handle
                .dialog()
                .message(
                    "Failed to download the update. \
                     Please try downloading manually from the releases page.",
                )
                .title("Update Failed")
                .show(move |_| restore_webview_focus(&h));
            return;
        }
    };

    let windows_backend_stopped = if cfg!(target_os = "windows") {
        // Windows locks the bundled sidecar executable while it is running.
        Some(stop_backend_and_wait(handle.clone(), UPDATE_SIDECAR_STOP_TIMEOUT).await)
    } else {
        None
    };
    let backend_stopped_for_update = windows_backend_stopped == Some(true);

    if let Err(err) = install_downloaded_update(
        update_bytes,
        windows_backend_stopped,
        || restart_backend_after_update(handle.clone()),
        |bytes| update.install(bytes),
    ) {
        eprintln!("[agentsview] update install failed: {err}");
        let h = handle.clone();
        handle
            .dialog()
            .message(
                "Failed to install the update. \
                 Please try downloading manually from the releases page.",
            )
            .title("Update Failed")
            .show(move |_| restore_webview_focus(&h));
        return;
    }

    let restart = dialog_confirm(
        handle,
        "Update Complete",
        "Update installed. Restart now to apply?",
    )
    .await;

    let emit_handle = handle.clone();
    let restart_handle = handle.clone();
    let backend_handle = handle.clone();
    finish_successful_update(
        backend_stopped_for_update,
        restart,
        || {
            let _ = emit_handle.emit("restart", ());
        },
        || restart_handle.restart(),
        || restart_backend_after_update(backend_handle),
    );
}

#[derive(Debug, PartialEq, Eq)]
enum InstallDownloadedUpdateError<E> {
    BackendStopTimedOut,
    Install(E),
}

impl<E: std::fmt::Display> std::fmt::Display for InstallDownloadedUpdateError<E> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::BackendStopTimedOut => write!(f, "backend did not stop before update install"),
            Self::Install(err) => write!(f, "{err}"),
        }
    }
}

impl<E: std::fmt::Debug + std::fmt::Display> Error for InstallDownloadedUpdateError<E> {}

fn install_downloaded_update<R, I, E>(
    update_bytes: Vec<u8>,
    windows_backend_stopped: Option<bool>,
    restart_backend: R,
    install: I,
) -> Result<(), InstallDownloadedUpdateError<E>>
where
    R: FnOnce(),
    I: FnOnce(Vec<u8>) -> Result<(), E>,
{
    if windows_backend_stopped == Some(false) {
        return Err(InstallDownloadedUpdateError::BackendStopTimedOut);
    }
    match install(update_bytes) {
        Ok(()) => Ok(()),
        Err(err) => {
            if windows_backend_stopped.is_some() {
                restart_backend();
            }
            Err(InstallDownloadedUpdateError::Install(err))
        }
    }
}

fn finish_successful_update<E, A, B>(
    backend_stopped_for_update: bool,
    restart_confirmed: bool,
    emit_restart: E,
    restart_app: A,
    restart_backend: B,
) where
    E: FnOnce(),
    A: FnOnce(),
    B: FnOnce(),
{
    if restart_confirmed {
        emit_restart();
        restart_app();
    } else if backend_stopped_for_update {
        restart_backend();
    }
}

async fn dialog_confirm(handle: &AppHandle, title: &str, message: &str) -> bool {
    let (tx, rx) = tokio::sync::oneshot::channel();
    let h = handle.clone();
    handle
        .dialog()
        .message(message)
        .title(title)
        .buttons(MessageDialogButtons::OkCancel)
        .show(move |confirmed| {
            restore_webview_focus(&h);
            let _ = tx.send(confirmed);
        });
    rx.await.unwrap_or(false)
}

fn stop_backend(app: &AppHandle) {
    let _ = stop_backend_inner(app, None);
}

async fn stop_backend_and_wait(app: AppHandle, timeout: Duration) -> bool {
    tauri::async_runtime::spawn_blocking(move || stop_backend_inner(&app, Some(timeout)))
        .await
        .unwrap_or(false)
}

fn stop_backend_inner(app: &AppHandle, wait_timeout: Option<Duration>) -> bool {
    let state = app.state::<SidecarState>();
    if let Some(timeout) = wait_timeout {
        begin_update_stop_wait(&state);
        let mut waited_generation = None;
        let active_generation = {
            let Ok(guard) = state.child.lock() else {
                end_update_stop_wait(&state);
                return false;
            };
            guard.as_ref().map(|process| {
                let generation = process.generation;
                mark_sidecar_stopping(&state, generation);
                if let Err(err) = request_sidecar_stop(process) {
                    eprintln!("[agentsview] failed to stop sidecar: {err}");
                }
                generation
            })
        };
        let stopped = if let Some(generation) = active_generation {
            waited_generation = Some(generation);
            finish_backend_stop_wait(
                app,
                &state,
                generation,
                wait_for_sidecar_termination(&state, generation, timeout),
            )
        } else if let Some(generation) = current_stopping_generation(&state) {
            waited_generation = Some(generation);
            finish_backend_stop_wait(
                app,
                &state,
                generation,
                wait_for_sidecar_termination(&state, generation, timeout),
            )
        } else {
            clear_sidecar_port(app);
            true
        };
        end_update_stop_wait(&state);
        if let Some(generation) = waited_generation {
            restart_backend_after_stop_timeout_if_terminated(app, &state, generation);
        }
        return stopped;
    }

    let process = {
        let Ok(mut guard) = state.child.lock() else {
            return false;
        };
        guard.take()
    };
    if let Some(process) = process.as_ref() {
        let _ = mark_sidecar_inactive_if_current(&state, process.generation);
        clear_restart_after_stop_timeout_if_current(&state, process.generation);
        clear_stopping_generation_if_current(&state, process.generation);
    }

    if let Some(process) = process {
        if let Err(err) = process.child.kill() {
            eprintln!("[agentsview] failed to stop sidecar: {err}");
        }
        clear_sidecar_port(app);
        return true;
    }
    clear_sidecar_port(app);
    true
}

fn finish_backend_stop_wait(
    app: &AppHandle,
    state: &SidecarState,
    generation: u64,
    terminated: bool,
) -> bool {
    if terminated {
        clear_restart_after_stop_timeout_if_current(state, generation);
        let _ = mark_sidecar_inactive_if_current(state, generation);
        clear_sidecar_child_if_current(state, generation);
        clear_stopping_generation_if_current(state, generation);
        clear_sidecar_port(app);
    } else {
        mark_restart_after_stop_timeout(state, generation);
        eprintln!("[agentsview] timed out waiting for sidecar to stop before update install");
    }
    terminated
}

fn request_sidecar_stop(process: &SidecarProcess) -> io::Result<()> {
    request_process_stop(process.child.pid())
}

#[cfg(windows)]
fn request_process_stop(pid: u32) -> io::Result<()> {
    let status = std::process::Command::new("taskkill")
        .args(["/PID", &pid.to_string(), "/T", "/F"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    if status.success() {
        Ok(())
    } else {
        Err(io::Error::other(format!(
            "taskkill failed for pid {pid} with status {status}"
        )))
    }
}

#[cfg(unix)]
fn request_process_stop(pid: u32) -> io::Result<()> {
    let status = std::process::Command::new("kill")
        .args(["-TERM", &pid.to_string()])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    if status.success() {
        Ok(())
    } else {
        Err(io::Error::other(format!(
            "kill failed for pid {pid} with status {status}"
        )))
    }
}

#[cfg(not(any(unix, windows)))]
fn request_process_stop(pid: u32) -> io::Result<()> {
    Err(io::Error::other(format!(
        "stopping pid {pid} is unsupported on this platform"
    )))
}

fn restart_backend_after_update(handle: AppHandle) {
    if let Err(err) = launch_backend_from_handle(&handle) {
        eprintln!("[agentsview] failed to restart backend after update: {err}");
    }
}

fn wait_for_server(port: u16, timeout: Duration) -> bool {
    let deadline = Instant::now() + timeout;
    while Instant::now() < deadline {
        if backend_endpoint_ready(port) {
            return true;
        }
        thread::sleep(READY_POLL_INTERVAL);
    }
    false
}

fn backend_endpoint_ready(port: u16) -> bool {
    let request =
        format!("GET /api/v1/version HTTP/1.1\r\nHost: {HOST}:{port}\r\nConnection: close\r\n\r\n");
    let response = match read_http_response(port, request.as_str()) {
        Some(resp) => resp,
        None => return false,
    };
    version_response_looks_valid(response.as_slice())
}

fn read_http_response(port: u16, request: &str) -> Option<Vec<u8>> {
    let addr = SocketAddrV4::new(Ipv4Addr::LOCALHOST, port);
    let mut stream = match TcpStream::connect_timeout(&addr.into(), Duration::from_millis(250)) {
        Ok(stream) => stream,
        Err(_) => return None,
    };

    let _ = stream.set_read_timeout(Some(Duration::from_millis(250)));
    let _ = stream.set_write_timeout(Some(Duration::from_millis(250)));

    if stream.write_all(request.as_bytes()).is_err() {
        return None;
    }

    let mut buf = Vec::with_capacity(4096);
    if stream.read_to_end(&mut buf).is_err() {
        return None;
    }
    if buf.is_empty() {
        return None;
    }
    Some(buf)
}

fn version_response_looks_valid(response: &[u8]) -> bool {
    if !(response.starts_with(b"HTTP/1.1 200") || response.starts_with(b"HTTP/1.0 200")) {
        return false;
    }
    let body = if let Some(idx) = response.windows(4).position(|w| w == b"\r\n\r\n") {
        &response[(idx + 4)..]
    } else if let Some(idx) = response.windows(2).position(|w| w == b"\n\n") {
        &response[(idx + 2)..]
    } else {
        return false;
    };
    let body = String::from_utf8_lossy(body);
    body.contains("\"version\"") && body.contains("\"commit\"") && body.contains("\"build_date\"")
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    #[cfg(unix)]
    use std::os::unix::ffi::OsStrExt;
    #[cfg(unix)]
    use std::os::unix::fs::PermissionsExt;
    #[cfg(unix)]
    use std::time::{SystemTime, UNIX_EPOCH};

    #[test]
    fn sidecar_args_use_cobra_long_flags() {
        assert_eq!(
            sidecar_args("18080"),
            vec![
                "serve".to_string(),
                "--host".to_string(),
                HOST.to_string(),
                "--port".to_string(),
                "18080".to_string(),
            ]
        );
    }

    #[test]
    fn parse_listening_port_extracts_backend_port() {
        let line = "agentsview dev listening at http://127.0.0.1:18080 (started in 1.2s)";
        assert_eq!(parse_listening_port(line), Some(18080));
        assert_eq!(parse_listening_port("unrelated line"), None);
    }

    #[test]
    fn parse_listening_port_ignores_non_listening_urls() {
        let line = "probe successful for http://127.0.0.1:19090/health";
        assert_eq!(parse_listening_port(line), None);
    }

    #[test]
    fn parse_listening_port_from_stdout_buffer_handles_chunked_output() {
        let mut buf = String::new();
        assert_eq!(
            parse_listening_port_from_stdout_buffer(
                &mut buf,
                "agentsview dev listening at http://127.0.0.1:18"
            ),
            None
        );
        assert_eq!(
            parse_listening_port_from_stdout_buffer(&mut buf, "080 (started in 1.2s)\n"),
            Some(18080)
        );
    }

    #[test]
    fn extract_startup_status_parses_progress_and_messages() {
        // Carriage-return progress line
        let chunk = "\r  25/100 sessions (25%) · 1250 messages";
        assert_eq!(
            extract_startup_status(chunk),
            Some("25/100 sessions (25%) · 1250 messages".to_string())
        );

        // Multiple \r-delimited updates: takes the last one
        let chunk = "\r  5/100 sessions (5%) · 25 messages\r  10/100 sessions (10%) · 50 messages";
        assert_eq!(
            extract_startup_status(chunk),
            Some("10/100 sessions (10%) · 50 messages".to_string())
        );

        // Newline-delimited sync messages
        assert_eq!(
            extract_startup_status("Running initial sync...\n"),
            Some("Running initial sync...".to_string())
        );
        assert_eq!(
            extract_startup_status("Sync complete: 42 sessions synced in 125ms\n"),
            Some("Sync complete: 42 sessions synced in 125ms".to_string())
        );
        assert_eq!(
            extract_startup_status("Watching 50 directories for changes (12ms)\n"),
            Some("Watching 50 directories for changes (12ms)".to_string())
        );

        // Unrelated output is ignored
        assert_eq!(extract_startup_status("some random log line\n"), None);
        assert_eq!(extract_startup_status(""), None);
    }

    #[test]
    fn is_allowed_navigation_url_allows_local_only() {
        // macOS/Linux: tauri://localhost
        let tauri_url = Url::parse("tauri://localhost/index.html").expect("valid tauri url");
        assert!(is_allowed_navigation_url(&tauri_url, None));
        assert!(is_allowed_navigation_url(&tauri_url, Some(18080)));

        // Windows (WebView2): http://tauri.localhost (default origin)
        let win_http =
            Url::parse("http://tauri.localhost/index.html").expect("valid windows tauri url");
        assert!(is_allowed_navigation_url(&win_http, None));
        assert!(is_allowed_navigation_url(&win_http, Some(18080)));

        // Windows: https://tauri.localhost also allowed
        let win_https =
            Url::parse("https://tauri.localhost/index.html").expect("valid windows https url");
        assert!(is_allowed_navigation_url(&win_https, None));

        // Reject tauri.localhost with an explicit port
        let win_port =
            Url::parse("https://tauri.localhost:9999/").expect("valid tauri localhost with port");
        assert!(!is_allowed_navigation_url(&win_port, None));

        let local_backend = Url::parse("http://127.0.0.1:18080/").expect("valid localhost url");
        assert!(is_allowed_navigation_url(&local_backend, Some(18080)));

        // Reject when port is unknown
        assert!(!is_allowed_navigation_url(&local_backend, None));

        // Reject when port doesn't match
        assert!(!is_allowed_navigation_url(&local_backend, Some(9999)));

        let remote = Url::parse("https://example.com/").expect("valid remote url");
        assert!(!is_allowed_navigation_url(&remote, Some(18080)));

        let localhost_name =
            Url::parse("http://localhost:18080/").expect("valid localhost-name url");
        assert!(!is_allowed_navigation_url(&localhost_name, Some(18080)));
    }

    #[test]
    fn is_allowed_external_open_url_limits_schemes() {
        let https = Url::parse("https://example.com").expect("valid https url");
        assert!(is_allowed_external_open_url(&https));

        let http = Url::parse("http://example.com").expect("valid http url");
        assert!(is_allowed_external_open_url(&http));

        let mailto = Url::parse("mailto:test@example.com").expect("valid mailto url");
        assert!(is_allowed_external_open_url(&mailto));

        let file = Url::parse("file:///tmp/foo").expect("valid file url");
        assert!(!is_allowed_external_open_url(&file));

        let custom = Url::parse("custom-scheme://foo").expect("valid custom url");
        assert!(!is_allowed_external_open_url(&custom));
    }

    #[test]
    fn set_sidecar_port_updates_and_clears_state() {
        let state = SidecarState::default();
        set_sidecar_port(&state, Some(18080));
        let port = state
            .backend_port
            .lock()
            .expect("lock backend_port after set")
            .to_owned();
        assert_eq!(port, Some(18080));

        set_sidecar_port(&state, None);
        let cleared = state
            .backend_port
            .lock()
            .expect("lock backend_port after clear")
            .to_owned();
        assert_eq!(cleared, None);
    }

    #[test]
    fn handle_sidecar_terminated_clears_port_and_marks_startup() {
        let state = SidecarState::default();
        set_sidecar_port(&state, Some(18080));
        *state
            .active_generation
            .lock()
            .expect("lock active_generation") = Some(1);
        *state
            .stopping_generation
            .lock()
            .expect("lock stopping_generation") = Some(1);
        let startup_handled = AtomicBool::new(false);

        assert!(handle_sidecar_terminated(&state, &startup_handled, 1));
        assert_eq!(
            state
                .backend_port
                .lock()
                .expect("lock backend_port after terminated")
                .to_owned(),
            None
        );
        assert!(startup_handled.load(Ordering::SeqCst));
        assert_eq!(
            state
                .stopping_generation
                .lock()
                .expect("lock stopping_generation after terminated")
                .to_owned(),
            None
        );

        // Termination handling is idempotent for state and should only
        // report first-time transition once.
        assert!(!handle_sidecar_terminated(&state, &startup_handled, 1));
    }

    #[test]
    fn restart_after_stop_timeout_is_consumed_for_matching_generation() {
        let state = SidecarState::default();

        mark_restart_after_stop_timeout(&state, 2);

        assert!(!take_restart_after_stop_timeout_if_current(&state, 1));
        assert!(take_restart_after_stop_timeout_if_current(&state, 2));
        assert!(!take_restart_after_stop_timeout_if_current(&state, 2));
    }

    #[test]
    fn restart_after_stop_timeout_waits_for_active_update_stop_waiter() {
        let state = SidecarState::default();

        mark_restart_after_stop_timeout(&state, 2);
        begin_update_stop_wait(&state);

        assert!(!take_restart_after_stop_timeout_for_terminated_sidecar(
            &state, 2
        ));

        end_update_stop_wait(&state);

        assert!(take_restart_after_stop_timeout_for_terminated_sidecar(
            &state, 2
        ));
    }

    #[test]
    fn install_downloaded_update_installs_after_windows_backend_stop() {
        let events = Mutex::new(Vec::new());

        let result = install_downloaded_update(
            b"update-bytes".to_vec(),
            Some(true),
            || events.lock().expect("lock events").push("restart"),
            |bytes| {
                assert_eq!(bytes, b"update-bytes");
                events.lock().expect("lock events").push("install");
                Ok::<(), ()>(())
            },
        );

        assert_eq!(result, Ok(()));
        assert_eq!(events.lock().expect("lock events").as_slice(), ["install"]);
    }

    #[test]
    fn install_downloaded_update_installs_without_backend_stop_on_non_windows() {
        let events = Mutex::new(Vec::new());

        let result = install_downloaded_update(
            b"update-bytes".to_vec(),
            None,
            || events.lock().expect("lock events").push("restart"),
            |bytes| {
                assert_eq!(bytes, b"update-bytes");
                events.lock().expect("lock events").push("install");
                Ok::<(), ()>(())
            },
        );

        assert_eq!(result, Ok(()));
        assert_eq!(events.lock().expect("lock events").as_slice(), ["install"]);
    }

    #[test]
    fn install_downloaded_update_does_not_restart_after_windows_stop_timeout() {
        let events = Mutex::new(Vec::new());

        let result = install_downloaded_update(
            b"update-bytes".to_vec(),
            Some(false),
            || events.lock().expect("lock events").push("restart"),
            |_| {
                events.lock().expect("lock events").push("install");
                Ok::<(), ()>(())
            },
        );

        assert_eq!(
            result,
            Err(InstallDownloadedUpdateError::BackendStopTimedOut)
        );
        assert!(events.lock().expect("lock events").is_empty());
    }

    #[test]
    fn install_downloaded_update_restarts_backend_after_windows_install_failure() {
        let events = Mutex::new(Vec::new());

        let result = install_downloaded_update(
            b"update-bytes".to_vec(),
            Some(true),
            || events.lock().expect("lock events").push("restart"),
            |bytes| {
                assert_eq!(bytes, b"update-bytes");
                events.lock().expect("lock events").push("install");
                Err::<(), &str>("install failed")
            },
        );

        assert_eq!(
            result,
            Err(InstallDownloadedUpdateError::Install("install failed"))
        );
        assert_eq!(
            events.lock().expect("lock events").as_slice(),
            ["install", "restart"]
        );
    }

    #[test]
    fn finish_successful_update_restarts_backend_when_windows_restart_is_declined() {
        let events = Mutex::new(Vec::new());

        finish_successful_update(
            true,
            false,
            || events.lock().expect("lock events").push("emit-restart"),
            || events.lock().expect("lock events").push("restart-app"),
            || events.lock().expect("lock events").push("restart-backend"),
        );

        assert_eq!(
            events.lock().expect("lock events").as_slice(),
            ["restart-backend"]
        );
    }

    #[test]
    fn shell_login_env_flag_matches_shell_compatibility() {
        assert_eq!(shell_login_env_flag("/bin/sh"), "-c");
        assert_eq!(shell_login_env_flag("/usr/bin/dash"), "-c");
        assert_eq!(shell_login_env_flag("/opt/homebrew/bin/fish"), "-lc");
        assert_eq!(shell_login_env_flag("/bin/bash"), "-lic");
        assert_eq!(shell_login_env_flag("/bin/zsh"), "-lic");
    }

    #[test]
    fn version_response_requires_identity_fields() {
        let valid = b"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{\"version\":\"1.0.0\",\"commit\":\"abc\",\"build_date\":\"2026-01-01T00:00:00Z\"}";
        assert!(version_response_looks_valid(valid));

        let missing = b"HTTP/1.1 200 OK\r\n\r\n{\"version\":\"1.0.0\"}";
        assert!(!version_response_looks_valid(missing));

        let wrong_status = b"HTTP/1.1 404 Not Found\r\n\r\n{}";
        assert!(!version_response_looks_valid(wrong_status));
    }

    #[test]
    fn should_probe_login_shell_skips_windows_or_explicit_skip() {
        assert!(should_probe_login_shell(None, false));
        assert!(!should_probe_login_shell(Some(&OsString::from("1")), false));
        assert!(!should_probe_login_shell(None, true));
    }

    #[test]
    fn build_sidecar_env_applies_precedence_and_path_override() {
        let merged = build_sidecar_env(
            vec![
                (OsString::from("PATH"), OsString::from("/bin")),
                (OsString::from("HOME"), OsString::from("/base")),
            ],
            vec![(OsString::from("HOME"), OsString::from("/login"))],
            vec![(OsString::from("HOME"), OsString::from("/desktop"))],
            Some(OsString::from("/custom/path")),
            false,
        );
        let map: HashMap<_, _> = merged.into_iter().collect();
        assert_eq!(
            map.get(&OsString::from("HOME")),
            Some(&OsString::from("/desktop"))
        );
        assert_eq!(
            map.get(&OsString::from("PATH")),
            Some(&OsString::from("/custom/path"))
        );
    }

    #[test]
    fn build_sidecar_env_supports_case_insensitive_windows_keys() {
        let merged = build_sidecar_env(
            vec![(OsString::from("Path"), OsString::from("A"))],
            vec![(OsString::from("PATH"), OsString::from("B"))],
            vec![],
            Some(OsString::from("C")),
            true,
        );
        let map: HashMap<_, _> = merged.into_iter().collect();
        assert_eq!(map.len(), 1);
        assert_eq!(map.get(&OsString::from("PATH")), Some(&OsString::from("C")));
    }

    #[test]
    fn parse_desktop_env_content_ignores_comments_and_invalid_lines() {
        let parsed = parse_desktop_env_content(
            r#"
            # comment
            PATH=/custom/bin
            BADLINE
            =missingkey
            FOO = bar
            "#,
        );
        let map: HashMap<_, _> = parsed.into_iter().collect();
        assert_eq!(
            map.get(&OsString::from("PATH")),
            Some(&OsString::from("/custom/bin"))
        );
        assert_eq!(
            map.get(&OsString::from("FOO")),
            Some(&OsString::from("bar"))
        );
        assert!(!map.contains_key(&OsString::from("BADLINE")));
    }

    #[test]
    fn resolve_home_dir_from_lookup_honors_platform_precedence() {
        let mut lookup = HashMap::new();
        lookup.insert("HOME".to_string(), OsString::from("/home/a"));
        lookup.insert("USERPROFILE".to_string(), OsString::from("C:\\Users\\a"));
        let resolved_unix = resolve_home_dir_from_lookup(|k| lookup.get(k).cloned(), false);
        assert_eq!(resolved_unix, Some(PathBuf::from("/home/a")));

        let resolved_windows = resolve_home_dir_from_lookup(|k| lookup.get(k).cloned(), true);
        assert_eq!(resolved_windows, Some(PathBuf::from("C:\\Users\\a")));
    }

    #[test]
    fn parse_nul_env_tolerates_invalid_utf8_entries() {
        let raw = b"PATH=/bin\0BROKEN=\xFF\xFE\0EMPTY=\0\0";
        let parsed = parse_nul_env(raw);
        let map: HashMap<_, _> = parsed.into_iter().collect();
        assert!(map.contains_key(&OsString::from("PATH")));

        #[cfg(unix)]
        {
            let broken = map
                .get(&OsString::from("BROKEN"))
                .expect("BROKEN key present");
            assert_eq!(broken.as_os_str().as_bytes(), b"\xFF\xFE");
        }
    }

    #[cfg(unix)]
    #[test]
    fn run_login_shell_env_handles_large_stdout() {
        let stamp = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("valid clock")
            .as_nanos();
        let script_path = std::env::temp_dir().join(format!(
            "agentsview-login-shell-{stamp}-{}.sh",
            std::process::id()
        ));
        // Probe absolute paths for the byte-emitting tool. Earlier
        // versions called bare `head` which silently exited
        // non-zero on CI runners with a stripped PATH (the
        // function then returns None and the test panicked with
        // the unhelpful "expected shell output" message). Fall
        // back across known coreutils locations and finally to dd
        // so the test does not depend on PATH or any single
        // distro layout.
        let head_candidates = ["/usr/bin/head", "/bin/head", "/usr/local/bin/head"];
        let dd_candidates = ["/usr/bin/dd", "/bin/dd"];
        let head = head_candidates
            .iter()
            .find(|p| Path::new(p).exists())
            .copied();
        let dd = dd_candidates
            .iter()
            .find(|p| Path::new(p).exists())
            .copied();
        let script_body = match (head, dd) {
            (Some(h), _) => format!("#!/bin/sh\nexec {h} -c 262144 /dev/zero\n"),
            (None, Some(d)) => format!(
                "#!/bin/sh\nexec {d} if=/dev/zero bs=1024 count=256 \
                 status=none\n"
            ),
            (None, None) => {
                eprintln!(
                    "skipping: neither head nor dd found in standard \
                     paths"
                );
                return;
            }
        };
        fs::write(&script_path, &script_body).expect("write shell script");
        let mut perms = fs::metadata(&script_path)
            .expect("read shell script metadata")
            .permissions();
        perms.set_mode(0o700);
        fs::set_permissions(&script_path, perms).expect("set executable permissions");

        // 10s gives slow ARM64 CI runners headroom; the script
        // itself completes in milliseconds. Call the
        // Result-returning variant so a CI flake prints the real
        // reason (spawn error, non-zero exit + stderr, timeout,
        // etc.) instead of an opaque "returned None".
        //
        // Linux can return ETXTBSY (OS error 26) on execve when a
        // parallel test thread's fork briefly holds a writable fd
        // for the script we just wrote. Retry a few times on that
        // race so cargo test -j N doesn't flake.
        let mut attempts_left = 5;
        let result = loop {
            let result = try_run_login_shell_env(
                script_path.to_str().expect("script path utf-8"),
                Duration::from_secs(10),
            );
            match &result {
                Err(LoginShellEnvError::Spawn(e)) if e.raw_os_error() == Some(26) => {
                    attempts_left -= 1;
                    if attempts_left == 0 {
                        break result;
                    }
                    thread::sleep(Duration::from_millis(50));
                    continue;
                }
                _ => break result,
            }
        };
        let removed = fs::remove_file(&script_path);

        let output = result.unwrap_or_else(|err| {
            panic!(
                "try_run_login_shell_env failed: {err}\n\
                 script_path={script_path:?} (removed={removed:?})\n\
                 script_body={script_body:?}"
            )
        });
        assert!(
            output.len() >= 262_144,
            "expected at least 262144 bytes, got {}",
            output.len()
        );
    }

    #[cfg(unix)]
    #[test]
    fn run_login_shell_env_timeout_returns_when_stdout_fd_stays_open() {
        let stamp = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("valid clock")
            .as_nanos();
        let script_path = std::env::temp_dir().join(format!(
            "agentsview-login-shell-timeout-{stamp}-{}.sh",
            std::process::id()
        ));
        fs::write(&script_path, "#!/bin/sh\n(sleep 2) &\nsleep 10\n").expect("write shell script");
        let mut perms = fs::metadata(&script_path)
            .expect("read shell script metadata")
            .permissions();
        perms.set_mode(0o700);
        fs::set_permissions(&script_path, perms).expect("set executable permissions");

        // Linux can return ETXTBSY (OS error 26) on execve when a
        // parallel test thread's fork briefly holds a writable fd
        // for the script we just wrote. Retry a few times on that
        // race so cargo test -j N doesn't flake.
        let mut attempts_left = 5;
        let (result, elapsed) = loop {
            let started = Instant::now();
            let result = try_run_login_shell_env(
                script_path.to_str().expect("script path utf-8"),
                Duration::from_millis(120),
            );
            let elapsed = started.elapsed();
            match &result {
                Err(LoginShellEnvError::Spawn(e)) if e.raw_os_error() == Some(26) => {
                    attempts_left -= 1;
                    if attempts_left == 0 {
                        break (result, elapsed);
                    }
                    thread::sleep(Duration::from_millis(50));
                    continue;
                }
                _ => break (result, elapsed),
            }
        };
        let _ = fs::remove_file(&script_path);

        match result {
            Err(LoginShellEnvError::Timeout { .. }) => {}
            other => panic!("expected Timeout error; got {other:?}"),
        }
        assert!(
            elapsed < Duration::from_secs(1),
            "timeout path took too long: {elapsed:?}"
        );
    }

    #[test]
    fn desktop_redirect_url_includes_desktop_query_param() {
        let url = desktop_redirect_url(18080);
        assert_eq!(url, "http://127.0.0.1:18080?desktop=1");

        let url2 = desktop_redirect_url(8080);
        assert!(url2.contains("?desktop=1"));
        assert!(url2.starts_with("http://127.0.0.1:8080"));
    }

    #[test]
    fn run_login_shell_env_returns_none_when_shell_missing() {
        let output = run_login_shell_env(
            "agentsview-missing-shell-binary",
            Duration::from_millis(100),
        );
        assert!(output.is_none(), "missing shell should return None");
    }

    #[test]
    fn try_run_login_shell_env_reports_spawn_error_when_shell_missing() {
        let result = try_run_login_shell_env(
            "agentsview-missing-shell-binary",
            Duration::from_millis(100),
        );
        match result {
            Err(LoginShellEnvError::Spawn(_)) => {}
            other => panic!("expected Spawn error; got {other:?}"),
        }
    }

    #[cfg(unix)]
    #[test]
    fn try_run_login_shell_env_reports_non_zero_with_stderr() {
        // Script that writes to stderr and exits non-zero, so we
        // can confirm the NonZero variant carries both the code
        // and the captured stderr. Future CI flakes in the large-
        // stdout test will surface the same info.
        let stamp = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("valid clock")
            .as_nanos();
        let script_path = std::env::temp_dir().join(format!(
            "agentsview-login-shell-fail-{stamp}-{}.sh",
            std::process::id()
        ));
        fs::write(&script_path, "#!/bin/sh\necho diag-stderr >&2\nexit 42\n")
            .expect("write shell script");
        let mut perms = fs::metadata(&script_path)
            .expect("read shell script metadata")
            .permissions();
        perms.set_mode(0o700);
        fs::set_permissions(&script_path, perms).expect("set executable permissions");

        let mut attempts_left = 5;
        let result = loop {
            let result = try_run_login_shell_env(
                script_path.to_str().expect("script path utf-8"),
                Duration::from_secs(2),
            );
            match &result {
                Err(LoginShellEnvError::Spawn(e)) if e.raw_os_error() == Some(26) => {
                    attempts_left -= 1;
                    if attempts_left == 0 {
                        break result;
                    }
                    thread::sleep(Duration::from_millis(50));
                    continue;
                }
                _ => break result,
            }
        };
        let _ = fs::remove_file(&script_path);

        match result {
            Err(LoginShellEnvError::NonZero {
                code: Some(42),
                stderr,
                ..
            }) => {
                let s = String::from_utf8_lossy(&stderr);
                assert!(
                    s.contains("diag-stderr"),
                    "stderr should be captured; got {s:?}"
                );
            }
            other => panic!("expected NonZero{{code=42}}; got {other:?}"),
        }
    }
}
