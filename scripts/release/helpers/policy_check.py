#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Static security policy and regression guards for GDC Gardener repositories.

Runs in Stage 1b of the release pipeline (across all GDC component checkouts and
gardener-gdc-ci itself) and in gardener-gdc-ci presubmit CI to prevent known
security vulnerabilities and CI trust-boundary weaknesses from regressing.

Uses Python 3 standard library only so it runs without external packages.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Iterable


SHA256_RE = re.compile(r"@sha256:[0-9a-f]{64}$")
FROM_RE = re.compile(
    r"^\s*FROM\s+(?:--platform=\S+\s+)?(?P<image>\S+)(?:\s+[Aa][Ss]\s+(?P<alias>\S+))?",
    re.IGNORECASE,
)
USER_RE = re.compile(r"^\s*USER\s+(?P<user>\S+)", re.IGNORECASE)
TLS_BYPASS_SHELL_RE = re.compile(
    r"(?:--no-check-certificate|--insecure|\bcurl\b[^|;&\n]*\s-[A-Za-z]*k\b)"
)
GO_INSECURE_TLS_RE = re.compile(r"\bInsecureSkipVerify\s*:\s*true\b")
GO_INSTALL_LATEST_RE = re.compile(r"\bgo\s+install\b[^\n#]*@latest\b")
JWT_MULTI_HOUR_RE = re.compile(r"\b([2-9]|[1-9][0-9]+)\s*\*\s*time\.Hour\b")
CANARY_VALUE = 'canary"\n  injectedKey: pwned\n'
CANARY_BREAKOUT_RE = re.compile(r"^\s*injectedKey\s*:", re.MULTILINE)


@dataclasses.dataclass(frozen=True)
class Finding:
    rule_id: str
    severity: str  # "BLOCKING" or "ADVISORY"
    repo: str
    path: str
    message: str


def _iter_files(root: Path, globs: Iterable[str]) -> list[Path]:
    seen: set[Path] = set()
    results: list[Path] = []
    for pattern in globs:
        for path in sorted(root.glob(pattern)):
            if path.is_file() and path not in seen:
                seen.add(path)
                results.append(path)
    return results


def _rel(root: Path, path: Path) -> str:
    try:
        return str(path.relative_to(root))
    except ValueError:
        return str(path)


def _strip_shell_comment(line: str) -> str:
    stripped = line.lstrip()
    if stripped.startswith("#"):
        return ""
    return line


def check_dockerfile_pinning(repo_name: str, repo_dir: Path) -> list[Finding]:
    """DOCKER-PIN-001: Pin external Dockerfile FROM images by SHA256 digest and run non-root."""
    dockerfile = repo_dir / "Dockerfile"
    if not dockerfile.is_file():
        return []

    findings: list[Finding] = []
    aliases: set[str] = set()
    stage_users: dict[str, str] = {}
    current_stage_idx = 0
    current_alias = ""
    current_base = ""
    final_user = ""
    final_base = ""

    for lineno, raw_line in enumerate(dockerfile.read_text(encoding="utf-8").splitlines(), 1):
        line = _strip_shell_comment(raw_line)
        if not line:
            continue
        m_from = FROM_RE.match(line)
        if m_from:
            current_stage_idx += 1
            image = m_from.group("image")
            alias = (m_from.group("alias") or "").lower()
            img_lower = image.lower()
            if img_lower != "scratch" and img_lower not in aliases:
                if not SHA256_RE.search(image):
                    findings.append(
                        Finding(
                            "DOCKER-PIN-001",
                            "BLOCKING",
                            repo_name,
                            f"Dockerfile:{lineno}",
                            f"External base image '{image}' is not pinned by @sha256 digest.",
                        )
                    )
            current_alias = alias or f"stage-{current_stage_idx}"
            current_base = img_lower
            inherited = stage_users.get(img_lower, "")
            stage_users[current_alias] = inherited
            final_user = inherited
            final_base = img_lower
            if alias:
                aliases.add(alias)
            continue

        m_user = USER_RE.match(line)
        if m_user and current_stage_idx > 0:
            user_val = m_user.group("user")
            stage_users[current_alias] = user_val
            final_user = user_val

    if current_stage_idx > 0 and final_base != "scratch":
        user_head = final_user.split(":", 1)[0].strip().lower()
        if not user_head or user_head in {"root", "0"}:
            findings.append(
                Finding(
                    "DOCKER-PIN-001",
                    "BLOCKING",
                    repo_name,
                    "Dockerfile",
                    f"Final stage does not set a non-root USER (resolved USER='{final_user or '<unset>'}').",
                )
            )
    return findings


