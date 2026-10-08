#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/helpers/constants.sh"
# shellcheck source=scripts/ci-common.sh
source "${REPO_ROOT}/scripts/ci-common.sh"

echo "=== Stage 1: Build and Push Candidate Artifacts (GitHub -> GHCR) ==="

BUILD_WORK_DIR="${RELEASE_WORK_DIR}/src"
mkdir -p "${BUILD_WORK_DIR}"

clone_github_repo() {
  local repo_url="$1"
  local ref="$2"
  local target_dir="$3"

  rm -rf "${target_dir}"
  echo "Cloning ${repo_url} (ref: ${ref})..." >&2
  git clone --branch "${ref}" --tags "${repo_url}" "${target_dir}" >/dev/null 2>&1 || {
    git clone --tags "${repo_url}" "${target_dir}" >&2
    git -C "${target_dir}" checkout "${ref}" >&2
  }
  git -C "${target_dir}" rev-parse --short=7 HEAD
}

# 1. Clone GitHub repositories (no Git-on-Borg mirror repositories)
EXT_DIR="${BUILD_WORK_DIR}/gardener-extension-provider-gdc"
MCM_GDC_DIR="${BUILD_WORK_DIR}/machine-controller-manager-provider-gdc"
DNS_DIR="${BUILD_WORK_DIR}/external-dns-management"
CCM_GDC_DIR="${BUILD_WORK_DIR}/cloud-provider-gdc"

EXT_SHA="$(clone_github_repo "${EXTENSION_GDC_REPO}" "${EXTENSION_GDC_REF}" "${EXT_DIR}")"
MCM_GDC_SHA="$(clone_github_repo "${MCM_PROVIDER_GDC_REPO}" "${MCM_PROVIDER_GDC_REF}" "${MCM_GDC_DIR}")"
DNS_SHA="$(clone_github_repo "${EXTERNAL_DNS_REPO}" "${EXTERNAL_DNS_REF}" "${DNS_DIR}")"
CCM_GDC_SHA="$(clone_github_repo "${CLOUD_PROVIDER_GDC_REPO}" "${CLOUD_PROVIDER_GDC_REF}" "${CCM_GDC_DIR}")"

read_repo_version() {
  local repo_dir="$1"
  if [[ ! -f "${repo_dir}/VERSION" ]]; then
    echo "::error::VERSION file not found in ${repo_dir}" >&2
    return 1
  fi
  tr -d '[:space:]' < "${repo_dir}/VERSION"
}

get_last_release_tag() {
  local repo_dir="$1"
  git -C "${repo_dir}" describe --tags --abbrev=0 --match 'v[0-9]*' 2>/dev/null || echo ""
}

has_code_changes_since_tag() {
  local repo_dir="$1"
  local last_tag="$2"
  if [[ -z "${last_tag}" ]]; then
    return 0
  fi
  if git -C "${repo_dir}" diff --quiet "${last_tag}"..HEAD -- . ':!VERSION'; then
    return 1
  fi
  return 0
}

RELEASE_MODE="${RELEASE_MODE:-snapshot}"
NEXT_VERSION="${NEXT_VERSION:-bump-patch}"

EXT_BASE_VERSION="$(read_repo_version "${EXT_DIR}")"
MCM_GDC_BASE_VERSION="$(read_repo_version "${MCM_GDC_DIR}")"
CCM_GDC_BASE_VERSION="$(read_repo_version "${CCM_GDC_DIR}")"
DNS_BASE_VERSION="$(read_repo_version "${DNS_DIR}")"

EXT_LAST_TAG="$(get_last_release_tag "${EXT_DIR}")"
MCM_LAST_TAG="$(get_last_release_tag "${MCM_GDC_DIR}")"
CCM_LAST_TAG="$(get_last_release_tag "${CCM_GDC_DIR}")"

EXT_NEEDS_RELEASE="true"
MCM_NEEDS_RELEASE="true"
CCM_NEEDS_RELEASE="true"

