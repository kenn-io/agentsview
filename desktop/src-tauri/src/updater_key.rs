//! Checks the public key the updater verifies downloaded updates with.
//!
//! `build.rs` includes this file so a build whose updater could not verify
//! any update fails before it can ship.

use base64::Engine;
use serde_json::Value;

const PUBKEY_PATH: [&str; 3] = ["plugins", "updater", "pubkey"];

/// Fails unless the updater public key the build embeds is a minisign public
/// key that the updater can decode.
///
/// `overlay` is the `TAURI_CONFIG` value the Tauri CLI sets for `--config`.
/// Tauri merges it into `tauri.conf.json` as a JSON merge patch.
pub fn check(config: &Value, overlay: Option<&Value>) -> Result<(), String> {
    let Some(pubkey) = effective_pubkey(config, overlay) else {
        return Err("plugins.updater.pubkey is missing or not a string".to_string());
    };
    decode(pubkey)
        .map(|_| ())
        .map_err(|err| format!("plugins.updater.pubkey is {err}"))
}

fn effective_pubkey<'a>(config: &'a Value, overlay: Option<&'a Value>) -> Option<&'a str> {
    let mut patch = overlay;
    for (depth, key) in PUBKEY_PATH.iter().enumerate() {
        let Some(node) = patch else { break };
        // A merge patch that is not an object replaces everything below it.
        let Value::Object(fields) = node else {
            return None;
        };
        match fields.get(*key) {
            None => patch = None,
            Some(next) if depth == PUBKEY_PATH.len() - 1 => return next.as_str(),
            Some(next) => patch = Some(next),
        }
    }
    PUBKEY_PATH
        .iter()
        .try_fold(config, |node, key| node.get(key))
        .and_then(Value::as_str)
}

/// Decodes the key the same way tauri-plugin-updater does before it verifies
/// an update signature.
fn decode(pubkey: &str) -> Result<minisign_verify::PublicKey, String> {
    let bytes = base64::engine::general_purpose::STANDARD
        .decode(pubkey)
        .map_err(|err| format!("not base64: {err}"))?;
    let text = String::from_utf8(bytes).map_err(|_| "not UTF-8 text".to_string())?;
    minisign_verify::PublicKey::decode(&text)
        .map_err(|err| format!("not a minisign public key: {err}"))
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    const CONFIG: &str = include_str!("../tauri.conf.json");

    // Updater signature of AgentsView_0.47.0_x64-setup.nsis.zip from the
    // published updater/latest.json.
    const RELEASE_SIGNATURE: &str = "dW50cnVzdGVkIGNvbW1lbnQ6IHNpZ25hdHVyZSBmcm9tIHRhdXJpIHNlY3JldCBrZXkKUlVUTGMzajVIVnVrd0VuN21XUzl2a1ZpN21qbXNaS25aeExkZUVkdnlkUUt3Y2hJckNCdlpvM3RYM1Vsa3JwVU83RDk1UHZnNXFCZW1qNjEyZlJTSjAvd3kzUE9BVHJSUmdjPQp0cnVzdGVkIGNvbW1lbnQ6IHRpbWVzdGFtcDoxNzk0MjQxODg0CWZpbGU6QWdlbnRzVmlld18wLjQ3LjBfeDY0LXNldHVwLm5zaXMuemlwCmpIcEJwcjVmRHc4WEx5WFVzblRTZjVWT3o5ZytGQWpzWkUzeGNSOVpRNDZ4UGFSSlNhckVvZ1JmTjJPRzlBelgveGloUzhCOGhQZGs4TjVXSlNOcEJBPT0K";

    fn committed_config() -> Value {
        serde_json::from_str(CONFIG).expect("tauri.conf.json is JSON")
    }

    fn committed_pubkey() -> String {
        effective_pubkey(&committed_config(), None)
            .expect("tauri.conf.json has an updater pubkey")
            .to_string()
    }

    /// Key ID bytes from the second line of a base64-wrapped minisign key or
    /// signature file.
    fn key_id(wrapped: &str) -> Vec<u8> {
        let engine = base64::engine::general_purpose::STANDARD;
        let text = String::from_utf8(engine.decode(wrapped).unwrap()).unwrap();
        let line = text.lines().nth(1).unwrap();
        engine.decode(line).unwrap()[2..10].to_vec()
    }

    #[test]
    fn committed_key_is_the_release_signing_key() {
        assert_eq!(check(&committed_config(), None), Ok(()));
        assert_eq!(key_id(&committed_pubkey()), key_id(RELEASE_SIGNATURE));
    }

    #[test]
    fn rejects_builds_without_a_usable_key() {
        let valid = committed_pubkey();
        let placeholder = json!({ "plugins": { "updater": { "pubkey": "NOT_SET" } } });
        let with_valid_key = json!({ "plugins": { "updater": { "pubkey": valid } } });
        let not_a_key = base64::engine::general_purpose::STANDARD.encode("hello\nworld\n");
        let cases = [
            ("placeholder in config", placeholder.clone(), None),
            ("no updater config", json!({ "plugins": {} }), None),
            (
                "empty override",
                with_valid_key.clone(),
                Some(json!({ "plugins": { "updater": { "pubkey": "" } } })),
            ),
            (
                "override deletes the key",
                with_valid_key.clone(),
                Some(json!({ "plugins": { "updater": { "pubkey": null } } })),
            ),
            (
                "override deletes the updater config",
                with_valid_key.clone(),
                Some(json!({ "plugins": { "updater": null } })),
            ),
            (
                "override replaces the whole config",
                with_valid_key.clone(),
                Some(json!("not an object")),
            ),
            (
                "base64 text that is not a key",
                with_valid_key,
                Some(json!({ "plugins": { "updater": { "pubkey": not_a_key } } })),
            ),
        ];
        for (name, config, overlay) in cases {
            assert!(
                check(&config, overlay.as_ref()).is_err(),
                "{name} should fail the build"
            );
        }
    }

    #[test]
    fn accepts_overrides_that_keep_a_valid_key() {
        let valid = committed_pubkey();
        let placeholder = json!({ "plugins": { "updater": { "pubkey": "NOT_SET" } } });
        // The release pipeline passes its key through --config.
        let release_override = json!({
            "version": "1.2.3",
            "plugins": { "updater": { "pubkey": valid, "endpoints": ["https://example.com"] } }
        });
        assert_eq!(check(&placeholder, Some(&release_override)), Ok(()));
        // CI artifact builds override only bundle settings.
        let bundle_override = json!({ "bundle": { "createUpdaterArtifacts": false } });
        assert_eq!(check(&committed_config(), Some(&bundle_override)), Ok(()));
    }
}
