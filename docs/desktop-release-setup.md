# Desktop Release: Signing and Configuration Guide

This document covers the one-time setup for desktop release signing (macOS
notarization, Tauri update signing) and the GitHub secrets required by the
`desktop-release.yml` workflow.

## Overview

The desktop release workflow (`.github/workflows/desktop-release.yml`) triggers
on `v*` tag pushes and produces:

- **macOS**: Signed and notarized `.dmg` installer + `.app.tar.gz` updater
  bundle
- **Windows**: `.exe` NSIS installer + `.nsis.zip` updater bundle
- **Updater manifest**: `latest.json` with platform URLs and Ed25519 signatures

Three separate credential sets are needed:

| Credential Set                  | Purpose                       | Platforms       |
| ------------------------------- | ----------------------------- | --------------- |
| Apple Developer certificate     | Code signing                  | macOS           |
| Apple App Store Connect API key | Notarization                  | macOS           |
| Tauri signing key               | Update signature verification | macOS + Windows |

## 1. Apple Developer Certificate (macOS code signing)

Code signing proves the app was built by a known developer. macOS Gatekeeper
blocks unsigned apps. The CI workflow imports this certificate into a temporary
keychain, signs the `.app` bundle and DMG, then deletes the keychain.

### Prerequisites

- An [Apple Developer Program](https://developer.apple.com/programs/) membership
  ($99/year, required for "Developer ID" certificates)
- A Mac with Keychain Access (needed to generate the CSR and export the `.p12`)

### Step 1: Create a Certificate Signing Request (CSR)

1. Open **Keychain Access** (in `/Applications/Utilities/`)
1. Menu bar: **Keychain Access > Certificate Assistant > Request a Certificate
   from a Certificate Authority...**
1. Fill in:
   - **User Email Address**: your Apple ID email
   - **Common Name**: your name (can be anything)
   - **CA Email Address**: leave blank
   - Select **Saved to disk**
1. Click **Continue** and save the `.certSigningRequest` file

### Step 2: Create the certificate on Apple's portal

1. Go to
   [developer.apple.com/account/resources/certificates/list](https://developer.apple.com/account/resources/certificates/list)
1. Click the **+** button
1. Under "Software", select **Developer ID Application** (this is for apps
   distributed outside the App Store — do **not** choose "Mac App Distribution"
   or "Apple Development")
1. Click **Continue**, upload the `.certSigningRequest` file from step 1
1. Click **Continue**, then **Download** to get the `.cer` file
1. Double-click the `.cer` file to install it into Keychain Access

### Step 3: Export as .p12

The CI runner needs the certificate as a `.p12` file (which bundles the
certificate and its private key).

1. Open **Keychain Access**
1. In the left sidebar, select **login** keychain, then **My Certificates**
   category
1. Find the certificate named `Developer ID Application: Your Name (TEAMID)` —
   it should have a disclosure triangle showing a private key underneath
1. Right-click the certificate (not the private key) > **Export "Developer ID
   Application: ..."**
1. Format: **Personal Information Exchange (.p12)**
1. Set a strong password when prompted — you will need this for the
   `APPLE_CERTIFICATE_PASSWORD` secret

Base64-encode the `.p12` for storage as a GitHub secret:

```bash
base64 -i "Developer_ID_Application.p12" | pbcopy
# The base64 string is now on your clipboard
```

The output is a long base64 string (typically 3000-5000 characters). It starts
with something like `MIIKcQIBAzCCCjcGCS...`. This entire string goes into the
`APPLE_CERTIFICATE` secret.

### Step 4: Find your signing identity

Run this to list available code signing identities:

```bash
security find-identity -v -p codesigning
```

You should see output like:

```
  1) A1B2C3D4E5F6A1B2C3D4E5F6A1B2C3D4E5F6A1B2 "Developer ID Application: Jane Smith (ABC123XYZ)"
     1 valid identities found
```

The full quoted string — `Developer ID Application: Jane Smith (ABC123XYZ)` — is
your signing identity. The 10-character code in parentheses is your Team ID.

If you see multiple identities, use the one that matches the certificate you
just created. If you see no identities, the certificate wasn't installed
correctly — check that the `.cer` was imported and that the private key from the
CSR is in the same keychain.

### GitHub secrets for code signing

| Secret                       | Example value                                      | Notes                                                             |
| ---------------------------- | -------------------------------------------------- | ----------------------------------------------------------------- |
| `APPLE_CERTIFICATE`          | `MIIKcQIBAzCCCjcGCS...` (long base64)              | The entire base64-encoded `.p12` file                             |
| `APPLE_CERTIFICATE_PASSWORD` | `your-p12-export-password`                         | The password you set when exporting the `.p12`                    |
| `APPLE_SIGNING_IDENTITY`     | `Developer ID Application: Jane Smith (ABC123XYZ)` | Exact string from `security find-identity`, including the Team ID |

## 2. Apple App Store Connect API Key (notarization)

Notarization sends the signed app to Apple's servers for automated malware
scanning. After approval (usually 1-5 minutes), macOS recognizes the app as
checked by Apple and won't show the "unidentified developer" warning. The CI
workflow uses an App Store Connect API key to authenticate with Apple's notary
service.

### Step 1: Create the API key

1. Go to
   [appstoreconnect.apple.com/access/integrations/api](https://appstoreconnect.apple.com/access/integrations/api)
   - If you haven't used the API before, you'll need to click **Request Access**
     first
1. Note the **Issuer ID** displayed at the top of the page. It looks like a
   UUID:
   ```
   Issuer ID: a1b2c3d4-e5f6-7890-abcd-ef1234567890
   ```
1. Click **Generate API Key** (or the **+** button)
1. Name: `AgentsView Notarization` (or any descriptive name)
1. Access: **Developer** (minimum role needed for notarization)
1. Click **Generate**

### Step 2: Download the key

After generating, the key appears in the table with a **Download** link.

**Download the `.p8` file immediately.** Apple only lets you download it once.
If you lose it, you must revoke the key and create a new one.

The downloaded file is named `AuthKey_XXXXXXXXXX.p8` where `XXXXXXXXXX` is the
Key ID. For example: `AuthKey_ABC123DEF0.p8`.

The Key ID is also shown in the "Key ID" column of the table. It is a
10-character alphanumeric string like `ABC123DEF0`.

### Step 3: Inspect what you have

At this point you should have three pieces of information:

```
Issuer ID:  a1b2c3d4-e5f6-7890-abcd-ef1234567890    (from the top of the API keys page)
Key ID:     ABC123DEF0                                 (from the table, also in the filename)
Key file:   ~/Downloads/AuthKey_ABC123DEF0.p8          (the downloaded file)
```

The `.p8` file is a short PEM-encoded private key (about 300 bytes). It looks
like:

```
-----BEGIN PRIVATE KEY-----
MIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQg...
(2-3 lines of base64)
-----END PRIVATE KEY-----
```

### Step 4: Base64-encode the key file

```bash
base64 -i ~/Downloads/AuthKey_ABC123DEF0.p8 | pbcopy
# The base64 string is now on your clipboard
```

The base64 output is relatively short (about 400 characters). This goes into
`APPLE_API_KEY_CONTENT`.

### GitHub secrets for notarization

| Secret                  | Example value                          | Notes                                       |
| ----------------------- | -------------------------------------- | ------------------------------------------- |
| `APPLE_API_KEY_CONTENT` | `LS0tLS1CRUdJTiBQUk...` (base64)       | Base64-encoded `.p8` key file               |
| `APPLE_API_KEY`         | `ABC123DEF0`                           | The 10-character Key ID (not the Issuer ID) |
| `APPLE_API_ISSUER`      | `a1b2c3d4-e5f6-7890-abcd-ef1234567890` | UUID from the top of the API keys page      |

### How the workflow uses these

The workflow reconstructs the `.p8` file on the runner:

```bash
echo "$APPLE_API_KEY_CONTENT" | base64 --decode > AuthKey_${APPLE_API_KEY}.p8
```

Then Tauri's build process passes the key to Apple's notary service via
`notarytool`. The `APPLE_API_ISSUER` and `APPLE_API_KEY` identify which key to
use. If notarization succeeds, `tauri build` staples the notarization ticket to
the DMG automatically.

## 3. Tauri Update Signing Key (auto-updater)

The Tauri updater uses Ed25519 signatures to verify that update bundles are
authentic. A keypair is generated once; the private key signs bundles during CI,
and the public key is compiled into the app binary.

### Generate the keypair

```bash
npx @tauri-apps/cli signer generate -w ~/.tauri/agentsview.key
```

This creates two files:

- `~/.tauri/agentsview.key` -- the private key (keep secret)
- `~/.tauri/agentsview.key.pub` -- the public key

The command will prompt for a password. You can leave it empty for an
unencrypted key, or set one (you'll need to provide it as a GitHub secret).

### Configure the public key

The public key needs to go in **two** places:

**Option A (recommended):** Add `AGENTSVIEW_UPDATER_PUBKEY` as a GitHub Actions
secret containing the public key string. The release workflow passes it as an
env var to both Tauri build steps, and the Rust code reads it at compile time
via `option_env!("AGENTSVIEW_UPDATER_PUBKEY")` to override the placeholder in
`tauri.conf.json`. The relevant workflow lines look like:

```yaml
env:
  AGENTSVIEW_UPDATER_PUBKEY: ${{ secrets.AGENTSVIEW_UPDATER_PUBKEY }}
  TAURI_SIGNING_PRIVATE_KEY: ${{ secrets.TAURI_SIGNING_PRIVATE_KEY }}
  TAURI_SIGNING_PRIVATE_KEY_PASSWORD: ${{ secrets.TAURI_SIGNING_PRIVATE_KEY_PASSWORD }}
```

If this secret is missing or empty, the app compiles but the updater falls back
to the `"NOT_SET"` placeholder and shows "updater is not configured" at runtime.

**Option B:** Replace `"NOT_SET"` in `desktop/src-tauri/tauri.conf.json`
directly:

```json
"plugins": {
  "updater": {
    "pubkey": "<paste contents of agentsview.key.pub here>",
    "endpoints": [
      "https://github.com/kenn-io/agentsview/releases/latest/download/latest.json"
    ]
  }
}
```

### GitHub secrets

| Secret                               | Value                                  |
| ------------------------------------ | -------------------------------------- |
| `TAURI_SIGNING_PRIVATE_KEY`          | Contents of `~/.tauri/agentsview.key`  |
| `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` | Password (empty string if unencrypted) |

If using Option A for the public key:

| Secret                      | Value                                     |
| --------------------------- | ----------------------------------------- |
| `AGENTSVIEW_UPDATER_PUBKEY` | Contents of `~/.tauri/agentsview.key.pub` |

## Complete GitHub Secrets Reference

All secrets are configured at **Settings > Secrets and variables > Actions** in
the GitHub repository.

| Secret                               | Used By        | Purpose                            |
| ------------------------------------ | -------------- | ---------------------------------- |
| `APPLE_CERTIFICATE`                  | macOS build    | Signing certificate (.p12, base64) |
| `APPLE_CERTIFICATE_PASSWORD`         | macOS build    | Certificate password               |
| `APPLE_SIGNING_IDENTITY`             | macOS build    | Certificate CN identity string     |
| `APPLE_API_KEY_CONTENT`              | macOS build    | Notarization API key (.p8, base64) |
| `APPLE_API_KEY`                      | macOS build    | API key ID                         |
| `APPLE_API_ISSUER`                   | macOS build    | API issuer ID                      |
| `TAURI_SIGNING_PRIVATE_KEY`          | Both platforms | Tauri updater signing key          |
| `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` | Both platforms | Signing key password               |
| `AGENTSVIEW_UPDATER_PUBKEY`          | Both platforms | Updater public key (Option A)      |

## Key Rotation

### Rotating the Apple certificate

Apple Developer ID Application certificates are valid for 5 years. To rotate:

1. Generate a new certificate following section 1 above
1. Export as `.p12` and base64-encode
1. Update `APPLE_CERTIFICATE` and `APPLE_CERTIFICATE_PASSWORD` in GitHub secrets
1. Update `APPLE_SIGNING_IDENTITY` if the identity string changed
1. The old certificate can be revoked in Apple Developer portal after confirming
   new builds work

### Rotating the Apple API key

API keys don't expire, but can be revoked. To rotate:

1. Generate a new key in App Store Connect
1. Base64-encode the new `.p8` file
1. Update `APPLE_API_KEY_CONTENT` and `APPLE_API_KEY` in GitHub secrets
1. `APPLE_API_ISSUER` doesn't change (it's per-organization)
1. Revoke the old key in App Store Connect

### Rotating the Tauri signing key

Changing the signing key means existing app installations cannot verify updates
signed with the new key. Plan for this:

1. Generate a new keypair:
   `npx @tauri-apps/cli signer generate -w ~/.tauri/agentsview-v2.key`
1. Update `TAURI_SIGNING_PRIVATE_KEY` and `TAURI_SIGNING_PRIVATE_KEY_PASSWORD`
   in GitHub secrets
1. Update the public key in `tauri.conf.json` or `AGENTSVIEW_UPDATER_PUBKEY`
1. Release a version with the new public key compiled in
1. Users on older versions will see update verification fail and need to
   download the new version manually from the GitHub releases page

## Build Artifacts

Each release produces these artifacts:

| File                                      | Description                                  |
| ----------------------------------------- | -------------------------------------------- |
| `AgentsView_x.y.z_aarch64.dmg`            | macOS Apple Silicon installer                |
| `AgentsView_x.y.z_x64.dmg`                | macOS Intel installer                        |
| `AgentsView_aarch64.app.tar.gz`           | macOS Apple Silicon updater bundle           |
| `AgentsView_aarch64.app.tar.gz.sig`       | macOS Apple Silicon updater signature        |
| `AgentsView_x86_64.app.tar.gz`            | macOS Intel updater bundle                   |
| `AgentsView_x86_64.app.tar.gz.sig`        | macOS Intel updater signature                |
| `AgentsView_x.y.z_x64-setup.exe`          | Windows NSIS installer                       |
| `AgentsView_x.y.z_x64-setup.nsis.zip`     | Windows updater bundle                       |
| `AgentsView_x.y.z_x64-setup.nsis.zip.sig` | Windows updater signature                    |
| `AgentsView_x.y.z_amd64.AppImage`         | Linux x86_64 AppImage                        |
| `AgentsView_x.y.z_aarch64.AppImage`       | Linux arm64 AppImage                         |
| `latest.json`                             | Updater manifest (version, URLs, signatures) |
| `SHA256SUMS-desktop`                      | Checksums for all desktop artifacts          |

## Runtime Configuration

These environment variables affect the desktop app at runtime (not build time):

| Variable                                  | Default | Purpose                                                 |
| ----------------------------------------- | ------- | ------------------------------------------------------- |
| `AGENTSVIEW_DESKTOP_AUTOUPDATE`           | enabled | Set to `0` to disable automatic update check on startup |
| `AGENTSVIEW_DESKTOP_SKIP_LOGIN_SHELL_ENV` | unset   | Set to skip inheriting login shell environment          |
| `AGENTSVIEW_DESKTOP_PATH`                 | unset   | Override PATH passed to the Go backend sidecar          |

Users can also set environment overrides in `~/.agentsview/desktop.env`
(KEY=VALUE format, one per line).

## Staging / Testing

Test the full release pipeline on a personal fork before shipping to production.
This covers code signing, notarization, updater artifacts, and the end-to-end
update flow.

### Fork setup

1. Fork the repository on GitHub.

1. Configure **all** secrets on the fork (Settings > Secrets and variables >
   Actions). The Apple secrets are the same ones used in production — they are
   tied to your Apple Developer account, not to a specific repository:

   | Secret                               | Notes                                            |
   | ------------------------------------ | ------------------------------------------------ |
   | `APPLE_CERTIFICATE`                  | Same certificate works on any repo               |
   | `APPLE_CERTIFICATE_PASSWORD`         |                                                  |
   | `APPLE_SIGNING_IDENTITY`             |                                                  |
   | `APPLE_API_KEY_CONTENT`              | Same API key works for any app                   |
   | `APPLE_API_KEY`                      |                                                  |
   | `APPLE_API_ISSUER`                   |                                                  |
   | `TAURI_SIGNING_PRIVATE_KEY`          | Generate a **separate** test keypair (see below) |
   | `TAURI_SIGNING_PRIVATE_KEY_PASSWORD` |                                                  |
   | `AGENTSVIEW_UPDATER_PUBKEY`          | Public key from the test keypair                 |

1. Generate a test Tauri signing keypair (do not reuse the production key):

   ```bash
   npx @tauri-apps/cli signer generate -w /tmp/staging-updater.key
   # Use the contents of /tmp/staging-updater.key for TAURI_SIGNING_PRIVATE_KEY
   # Use the contents of /tmp/staging-updater.key.pub for AGENTSVIEW_UPDATER_PUBKEY
   ```

No manual `tauri.conf.json` edits are needed. The workflow automatically patches
the updater endpoint URL and `latest.json` download URLs to use the current
repository (`$GITHUB_REPOSITORY`).

### Test the CI pipeline

Push the branch and a test tag to the fork:

```bash
git remote add staging git@github.com:YOUR_USER/agentsview.git
git push staging tauri-packaging
git tag v0.0.1-staging.1
git push staging v0.0.1-staging.1
```

Watch the workflow run. Verify:

- **macOS job**: Certificate import succeeds, code signing succeeds,
  notarization completes (Apple returns "Accepted"), DMG and `.app.tar.gz` +
  `.sig` are uploaded
- **Windows job**: NSIS installer and `.nsis.zip` + `.sig` are uploaded
- **Release job**: `latest.json` contains non-empty URLs and signatures for both
  platforms, all artifacts appear on the GitHub Release page

### Test the desktop update flow

This requires two releases on the fork — an older version to install, and a
newer version to update to.

1. After `v0.0.1-staging.1` finishes building, download and install the macOS
   DMG (or Windows installer).

1. Make a small commit (e.g. edit a comment), then push a second tag. The second
   tag **must** be on a different commit so the build produces a distinct
   version:

   ```bash
   git commit --allow-empty -m "staging: bump for v0.0.2 test"
   git tag v0.0.2-staging.1
   git push staging tauri-packaging v0.0.2-staging.1
   ```

1. Wait for the workflow to complete and the release to publish.

1. Launch the v0.0.1 app. Verify:

   - **Auto-check**: Within a few seconds of startup, a native dialog should
     appear offering to update to v0.0.2 (check stderr for `[agentsview]` log
     lines if it doesn't)
   - **Menu**: Click "AgentsView > Check for Updates..." — should show the
     update dialog
   - **Install**: Click OK to download and install, then confirm the restart
     prompt
   - **Post-restart**: The app should relaunch running v0.0.2

1. Check "Check for Updates..." again — should now show "You're running the
   latest version."

### Test the Go endpoint and frontend

No fork needed. Run locally with a low version number:

```bash
go build -tags fts5 \
  -ldflags "-X main.version=v0.1.0" \
  -o /tmp/agentsview-test ./cmd/agentsview
/tmp/agentsview-test serve
```

Verify:

- `GET /api/v1/update/check` returns `update_available: true` with the correct
  latest version
- The StatusBar shows "update available" — clicking it opens the UpdateModal
- The modal displays current vs latest version and CLI instructions

Repeat with `-X main.version=v99.99.99` (up-to-date) and `-X main.version=dev`
(dev build) to confirm those paths show no update indicator.

### Cleanup

After testing, delete the test tags and releases from the fork:

```bash
git push staging --delete v0.0.1-staging.1 v0.0.2-staging.1
git tag -d v0.0.1-staging.1 v0.0.2-staging.1
```

Delete the releases manually from the fork's GitHub Releases page.

## Troubleshooting

### Desktop app does not prompt for a just-published version

The desktop updater is published by `.github/workflows/desktop-release.yml`,
separately from the CLI/PyPI release workflow. A versioned GitHub release can
exist before the desktop workflow has uploaded installer assets and replaced the
permanent `updater` release's `latest.json`.

Check the desktop release workflow first:

```bash
gh run list \
  --repo kenn-io/agentsview \
  --workflow desktop-release.yml \
  --limit 5
```

Then verify the updater manifest:

```bash
curl -fsSL \
  https://github.com/kenn-io/agentsview/releases/download/updater/latest.json |
  python3 -m json.tool
```

The manifest's `version` must match the tag, and its platform URLs should point
at `https://github.com/kenn-io/agentsview/releases/download/updater/...`. If the
manifest still shows the previous version, wait for the `Desktop Release`
workflow to finish or rerun the failed job. The desktop app checks for updates
on startup, so an app launched before `latest.json` is replaced may need a
manual `Check for Updates...` after the workflow completes.

### Code signing: "no identity found" or "Developer ID Application" not found

The `APPLE_SIGNING_IDENTITY` secret must exactly match the identity string from
`security find-identity`. Common issues:

- **Wrong certificate type**: "Mac Developer" or "Apple Development"
  certificates don't work for distribution. You need "Developer ID Application".
- **Typo in identity string**: Copy-paste the entire quoted string from
  `security find-identity`, including the Team ID in parentheses.
- **Certificate expired**: Developer ID Application certificates are valid for 5
  years. Check expiry in Keychain Access or at
  [developer.apple.com/account/resources/certificates](https://developer.apple.com/account/resources/certificates/list).
- **Private key missing from .p12**: When exporting, make sure you export from
  "My Certificates" (which bundles the private key), not from "Certificates"
  (which exports only the public cert).

### Code signing: "errSecInternalComponent" or "User interaction is not allowed"

The keychain wasn't unlocked properly. This usually means the
`APPLE_CERTIFICATE_PASSWORD` secret doesn't match the password used when
exporting the `.p12`. Re-export with a known password and update the secret.

### Notarization: "invalid credentials" or "authentication failed"

Check each piece independently:

1. **Is the API key revoked?** Check at
   [appstoreconnect.apple.com/access/integrations/api](https://appstoreconnect.apple.com/access/integrations/api)
1. **Is `APPLE_API_KEY` the Key ID (not the Issuer ID)?** The Key ID is the
   10-character string like `ABC123DEF0`, not the UUID.
1. **Is `APPLE_API_ISSUER` the Issuer ID (not the Key ID)?** The Issuer ID is
   the UUID shown at the top of the API keys page.
1. **Is `APPLE_API_KEY_CONTENT` correctly base64-encoded?** Decode and verify it
   looks like a PEM private key:
   ```bash
   echo "$APPLE_API_KEY_CONTENT" | base64 --decode
   # Should print:
   # -----BEGIN PRIVATE KEY-----
   # (2-3 lines of base64)
   # -----END PRIVATE KEY-----
   ```
1. **Was the `.p8` file re-downloaded?** Apple only allows one download. If you
   lost the original, revoke the key and create a new one.

### Notarization: "package is invalid" or "the signature is invalid"

The app was signed with the wrong certificate type, or the entitlements are
incorrect. Verify:

- The certificate is "Developer ID Application" (not "Apple Development" or "3rd
  Party Mac Developer Application")
- `desktop/src-tauri/Entitlements.plist` includes the hardened runtime
  entitlements for WebKit JIT

### "The updater is not configured"

The `AGENTSVIEW_UPDATER_PUBKEY` env var was not set at compile time, or the
pubkey in `tauri.conf.json` is still `"NOT_SET"`. Make sure the secret is
configured in GitHub and that both build steps in `desktop-release.yml` pass it
as an env var.

### Update verification fails after key rotation

Expected. Users on versions compiled with the old public key cannot verify
signatures from the new private key. They must download the new version manually
from the releases page.
