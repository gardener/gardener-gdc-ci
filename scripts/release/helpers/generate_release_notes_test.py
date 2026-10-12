#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for generate_release_notes.py."""

import os
import subprocess
import tempfile
import unittest

import generate_release_notes as grn


class GenerateReleaseNotesTest(unittest.TestCase):

    def test_parse_fenced_release_notes_categories_and_none(self):
        pr_body = """
**What this PR does**:
Adds dual-zone BackupBucket support.

```feature user
Support dual-zone BackupBucket reconciliation on GDC.
```

```bugfix operator
Fix transient retry in VM controller.
```
"""
        notes, saw_none = grn.parse_fenced_release_notes(pr_body)
        self.assertFalse(saw_none)
        self.assertEqual(
            notes,
            [
                ("feature", "USER", "Support dual-zone BackupBucket reconciliation on GDC."),
                ("bugfix", "OPERATOR", "Fix transient retry in VM controller."),
            ],
        )

        renovate_body = """
**Release note**:
```other dependency
NONE
```
"""
        notes_none, saw_none2 = grn.parse_fenced_release_notes(renovate_body)
        self.assertTrue(saw_none2)
        self.assertEqual(notes_none, [])

    def test_format_entry_only_mentions_valid_github_logins(self):
        entry_login = grn.format_entry(
            "OPERATOR",
            "Initial commit",
            "dilipkumar2k6",
            1,
            "gardener/machine-controller-manager-provider-gdc",
        )
        self.assertEqual(
            entry_login,
            "- `[OPERATOR]` Initial commit by @dilipkumar2k6 [[#1](https://github.com/gardener/machine-controller-manager-provider-gdc/pull/1)]",
        )

        entry_display_name = grn.format_entry(
            "OPERATOR",
            "Initial commit for machine-controller-manager-provider-gdc",
            "Dilip Kumar",
            None,
            "gardener/machine-controller-manager-provider-gdc",
        )
        self.assertEqual(
            entry_display_name,
            "- `[OPERATOR]` Initial commit for machine-controller-manager-provider-gdc by Dilip Kumar",
        )
        self.assertNotIn("@Dilip", entry_display_name)

    def test_end_to_end_git_repo_and_subcomponent_cascading(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            mcm_dir = os.path.join(tmpdir, "mcm")
            ext_dir = os.path.join(tmpdir, "ext")
            os.makedirs(mcm_dir)
            os.makedirs(ext_dir)

            for d in (mcm_dir, ext_dir):
                subprocess.check_call(["git", "-C", d, "init", "-q"])
                subprocess.check_call(["git", "-C", d, "config", "user.name", "dev-author"])
                subprocess.check_call(
                    ["git", "-C", d, "config", "user.email", "dev@example.com"]
                )
                subprocess.check_call(["git", "-C", d, "config", "commit.gpgsign", "false"])
                subprocess.check_call(["git", "-C", d, "config", "tag.gpgSign", "false"])
                subprocess.check_call(["git", "-C", d, "config", "tag.forceSignAnnotated", "false"])

            # Initial release v0.5.3 in MCM
            with open(os.path.join(mcm_dir, "VERSION"), "w", encoding="utf-8") as f:
                f.write("v0.5.3\n")
            subprocess.check_call(["git", "-C", mcm_dir, "add", "VERSION"])
            subprocess.check_call(["git", "-C", mcm_dir, "commit", "-q", "-m", "release v0.5.3"])
            subprocess.check_call(["git", "-C", mcm_dir, "tag", "v0.5.3"])

            # Bump and add a bugfix commit + a Renovate NONE commit in MCM
            with open(os.path.join(mcm_dir, "VERSION"), "w", encoding="utf-8") as f:
                f.write("v0.5.4-dev\n")
            subprocess.check_call(["git", "-C", mcm_dir, "add", "VERSION"])
            subprocess.check_call(
                ["git", "-C", mcm_dir, "commit", "-q", "-m", "next version: v0.5.4-dev"]
            )

            with open(os.path.join(mcm_dir, "driver.go"), "w", encoding="utf-8") as f:
                f.write("package driver\n")
            subprocess.check_call(["git", "-C", mcm_dir, "add", "driver.go"])
            subprocess.check_call(
                [
                    "git",
                    "-C",
                    mcm_dir,
                    "commit",
                    "-q",
                    "-m",
                    "fix: handle boot disk attachment timeout (#33)\n\n```bugfix operator\nFix VM boot disk attachment retry on transient GDC API timeout.\n```",
                ]
            )

            # Initial release v0.5.3 in EXT
            with open(os.path.join(ext_dir, "VERSION"), "w", encoding="utf-8") as f:
                f.write("v0.5.3\n")
            subprocess.check_call(["git", "-C", ext_dir, "add", "VERSION"])
            subprocess.check_call(["git", "-C", ext_dir, "commit", "-q", "-m", "release v0.5.3"])
            subprocess.check_call(["git", "-C", ext_dir, "tag", "v0.5.3"])

            # Feature commit in EXT
            with open(os.path.join(ext_dir, "actuator.go"), "w", encoding="utf-8") as f:
                f.write("package actuator\n")
            subprocess.check_call(["git", "-C", ext_dir, "add", "actuator.go"])
            subprocess.check_call(
                [
                    "git",
                    "-C",
                    ext_dir,
                    "commit",
                    "-q",
                    "-m",
                    "feat: dual-zone backupbucket (#42)\n\n```feature user\nSupport dual-zone BackupBucket reconciliation on GDC.\n```",
                ]
            )

            mcm_sub_out = os.path.join(tmpdir, "mcm-sub.md")
            mcm_full_out = os.path.join(tmpdir, "mcm-full.md")
            ext_full_out = os.path.join(tmpdir, "ext-full.md")

            mcm_cat = grn.extract_repository_notes(
                repo_dir=mcm_dir,
                repo_slug="gardener/machine-controller-manager-provider-gdc",
                from_tag="v0.5.3",
                gh_token=None,
                use_github_api=False,
            )
            mcm_section = grn.render_component_section(
                "github.com/gardener/machine-controller-manager-provider-gdc",
                "v0.5.4",
                mcm_cat,
            )
            with open(mcm_sub_out, "w", encoding="utf-8") as f:
                f.write(mcm_section)

            self.assertIn(
                "# [github.com/gardener/machine-controller-manager-provider-gdc:v0.5.4]",
                mcm_section,
            )
            self.assertIn("## 🐛 Bug Fixes", mcm_section)
            self.assertIn(
                "- `[OPERATOR]` Fix VM boot disk attachment retry on transient GDC API timeout. by @dev-author [[#33](https://github.com/gardener/machine-controller-manager-provider-gdc/pull/33)]",
                mcm_section,
            )

            ext_cat = grn.extract_repository_notes(
                repo_dir=ext_dir,
                repo_slug="gardener/gardener-extension-provider-gdc",
                from_tag="v0.5.3",
                gh_token=None,
                image_updates=[
                    ("machine-controller-manager-provider-gdch", "v0.5.3", "v0.5.4"),
                    ("cloud-controller-manager", "v0.5.3", "v0.5.3"),
                ],
                use_github_api=False,
            )
            ext_primary = grn.render_component_section(
                "github.com/gardener/gardener-extension-provider-gdc",
                "v0.5.4",
                ext_cat,
            )
            ext_full = grn.render_full_release_notes(
                primary_section=ext_primary,
                subcomponent_sections=[mcm_section],
                helm_charts=[
                    (
                        "gardener-extension-provider-gdch-helm",
                        "europe-docker.pkg.dev/gardener-project/releases/gardener/gardener-gdc-ci/charts/gardener-extension-provider-gdch-helm:0.5.4",
                    )
                ],
                oci_images=[
                    (
                        "gardener-extension-provider-gdch",
                        "europe-docker.pkg.dev/gardener-project/releases/gardener/gardener-gdc-ci/gardener-extension-provider-gdch:v0.5.4",
                    )
                ],
            )

            self.assertIn("# [github.com/gardener/gardener-extension-provider-gdc:v0.5.4]", ext_full)
            self.assertIn("## ✨ New Features", ext_full)
            self.assertIn("`machine-controller-manager-provider-gdch`: `v0.5.3` -> `v0.5.4`", ext_full)
            self.assertNotIn("`cloud-controller-manager`:", ext_full)
            self.assertIn(
                "# [github.com/gardener/machine-controller-manager-provider-gdc:v0.5.4]",
                ext_full,
            )
            self.assertIn("## Helm Charts", ext_full)
            self.assertIn("## Container (OCI) Images", ext_full)


if __name__ == "__main__":
    unittest.main()