# Compute SemVer 2.0 artifact version tags from each repository's VERSION file:
# - In `release` mode: check if code changed since the last published release tag (excluding VERSION).
#   - If changed (or no prior tag): strip `-dev` (`vX.Y.Z`) and set `needsRelease=true`.
#   - If unchanged: reuse `<last_tag>` for E2E testing and set `needsRelease=false` so Stage 8 skips
#     pushing duplicate artifacts to `releases`, creating tags, or bumping `VERSION`.
# - In `snapshot` mode: use `<VERSION>-<short_sha>` (`vX.Y.Z-dev-<sha>`).
# - `external-dns-management` is upstream-managed and always uses `<VERSION>-<short_sha>`.
if [[ "${RELEASE_MODE}" == "release" ]]; then
  if has_code_changes_since_tag "${EXT_DIR}" "${EXT_LAST_TAG}"; then
    EXTENSION_GDC_ARTIFACTS_VERSION="${EXTENSION_GDC_ARTIFACTS_VERSION:-${EXT_BASE_VERSION%-dev}}"
  else
    EXT_NEEDS_RELEASE="false"
    EXTENSION_GDC_ARTIFACTS_VERSION="${EXTENSION_GDC_ARTIFACTS_VERSION:-${EXT_LAST_TAG}}"
  fi

  if has_code_changes_since_tag "${MCM_GDC_DIR}" "${MCM_LAST_TAG}"; then
    MCM_PROVIDER_GDC_ARTIFACTS_VERSION="${MCM_PROVIDER_GDC_ARTIFACTS_VERSION:-${MCM_GDC_BASE_VERSION%-dev}}"
  else
    MCM_NEEDS_RELEASE="false"
    MCM_PROVIDER_GDC_ARTIFACTS_VERSION="${MCM_PROVIDER_GDC_ARTIFACTS_VERSION:-${MCM_LAST_TAG}}"
  fi

  if has_code_changes_since_tag "${CCM_GDC_DIR}" "${CCM_LAST_TAG}"; then
    CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION:-${CCM_GDC_BASE_VERSION%-dev}}"
  else
    CCM_NEEDS_RELEASE="false"
    CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION:-${CCM_LAST_TAG}}"
  fi
else
  EXTENSION_GDC_ARTIFACTS_VERSION="${EXTENSION_GDC_ARTIFACTS_VERSION:-${EXT_BASE_VERSION}-${EXT_SHA}}"
  MCM_PROVIDER_GDC_ARTIFACTS_VERSION="${MCM_PROVIDER_GDC_ARTIFACTS_VERSION:-${MCM_GDC_BASE_VERSION}-${MCM_GDC_SHA}}"
  CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION:-${CCM_GDC_BASE_VERSION}-${CCM_GDC_SHA}}"
fi
EXTERNAL_DNS_ARTIFACTS_VERSION="${EXTERNAL_DNS_ARTIFACTS_VERSION:-${DNS_BASE_VERSION}-${DNS_SHA}}"
GARDENER_ARTIFACTS_VERSION="${GARDENER_ARTIFACTS_VERSION:-${EXTENSION_GDC_ARTIFACTS_VERSION}}"
# Drop leading 'v' for Helm chart SemVer
CHART_VERSION="${EXTENSION_GDC_ARTIFACTS_VERSION#v}"

echo "Resolved artifact versions (RELEASE_MODE=${RELEASE_MODE}):"
echo "  gardener-extension-provider-gdc:         ${EXTENSION_GDC_ARTIFACTS_VERSION} (chart: ${CHART_VERSION}, lastTag=${EXT_LAST_TAG:-none}, needsRelease=${EXT_NEEDS_RELEASE})"
echo "  machine-controller-manager-provider-gdc: ${MCM_PROVIDER_GDC_ARTIFACTS_VERSION} (lastTag=${MCM_LAST_TAG:-none}, needsRelease=${MCM_NEEDS_RELEASE})"
echo "  cloud-provider-gdc:                      ${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION} (lastTag=${CCM_LAST_TAG:-none}, needsRelease=${CCM_NEEDS_RELEASE})"
echo "  external-dns-management (snapshot):      ${EXTERNAL_DNS_ARTIFACTS_VERSION}"

# Helper to push a candidate image to GHCR
push_candidate_image() {
  local local_image="$1"
  local component_name="$2"
  local tag="$3"

  local ghcr_ref="${GHCR_REGISTRY}/${component_name}:${tag}"

  echo "Pushing candidate image ${ghcr_ref} to GitHub Container Registry..."
  docker tag "${local_image}" "${ghcr_ref}"
  docker push "${ghcr_ref}"
}

# Helper to package and push Helm charts with the `-helm` name suffix expected by
# integration/release/garden and integration/release/seed to GHCR.
package_and_push_helm_chart() {
  local chart_src_dir="$1"
  local chart_ver="$2"
  local out_dir="$3"

  local temp_dir
  temp_dir="$(mktemp -d)"
  cp -r "${chart_src_dir}"/* "${temp_dir}/"

  sed -i "s/^name: \(.*\)/name: \1-helm/" "${temp_dir}/Chart.yaml"
  sed -i "s/^version: .*/version: ${chart_ver}/" "${temp_dir}/Chart.yaml"

  helm package "${temp_dir}" --destination "${out_dir}"

  local chart_name
  chart_name="$(grep '^name:' "${temp_dir}/Chart.yaml" | awk '{print $2}')"
  local pkg_file="${out_dir}/${chart_name}-${chart_ver}.tgz"

  echo "Pushing ${pkg_file} to oci://${GHCR_REGISTRY} and oci://${GHCR_REGISTRY}/charts..."
  helm push "${pkg_file}" "oci://${GHCR_REGISTRY}"
  helm push "${pkg_file}" "oci://${GHCR_REGISTRY}/charts"

  rm -rf "${temp_dir}"
}

