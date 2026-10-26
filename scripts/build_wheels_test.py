from __future__ import annotations

import io
import os
import stat
import tarfile
import zipfile
from pathlib import Path

import pytest

from build_wheels import (
    PLATFORM_MAP,
    build_all_wheels,
    build_wheel,
    extract_binary,
    parse_archive_filename,
)


# ---------------------------------------------------------------------------
# Archive filename parsing
# ---------------------------------------------------------------------------


class TestParseArchiveFilename:
    def test_parse_linux_amd64_tar_gz(self) -> None:
        result = parse_archive_filename("agentsview_0.15.0_linux_amd64.tar.gz")
        assert result == ("linux_amd64", "0.15.0")

    def test_parse_darwin_arm64_tar_gz(self) -> None:
        result = parse_archive_filename("agentsview_1.2.3_darwin_arm64.tar.gz")
        assert result == ("darwin_arm64", "1.2.3")

    def test_parse_windows_amd64_zip(self) -> None:
        result = parse_archive_filename("agentsview_0.15.0_windows_amd64.zip")
        assert result == ("windows_amd64", "0.15.0")

    def test_parse_windows_arm64_zip(self) -> None:
        result = parse_archive_filename("agentsview_0.15.0_windows_arm64.zip")
        assert result == ("windows_arm64", "0.15.0")

    def test_parse_darwin_amd64_tar_gz(self) -> None:
        result = parse_archive_filename("agentsview_2.0.0_darwin_amd64.tar.gz")
        assert result == ("darwin_amd64", "2.0.0")

    def test_unrecognized_filename_returns_none(self) -> None:
        assert parse_archive_filename("somethingelse_0.1.0_linux_amd64.tar.gz") is None

    def test_unknown_platform_returns_none(self) -> None:
        assert parse_archive_filename("agentsview_0.1.0_freebsd_amd64.tar.gz") is None

    def test_no_extension_returns_none(self) -> None:
        assert parse_archive_filename("agentsview_0.1.0_linux_amd64") is None

    def test_sha256sums_returns_none(self) -> None:
        assert parse_archive_filename("agentsview_0.15.0_SHA256SUMS") is None

    def test_path_with_directory_uses_basename(self) -> None:
        result = parse_archive_filename(
            "releases/agentsview_0.15.0_linux_arm64.tar.gz"
        )
        # parse_archive_filename only accepts basenames, so paths return None
        assert result is None

        # The caller is responsible for passing just the filename
        result = parse_archive_filename("agentsview_0.15.0_linux_arm64.tar.gz")
        assert result == ("linux_arm64", "0.15.0")


# ---------------------------------------------------------------------------
# Archive extraction
# ---------------------------------------------------------------------------


def _make_targz(binary_name: str, content: bytes) -> bytes:
    """Create an in-memory .tar.gz with a single binary file."""
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tf:
        info = tarfile.TarInfo(name=binary_name)
        info.size = len(content)
        tf.addfile(info, io.BytesIO(content))
    return buf.getvalue()


def _make_zip(binary_name: str, content: bytes) -> bytes:
    """Create an in-memory .zip with a single binary file."""
    buf = io.BytesIO()
    with zipfile.ZipFile(buf, "w") as zf:
        zf.writestr(binary_name, content)
    return buf.getvalue()


class TestExtractBinary:
    def test_extract_from_tar_gz(self, tmp_path: Path) -> None:
        content = b"fake-binary-content"
        archive = tmp_path / "agentsview_0.15.0_linux_amd64.tar.gz"
        archive.write_bytes(_make_targz("agentsview", content))
        result = extract_binary(archive, "agentsview")
        assert result == content

    def test_extract_from_zip(self, tmp_path: Path) -> None:
        content = b"fake-binary-exe"
        archive = tmp_path / "agentsview_0.15.0_windows_amd64.zip"
        archive.write_bytes(_make_zip("agentsview.exe", content))
        result = extract_binary(archive, "agentsview.exe")
        assert result == content

    def test_missing_binary_raises_file_not_found(self, tmp_path: Path) -> None:
        archive = tmp_path / "agentsview_0.15.0_linux_amd64.tar.gz"
        archive.write_bytes(_make_targz("wrong_name", b"data"))
        with pytest.raises(FileNotFoundError, match="agentsview"):
            extract_binary(archive, "agentsview")

    def test_missing_binary_in_zip_raises_file_not_found(self, tmp_path: Path) -> None:
        archive = tmp_path / "agentsview_0.15.0_windows_amd64.zip"
        archive.write_bytes(_make_zip("wrong.exe", b"data"))
        with pytest.raises(FileNotFoundError, match="agentsview.exe"):
            extract_binary(archive, "agentsview.exe")

    def test_nested_path_in_tar_gz(self, tmp_path: Path) -> None:
        """Binary may be inside a subdirectory in the archive."""
        content = b"nested-binary"
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tf:
            info = tarfile.TarInfo(name="agentsview_0.15.0_linux_amd64/agentsview")
            info.size = len(content)
            tf.addfile(info, io.BytesIO(content))
        archive = tmp_path / "agentsview_0.15.0_linux_amd64.tar.gz"
        archive.write_bytes(buf.getvalue())
        result = extract_binary(archive, "agentsview")
        assert result == content