def check_tls_bypass(repo_name: str, repo_dir: Path) -> list[Finding]:
    """TLS-BYPASS-001: Disallow TLS verification bypass in scripts, workflows, and non-test Go code."""
    findings: list[Finding] = []

    shell_and_wf = _iter_files(
        repo_dir,
        ("scripts/**/*.sh", "scripts/*.sh", ".github/workflows/*.yaml", ".github/workflows/*.yml"),
    )
    for path in shell_and_wf:
        if path.name.endswith("_test.sh"):
            continue
        for lineno, raw_line in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
            line = _strip_shell_comment(raw_line)
            if not line:
                continue
            if TLS_BYPASS_SHELL_RE.search(line):
                findings.append(
                    Finding(
                        "TLS-BYPASS-001",
                        "BLOCKING",
                        repo_name,
                        f"{_rel(repo_dir, path)}:{lineno}",
                        "Disallowed TLS verification bypass flag (--no-check-certificate, --insecure, or curl -k).",
                    )
                )

    for path in _iter_files(repo_dir, ("**/*.go",)):
        rel = _rel(repo_dir, path)
        if path.name.endswith("_test.go") or rel.startswith(("vendor/", ".tools/")):
            continue
        for lineno, raw_line in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
            code = raw_line.split("//", 1)[0]
            if GO_INSECURE_TLS_RE.search(code):
                findings.append(
                    Finding(
                        "TLS-BYPASS-001",
                        "BLOCKING",
                        repo_name,
                        f"{rel}:{lineno}",
                        "Disallowed InsecureSkipVerify: true in non-test Go source.",
                    )
                )

    return findings


def check_go_tool_pinning(repo_name: str, repo_dir: Path) -> list[Finding]:
    """GO-TOOL-PIN-001: Disallow floating `go install ...@latest` in Makefiles, scripts, and workflows."""
    findings: list[Finding] = []
    targets = _iter_files(
        repo_dir,
        ("Makefile", "scripts/**/*.sh", "scripts/*.sh", ".github/workflows/*.yaml", ".github/workflows/*.yml"),
    )
    for path in targets:
        if path.name.endswith("_test.sh"):
            continue
        for lineno, raw_line in enumerate(path.read_text(encoding="utf-8", errors="replace").splitlines(), 1):
            line = _strip_shell_comment(raw_line)
            if not line:
                continue
            if GO_INSTALL_LATEST_RE.search(line):
                findings.append(
                    Finding(
                        "GO-TOOL-PIN-001",
                        "BLOCKING",
                        repo_name,
                        f"{_rel(repo_dir, path)}:{lineno}",
                        "Unpinned `go install ...@latest`; pin tool installations to an explicit version.",
                    )
                )
    return findings


def check_jwt_claims(repo_name: str, repo_dir: Path) -> list[Finding]:
    """JWT-TTL-001: Ensure STS assertion JWTs are short-lived (<=1h) and set jti (ID) and NotBefore."""
    findings: list[Finding] = []
    for path in _iter_files(repo_dir, ("**/pkg/auth/jwt.go",)):
        rel = _rel(repo_dir, path)
        if rel.startswith("vendor/"):
            continue
        text = path.read_text(encoding="utf-8", errors="replace")
        if "RegisteredClaims" not in text:
            continue
        m = JWT_MULTI_HOUR_RE.search(text)
        if m:
            findings.append(
                Finding(
                    "JWT-TTL-001",
                    "BLOCKING",
                    repo_name,
                    rel,
                    f"STS assertion JWT expiration exceeds 1 hour ('{m.group(0)}').",
                )
            )
        if not re.search(r"\bID\s*:", text):
            findings.append(
                Finding(
                    "JWT-TTL-001",
                    "BLOCKING",
                    repo_name,
                    rel,
                    "STS assertion JWT claims missing 'ID' (jti) replay-mitigation claim.",
                )
            )
        if not re.search(r"\bNotBefore\s*:", text):
            findings.append(
                Finding(
                    "JWT-TTL-001",
                    "BLOCKING",
                    repo_name,
                    rel,
                    "STS assertion JWT claims missing 'NotBefore' (nbf) claim.",
                )
            )
    return findings