# 2. Build & Push gardener-extension-provider-gdc images and Helm charts
echo "Building gardener-extension-provider-gdc (${EXT_SHA})..."
IMAGE_TAG="${EXTENSION_GDC_ARTIFACTS_VERSION}" make -C "${EXT_DIR}" docker-images

push_candidate_image "${PUBLIC_REGISTRY}/gardener-extension-provider-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}" \
  "gardener-extension-provider-gdch" "${EXTENSION_GDC_ARTIFACTS_VERSION}"
push_candidate_image "${PUBLIC_REGISTRY}/gardener-extension-admission-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}" \
  "gardener-extension-admission-gdch" "${EXTENSION_GDC_ARTIFACTS_VERSION}"

CHARTS_OUT_DIR="${RELEASE_ARTIFACTS_DIR}/charts"
TEST_CHARTS_OUT_DIR="${RELEASE_WORK_DIR}/test-charts"
mkdir -p "${CHARTS_OUT_DIR}" "${TEST_CHARTS_OUT_DIR}"
package_and_push_helm_chart "${EXT_DIR}/charts/extension-provider" "${CHART_VERSION}" "${CHARTS_OUT_DIR}"
package_and_push_helm_chart "${EXT_DIR}/charts/extension-admission/charts/application" "${CHART_VERSION}" "${CHARTS_OUT_DIR}"
package_and_push_helm_chart "${EXT_DIR}/charts/extension-admission/charts/runtime" "${CHART_VERSION}" "${CHARTS_OUT_DIR}"

# 3. Build & Push machine-controller-manager-provider-gdc
echo "Building machine-controller-manager-provider-gdc (${MCM_GDC_SHA})..."
IMAGE_TAG="${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" make -C "${MCM_GDC_DIR}" docker-images
push_candidate_image "${PUBLIC_REGISTRY}/machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  "machine-controller-manager-provider-gdch" "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"

# 4. Build & Push external-dns-management (next-gen dnsman2 target)
echo "Building external-dnsman2-gdch (${DNS_SHA})..."
docker build -t "${PUBLIC_REGISTRY}/external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}" \
  -f "${DNS_DIR}/Dockerfile" --target dns-controller-manager-next-generation "${DNS_DIR}"

push_candidate_image "${PUBLIC_REGISTRY}/external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}" \
  "external-dnsman2-gdch" "${EXTERNAL_DNS_ARTIFACTS_VERSION}"

# 5. Build & Push GoogleCloudPlatform/cloud-provider-gdc
echo "Building cloud-provider-gdc (${CCM_GDC_SHA})..."
IMAGE_TAG="${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" make -C "${CCM_GDC_DIR}" docker-images
push_candidate_image "${PUBLIC_REGISTRY}/cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  "cloud-controller-manager-gdch" "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"

# 6. Build & Push Supporting Test Helm Charts (gardenlet, shoot-dns-service, networking-cilium, os-gardenlinux) to GHCR
GARDENER_VERSION="$(yq -r '.gardener.gardenerVersion' "${PIPELINE_CONFIG_FILE}")"
GARDENLET_CHART_VERSION="${GARDENER_VERSION#v}"
echo "Building and pushing gardenlet Helm chart (${GARDENER_VERSION}) to oci://${GHCR_REGISTRY}/private-cloud..."
GARDENER_TEMP_DIR="$(mktemp -d)"
git clone --depth 1 --branch "${GARDENER_VERSION}" https://github.com/gardener/gardener.git "${GARDENER_TEMP_DIR}"
CLUSTERROLE_FILE="${GARDENER_TEMP_DIR}/charts/gardener/gardenlet/templates/clusterrole-gardenlet.yaml"
awk '
  /^- apiGroups:/ { in_apiext = 0 }
  /- apiextensions.k8s.io/ { in_apiext = 1 }
  in_apiext && /- create/ {
    print
    print "  - delete"
    print "  - deletecollection"
    in_apiext = 0
    next
  }
  { print }
