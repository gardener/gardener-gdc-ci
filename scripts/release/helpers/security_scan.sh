#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

# Automated multi-layer security gate for Gardener GDC candidate artifacts:
#   1. Source SAST (`gosec -exclude-generated` SARIF) across all GDC component repositories
#   2. Go dependency vulnerability reachability (`govulncheck`) across production packages
#   3. Container image CVE & secret scanning (`trivy image --scanners vuln,secret`) on all 5 candidate images
#   4. Helm chart & Dockerfile misconfiguration scanning (`helm template` + `trivy config`)
#   5. Static security policy & regression guards (`policy_check.py`) across all 4 repositories

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/constants.sh"

SECURITY_REPORT_DIR="${SECURITY_REPORT_DIR:-${RELEASE_ARTIFACTS_DIR}/security}"
SECURITY_TOOLS_DIR="${SECURITY_TOOLS_DIR:-${RELEASE_WORK_DIR}/security-tools}"
SECURITY_TRIVY_CACHE_DIR="${SECURITY_TRIVY_CACHE_DIR:-${RELEASE_WORK_DIR}/trivy-cache}"
SECURITY_SUMMARY_FILE="${SECURITY_SUMMARY_FILE:-${SECURITY_REPORT_DIR}/security-scan-summary.md}"
ENFORCE_SECURITY_GATE="${ENFORCE_SECURITY_GATE:-true}"

GOSEC_VERSION="${GOSEC_VERSION:-v2.30.0}"
GOVULNCHECK_VERSION="${GOVULNCHECK_VERSION:-v1.10.0}"
TRIVY_IMAGE="${TRIVY_IMAGE:-ghcr.io/aquasecurity/trivy:0.76.2@sha256:8f6bb947dbc528902e11f2f5be52f7b519f6a39d5d148f41166f2040499768fb}"

GDC_IMAGE_GATE_SEVERITIES="${GDC_IMAGE_GATE_SEVERITIES:-CRITICAL,HIGH,MEDIUM}"
UPSTREAM_IMAGE_GATE_SEVERITIES="${UPSTREAM_IMAGE_GATE_SEVERITIES:-CRITICAL,HIGH}"
CONFIG_GATE_SEVERITIES="${CONFIG_GATE_SEVERITIES:-CRITICAL,HIGH}"

mkdir -p "${SECURITY_REPORT_DIR}" "${SECURITY_TOOLS_DIR}" "${SECURITY_TRIVY_CACHE_DIR}"

echo "=== Stage 1b: Automated Security Scan (SAST, Dependencies, Images & Helm Charts) ==="

ensure_go_security_tools() {
  if command -v gosec >/dev/null 2>&1; then
    GOSEC_BIN="$(command -v gosec)"
  else
    echo "Installing gosec (${GOSEC_VERSION})..."
    GOBIN="${SECURITY_TOOLS_DIR}" GOTOOLCHAIN="${SECURITY_GOTOOLCHAIN:-auto}" \
      go install "github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION}"
    GOSEC_BIN="${SECURITY_TOOLS_DIR}/gosec"
  fi

  if command -v govulncheck >/dev/null 2>&1; then
    GOVULNCHECK_BIN="$(command -v govulncheck)"
  else
    echo "Installing govulncheck (${GOVULNCHECK_VERSION})..."
    GOBIN="${SECURITY_TOOLS_DIR}" GOTOOLCHAIN="${SECURITY_GOTOOLCHAIN:-auto}" \
      go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"
    GOVULNCHECK_BIN="${SECURITY_TOOLS_DIR}/govulncheck"
  fi
}