def check_helm_quoting(repo_dir: Path, *, run_helm_canary: bool = True) -> list[Finding]:
    """HELM-QUOTE-001: Verify security-sensitive Helm templates quote scalar values and resist injection."""
    repo_name = "gardener-extension-provider-gdc"
    findings: list[Finding] = []

    mc_tpl = repo_dir / "charts/internal/machineclass/templates/machineclass.yaml"
    if mc_tpl.is_file():
        text = mc_tpl.read_text(encoding="utf-8")
        for field in (
            "addressPoolName",
            "subnetName",
            "project",
            "orgClusterURL",
            "registryURL",
            "caData",
            "machineType",
        ):
            m = re.search(rf"^\s*{field}\s*:\s*(.+)$", text, re.MULTILINE)
            if not m or "| quote" not in m.group(1):
                findings.append(
                    Finding(
                        "HELM-QUOTE-001",
                        "BLOCKING",
                        repo_name,
                        "charts/internal/machineclass/templates/machineclass.yaml",
                        f"MachineClass field '{field}' must be piped through '| quote'.",
                    )
                )
        if re.search(r"toYaml\s+\$machineClass\.annotations\s*\|\s*indent\s+2\b", text):
            findings.append(
                Finding(
                    "HELM-QUOTE-001",
                    "BLOCKING",
                    repo_name,
                    "charts/internal/machineclass/templates/machineclass.yaml",
                    "MachineClass providerSpec.annotations must use 'indent 4' (not 'indent 2').",
                )
            )

    cpc_tpl = repo_dir / "charts/internal/cloud-provider-config/templates/cloud-provider-config.yaml"
    if cpc_tpl.is_file():
        text = cpc_tpl.read_text(encoding="utf-8")
        for key in ("project", "region", "global-management-server-url", "ca", "name", "management-api"):
            m = re.search(rf"^\s*{re.escape(key)}\s*=\s*(.+)$", text, re.MULTILINE)
            if not m or "| quote" not in m.group(1):
                findings.append(
                    Finding(
                        "HELM-QUOTE-001",
                        "BLOCKING",
                        repo_name,
                        "charts/internal/cloud-provider-config/templates/cloud-provider-config.yaml",
                        f"cloudprovider.conf field '{key}' must be piped through '| quote'.",
                    )
                )

    kvcsi_tpl = (
        repo_dir
        / "charts/internal/seed-controlplane/charts/csi-driver-controller/templates/kvcsi-driver-config.yaml"
    )
    if kvcsi_tpl.is_file():
        text = kvcsi_tpl.read_text(encoding="utf-8")
        for key in ("infraClusterLabels", "infraClusterNamespace"):
            m = re.search(rf"^\s*{key}\s*:\s*(.+)$", text, re.MULTILINE)
            if not m or "| quote" not in m.group(1):
                findings.append(
                    Finding(
                        "HELM-QUOTE-001",
                        "BLOCKING",
                        repo_name,
                        "charts/internal/seed-controlplane/charts/csi-driver-controller/templates/kvcsi-driver-config.yaml",
                        f"kvcsi-driver-config field '{key}' must be piped through '| quote'.",
                    )
                )

    if run_helm_canary and shutil.which("helm"):
        findings.extend(_run_helm_canary_checks(repo_dir))

    return findings


