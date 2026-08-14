fn main() {
    tauri_build::try_build(tauri_build::Attributes::new().app_manifest(
        tauri_build::AppManifest::new().commands(&[
            "claude_auth_start",
            "claude_auth_disconnect",
            "claude_auth_status",
            "claude_auth_fetch_result",
        ]),
    ))
    .expect("failed to build Tauri application manifest");
}