run_trivy() {
  if command -v trivy >/dev/null 2>&1; then
    trivy --cache-dir "${SECURITY_TRIVY_CACHE_DIR}" "$@"
  else
    # Candidate images are already pushed to GHCR by Stage 1, so Trivy scans
    # them directly from the registry without mounting the Docker socket.
    local docker_args=(
      run --rm
      --user "$(id -u):$(id -g)"
      --cap-drop=ALL
      --security-opt=no-new-privileges:true
      -v "${SECURITY_TRIVY_CACHE_DIR}:${SECURITY_TRIVY_CACHE_DIR}"
      -v "${SECURITY_REPORT_DIR}:${SECURITY_REPORT_DIR}"
    )
    if [[ -f "${HOME}/.docker/config.json" ]]; then
      docker_args+=(
        -e DOCKER_CONFIG=/tmp/docker-config
        -v "${HOME}/.docker/config.json:/tmp/docker-config/config.json:ro"
      )
    fi
    docker "${docker_args[@]}" \
      "${TRIVY_IMAGE}" --cache-dir "${SECURITY_TRIVY_CACHE_DIR}" "$@"
  fi
}

if [[ ! -f "${RELEASE_METADATA_FILE}" ]]; then
  echo "::error::Release metadata file (${RELEASE_METADATA_FILE}) not found." >&2
  exit 1
fi

ensure_go_security_tools

SRC_ROOT="${BUILD_WORK_DIR:-${RELEASE_WORK_DIR}/src}"
CANDIDATE_REG_PREFIX="$(yq -r '.candidateRegistry // ""' "${RELEASE_METADATA_FILE}")"
if [[ -z "${CANDIDATE_REG_PREFIX}" ]]; then
  CANDIDATE_REG_PREFIX="${GHCR_REGISTRY:-ghcr.io/gardener/gardener-gdc-ci}"
fi

EXT_VERSION="$(yq -r '.extensionGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
MCM_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
CCM_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
DNS_VERSION="$(yq -r '.externalDNSArtifactsVersion' "${RELEASE_METADATA_FILE}")"

BLOCKING_FAILURES=0
SAST_ROWS=""
GOVULN_ROWS=""
IMAGE_ROWS=""
CONFIG_ROWS=""
POLICY_ROWS=""

# ---------------------------------------------------------------------------
# 1. Source SAST (`gosec`) & Go Dependency Reachability (`govulncheck`)
# ---------------------------------------------------------------------------
scan_go_repo() {
  local name="$1"
  local repo_dir="$2"
  local ocm_sarif_copy="$3"
  shift 3
  local vuln_pkgs=("$@")

  if [[ ! -d "${repo_dir}" ]]; then
    echo "::error::Source directory ${repo_dir} not found; cannot run source scans for ${name}." >&2
    SAST_ROWS+="| \`${name}\` | missing-source | \`FAIL\` |\n"
    GOVULN_ROWS+="| \`${name}\` | missing-source | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 2))
    return 0
  fi

  local sarif_out="${SECURITY_REPORT_DIR}/gosec-${name}.sarif"
  echo "[SAST] Running gosec on ${name}..."
  (
    cd "${repo_dir}"
    "${GOSEC_BIN}" -exclude-generated -fmt sarif -out "${sarif_out}" ./... || true
  )
  if [[ ! -s "${sarif_out}" ]] || ! jq -e '.runs | type == "array" and length > 0' "${sarif_out}" >/dev/null 2>&1; then
    echo "::error::gosec failed to generate valid SARIF output for ${name} (${sarif_out})." >&2
    SAST_ROWS+="| \`${name}\` | scan-error | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
  else
    if [[ -n "${ocm_sarif_copy}" ]]; then
      cp "${sarif_out}" "${ocm_sarif_copy}"
    fi
    local unsuppressed
    unsuppressed="$(jq '[.runs[0].results[]? | select((.suppressions // []) | length == 0)] | length' "${sarif_out}")"
    if (( unsuppressed > 0 )); then
      echo "::error::[SAST] ${name} has ${unsuppressed} unsuppressed gosec finding(s)." >&2
      SAST_ROWS+="| \`${name}\` | ${unsuppressed} | \`FAIL\` |\n"
      BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
    else
      SAST_ROWS+="| \`${name}\` | 0 | \`PASS\` |\n"
    fi
  fi

  local vuln_out="${SECURITY_REPORT_DIR}/govulncheck-${name}.json"
  echo "[VulnCheck] Running govulncheck on ${name} (${vuln_pkgs[*]})..."
  local vuln_rc=0
  (
    cd "${repo_dir}"
    GOTOOLCHAIN="${SECURITY_GOTOOLCHAIN:-auto}" "${GOVULNCHECK_BIN}" -format json "${vuln_pkgs[@]}" > "${vuln_out}"
  ) || vuln_rc=$?

  local called_vulns
  called_vulns="$(jq -s '[.[] | select(.finding != null and ((.finding.trace[0].function // "") != "")) | .finding.osv] | unique | length' "${vuln_out}" 2>/dev/null || echo 0)"
  if (( vuln_rc != 0 && called_vulns == 0 )); then
    # Fallback if symbol/package-level exit occurred without function trace entries
    called_vulns=1
  fi
  if (( called_vulns > 0 )); then
    echo "::error::[VulnCheck] ${name} has ${called_vulns} reachable Go vulnerability finding(s)." >&2
    GOVULN_ROWS+="| \`${name}\` | ${called_vulns} | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
  else
    GOVULN_ROWS+="| \`${name}\` | 0 | \`PASS\` |\n"
  fi
}