def _run_helm_canary_checks(repo_dir: Path) -> list[Finding]:
    repo_name = "gardener-extension-provider-gdc"
    findings: list[Finding] = []
    canary_cases = [
        (
            "charts/internal/cloud-provider-config",
            None,
            {
                "project": CANARY_VALUE,
                "region": "us-west6-a",
                "globalManagementServerURL": "https://global.example.invalid",
                "caData": "Y2E=",
                "zones": [{"name": "zone-a", "managementAPI": "https://mgmt.example.invalid"}],
            },
        ),
        (
            "charts/internal/machineclass",
            None,
            {
                "machineClasses": [
                    {
                        "name": "mc-canary",
                        "project": CANARY_VALUE,
                        "addressPoolName": "pool",
                        "subnetName": "subnet",
                        "orgClusterURL": "https://org.example.invalid",
                        "registryURL": "reg.example.invalid",
                        "caData": "Y2E=",
                        "machineType": "n2-standard-2",
                        "disks": [{"name": "boot"}],
                        "secret": {"cloudConfig": "#cloud-config"},
                        "credentialsSecretRef": {"name": "cloudprovider", "namespace": "shoot--test"},
                    }
                ]
            },
        ),
        (
            "charts/internal/seed-controlplane/charts/csi-driver-controller",
            "templates/kvcsi-driver-config.yaml",
            {
                "global": {"genericTokenKubeconfigSecretName": "generic-token-kubeconfig"},
                "infraClusterLabels": "k=v",
                "infraClusterNamespace": CANARY_VALUE,
            },
        ),
    ]

    with tempfile.TemporaryDirectory(prefix="gdc-helm-canary-") as tmpdir:
        for idx, (chart_rel, show_only, values_obj) in enumerate(canary_cases):
            chart_dir = repo_dir / chart_rel
            if not (chart_dir / "Chart.yaml").is_file():
                continue
            values_file = Path(tmpdir) / f"values-{idx}.json"
            values_file.write_text(json.dumps(values_obj), encoding="utf-8")
            cmd = ["helm", "template", f"canary-{idx}", str(chart_dir), "-f", str(values_file)]
            if show_only:
                cmd.extend(["-s", show_only])
            res = subprocess.run(cmd, capture_output=True, text=True, check=False)
            if res.returncode != 0:
                findings.append(
                    Finding(
                        "HELM-QUOTE-001",
                        "BLOCKING",
                        repo_name,
                        chart_rel,
                        f"Helm canary render failed: {res.stderr.strip() or res.stdout.strip()}",
                    )
                )
                continue
            if CANARY_BREAKOUT_RE.search(res.stdout):
                findings.append(
                    Finding(
                        "HELM-QUOTE-001",
                        "BLOCKING",
                        repo_name,
                        chart_rel,
                        "Helm canary value broke out into an injected YAML/INI line (unquoted scalar interpolation).",
                    )
                )
    return findings


def check_terraform_permissions(repo_dir: Path) -> list[Finding]:
    """TF-PERM-001: Ensure Terraform local_file resources write credentials with mode 0600."""
    repo_name = "gardener-extension-provider-gdc"
    findings: list[Finding] = []
    tf_files = _iter_files(repo_dir, ("gdc-sa-auth-plugin/terraform/*.tf",))
    block_re = re.compile(r'resource\s+"local_file"\s+"([^"]+)"\s*\{(.*?)\n\}', re.DOTALL)
    perm_re = re.compile(r'^\s*file_permission\s*=\s*"0600"\s*$', re.MULTILINE)
    for path in tf_files:
        text = path.read_text(encoding="utf-8", errors="replace")
        for m in block_re.finditer(text):
            res_name, body = m.group(1), m.group(2)
            if not perm_re.search(body):
                findings.append(
                    Finding(
                        "TF-PERM-001",
                        "BLOCKING",
                        repo_name,
                        _rel(repo_dir, path),
                        f'Terraform local_file.{res_name} must set file_permission = "0600".',
                    )
                )
    return findings


def check_ocm_sast_skip(repo_dir: Path) -> list[Finding]:
    """OCM-SAST-001: Prevent re-introducing SAST skip labels in .ocm/base-component.yaml."""
    base_comp = repo_dir / ".ocm/base-component.yaml"
    if not base_comp.is_file():
        return []
    text = base_comp.read_text(encoding="utf-8", errors="replace")
    if re.search(r"(?:skip[-_]?sast|sast[-_]?scan\s*:\s*skip|sast\b[^\n]*skipped)", text, re.IGNORECASE):
        return [
            Finding(
                "OCM-SAST-001",
                "BLOCKING",
                "gardener-extension-provider-gdc",
                ".ocm/base-component.yaml",
                "Disallowed SAST skip label in OCM base component descriptor.",
            )
        ]
    return []


