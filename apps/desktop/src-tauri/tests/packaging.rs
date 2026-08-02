//! Packaging invariants for the desktop shell.
//!
//! These assert the things that broke a real Ubuntu build and produced errors
//! naming the wrong culprit — a proc-macro panic for a missing file, a crate
//! resolution failure for a toolchain floor. Each is cheap to check and
//! impossible to notice by reading the source.

use std::fs;
use std::path::{Path, PathBuf};

fn crate_root() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
}

/// Reads a PNG's IHDR without an image library.
///
/// Byte 25 is the colour type: 6 is RGBA, 3 is palette, 4 is grey+alpha, 0 is
/// greyscale. Byte 24 is the bit depth.
fn png_header(path: &Path) -> Option<(u8, u8)> {
    let bytes = fs::read(path).ok()?;
    if bytes.len() < 26 || &bytes[0..8] != b"\x89PNG\r\n\x1a\n" {
        return None;
    }
    Some((bytes[24], bytes[25]))
}

/// Every icon named in tauri.conf.json must exist.
///
/// A missing icon is not reported as a missing file: `generate_context!`
/// panics inside a procedural macro, so the compiler blames the macro
/// invocation and the real cause is two lines of JSON away.
#[test]
fn every_configured_icon_exists() {
    let config = fs::read_to_string(crate_root().join("tauri.conf.json"))
        .expect("tauri.conf.json is readable");

    let mut checked = 0;
    for line in config.lines() {
        for fragment in line.split('"') {
            if !fragment.starts_with("icons/") {
                continue;
            }
            let path = crate_root().join(fragment);
            assert!(
                path.exists(),
                "tauri.conf.json references {fragment}, which does not exist. \
                 Run: python3 src-tauri/icons/generate.py"
            );
            checked += 1;
        }
    }

    assert!(checked > 0, "no icons are configured; the bundle would be unbranded");
}

/// Tauri rejects any icon that is not 8-bit RGBA.
///
/// ImageMagick optimises small images into PaletteAlpha or GrayscaleAlpha,
/// which are valid PNG and are refused with "icon ... is not RGBA" — again as
/// a proc-macro panic rather than an image error.
#[test]
fn icons_are_eight_bit_rgba() {
    let dir = crate_root().join("icons");
    let entries = fs::read_dir(&dir).expect("the icons directory exists");

    let mut checked = 0;
    for entry in entries.flatten() {
        let path = entry.path();
        if path.extension().and_then(|e| e.to_str()) != Some("png") {
            continue;
        }

        let (depth, colour_type) =
            png_header(&path).unwrap_or_else(|| panic!("{} is not a valid PNG", path.display()));

        assert_eq!(
            colour_type,
            6,
            "{} has PNG colour type {colour_type}, but Tauri requires 6 (RGBA). \
             Regenerate with: python3 src-tauri/icons/generate.py",
            path.display()
        );
        assert_eq!(depth, 8, "{} is {depth}-bit; Tauri requires 8", path.display());
        checked += 1;
    }

    assert!(checked >= 2, "only {checked} PNG icons were found");
}

/// The Windows icon must be a real ICO container.
#[test]
fn windows_icon_is_an_ico() {
    let path = crate_root().join("icons/icon.ico");
    let bytes = fs::read(&path).expect("icon.ico exists");

    // ICONDIR: reserved=0, type=1 (icon), then the image count.
    assert!(bytes.len() > 6, "icon.ico is truncated");
    assert_eq!(&bytes[0..4], &[0, 0, 1, 0], "icon.ico is not an ICO container");

    let count = u16::from_le_bytes([bytes[4], bytes[5]]);
    assert!(count > 0, "icon.ico declares no images");
}

/// The declared toolchain floor must admit Tauri 2's dependency tree.
///
/// Several transitive crates publish manifests declaring `edition2024`, which
/// Cargo below 1.85 cannot parse at all. Resolution then fails with an error
/// naming an unrelated crate, so a stale floor here sends people to the wrong
/// place entirely.
#[test]
fn rust_version_floor_supports_edition_2024() {
    let manifest = fs::read_to_string(crate_root().join("Cargo.toml"))
        .expect("Cargo.toml is readable");

    let declared = manifest
        .lines()
        .find_map(|line| line.strip_prefix("rust-version = "))
        .map(|value| value.trim().trim_matches('"').to_string())
        .expect("Cargo.toml declares rust-version");

    let mut parts = declared.split('.');
    let major: u32 = parts.next().unwrap_or("0").parse().unwrap_or(0);
    let minor: u32 = parts.next().unwrap_or("0").parse().unwrap_or(0);

    assert!(
        major > 1 || (major == 1 && minor >= 85),
        "rust-version is {declared}, but Tauri 2's dependencies need 1.85+ \
         to parse their edition2024 manifests"
    );
}
