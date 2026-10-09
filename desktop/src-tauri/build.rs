fn main() {
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