def check_ci_trust_boundary(repo_name: str, repo_dir: Path) -> list[Finding]:
    """CI-TRUST-001: Guard presubmit integration test workflow and scripts against trust-boundary regressions."""
    findings: list[Finding] = []

    wf = repo_dir / ".github/workflows/integration-test.yaml"
    if wf.is_file():
        text = wf.read_text(encoding="utf-8", errors="replace")
        if re.search(r"source\s+\./scripts/ci-common\.sh", text):
            findings.append(
                Finding(
                    "CI-TRUST-001",
                    "BLOCKING",
                    repo_name,
                    ".github/workflows/integration-test.yaml",
                    "Workflow must load scripts/ci-common.sh strictly from origin/main (no fallback to ./scripts/ci-common.sh).",
                )
            )
        if "pr_number:" in text and "head_sha:" not in text:
            findings.append(
                Finding(
                    "CI-TRUST-001",
                    "BLOCKING",
                    repo_name,
                    ".github/workflows/integration-test.yaml",
                    "workflow_dispatch accepts pr_number without a head_sha commit-pinning input.",
                )
            )

    ci_common = repo_dir / "scripts/ci-common.sh"
    if ci_common.is_file():
        text = ci_common.read_text(encoding="utf-8", errors="replace")
        # Check for old author_association-based authorization fallback
        if re.search(r'MEMBER\|OWNER\|COLLABORATOR|OWNER\|MEMBER\|COLLABORATOR', text):
            findings.append(
                Finding(
                    "CI-TRUST-001",
                    "BLOCKING",
                    repo_name,
                    "scripts/ci-common.sh",
                    "author_association (MEMBER/OWNER/COLLABORATOR) must not authorize staging runs; use OWNERS files on origin/main only.",
                )
            )
        if re.search(r"origin/main:scripts/ci-common\.sh.*\|\|.*cp\s+scripts/ci-common\.sh", text):
            findings.append(
                Finding(
                    "CI-TRUST-001",
                    "BLOCKING",
                    repo_name,
                    "scripts/ci-common.sh",
                    "prepare_trusted_ci_files must not fall back to checked-out scripts/ci-common.sh.",
                )
            )

    return findings


def check_mcm_core_hardening(repo_dir: Path) -> list[Finding]:
    """MCM-INIT-001: Prevent shell-injection regression in CA init script and empty-label ListMachines."""
    repo_name = "machine-controller-manager-provider-gdc"
    core_go = repo_dir / "pkg/provider/core.go"
    if not core_go.is_file():
        return []
    text = core_go.read_text(encoding="utf-8", errors="replace")
    findings: list[Finding] = []

    if "shellQuote(" not in text:
        findings.append(
            Finding(
                "MCM-INIT-001",
                "BLOCKING",
                repo_name,
                "pkg/provider/core.go",
                "CA init script renderer must use shell quoting (shellQuote) instead of Go %q formatting.",
            )
        )
    if not re.search(r"len\(\s*(?:providerSpec|spec)\.Labels\s*\)\s*==\s*0", text):
        findings.append(
            Finding(
                "MCM-INIT-001",
                "BLOCKING",
                repo_name,
                "pkg/provider/core.go",
                "ListMachines must reject empty providerSpec/spec.Labels to prevent project-wide VM listing.",
            )
        )
    return findings


def check_token_uri_validation(repo_name: str, repo_dir: Path) -> list[Finding]:
    """GDC-AUTH-URI-001: Ensure credential loaders enforce validateTokenURI."""
    candidates = [
        repo_dir / "pkg/gdc/serviceaccount.go",
        repo_dir / "pkg/spi/spi.go",
        repo_dir / "pkg/cloudprovider/gdc/config.go",
    ]
    existing = [p for p in candidates if p.is_file()]
    if not existing:
        return []
    for path in existing:
        text = path.read_text(encoding="utf-8", errors="replace")
        if re.search(r"\bfunc\s+[Vv]alidateTokenURI\s*\(", text):
            return []
    return [
        Finding(
            "GDC-AUTH-URI-001",
            "BLOCKING",
            repo_name,
            _rel(repo_dir, existing[0]),
            "Credential loader missing validateTokenURI check.",
        )
    ]


