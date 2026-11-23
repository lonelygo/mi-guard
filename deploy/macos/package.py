#!/usr/bin/env python3
"""Build and package the native macOS arm64 release."""

from __future__ import annotations

import argparse
import gzip
import hashlib
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile


VERSION_RE = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)")
TARGET_RE = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)")


def validate(version: str, target: str) -> None:
    if not VERSION_RE.fullmatch(version):
        raise ValueError("VERSION must be an unprefixed MAJOR.MINOR.PATCH release version")
    if not TARGET_RE.fullmatch(target) or tuple(map(int, target.split("."))) < (12, 0):
        raise ValueError("the macOS deployment target must be 12.0 or newer")
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        raise RuntimeError("release packaging requires a native Apple Silicon macOS host")


def output(*arguments: str, cwd: Path) -> str:
    return subprocess.check_output(arguments, cwd=cwd, text=True, stderr=subprocess.STDOUT)


def verify_binary(binary: Path, target: str, repo: Path) -> None:
    architectures = output("lipo", "-archs", str(binary), cwd=repo).split()
    if architectures != ["arm64"]:
        raise RuntimeError(f"{binary.name} has unexpected architectures: {' '.join(architectures)}")
    load_commands = output("xcrun", "vtool", "-show-build", str(binary), cwd=repo)
    match = re.search(r"^\s*minos\s+(\S+)\s*$", load_commands, flags=re.MULTILINE)
    if match is None or match.group(1) != target:
        actual = match.group(1) if match else "missing"
        raise RuntimeError(f"{binary.name} has deployment target {actual}, expected {target}")


def add_tree(archive: tarfile.TarFile, root: Path) -> None:
    for path in [root, *sorted(root.rglob("*"), key=lambda item: item.as_posix())]:
        info = archive.gettarinfo(str(path), arcname=path.relative_to(root.parent).as_posix())
        info.uid = info.gid = info.mtime = 0
        info.uname = info.gname = "root"
        info.mode = 0o755 if info.isdir() or path.name in {"mihomo-guard", "mi-guard-notify"} else 0o644
        if info.isfile():
            with path.open("rb") as source:
                archive.addfile(info, source)
        else:
            archive.addfile(info)


def write_archive(root: Path, destination: Path) -> None:
    temporary = destination.with_suffix(destination.suffix + ".tmp")
    with temporary.open("wb") as output_file:
        with gzip.GzipFile(filename="", mode="wb", fileobj=output_file, compresslevel=9, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                add_tree(archive, root)
    os.replace(temporary, destination)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--deployment-target", default="12.0")
    args = parser.parse_args()
    validate(args.version, args.deployment_target)

    repo = Path(__file__).resolve().parents[2]
    release_dir = repo / "build/release"
    release_dir.mkdir(parents=True, exist_ok=True)
    root_name = f"mi-guard-{args.version}-darwin-arm64"
    archive = release_dir / f"{root_name}.tar.gz"

    with tempfile.TemporaryDirectory(prefix=".mi-guard-package-", dir=release_dir) as temporary:
        scratch = Path(temporary)
        build_dir = scratch / "build"
        relative_build_dir = build_dir.relative_to(repo)
        package_root = scratch / root_name
        subprocess.run(
            [
                "make",
                "cli",
                "app",
                f"BUILD_DIR={relative_build_dir}",
                f"RELEASE_VERSION={args.version}",
                f"MACOSX_DEPLOYMENT_TARGET={args.deployment_target}",
            ],
            cwd=repo,
            check=True,
        )

        cli = package_root / "bin/mihomo-guard"
        app = package_root / "libexec/mi-guard.app"
        share = package_root / "share/mi-guard"
        cli.parent.mkdir(parents=True)
        share.mkdir(parents=True)
        shutil.copyfile(build_dir / "mihomo-guard", cli)
        shutil.copytree(build_dir / "mi-guard.app", app)
        shutil.copyfile(repo / "deploy/mihomo-overlay.yaml", share / "mihomo-overlay.yaml")
        shutil.copyfile(repo / "internal/config/mihomo-guard-default.yaml", share / "mihomo-guard-default.yaml")
        shutil.copyfile(repo / "LICENSE", package_root / "LICENSE")
        shutil.copyfile(repo / "README.md", package_root / "README.md")

        helper = app / "Contents/MacOS/mi-guard-notify"
        plist = app / "Contents/Info.plist"
        verify_binary(cli, args.deployment_target, repo)
        verify_binary(helper, args.deployment_target, repo)
        for key, expected in (
            ("CFBundleShortVersionString", args.version),
            ("CFBundleVersion", args.version),
            ("LSMinimumSystemVersion", args.deployment_target),
        ):
            actual = output("plutil", "-extract", key, "raw", "-o", "-", str(plist), cwd=repo).strip()
            if actual != expected:
                raise RuntimeError(f"{key} is {actual}, expected {expected}")
        subprocess.run(["codesign", "--verify", "--deep", "--strict", "--verbose=2", str(app)], check=True)
        write_archive(package_root, archive)

    digest = hashlib.sha256(archive.read_bytes()).hexdigest()
    sums = release_dir / "SHA256SUMS"
    temporary_sums = release_dir / ".SHA256SUMS.tmp"
    temporary_sums.write_text(f"{digest}  {archive.name}\n", encoding="utf-8")
    os.replace(temporary_sums, sums)
    print(f"created {archive.relative_to(repo)}")
    print(f"created {sums.relative_to(repo)}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError, ValueError, subprocess.CalledProcessError) as error:
        print(f"package: {error}", file=sys.stderr)
        raise SystemExit(1)
