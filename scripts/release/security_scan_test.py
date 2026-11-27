#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for scripts/release/helpers/security_scan.sh."""

import json
import os
import stat
import subprocess
import tempfile
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[2]
SECURITY_SCAN_SH = REPO_ROOT / "scripts" / "release" / "helpers" / "security_scan.sh"


class SecurityScanTest(unittest.TestCase):
    def _setup_fixture(self, root: Path, *, gosec_unsuppressed: bool = False,
                       govuln_finding: bool = False, image_medium_cve: bool = False,
                       dns_medium_only: bool = False, config_high: bool = False,
                       policy_violation: bool = False,
                       with_charts: bool = True, with_helm: bool = True,
                       helm_fails: bool = False, drop_repo: str | None = None,
                       drop_dockerfile: str | None = None) -> tuple[Path, Path]:
        work_dir = root / "work"
        artifacts_dir = work_dir / "artifacts"
        src_dir = work_dir / "src"
        bin_dir = root / "mock-bin"
        artifacts_dir.mkdir(parents=True)
        bin_dir.mkdir(parents=True)

        for repo in (
            "gardener-extension-provider-gdc",
            "machine-controller-manager-provider-gdc",
            "cloud-provider-gdc",
        ):
            if repo == drop_repo:
                continue
            repo_path = src_dir / repo
            repo_path.mkdir(parents=True)
            if repo != drop_dockerfile:
                if policy_violation and repo == "cloud-provider-gdc":
                    (repo_path / "Dockerfile").write_text("FROM gcr.io/distroless/static-debian13:nonroot\n", encoding="utf-8")
                else:
                    (repo_path / "Dockerfile").write_text("FROM scratch\n", encoding="utf-8")

        if with_charts and drop_repo != "gardener-extension-provider-gdc":
            charts_root = src_dir / "gardener-extension-provider-gdc" / "charts"
            for rel in (
                "extension-provider",
                "extension-admission/charts/application",
                "extension-admission/charts/runtime",
                "internal/seed-controlplane/charts/cloud-controller-manager",
            ):
                (charts_root / rel).mkdir(parents=True)

        (work_dir / "release-metadata.yaml").write_text(
            "\n".join([
                'gardenerArtifactsVersion: "v0.6.0"',
                'extensionGDCArtifactsVersion: "v0.6.0"',
                'mcmProviderGDCArtifactsVersion: "v0.6.0"',
                'cloudProviderGDCArtifactsVersion: "v0.6.0"',
                'externalDNSArtifactsVersion: "v0.32.0"',
                'candidateRegistry: "ghcr.io/gardener/gardener-gdc-ci"',
            ]) + "\n",
            encoding="utf-8",
        )

        if with_helm:
            helm_script = bin_dir / "helm"
            if helm_fails:
                helm_script.write_text("#!/usr/bin/env bash\nexit 1\n", encoding="utf-8")
            else:
                helm_script.write_text(
                    "#!/usr/bin/env bash\n"
                    "printf 'apiVersion: v1\\nkind: ConfigMap\\nmetadata:\\n  name: mock\\n'\n",
                    encoding="utf-8",
                )
            helm_script.chmod(stat.S_IRWXU)

        # Mock gosec
        gosec_results = (
            [{"ruleId": "G304", "message": {"text": "unsuppressed"}}]
            if gosec_unsuppressed
            else [{"ruleId": "G101", "suppressions": [{"kind": "inSource"}]}]
        )
        gosec_script = bin_dir / "gosec"
        gosec_script.write_text(
            "#!/usr/bin/env bash\n"
            "out=''\n"
            "while [[ $# -gt 0 ]]; do\n"
            "  if [[ \"$1\" == \"-out\" ]]; then out=\"$2\"; shift 2; else shift; fi\n"
            "done\n"
            f"printf '%s\\n' '{json.dumps({'runs': [{'results': gosec_results}]})}' > \"${{out}}\"\n",
            encoding="utf-8",
        )
        gosec_script.chmod(stat.S_IRWXU)

        # Mock govulncheck
        govuln_payload = (
            json.dumps({"finding": {"osv": "GO-2026-9999", "trace": [{"function": "VulnFunc"}]}})
            if govuln_finding
            else json.dumps({"finding": {"osv": "GO-2022-0646", "trace": [{"module": "example.com/uncalled"}]}})
        )
        govuln_script = bin_dir / "govulncheck"
        govuln_script.write_text(
            "#!/usr/bin/env bash\n"
            f"printf '%s\\n' '{govuln_payload}'\n",
            encoding="utf-8",
        )
        govuln_script.chmod(stat.S_IRWXU)

        # Mock trivy
        trivy_script = bin_dir / "trivy"
        trivy_script.write_text(
            "#!/usr/bin/env python3\n"
            "import json, sys\n"
            "args = sys.argv[1:]\n"
            "out = args[args.index('--output') + 1]\n"
            "mode = 'image' if 'image' in args else 'config'\n"
            "target = args[-1]\n"
            "if mode == 'image':\n"
            "    vulns = []\n"
            f"    if {image_medium_cve!r} and 'cloud-controller-manager-gdch' in target:\n"
            "        vulns.append({'VulnerabilityID': 'CVE-2026-0001', 'Severity': 'MEDIUM'})\n"
            f"    if {dns_medium_only!r} and 'external-dnsman2-gdch' in target:\n"
            "        vulns.append({'VulnerabilityID': 'CVE-2026-0002', 'Severity': 'MEDIUM'})\n"
            "    data = {'Results': [{'Vulnerabilities': vulns, 'Secrets': []}]}\n"
            "else:\n"
            "    misconfigs = []\n"
            f"    if {config_high!r}:\n"
            "        misconfigs.append({'ID': 'DS002', 'Status': 'FAIL', 'Severity': 'HIGH'})\n"
            "    data = {'Results': [{'Misconfigurations': misconfigs}]}\n"
            "with open(out, 'w', encoding='utf-8') as f:\n"
            "    json.dump(data, f)\n",
            encoding="utf-8",
        )
        trivy_script.chmod(stat.S_IRWXU)

        return work_dir, bin_dir

    def _run_scan(self, work_dir: Path, bin_dir: Path, extra_env: dict[str, str] | None = None):
        env = os.environ.copy()
        env.update({
            "PATH": f"{bin_dir}:{env['PATH']}",
            "RELEASE_WORK_DIR": str(work_dir),
            "RELEASE_ARTIFACTS_DIR": str(work_dir / "artifacts"),
            "RELEASE_METADATA_FILE": str(work_dir / "release-metadata.yaml"),
        })
        if extra_env:
            env.update(extra_env)
        return subprocess.run(
            ["bash", str(SECURITY_SCAN_SH)],
            env=env,
            capture_output=True,
            text=True,
            check=False,
        )

    def test_passes_clean_artifacts_and_allows_upstream_dns_medium(self):
        with tempfile.TemporaryDirectory() as tmp:
            work_dir, bin_dir = self._setup_fixture(Path(tmp), dns_medium_only=True)
            summary_file = Path(tmp) / "step-summary.md"
            res = self._run_scan(work_dir, bin_dir, {"GITHUB_STEP_SUMMARY": str(summary_file)})
            self.assertEqual(res.returncode, 0, msg=res.stderr)
            self.assertTrue((work_dir / "artifacts" / "gosec-report.sarif").is_file())
            self.assertTrue((work_dir / "artifacts" / "gosec-report-mcm.sarif").is_file())
            self.assertTrue((work_dir / "artifacts" / "gosec-report-ccm.sarif").is_file())
            summary = summary_file.read_text(encoding="utf-8")
            self.assertIn("Automated Security Scan Summary (`PASS`)", summary)
            self.assertIn("external-dnsman2-gdch", summary)

    def test_blocks_on_unsuppressed_sast_or_govulncheck_or_gdc_image_cve_or_config(self):
        for label, kwargs in (
            ("gosec", {"gosec_unsuppressed": True}),
            ("govuln", {"govuln_finding": True}),
            ("gdc_image_medium", {"image_medium_cve": True}),
            ("config_high", {"config_high": True}),
            ("policy_violation", {"policy_violation": True}),
        ):
            with self.subTest(case=label), tempfile.TemporaryDirectory() as tmp:
                work_dir, bin_dir = self._setup_fixture(Path(tmp), **kwargs)
                res = self._run_scan(work_dir, bin_dir)
                self.assertNotEqual(res.returncode, 0)
                self.assertIn("Automated security gate failed", res.stderr)

    def test_emergency_bypass_warns_without_failing_when_disabled(self):
        with tempfile.TemporaryDirectory() as tmp:
            work_dir, bin_dir = self._setup_fixture(Path(tmp), image_medium_cve=True)
            res = self._run_scan(work_dir, bin_dir, {"ENFORCE_SECURITY_GATE": "false"})
            self.assertEqual(res.returncode, 0, msg=res.stderr)
            self.assertIn("::warning::Automated security gate detected", res.stderr)

    def test_fails_closed_when_source_charts_dockerfile_or_helm_missing_or_fails(self):
        for label, kwargs in (
            ("missing_repo", {"drop_repo": "cloud-provider-gdc"}),
            ("missing_charts", {"with_charts": False}),
            ("missing_dockerfile", {"drop_dockerfile": "machine-controller-manager-provider-gdc"}),
            ("helm_render_error", {"helm_fails": True}),
        ):
            with self.subTest(case=label), tempfile.TemporaryDirectory() as tmp:
                work_dir, bin_dir = self._setup_fixture(Path(tmp), **kwargs)
                res = self._run_scan(work_dir, bin_dir)
                self.assertNotEqual(res.returncode, 0, msg=f"{label} unexpectedly passed")
                self.assertIn("Automated security gate failed", res.stderr)

    def test_trivy_image_is_digest_pinned_and_does_not_mount_docker_socket(self):
        content = SECURITY_SCAN_SH.read_text(encoding="utf-8")
        self.assertNotIn("trivy:latest", content)
        self.assertNotIn("/var/run/docker.sock", content)
        self.assertRegex(content, r"ghcr\.io/aquasecurity/trivy:[0-9]+\.[0-9]+\.[0-9]+@sha256:[0-9a-f]{64}")


if __name__ == "__main__":
    unittest.main()