def check_advisory_extension_policies(repo_dir: Path) -> list[Finding]:
    """Advisory checks tracking open design-level hardening items in gardener-extension-provider-gdc."""
    repo_name = "gardener-extension-provider-gdc"
    findings: list[Finding] = []

    infra_files = _iter_files(repo_dir, ("pkg/controller/infrastructure/*.go",))
    infra_src = "\n".join(
        p.read_text(encoding="utf-8", errors="replace")
        for p in infra_files
        if not p.name.endswith("_test.go")
    )
    if infra_src and "isRootSubnetOwned" not in infra_src and "ownsChildSubnet" not in infra_src:
        findings.append(
            Finding(
                "EXT-SUBNET-001",
                "ADVISORY",
                repo_name,
                "pkg/controller/infrastructure/",
                "Subnet deletion/reuse does not yet enforce shoot ownership verification (isRootSubnetOwned/ownsChildSubnet).",
            )
        )

    bastion_files = _iter_files(repo_dir, ("pkg/controller/bastion/*.go",))
    bastion_src = "\n".join(
        p.read_text(encoding="utf-8", errors="replace")
        for p in bastion_files
        if not p.name.endswith("_test.go")
    )
    if bastion_src and "sshPort" not in bastion_src and "intstr.FromInt32(22)" not in bastion_src:
        findings.append(
            Finding(
                "EXT-BASTION-001",
                "ADVISORY",
                repo_name,
                "pkg/controller/bastion/",
                "Bastion ProjectNetworkPolicy does not yet restrict ingress to SSH (TCP/22).",
            )
        )

    ccm_rbac = repo_dir / "charts/internal/shoot-system-components/charts/cloud-controller-manager/templates/rbac-cloud-controller-manager.yaml"
    if ccm_rbac.is_file() and "cluster-admin" in ccm_rbac.read_text(encoding="utf-8", errors="replace"):
        findings.append(
            Finding(
                "EXT-RBAC-001",
                "ADVISORY",
                repo_name,
                "charts/internal/shoot-system-components/charts/cloud-controller-manager/templates/rbac-cloud-controller-manager.yaml",
                "Shoot cloud-controller-manager still binds to cluster-admin.",
            )
        )

    images_yaml = repo_dir / "imagevector/images.yaml"
    if images_yaml.is_file():
        text = images_yaml.read_text(encoding="utf-8", errors="replace")
        if "tag:" in text and "@sha256:" not in text:
            findings.append(
                Finding(
                    "EXT-IMAGE-001",
                    "ADVISORY",
                    repo_name,
                    "imagevector/images.yaml",
                    "imagevector/images.yaml uses mutable image tags without @sha256 digests.",
                )
            )

    return findings


def evaluate_repositories(
    *,
    src_root: Path | None = None,
    ci_repo: Path | None = None,
    run_helm_canary: bool = True,
    promote_advisory: bool = False,
) -> list[Finding]:
    findings: list[Finding] = []

    if ci_repo is not None and ci_repo.is_dir():
        findings.extend(check_tls_bypass("gardener-gdc-ci", ci_repo))
        findings.extend(check_go_tool_pinning("gardener-gdc-ci", ci_repo))
        findings.extend(check_jwt_claims("gardener-gdc-ci", ci_repo))

    if src_root is not None and src_root.is_dir():
        for repo_name in (
            "gardener-extension-provider-gdc",
            "machine-controller-manager-provider-gdc",
            "cloud-provider-gdc",
        ):
            repo_dir = src_root / repo_name
            if not repo_dir.is_dir():
                continue
            findings.extend(check_dockerfile_pinning(repo_name, repo_dir))
            findings.extend(check_tls_bypass(repo_name, repo_dir))
            findings.extend(check_go_tool_pinning(repo_name, repo_dir))
            findings.extend(check_jwt_claims(repo_name, repo_dir))
            findings.extend(check_ci_trust_boundary(repo_name, repo_dir))
            findings.extend(check_token_uri_validation(repo_name, repo_dir))

            if repo_name == "gardener-extension-provider-gdc":
                findings.extend(check_helm_quoting(repo_dir, run_helm_canary=run_helm_canary))
                findings.extend(check_terraform_permissions(repo_dir))
                findings.extend(check_ocm_sast_skip(repo_dir))
                findings.extend(check_advisory_extension_policies(repo_dir))
            elif repo_name == "machine-controller-manager-provider-gdc":
                findings.extend(check_mcm_core_hardening(repo_dir))

    if promote_advisory:
        findings = [
            dataclasses.replace(f, severity="BLOCKING") if f.severity == "ADVISORY" else f
            for f in findings
        ]

    return findings


