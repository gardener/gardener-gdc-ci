#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for scripts/release/helpers/policy_check.py."""

from __future__ import annotations

import shutil
import sys
import tempfile
import unittest
from pathlib import Path

HELPERS_DIR = Path(__file__).resolve().parent
REPO_ROOT = HELPERS_DIR.parents[2]
sys.path.insert(0, str(HELPERS_DIR))

import policy_check  # noqa: E402


DIGEST_A = "a" * 64
DIGEST_B = "b" * 64


def _write(path: Path, content: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


class PolicyCheckTest(unittest.TestCase):
    def _build_clean_src_root(self, root: Path) -> Path:
        src = root / "src"
        for repo in (
            "gardener-extension-provider-gdc",
            "machine-controller-manager-provider-gdc",
            "cloud-provider-gdc",
        ):
            r = src / repo
            _write(
                r / "Dockerfile",
                f"FROM golang:1.27.4@sha256:{DIGEST_A} AS builder\n"
                f"FROM gcr.io/distroless/static-debian13:nonroot@sha256:{DIGEST_B} AS base\n"
                "USER 65532:65532\n"
                "FROM base AS app\n"
                'ENTRYPOINT ["/app"]\n',
            )
            _write(r / "Makefile", "GOIMPORTS_VERSION := v0.53.0\n\tgo install golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VERSION)\n")
            _write(
                r / ".github/workflows/integration-test.yaml",
                "on:\n  workflow_dispatch:\n    inputs:\n      pr_number:\n      head_sha:\n"
                "steps:\n  - run: git show refs/remotes/origin/main:scripts/ci-common.sh > /tmp/ci-common.sh\n",
            )
            _write(r / "scripts/ci-common.sh", "#!/usr/bin/env bash\ncurl --cacert ca.pem --proto '=https' https://example.invalid\n")
            _write(
                r / "gdc/pkg/auth/jwt.go",
                "package auth\n"
                "var claims = jwt.RegisteredClaims{ID: jti, NotBefore: nbf, ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}\n",
            )

        ext = src / "gardener-extension-provider-gdc"
        _write(ext / "pkg/gdc/serviceaccount.go", "package gdc\nfunc validateTokenURI(rawURI string) error { return nil }\n")
        _write(
            ext / "charts/internal/machineclass/Chart.yaml",
            "apiVersion: v2\nname: machineclass\nversion: 0.1.0\n",
        )
        _write(
            ext / "charts/internal/machineclass/templates/machineclass.yaml",
            "{{- range $index, $machineClass := .Values.machineClasses }}\n"
            "apiVersion: machine.sapcloud.io/v1alpha1\nkind: MachineClass\nproviderSpec:\n"
            "  addressPoolName: {{ $machineClass.addressPoolName | quote }}\n"
            "  subnetName: {{ $machineClass.subnetName | quote }}\n"
            "  project: {{ $machineClass.project | quote }}\n"
            "  orgClusterURL: {{ $machineClass.orgClusterURL | quote }}\n"
            "  registryURL: {{ $machineClass.registryURL | quote }}\n"
            "  caData: {{ $machineClass.caData | quote }}\n"
            "  machineType: {{ $machineClass.machineType | quote }}\n"
            "{{- if $machineClass.annotations }}\n  annotations:\n{{ toYaml $machineClass.annotations | indent 4 }}\n{{- end }}\n"
            "{{- end }}\n",
        )
        _write(
            ext / "charts/internal/cloud-provider-config/Chart.yaml",
            "apiVersion: v2\nname: cloud-provider-config\nversion: 0.1.0\n",
        )
        _write(
            ext / "charts/internal/cloud-provider-config/templates/cloud-provider-config.yaml",
            "apiVersion: v1\nkind: ConfigMap\ndata:\n  cloudprovider.conf: |\n    [Global]\n"
            "    project={{ .Values.project | quote }}\n"
            "    region={{ .Values.region | quote }}\n"
            "    global-management-server-url={{ .Values.globalManagementServerURL | quote }}\n"
            "    ca={{ .Values.caData | quote }}\n"
            "    {{- range $index, $zone := .Values.zones }}\n"
            "    [zone_{{ $index }}]\n"
            "    name={{ $zone.name | quote }}\n"
            "    management-api={{ $zone.managementAPI | quote }}\n"
            "    {{- end }}\n",
        )
        _write(
            ext / "charts/internal/seed-controlplane/charts/csi-driver-controller/Chart.yaml",
            "apiVersion: v2\nname: csi-driver-controller\nversion: 0.1.0\n",
        )
        _write(
            ext / "charts/internal/seed-controlplane/charts/csi-driver-controller/templates/kvcsi-driver-config.yaml",
            "apiVersion: v1\nkind: ConfigMap\ndata:\n"
            "  infraClusterLabels: {{ .Values.infraClusterLabels | quote }}\n"
            "  infraClusterNamespace: {{ .Values.infraClusterNamespace | quote }}\n",
        )
        _write(
            ext / "gdc-sa-auth-plugin/terraform/main.tf",
            'resource "local_file" "key_json" {\n  filename = "k.json"\n  file_permission = "0600"\n}\n',
        )
        _write(ext / ".ocm/base-component.yaml", "name: github.com/gardener/gardener-extension-provider-gdc\n")

        mcm = src / "machine-controller-manager-provider-gdc"
        _write(mcm / "pkg/spi/spi.go", "package spi\nfunc validateTokenURI(rawURI string) error { return nil }\n")
        _write(
            mcm / "pkg/provider/core.go",
            "package provider\n"
            "func renderCAInitScript(r, c string) string { return shellQuote(r) }\n"
            "func ListMachines() { if len(providerSpec.Labels) == 0 { return } }\n",
        )

        ccm = src / "cloud-provider-gdc"
        _write(ccm / "pkg/cloudprovider/gdc/config.go", "package gdc\nfunc validateTokenURI(rawURI string) error { return nil }\n")
        return src

    def test_clean_fixture_has_zero_blocking_findings(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = self._build_clean_src_root(Path(tmp))
            findings = policy_check.evaluate_repositories(src_root=src, ci_repo=REPO_ROOT)
            blocking = [f for f in findings if f.severity == "BLOCKING"]
            self.assertEqual(blocking, [])

    def test_self_repo_passes_cleanly(self):
        findings = policy_check.evaluate_repositories(ci_repo=REPO_ROOT)
        blocking = [f for f in findings if f.severity == "BLOCKING"]
        self.assertEqual(blocking, [])

    def test_catches_unpinned_dockerfile_and_root_user(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = self._build_clean_src_root(Path(tmp))
            _write(src / "cloud-provider-gdc/Dockerfile", "FROM gcr.io/distroless/static-debian13:nonroot\n")
            findings = policy_check.evaluate_repositories(src_root=src, run_helm_canary=False)
            rules = [f.rule_id for f in findings if f.severity == "BLOCKING"]
            self.assertEqual(rules.count("DOCKER-PIN-001"), 2)

    def test_catches_tls_bypass_in_shell_and_go(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = self._build_clean_src_root(Path(tmp))
            _write(src / "cloud-provider-gdc/scripts/ci-common.sh", "wget --no-check-certificate https://x\ncurl -sSLk https://y\n")
            _write(src / "cloud-provider-gdc/pkg/net.go", "package pkg\nvar t = tls.Config{InsecureSkipVerify: true}\n")
            _write(src / "cloud-provider-gdc/pkg/net_test.go", "package pkg\nvar t = tls.Config{InsecureSkipVerify: true}\n")
            findings = policy_check.evaluate_repositories(src_root=src, run_helm_canary=False)
            tls_hits = [f for f in findings if f.rule_id == "TLS-BYPASS-001"]
            self.assertEqual(len(tls_hits), 3)
            self.assertTrue(all("_test.go" not in f.path for f in tls_hits))

    def test_catches_go_install_latest_and_long_jwt_ttl(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = self._build_clean_src_root(Path(tmp))
            _write(src / "cloud-provider-gdc/Makefile", "\tgo install golang.org/x/tools/cmd/goimports@latest\n")
            _write(
                src / "cloud-provider-gdc/gdc/pkg/auth/jwt.go",
                "package auth\nvar c = jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour))}\n",
            )
            findings = policy_check.evaluate_repositories(src_root=src, run_helm_canary=False)
            rules = {f.rule_id for f in findings if f.severity == "BLOCKING"}
            self.assertIn("GO-TOOL-PIN-001", rules)
            self.assertIn("JWT-TTL-001", rules)

    def test_catches_unquoted_helm_templates_and_canary_breakout(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = self._build_clean_src_root(Path(tmp))
            _write(
                src / "gardener-extension-provider-gdc/charts/internal/cloud-provider-config/templates/cloud-provider-config.yaml",
                'apiVersion: v1\nkind: ConfigMap\ndata:\n  cloudprovider.conf: |\n    [Global]\n'
                '    project="{{ .Values.project }}"\n'
                "    region={{ .Values.region | quote }}\n"
                "    global-management-server-url={{ .Values.globalManagementServerURL | quote }}\n"
                "    ca={{ .Values.caData | quote }}\n"
                "    {{- range $index, $zone := .Values.zones }}\n"
                "    name={{ $zone.name | quote }}\n"
                "    management-api={{ $zone.managementAPI | quote }}\n"
                "    {{- end }}\n",
            )
            findings = policy_check.evaluate_repositories(src_root=src, run_helm_canary=True)
            helm_hits = [f for f in findings if f.rule_id == "HELM-QUOTE-001"]
            self.assertGreaterEqual(len(helm_hits), 1)
            if shutil.which("helm"):
                self.assertTrue(
                    any("broke out" in f.message or "Helm canary render failed" in f.message for f in helm_hits)
                )

    def test_catches_tf_perm_ocm_sast_ci_trust_mcm_and_token_uri(self):
        with tempfile.TemporaryDirectory() as tmp:
            src = self._build_clean_src_root(Path(tmp))
            _write(
                src / "gardener-extension-provider-gdc/gdc-sa-auth-plugin/terraform/main.tf",
                'resource "local_file" "key_json" {\n  filename = "k.json"\n}\n',
            )
            _write(
                src / "gardener-extension-provider-gdc/.ocm/base-component.yaml",
                "labels:\n  - name: sast-scan: skipped\n",
            )
            _write(
                src / "cloud-provider-gdc/.github/workflows/integration-test.yaml",
                "on:\n  workflow_dispatch:\n    inputs:\n      pr_number:\nsteps:\n  - run: source ./scripts/ci-common.sh\n",
            )
            _write(
                src / "cloud-provider-gdc/scripts/ci-common.sh",
                'if [[ "$assoc" =~ ^(MEMBER|OWNER|COLLABORATOR)$ ]]; then return 0; fi\n',
            )
            _write(src / "machine-controller-manager-provider-gdc/pkg/provider/core.go", "package provider\nfunc ListMachines() {}\n")
            _write(src / "cloud-provider-gdc/pkg/cloudprovider/gdc/config.go", "package gdc\n")

            findings = policy_check.evaluate_repositories(src_root=src, run_helm_canary=False)
            rules = {f.rule_id for f in findings if f.severity == "BLOCKING"}
            for expected in ("TF-PERM-001", "OCM-SAST-001", "CI-TRUST-001", "MCM-INIT-001", "GDC-AUTH-URI-001"):
                self.assertIn(expected, rules)


if __name__ == "__main__":
    unittest.main()