# ---------------------------------------------------------------------------
# Wheel assembly
# ---------------------------------------------------------------------------


class TestBuildWheel:
    def test_wheel_filename_matches_convention(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        assert whl.name == "agentsview-0.15.0-py3-none-manylinux_2_28_x86_64.whl"

    def test_wheel_contains_expected_files(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            names = set(zf.namelist())
        assert "agentsview/__init__.py" in names
        assert "agentsview/__main__.py" in names
        assert "agentsview/bin/agentsview" in names
        assert "agentsview-0.15.0.dist-info/METADATA" in names
        assert "agentsview-0.15.0.dist-info/WHEEL" in names
        assert "agentsview-0.15.0.dist-info/entry_points.txt" in names
        assert "agentsview-0.15.0.dist-info/RECORD" in names

    def test_binary_has_executable_permissions(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            info = zf.getinfo("agentsview/bin/agentsview")
        unix_mode = (info.external_attr >> 16) & 0xFFFF
        assert oct(unix_mode & 0o777) == oct(0o755)

    def test_wheel_files_are_compressed(self, tmp_path: Path) -> None:
        whl = build_wheel(b"x" * 10000, tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            for info in zf.infolist():
                assert info.compress_type == zipfile.ZIP_DEFLATED, (
                    f"{info.filename} uses {info.compress_type}, expected DEFLATED"
                )

    def test_binary_has_regular_file_type_bit(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            info = zf.getinfo("agentsview/bin/agentsview")
        # S_IFREG (0o100000) must be set so pip applies permissions
        unix_mode = (info.external_attr >> 16) & 0xFFFF
        assert unix_mode & stat.S_IFREG, "S_IFREG must be set"

    def test_windows_wheel_uses_exe_binary(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "windows_amd64")
        with zipfile.ZipFile(whl) as zf:
            names = set(zf.namelist())
        assert "agentsview/bin/agentsview.exe" in names
        assert "agentsview/bin/agentsview" not in names

    def test_metadata_required_fields(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            metadata = zf.read("agentsview-0.15.0.dist-info/METADATA").decode()
        assert "Metadata-Version: 2.1" in metadata
        assert "Name: agentsview" in metadata
        assert "Version: 0.15.0" in metadata
        assert "Requires-Python: >=3.9" in metadata
        assert "License: MIT" in metadata
        assert "Author: Kenn Software LLC" in metadata

    def test_wheel_file_has_root_is_purelib_false(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            wheel_meta = zf.read("agentsview-0.15.0.dist-info/WHEEL").decode()
        assert "Root-Is-Purelib: false" in wheel_meta
        assert "Generator: agentsview-build-wheels" in wheel_meta

    def test_entry_points_correct(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            ep = zf.read("agentsview-0.15.0.dist-info/entry_points.txt").decode()
        assert "[console_scripts]" in ep
        assert "agentsview = agentsview:main" in ep

    def test_record_contains_hashes(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            record = zf.read("agentsview-0.15.0.dist-info/RECORD").decode()
        # Each non-RECORD entry should have a sha256 hash
        lines = [ln for ln in record.splitlines() if ln.strip()]
        record_line = None
        for line in lines:
            if "RECORD" in line:
                record_line = line
            else:
                assert "sha256=" in line, f"Missing hash in RECORD line: {line}"
        # RECORD itself listed as empty
        assert record_line is not None
        assert record_line.endswith(",,")

    def test_readme_included_in_metadata(self, tmp_path: Path) -> None:
        readme = "# agentsview\nA great tool."
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64", readme=readme)
        with zipfile.ZipFile(whl) as zf:
            metadata = zf.read("agentsview-0.15.0.dist-info/METADATA").decode()
        assert "A great tool." in metadata

    def test_init_py_uses_execvp_on_unix(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "linux_amd64")
        with zipfile.ZipFile(whl) as zf:
            init = zf.read("agentsview/__init__.py").decode()
        assert "os.execvp" in init

    def test_init_py_uses_subprocess_on_windows(self, tmp_path: Path) -> None:
        whl = build_wheel(b"fake", tmp_path, "0.15.0", "windows_amd64")
        with zipfile.ZipFile(whl) as zf:
            init = zf.read("agentsview/__init__.py").decode()
        assert "subprocess.call" in init


# ---------------------------------------------------------------------------
# End-to-end: build_all_wheels
# ---------------------------------------------------------------------------


class TestBuildAllWheels:
    def _make_fake_archives(self, input_dir: Path, version: str) -> None:
        """Create fake release archives for every supported platform."""
        platforms = [
            ("linux_amd64", "agentsview", ".tar.gz"),
            ("linux_arm64", "agentsview", ".tar.gz"),
            ("darwin_amd64", "agentsview", ".tar.gz"),
            ("darwin_arm64", "agentsview", ".tar.gz"),
            ("windows_amd64", "agentsview.exe", ".zip"),
            ("windows_arm64", "agentsview.exe", ".zip"),
        ]
        for platform_key, binary_name, ext in platforms:
            content = f"binary-for-{platform_key}".encode()
            filename = f"agentsview_{version}_{platform_key}{ext}"
            archive_path = input_dir / filename
            if ext == ".tar.gz":
                archive_path.write_bytes(_make_targz(binary_name, content))
            else:
                archive_path.write_bytes(_make_zip(binary_name, content))
        # Also add a SHA256SUMS file that should be skipped
        (input_dir / f"agentsview_{version}_SHA256SUMS").write_text("checksums here")

    def test_produces_a_wheel_per_platform(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "0.15.0")
        wheels = build_all_wheels(input_dir, output_dir, "0.15.0")
        assert len(wheels) == len(PLATFORM_MAP)

    def test_correct_wheel_names(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "0.15.0")
        wheels = build_all_wheels(input_dir, output_dir, "0.15.0")
        names = {w.name for w in wheels}
        expected = {
            "agentsview-0.15.0-py3-none-manylinux_2_28_x86_64.whl",
            "agentsview-0.15.0-py3-none-manylinux_2_28_aarch64.whl",
            "agentsview-0.15.0-py3-none-macosx_11_0_x86_64.whl",
            "agentsview-0.15.0-py3-none-macosx_11_0_arm64.whl",
            "agentsview-0.15.0-py3-none-win_amd64.whl",
            "agentsview-0.15.0-py3-none-win_arm64.whl",
        }
        assert names == expected

    def test_unknown_platforms_skipped(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "0.15.0")
        # Add an unknown platform archive
        unknown = input_dir / "agentsview_0.15.0_freebsd_amd64.tar.gz"
        unknown.write_bytes(_make_targz("agentsview", b"fake"))
        wheels = build_all_wheels(input_dir, output_dir, "0.15.0")
        assert len(wheels) == len(PLATFORM_MAP)  # unknown skipped

    def test_output_dir_created_if_missing(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output" / "nested"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "0.15.0")
        build_all_wheels(input_dir, output_dir, "0.15.0")
        assert output_dir.exists()

    def test_version_mismatch_raises(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "0.15.0")
        with pytest.raises(RuntimeError, match="does not match"):
            build_all_wheels(input_dir, output_dir, "0.16.0")

    def test_all_wheels_are_valid_zips(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "0.15.0")
        wheels = build_all_wheels(input_dir, output_dir, "0.15.0")
        for whl in wheels:
            assert zipfile.is_zipfile(whl), f"{whl.name} is not a valid zip"

    def test_require_all_fails_on_missing_platform(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        # Only create linux_amd64
        (input_dir / "agentsview_1.0.0_linux_amd64.tar.gz").write_bytes(
            _make_targz("agentsview", b"fake")
        )
        with pytest.raises(RuntimeError, match="Missing archives"):
            build_all_wheels(
                input_dir, output_dir, "1.0.0", require_all=True
            )

    def test_require_all_passes_with_all_platforms(self, tmp_path: Path) -> None:
        input_dir = tmp_path / "input"
        output_dir = tmp_path / "output"
        input_dir.mkdir()
        self._make_fake_archives(input_dir, "1.0.0")
        wheels = build_all_wheels(
            input_dir, output_dir, "1.0.0", require_all=True
        )
        assert len(wheels) == len(PLATFORM_MAP)
