# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Contract tests for the image's Rust notice inventory."""

from pathlib import Path
import tempfile
import unittest

from rust_licenses import render_notices


class RustNoticesTest(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.write("toolchain-version.txt", "rustc 1.95.0\n")
        self.write("toolchain-notices/COPYRIGHT-library.html",
                   "<h1>Rust runtime</h1>\n<p>Copyright A &amp; B</p>\n")
        self.write("toolchain-notices/licenses/MIT.txt", "Rust license text\n")

    def write(self, name, content):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")

    def crates(self, *identities):
        lock = 'version = 4\n[[package]]\nname = "workspace"\nversion = "0.1.0"\n'
        for name, version in identities:
            package = f'name = "{name}"\nversion = "{version}"\n'
            lock += f'[[package]]\n{package}source = "registry+https://example.invalid"\n'
            self.write(f"vendor/{name}-{version}/Cargo.toml",
                       f'[package]\n{package}license = "MIT OR Apache-2.0"\n')
        self.write("Cargo.lock", lock)

    def test_each_component_has_its_own_notices_and_deterministic_order(self):
        self.crates(("second", "2.0.0"), ("first", "1.0.0"))
        self.write("vendor/second-2.0.0/LICENSE", "Second license\n")
        self.write("vendor/first-1.0.0/LICENSE-MIT", "First MIT\n")
        self.write("vendor/first-1.0.0/LICENSE-APACHE", "First Apache\n")
        self.write("vendor/first-1.0.0/NOTICE", "First notice\n")
        output = render_notices(self.root)
        self.assertLess(output.index("COMPONENT: first"), output.index("COMPONENT: second"))
        for text in ("First MIT", "First Apache", "First notice", "Second license",
                     "Copyright A & B", "Rust license text", "rustc 1.95.0"):
            self.assertIn(text, output)
        self.assertNotIn("<p>", output)
        self.assertEqual(output, render_notices(self.root))

    def test_license_file_and_license_directory_are_included(self):
        self.crates(("custom", "1.0.0"))
        with (self.root / "vendor/custom-1.0.0/Cargo.toml").open("a") as file:
            file.write('license-file = "terms.txt"\n')
        self.write("vendor/custom-1.0.0/terms.txt", "Custom terms\n")
        self.write("vendor/custom-1.0.0/LICENSES/ISC.txt", "ISC terms\n")
        output = render_notices(self.root)
        self.assertIn("Custom terms", output)
        self.assertIn("ISC terms", output)

    def test_missing_or_empty_license_is_an_error(self):
        self.crates(("missing", "1.0.0"))
        self.write("vendor/missing-1.0.0/AUTHORS", "Names alone are not a license\n")
        with self.assertRaisesRegex(ValueError, "no license text for missing"):
            render_notices(self.root)
        self.write("vendor/missing-1.0.0/LICENSE", " \n")
        with self.assertRaisesRegex(ValueError, "empty notice"):
            render_notices(self.root)

    def test_vendor_inventory_must_match_lock(self):
        self.crates(("absent", "1.0.0"))
        (self.root / "vendor/absent-1.0.0/Cargo.toml").unlink()
        with self.assertRaisesRegex(ValueError, r"Cargo.lock/vendor mismatch: missing=\[\('absent', '1.0.0'\)\]"):
            render_notices(self.root)

    def test_license_file_cannot_escape_crate(self):
        self.crates(("escape", "1.0.0"))
        with (self.root / "vendor/escape-1.0.0/Cargo.toml").open("a") as file:
            file.write('license-file = "../../toolchain-version.txt"\n')
        with self.assertRaisesRegex(ValueError, "invalid license-file"):
            render_notices(self.root)

    def test_missing_rust_runtime_inventory_is_an_error(self):
        self.crates(("crate", "1.0.0"))
        self.write("vendor/crate-1.0.0/LICENSE", "License\n")
        (self.root / "toolchain-notices/COPYRIGHT-library.html").unlink()
        with self.assertRaises(FileNotFoundError):
            render_notices(self.root)

    def test_nonstandard_license_location_requires_the_audited_version(self):
        self.crates(("r-efi", "6.0.0"))
        self.write("vendor/r-efi-6.0.0/AUTHORS", "Audited MIT grant and copyrights\n")
        self.assertIn("Audited MIT grant", render_notices(self.root))
        self.crates(("r-efi", "6.0.1"))
        (self.root / "vendor/r-efi-6.0.0/Cargo.toml").unlink()
        self.write("vendor/r-efi-6.0.1/AUTHORS", "Needs a new source audit\n")
        with self.assertRaisesRegex(ValueError, "no license text for r-efi 6.0.1"):
            render_notices(self.root)


if __name__ == "__main__":
    unittest.main()
