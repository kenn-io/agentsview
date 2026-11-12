//! The Windows updater unpacks the `.nsis.zip` update archive with
//! `zip::ZipArchive::extract`. The release pipeline writes that archive with
//! Deflate, so the zip build the updater links must support it (#2839).

use std::io::Cursor;

// Written the way the release pipeline writes the archive: one Deflate entry
// holding the setup executable.
const ARCHIVE: &[u8] = include_bytes!("../testdata/deflated-setup.nsis.zip");
const SETUP_NAME: &str = "AgentsView_0.0.0_x64-setup.exe";

#[test]
fn updater_unpacks_deflated_nsis_archive() {
    // Compression method field of the first local file header; 8 is Deflate.
    assert_eq!(
        u16::from_le_bytes([ARCHIVE[8], ARCHIVE[9]]),
        8,
        "the fixture must stay Deflate-compressed to cover the release format"
    );

    let mut archive = zip::ZipArchive::new(Cursor::new(ARCHIVE)).expect("open archive");
    let dir = tempfile::tempdir().expect("temp dir");
    archive.extract(dir.path()).expect("extract archive");

    let setup = std::fs::read(dir.path().join(SETUP_NAME)).expect("read setup");
    assert_eq!(setup, b"AgentsView setup payload\n".repeat(400));
}