def format_markdown_rows(findings: list[Finding], checked_repos: list[str]) -> str:
    rows: list[str] = []
    for repo in checked_repos:
        repo_findings = [f for f in findings if f.repo == repo]
        blocking = [f for f in repo_findings if f.severity == "BLOCKING"]
        advisory = [f for f in repo_findings if f.severity == "ADVISORY"]
        status = "`FAIL`" if blocking else "`PASS`"
        rule_summary = ", ".join(sorted({f.rule_id for f in blocking})) if blocking else "none"
        adv_summary = ", ".join(sorted({f.rule_id for f in advisory})) if advisory else "none"
        rows.append(
            f"| `{repo}` | {len(blocking)} ({rule_summary}) | {len(advisory)} ({adv_summary}) | {status} |"
        )
    return "\n".join(rows) + ("\n" if rows else "")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--src-root", type=Path, default=None, help="Directory containing component repo checkouts.")
    parser.add_argument("--ci-repo", type=Path, default=None, help="Path to gardener-gdc-ci repository root.")
    parser.add_argument("--json-out", type=Path, default=None, help="Optional path to write JSON report.")
    parser.add_argument("--markdown-out", type=Path, default=None, help="Optional path to write summary table rows.")
    parser.add_argument("--skip-helm", action="store_true", help="Skip dynamic helm template canary checks.")
    parser.add_argument(
        "--promote-advisory",
        action="store_true",
        default=os.environ.get("PROMOTE_ADVISORY_POLICIES", "false").lower() == "true",
        help="Treat advisory findings as blocking.",
    )
    args = parser.parse_args(argv)

    checked_repos: list[str] = []
    if args.ci_repo is not None and args.ci_repo.is_dir():
        checked_repos.append("gardener-gdc-ci")
    if args.src_root is not None and args.src_root.is_dir():
        for name in (
            "gardener-extension-provider-gdc",
            "machine-controller-manager-provider-gdc",
            "cloud-provider-gdc",
        ):
            if (args.src_root / name).is_dir():
                checked_repos.append(name)

    findings = evaluate_repositories(
        src_root=args.src_root,
        ci_repo=args.ci_repo,
        run_helm_canary=not args.skip_helm,
        promote_advisory=args.promote_advisory,
    )

    blocking = [f for f in findings if f.severity == "BLOCKING"]
    advisory = [f for f in findings if f.severity == "ADVISORY"]

    for f in blocking:
        print(f"::error::[Policy {f.rule_id}] {f.repo}/{f.path}: {f.message}", file=sys.stderr)
    for f in advisory:
        print(f"::warning::[Advisory {f.rule_id}] {f.repo}/{f.path}: {f.message}", file=sys.stderr)

    if args.json_out is not None:
        args.json_out.parent.mkdir(parents=True, exist_ok=True)
        payload = {
            "checked_repos": checked_repos,
            "blocking_count": len(blocking),
            "advisory_count": len(advisory),
            "findings": [dataclasses.asdict(f) for f in findings],
        }
        args.json_out.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")

    if args.markdown_out is not None:
        args.markdown_out.parent.mkdir(parents=True, exist_ok=True)
        args.markdown_out.write_text(format_markdown_rows(findings, checked_repos), encoding="utf-8")

    print(
        f"[Policy Check] Scanned {len(checked_repos)} repo(s): "
        f"{len(blocking)} blocking finding(s), {len(advisory)} advisory finding(s)."
    )
    return 1 if blocking else 0


if __name__ == "__main__":
    sys.exit(main())