' "${CLUSTERROLE_FILE}" > "${CLUSTERROLE_FILE}.tmp" && mv "${CLUSTERROLE_FILE}.tmp" "${CLUSTERROLE_FILE}"
helm package "${GARDENER_TEMP_DIR}/charts/gardener/gardenlet" --version "${GARDENLET_CHART_VERSION}" --destination "${TEST_CHARTS_OUT_DIR}"
helm push "${TEST_CHARTS_OUT_DIR}/gardenlet-${GARDENLET_CHART_VERSION}.tgz" "oci://${GHCR_REGISTRY}/private-cloud"
rm -rf "${GARDENER_TEMP_DIR}"

SHOOT_DNS_SERVICE_VERSION="$(yq -r '.gardener.externalDNSExtension.version' "${PIPELINE_CONFIG_FILE}")"
SHOOT_DNS_SERVICE_CHART_REF="$(yq -r '.gardener.externalDNSExtension.helmChartRef' "${PIPELINE_CONFIG_FILE}")"
SHOOT_DNS_CHART_VERSION="${SHOOT_DNS_SERVICE_CHART_REF##*:}"
echo "Building and pushing gardener-extension-shoot-dns-service Helm chart (${SHOOT_DNS_SERVICE_VERSION}) to oci://${GHCR_REGISTRY}..."
DNS_EXT_TEMP_DIR="$(mktemp -d)"
git clone --depth 1 --branch "${SHOOT_DNS_SERVICE_VERSION}" https://github.com/gardener/gardener-extension-shoot-dns-service.git "${DNS_EXT_TEMP_DIR}"
git -C "${DNS_EXT_TEMP_DIR}" apply - <<'PATCH_EOF'
diff --git a/charts/gardener-extension-shoot-dns-service/templates/dnsman-clusterrole.yaml b/charts/gardener-extension-shoot-dns-service/templates/dnsman-clusterrole.yaml
--- a/charts/gardener-extension-shoot-dns-service/templates/dnsman-clusterrole.yaml
+++ b/charts/gardener-extension-shoot-dns-service/templates/dnsman-clusterrole.yaml
@@ -67,6 +67,8 @@ rules:
   - dnslocks/status
   - remoteaccesscertificates
   - remoteaccesscertificates/status
+  - dnsowners
+  - dnsowners/status
   verbs:
   - get
   - list
@@ -104,6 +106,7 @@ rules:
   - patch
   - create
   - watch
+  - delete
 - apiGroups:
   - ""
   resources:
diff --git a/charts/gardener-extension-shoot-dns-service/templates/dnsman-crds.yaml b/charts/gardener-extension-shoot-dns-service/templates/dnsman-crds.yaml
--- a/charts/gardener-extension-shoot-dns-service/templates/dnsman-crds.yaml
+++ b/charts/gardener-extension-shoot-dns-service/templates/dnsman-crds.yaml
@@ -1,4 +1,4 @@
-{{- if and .Values.dnsControllerManager.deploy .Values.dnsControllerManager.createCRDs }}
+{{- if .Values.dnsControllerManager.createCRDs }}
 ---
 apiVersion: apiextensions.k8s.io/v1
 kind: CustomResourceDefinition
PATCH_EOF
helm package "${DNS_EXT_TEMP_DIR}/charts/gardener-extension-shoot-dns-service" --version "${SHOOT_DNS_CHART_VERSION}" --destination "${TEST_CHARTS_OUT_DIR}"
helm push "${TEST_CHARTS_OUT_DIR}/gardener-extension-shoot-dns-service-${SHOOT_DNS_CHART_VERSION}.tgz" "oci://${GHCR_REGISTRY}"
rm -rf "${DNS_EXT_TEMP_DIR}"

CILIUM_EXT_VERSION="$(yq -r '.gardener.ciliumExtension.version' "${PIPELINE_CONFIG_FILE}")"
CILIUM_CHART_REF="$(yq -r '.gardener.ciliumExtension.helmChartRef' "${PIPELINE_CONFIG_FILE}")"
CILIUM_CHART_VERSION="${CILIUM_CHART_REF##*:}"
echo "Building and pushing gardener-extension-networking-cilium Helm chart (${CILIUM_EXT_VERSION}) to oci://${GHCR_REGISTRY}..."
CILIUM_TEMP_DIR="$(mktemp -d)"
git clone --depth 1 --branch "${CILIUM_EXT_VERSION}" https://github.com/gardener/gardener-extension-networking-cilium.git "${CILIUM_TEMP_DIR}"
helm package "${CILIUM_TEMP_DIR}/charts/gardener-extension-networking-cilium" --version "${CILIUM_CHART_VERSION}" --destination "${TEST_CHARTS_OUT_DIR}"
helm push "${TEST_CHARTS_OUT_DIR}/gardener-extension-networking-cilium-${CILIUM_CHART_VERSION}.tgz" "oci://${GHCR_REGISTRY}"
rm -rf "${CILIUM_TEMP_DIR}"

