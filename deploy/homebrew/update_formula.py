#!/usr/bin/env python3
"""Promote a published mi-guard archive into the Homebrew formula."""

import re
import sys
from pathlib import Path


FORMULA_PATH = Path(__file__).resolve().parents[2] / "Formula" / "mi-guard.rb"
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


def promote(version, sha256):
    requested_version = parse_version(version)
    if SHA256_RE.fullmatch(sha256) is None:
        raise PromotionError("SHA256 must be exactly 64 lowercase hexadecimal characters")

    try:
        formula = FORMULA_PATH.read_text(encoding="utf-8")
    except OSError as error:
        raise PromotionError(f"cannot read {FORMULA_PATH}: {error}") from error

    if formula.count(START_MARKER) != 1 or formula.count(END_MARKER) != 1:
        raise PromotionError("formula does not contain exactly one stable metadata marker block")
    if formula.index(END_MARKER) < formula.index(START_MARKER):
        raise PromotionError("formula stable metadata markers are out of order")

    before, remainder = formula.split(START_MARKER, 1)
    current, after = remainder.split(END_MARKER, 1)
    if current:
        match = METADATA_RE.fullmatch(current)
        if match is None:
            raise PromotionError("formula stable metadata block has an unexpected structure")

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
            print(f"Formula is already at mi-guard {version} with the requested checksum")
            return

    updated = before + START_MARKER + metadata(version, sha256) + END_MARKER + after
    try:
        FORMULA_PATH.write_text(updated, encoding="utf-8")
    except OSError as error:
        raise PromotionError(f"cannot write {FORMULA_PATH}: {error}") from error
    print(f"Promoted Homebrew stable metadata to mi-guard {version}")


def main(argv):
    if len(argv) != 2:
        raise PromotionError("usage: update_formula.py VERSION SHA256")
    promote(argv[0], argv[1])


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except PromotionError as error:
        print(f"error: {error}", file=sys.stderr)
        sys.exit(2)
