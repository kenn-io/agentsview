use std::path::Path;

#[path = "src/updater_key.rs"]
mod updater_key;

fn main() {
    check_updater_pubkey();
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "claude_auth_connect",
            "claude_auth_fetch",
            "claude_auth_close",
            "claude_auth_fetch_result",
        ]),
    ))
    .expect("desktop build failed")
}

/// Stops the build when the updater could not verify downloaded updates.
fn check_updater_pubkey() {
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("tauri.conf.json");
    println!("cargo:rerun-if-changed={}", path.display());
    println!("cargo:rerun-if-env-changed=TAURI_CONFIG");
    let raw = std::fs::read_to_string(&path).expect("read tauri.conf.json");
    let config: serde_json::Value = serde_json::from_str(&raw).expect("parse tauri.conf.json");
    let overlay = std::env::var("TAURI_CONFIG")
        .ok()
        .map(|raw| serde_json::from_str(&raw).expect("parse TAURI_CONFIG"));
    if let Err(err) = updater_key::check(&config, overlay.as_ref()) {
        panic!(
            "{err}\nThe updater refuses every update without a valid key. \
             Set plugins.updater.pubkey in tauri.conf.json or pass it with \
             `tauri build --config`."
        );
    }
}