GARDENLINUX_EXT_VERSION="$(yq -r '.gardener.gardenlinuxExtension.version' "${PIPELINE_CONFIG_FILE}")"
GARDENLINUX_CHART_REF="$(yq -r '.gardener.gardenlinuxExtension.helmChartRef' "${PIPELINE_CONFIG_FILE}")"
GARDENLINUX_CHART_VERSION="${GARDENLINUX_CHART_REF##*:}"
echo "Building and pushing gardener-extension-os-gardenlinux Helm chart (${GARDENLINUX_EXT_VERSION}) to oci://${GHCR_REGISTRY}..."
GARDENLINUX_TEMP_DIR="$(mktemp -d)"
git clone --depth 1 --branch "${GARDENLINUX_EXT_VERSION}" https://github.com/gardener/gardener-extension-os-gardenlinux.git "${GARDENLINUX_TEMP_DIR}"
helm package "${GARDENLINUX_TEMP_DIR}/charts/gardener-extension-os-gardenlinux" --version "${GARDENLINUX_CHART_VERSION}" --destination "${TEST_CHARTS_OUT_DIR}"
helm push "${TEST_CHARTS_OUT_DIR}/gardener-extension-os-gardenlinux-${GARDENLINUX_CHART_VERSION}.tgz" "oci://${GHCR_REGISTRY}"
rm -rf "${GARDENLINUX_TEMP_DIR}"

# 7. Write release-metadata.yaml for downstream pipeline stages and promotion
cat > "${RELEASE_METADATA_FILE}" <<EOF
releaseMode: "${RELEASE_MODE}"
nextVersion: "${NEXT_VERSION}"
gardenerArtifactsVersion: "${GARDENER_ARTIFACTS_VERSION}"
extensionGDCArtifactsVersion: "${EXTENSION_GDC_ARTIFACTS_VERSION}"
mcmProviderGDCArtifactsVersion: "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
externalDNSArtifactsVersion: "${EXTERNAL_DNS_ARTIFACTS_VERSION}"
cloudProviderGDCArtifactsVersion: "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
candidateRegistry: "${GHCR_REGISTRY}"
publicRegistry: "${PUBLIC_REGISTRY}"
components:
  gardenerExtensionProviderGDC:
    repository: "${EXTENSION_GDC_REPO}"
    ref: "${EXTENSION_GDC_REF}"
    commitSHA: "${EXT_SHA}"
    version: "${EXTENSION_GDC_ARTIFACTS_VERSION}"
    lastReleaseTag: "${EXT_LAST_TAG}"
    needsRelease: ${EXT_NEEDS_RELEASE}
    images:
      - name: "gardener-extension-provider-gdch"
        tag: "${EXTENSION_GDC_ARTIFACTS_VERSION}"
      - name: "gardener-extension-admission-gdch"
        tag: "${EXTENSION_GDC_ARTIFACTS_VERSION}"
  machineControllerManagerProviderGDC:
    repository: "${MCM_PROVIDER_GDC_REPO}"
    ref: "${MCM_PROVIDER_GDC_REF}"
    commitSHA: "${MCM_GDC_SHA}"
    version: "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
    lastReleaseTag: "${MCM_LAST_TAG}"
    needsRelease: ${MCM_NEEDS_RELEASE}
    images:
      - name: "machine-controller-manager-provider-gdch"
        tag: "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
  externalDNSManagement:
    repository: "${EXTERNAL_DNS_REPO}"
    ref: "${EXTERNAL_DNS_REF}"
    commitSHA: "${DNS_SHA}"
    version: "${EXTERNAL_DNS_ARTIFACTS_VERSION}"
    images:
      - name: "external-dnsman2-gdch"
        tag: "${EXTERNAL_DNS_ARTIFACTS_VERSION}"
  cloudProviderGDC:
    repository: "${CLOUD_PROVIDER_GDC_REPO}"
    ref: "${CLOUD_PROVIDER_GDC_REF}"
    commitSHA: "${CCM_GDC_SHA}"
    version: "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
    lastReleaseTag: "${CCM_LAST_TAG}"
    needsRelease: ${CCM_NEEDS_RELEASE}
    images:
      - name: "cloud-controller-manager-gdch"
        tag: "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
EOF

echo "Release metadata written to ${RELEASE_METADATA_FILE}:"
cat "${RELEASE_METADATA_FILE}"
