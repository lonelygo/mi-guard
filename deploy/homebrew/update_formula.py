#!/usr/bin/env python3
"""Promote the published CLI and App archives in the Homebrew tap."""

import re
import sys
from pathlib import Path


REPO = Path(__file__).resolve().parents[2]
FORMULA_PATH = REPO / "Formula" / "mi-guard.rb"
CASK_PATH = REPO / "Casks" / "mi-guard-app.rb"
START_MARKER = "  # BEGIN HOMEBREW_STABLE_METADATA\n"
END_MARKER = "  # END HOMEBREW_STABLE_METADATA\n"
VERSION_RE = re.compile(r"(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)")
SHA256_RE = re.compile(r"[0-9a-f]{64}")
METADATA_RE = re.compile(
    r'  stable do\n'
    r'    url "https://github\.com/lonelygo/mi-guard/releases/download/'
    r'v(?P<version>[0-9]+\.[0-9]+\.[0-9]+)/'
    r'mi-guard-(?P=version)-darwin-arm64\.tar\.gz"\n'
    r'    version "(?P=version)"\n'
    r'    sha256 "(?P<sha256>[0-9a-f]{64})"\n'
    r'    depends_on arch: :arm64\n'
    r'  end\n'
)
CASK_METADATA_RE = re.compile(
    r'  version "(?P<version>[0-9]+\.[0-9]+\.[0-9]+)"\n'
    r'  sha256 "(?P<sha256>[0-9a-f]{64})"\n'
)


class PromotionError(Exception):
    pass


def parse_version(value):
    match = VERSION_RE.fullmatch(value)
    if match is None:
        raise PromotionError("VERSION must be X.Y.Z without a leading v or leading zeroes")
    return tuple(int(part) for part in match.groups())


def metadata(version, sha256):
    url = (
        "https://github.com/lonelygo/mi-guard/releases/download/"
        f"v{version}/mi-guard-{version}-darwin-arm64.tar.gz"
    )
    return (
        "  stable do\n"
        f'    url "{url}"\n'
        f'    version "{version}"\n'
        f'    sha256 "{sha256}"\n'
        "    depends_on arch: :arm64\n"
        "  end\n"
    )


def promoted_contents(path, pattern, block, version, sha256):
    requested_version = parse_version(version)
    if SHA256_RE.fullmatch(sha256) is None:
        raise PromotionError("SHA256 must be exactly 64 lowercase hexadecimal characters")

    try:
        content = path.read_text(encoding="utf-8")
    except OSError as error:
        raise PromotionError(f"cannot read {path}: {error}") from error

    if content.count(START_MARKER) != 1 or content.count(END_MARKER) != 1:
        raise PromotionError(f"{path.name} does not contain exactly one stable metadata marker block")
    if content.index(END_MARKER) < content.index(START_MARKER):
        raise PromotionError(f"{path.name} stable metadata markers are out of order")

    before, remainder = content.split(START_MARKER, 1)
    current, after = remainder.split(END_MARKER, 1)
    if current:
        match = pattern.fullmatch(current)
        if match is None:
            raise PromotionError(f"{path.name} stable metadata block has an unexpected structure")

        current_version_text = match.group("version")
        current_version = parse_version(current_version_text)
        current_sha256 = match.group("sha256")
        if requested_version < current_version:
            raise PromotionError(
                f"refusing to downgrade stable metadata from {current_version_text} to {version}"
            )
        if requested_version == current_version:
            if sha256 != current_sha256:
                raise PromotionError(
                    f"refusing to replace the checksum for already-promoted version {version}"
                )
            return None

    return before + START_MARKER + block + END_MARKER + after


def promote(version, sha256, app_sha256=None):
    formula = promoted_contents(
        FORMULA_PATH, METADATA_RE, metadata(version, sha256), version, sha256,
    )
    updates = [(FORMULA_PATH, formula)]
    if app_sha256 is not None:
        block = f'  version "{version}"\n  sha256 "{app_sha256}"\n'
        cask = promoted_contents(CASK_PATH, CASK_METADATA_RE, block, version, app_sha256)
        updates.append((CASK_PATH, cask))

    for path, content in updates:
        if content is not None:
            try:
                path.write_text(content, encoding="utf-8")
            except OSError as error:
                raise PromotionError(f"cannot write {path}: {error}") from error
            print(f"Promoted {path.name} to {version}")


def main(argv):
    # Releases up to v0.2.0 call this script with only the CLI checksum.
    if len(argv) not in (2, 3):
        raise PromotionError("usage: update_formula.py VERSION CLI_SHA256 [APP_SHA256]")
    promote(*argv)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except PromotionError as error:
        print(f"error: {error}", file=sys.stderr)
        sys.exit(2)