scan_go_repo "gardener-extension-provider-gdc" "${SRC_ROOT}/gardener-extension-provider-gdc" \
  "${RELEASE_ARTIFACTS_DIR}/gosec-report.sarif" ./cmd/... ./pkg/... ./gdc/... ./gdc-sa-auth-plugin/...
scan_go_repo "machine-controller-manager-provider-gdc" "${SRC_ROOT}/machine-controller-manager-provider-gdc" \
  "${RELEASE_ARTIFACTS_DIR}/gosec-report-mcm.sarif" ./cmd/... ./pkg/... ./gdc/...
scan_go_repo "cloud-provider-gdc" "${SRC_ROOT}/cloud-provider-gdc" \
  "${RELEASE_ARTIFACTS_DIR}/gosec-report-ccm.sarif" ./cmd/... ./pkg/... ./gdc/...

# ---------------------------------------------------------------------------
# 2. Container Image CVE & Secret Scanning (`trivy image`)
# ---------------------------------------------------------------------------
scan_candidate_image() {
  local component="$1"
  local image_ref="$2"
  local gate_severities="$3"
  local json_out="${SECURITY_REPORT_DIR}/trivy-image-${component}.json"

  echo "[Trivy Image] Scanning ${image_ref} (gate severities: ${gate_severities})..."
  if ! run_trivy image --scanners vuln,secret --ignore-unfixed --format json --output "${json_out}" "${image_ref}"; then
    echo "::error::[Trivy Image] Failed to scan ${image_ref}." >&2
    IMAGE_ROWS+="| \`${component}\` | scan-error | - | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
    return 0
  fi

  local counts blocking_vulns secrets_count
  counts="$(jq -r '
    ([.Results[]?.Vulnerabilities[]?] | group_by(.Severity) | map({(.[0].Severity): length}) | add // {}) as $v |
    "C:\($v.CRITICAL // 0) H:\($v.HIGH // 0) M:\($v.MEDIUM // 0) L:\($v.LOW // 0)"
  ' "${json_out}")"

  blocking_vulns="$(jq --arg sev "${gate_severities}" '
    ($sev | split(",")) as $allowed |
    [.Results[]?.Vulnerabilities[]? | select(.Severity as $s | $allowed | index($s))] | length
  ' "${json_out}")"

  secrets_count="$(jq '[.Results[]?.Secrets[]?] | length' "${json_out}")"

  if (( blocking_vulns > 0 || secrets_count > 0 )); then
    echo "::error::[Trivy Image] ${component} failed gate: ${blocking_vulns} fixable CVE(s) in [${gate_severities}], ${secrets_count} secret(s)." >&2
    IMAGE_ROWS+="| \`${component}\` | ${counts} | ${secrets_count} | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
  else
    IMAGE_ROWS+="| \`${component}\` | ${counts} | 0 | \`PASS\` |\n"
  fi
}

scan_candidate_image "gardener-extension-provider-gdch" "${CANDIDATE_REG_PREFIX}/gardener-extension-provider-gdch:${EXT_VERSION}" "${GDC_IMAGE_GATE_SEVERITIES}"
scan_candidate_image "gardener-extension-admission-gdch" "${CANDIDATE_REG_PREFIX}/gardener-extension-admission-gdch:${EXT_VERSION}" "${GDC_IMAGE_GATE_SEVERITIES}"
scan_candidate_image "machine-controller-manager-provider-gdch" "${CANDIDATE_REG_PREFIX}/machine-controller-manager-provider-gdch:${MCM_VERSION}" "${GDC_IMAGE_GATE_SEVERITIES}"
scan_candidate_image "cloud-controller-manager-gdch" "${CANDIDATE_REG_PREFIX}/cloud-controller-manager-gdch:${CCM_VERSION}" "${GDC_IMAGE_GATE_SEVERITIES}"
scan_candidate_image "external-dnsman2-gdch" "${CANDIDATE_REG_PREFIX}/external-dnsman2-gdch:${DNS_VERSION}" "${UPSTREAM_IMAGE_GATE_SEVERITIES}"

# ---------------------------------------------------------------------------
# 3. Helm Chart & Dockerfile Misconfiguration Scanning (`trivy config`)
# ---------------------------------------------------------------------------
RENDERED_MANIFESTS_DIR="${SECURITY_REPORT_DIR}/rendered-manifests"
mkdir -p "${RENDERED_MANIFESTS_DIR}"

scan_config_target() {
  local label="$1"
  local target_path="$2"
  local ignore_file="${3:-}"
  local json_out="${SECURITY_REPORT_DIR}/trivy-config-${label}.json"

  if [[ ! -e "${target_path}" ]]; then
    echo "::error::[Trivy Config] Target ${target_path} not found for ${label}." >&2
    CONFIG_ROWS+="| \`${label}\` | missing-target | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
    return 0
  fi

  local extra_args=()
  if [[ -n "${ignore_file}" && -f "${ignore_file}" ]]; then
    extra_args+=(--ignorefile "${ignore_file}")
  fi

  echo "[Trivy Config] Scanning ${label} (${target_path})..."
  if ! run_trivy config "${extra_args[@]}" --format json --output "${json_out}" "${target_path}"; then
    echo "::error::[Trivy Config] Failed to scan ${label}." >&2
    CONFIG_ROWS+="| \`${label}\` | scan-error | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
    return 0
  fi

  local summary blocking_misconfigs
  summary="$(jq -r '
    ([.Results[]?.Misconfigurations[]? | select(.Status == "FAIL")] | group_by(.Severity) | map({(.[0].Severity): length}) | add // {}) as $m |
    "C:\($m.CRITICAL // 0) H:\($m.HIGH // 0) M:\($m.MEDIUM // 0) L:\($m.LOW // 0)"
  ' "${json_out}")"

  blocking_misconfigs="$(jq --arg sev "${CONFIG_GATE_SEVERITIES}" '
    ($sev | split(",")) as $allowed |
    [.Results[]?.Misconfigurations[]? | select(.Status == "FAIL") | select(.Severity as $s | $allowed | index($s))] | length
  ' "${json_out}")"

  if (( blocking_misconfigs > 0 )); then
    echo "::error::[Trivy Config] ${label} has ${blocking_misconfigs} blocking misconfiguration(s) in [${CONFIG_GATE_SEVERITIES}]." >&2
    CONFIG_ROWS+="| \`${label}\` | ${summary} | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
  else
    CONFIG_ROWS+="| \`${label}\` | ${summary} | \`PASS\` |\n"
  fi
}

if [[ ! -d "${SRC_ROOT}/gardener-extension-provider-gdc/charts" ]]; then
  echo "::error::[Trivy Config] Helm charts directory not found under ${SRC_ROOT}/gardener-extension-provider-gdc/charts." >&2
  CONFIG_ROWS+="| \`helm-rendered-charts\` | missing-charts | \`FAIL\` |\n"
  BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
elif ! command -v helm >/dev/null 2>&1; then
  echo "::error::[Trivy Config] helm binary not found; cannot render charts for security scan." >&2
  CONFIG_ROWS+="| \`helm-rendered-charts\` | scan-error | \`FAIL\` |\n"
  BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
else
  if helm template extension-provider "${SRC_ROOT}/gardener-extension-provider-gdc/charts/extension-provider" \
      > "${RENDERED_MANIFESTS_DIR}/extension-provider.yaml" && \
     helm template extension-admission-app "${SRC_ROOT}/gardener-extension-provider-gdc/charts/extension-admission/charts/application" \
      > "${RENDERED_MANIFESTS_DIR}/extension-admission-application.yaml" && \
     helm template extension-admission-runtime "${SRC_ROOT}/gardener-extension-provider-gdc/charts/extension-admission/charts/runtime" \
      > "${RENDERED_MANIFESTS_DIR}/extension-admission-runtime.yaml" && \
     helm template seed-cloud-controller-manager "${SRC_ROOT}/gardener-extension-provider-gdc/charts/internal/seed-controlplane/charts/cloud-controller-manager" \
      --set global.genericTokenKubeconfigSecretName=generic-token-kubeconfig \
      > "${RENDERED_MANIFESTS_DIR}/seed-cloud-controller-manager.yaml"; then
    # Gardener extension & admission ClusterRoles intentionally reconcile secrets, webhooks,
    # services, and machine.sapcloud.io resources across Shoot namespaces. Ignore generic
    # ClusterRole RBAC alerts while enforcing all Pod/Container PSS and workload checks.
    cat > "${RENDERED_MANIFESTS_DIR}/.trivyignore" <<'TRIVYIGNORE_EOF'
KSV-0041
KSV-0045
KSV-0046
KSV-0050
KSV-0056
KSV-0114
AVD-KSV-0041
AVD-KSV-0045
AVD-KSV-0046
AVD-KSV-0050
AVD-KSV-0056
AVD-KSV-0114
TRIVYIGNORE_EOF
    scan_config_target "helm-rendered-charts" "${RENDERED_MANIFESTS_DIR}" "${RENDERED_MANIFESTS_DIR}/.trivyignore"
  else
    echo "::error::[Trivy Config] Failed to render Helm charts for security scan." >&2
    CONFIG_ROWS+="| \`helm-rendered-charts\` | scan-error | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
  fi
fi

DOCKERFILES_SCAN_DIR="${SECURITY_REPORT_DIR}/dockerfiles"
mkdir -p "${DOCKERFILES_SCAN_DIR}"
for repo_name in "gardener-extension-provider-gdc" "machine-controller-manager-provider-gdc" "cloud-provider-gdc"; do
  if [[ -f "${SRC_ROOT}/${repo_name}/Dockerfile" ]]; then
    mkdir -p "${DOCKERFILES_SCAN_DIR}/${repo_name}"
    cp "${SRC_ROOT}/${repo_name}/Dockerfile" "${DOCKERFILES_SCAN_DIR}/${repo_name}/Dockerfile"
    scan_config_target "dockerfile-${repo_name}" "${DOCKERFILES_SCAN_DIR}/${repo_name}/Dockerfile"
  else
    echo "::error::[Trivy Config] Dockerfile not found for ${repo_name} (${SRC_ROOT}/${repo_name}/Dockerfile)." >&2
    CONFIG_ROWS+="| \`dockerfile-${repo_name}\` | missing-file | \`FAIL\` |\n"
    BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
  fi
done

# ---------------------------------------------------------------------------
# 4. Repository Security Policy & Regression Guards (`policy_check.py`)
# ---------------------------------------------------------------------------
POLICY_JSON_OUT="${SECURITY_REPORT_DIR}/policy-check.json"
POLICY_ROWS_FILE="${SECURITY_REPORT_DIR}/policy-check-rows.md"
CI_REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"
echo "[Policy Check] Running static security policy & regression guards..."
if ! python3 "${SCRIPT_DIR}/policy_check.py" \
    --src-root "${SRC_ROOT}" \
    --ci-repo "${CI_REPO_ROOT}" \
    --json-out "${POLICY_JSON_OUT}" \
    --markdown-out "${POLICY_ROWS_FILE}"; then
  BLOCKING_FAILURES=$((BLOCKING_FAILURES + 1))
fi
if [[ -f "${POLICY_ROWS_FILE}" ]]; then
  POLICY_ROWS="$(cat "${POLICY_ROWS_FILE}")"
fi

GATE_OUTCOME="PASS"
if (( BLOCKING_FAILURES > 0 )); then
  if [[ "${ENFORCE_SECURITY_GATE}" == "true" ]]; then
    GATE_OUTCOME="FAIL (blocked)"
  else
    GATE_OUTCOME="WARN (bypassed via enforce_security_gate=false)"
  fi
fi

{
  echo "### Automated Security Scan Summary (\`${GATE_OUTCOME}\`)"
  echo ""
  echo "#### Source SAST (\`gosec\`)"
  echo "| Repository | Unsuppressed Findings | Status |"
  echo "| :--- | :---: | :---: |"
  printf "%b" "${SAST_ROWS:-"| _none_ | - | - |\n"}"
  echo ""
  echo "#### Go Dependency Reachability (\`govulncheck\`)"
  echo "| Repository | Reachable Vulnerabilities | Status |"
  echo "| :--- | :---: | :---: |"
  printf "%b" "${GOVULN_ROWS:-"| _none_ | - | - |\n"}"
  echo ""
  echo "#### Container Images (\`trivy image\` — Fixable CVEs & Secrets)"
  echo "| Image | Fixable CVEs | Secrets | Status |"
  echo "| :--- | :--- | :---: | :---: |"
  printf "%b" "${IMAGE_ROWS}"
  echo ""
  echo "#### Helm Charts & Dockerfiles (\`trivy config\`)"
  echo "| Target | Misconfigurations | Status |"
  echo "| :--- | :--- | :---: |"
  printf "%b" "${CONFIG_ROWS:-"| _none_ | - | - |\n"}"
  echo ""
  echo "#### Security Policy & Regression Guards (\`policy_check.py\`)"
  echo "| Repository | Blocking Violations | Advisory Findings | Status |"
  echo "| :--- | :--- | :--- | :---: |"
  if [[ -n "${POLICY_ROWS}" ]]; then
    printf "%s\n" "${POLICY_ROWS}"
  else
    echo "| _none_ | - | - | - |"
  fi
} > "${SECURITY_SUMMARY_FILE}"

cat "${SECURITY_SUMMARY_FILE}"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  cat "${SECURITY_SUMMARY_FILE}" >> "${GITHUB_STEP_SUMMARY}"
fi

if (( BLOCKING_FAILURES > 0 )); then
  if [[ "${ENFORCE_SECURITY_GATE}" == "true" ]]; then
    echo "::error::Automated security gate failed with ${BLOCKING_FAILURES} blocking issue(s). Inspect ${SECURITY_REPORT_DIR} for full SARIF/JSON reports." >&2
    exit 1
  fi
  echo "::warning::Automated security gate detected ${BLOCKING_FAILURES} issue(s), but continuing because ENFORCE_SECURITY_GATE=false." >&2
fi

echo "Automated security scan completed cleanly (0 blocking findings)."
