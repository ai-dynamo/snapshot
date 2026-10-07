#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Collect the notices for the exact vendored crates and pinned Rust runtime."""

import argparse
from html.parser import HTMLParser
from pathlib import Path
import tomllib


class NoticeText(HTMLParser):
    """Keep the text of Rust's generated copyright inventory, including licenses."""

    def __init__(self):
        super().__init__()
        self.parts = []

    def handle_data(self, data):
        self.parts.append(data)


def read_notice(path):
    text = path.read_text(encoding="utf-8")
    if path.suffix == ".html":
        parser = NoticeText()
        parser.feed(text)
        text = "".join(parser.parts)
    if not text.strip():
        raise ValueError(f"empty notice: {path}")
    return text.rstrip() + "\n"


def crate_notices(directory, package):
    files = set()
    licenses = set()
    for path in directory.rglob("*"):
        if not path.is_file():
            continue
        name = path.name.lower()
        if name.startswith(("license", "licence", "copying")) or any(
            part.lower() in ("licenses", "licences")
            for part in path.relative_to(directory).parts[:-1]
        ):
            licenses.add(path)
        elif name.startswith(("notice", "copyright", "authors")):
            files.add(path)
    if "license-file" in package:
        path = (directory / package["license-file"]).resolve()
        if not path.is_relative_to(directory.resolve()) or not path.is_file():
            raise ValueError(f"invalid license-file for {package['name']}: {path}")
        licenses.add(path)
    # This crate publishes its complete MIT grant and copyright in AUTHORS, rather
    # than a LICENSE file. Keep the exception tied to the version that was audited.
    if (package["name"], package["version"]) == ("r-efi", "6.0.0"):
        licenses.add(directory / "AUTHORS")
    if not licenses:
        raise ValueError(f"no license text for {package['name']} {package['version']}")
    return sorted(files | licenses)


def render_notices(root):
    lock = tomllib.loads((root / "Cargo.lock").read_text(encoding="utf-8"))
    expected = {(p["name"], p["version"]) for p in lock["package"] if "source" in p}
    crates = {}
    for manifest in sorted((root / "vendor").glob("*/Cargo.toml")):
        package = tomllib.loads(manifest.read_text(encoding="utf-8"))["package"]
        identity = (package["name"], package["version"])
        if identity in crates:
            raise ValueError(f"duplicate vendored crate: {identity}")
        crates[identity] = (manifest.parent, package)
    if crates.keys() != expected:
        raise ValueError(
            f"Cargo.lock/vendor mismatch: missing={sorted(expected - crates.keys())}, "
            f"extra={sorted(crates.keys() - expected)}"
        )

    sections = [
        "\nRUST COMPONENT NOTICES\n"
        "Includes every vendored crate, including build/test and other-target sources.\n"
        "These are upstream license expressions and notices, not a claim that every\n"
        "vendored component is linked into the Linux runtime.\n"
    ]
    for (name, version), (directory, package) in sorted(crates.items()):
        sections.append(f"\n{'=' * 80}\nCOMPONENT: {name} ({version})\n")
        sections.append(f"Declared license: {package.get('license', 'see license-file')}\n")
        for path in crate_notices(directory, package):
            sections.append(f"\nFILE: {path.relative_to(directory)}\n{read_notice(path)}")

    version = (root / "toolchain-version.txt").read_text(encoding="utf-8").strip()
    if not version:
        raise ValueError("missing Rust toolchain version")
    sections.append(f"\n{'=' * 80}\nCOMPONENT: Rust standard library ({version})\n")
    notices = root / "toolchain-notices"
    # Rust's runtime inventory names its own third-party components and contains
    # their copyright notices. Include the accompanying full license texts too.
    paths = [notices / "COPYRIGHT-library.html"]
    license_files = sorted((notices / "licenses").glob("*.txt"))
    if not license_files:
        raise ValueError("missing Rust toolchain license texts")
    for path in paths + license_files:
        sections.append(f"\nFILE: {path.relative_to(notices)}\n{read_notice(path)}")
    return "".join(sections)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("sources", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    try:
        output = render_notices(args.sources.resolve())
        args.output.write_text(output, encoding="utf-8")
    except (OSError, ValueError, KeyError) as error:
        parser.exit(1, f"Rust attribution failed: {error}\n")


if __name__ == "__main__":
    main()
