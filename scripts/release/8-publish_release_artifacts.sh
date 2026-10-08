#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/helpers/constants.sh"

echo "=== Stage 8: Promote Certified Artifacts from GHCR to Gardener Registry ==="

if [[ ! -f "${RELEASE_METADATA_FILE}" ]]; then
  echo "::error::Release metadata file (${RELEASE_METADATA_FILE}) not found." >&2
  exit 1
fi

RELEASE_MODE="${RELEASE_MODE:-$(yq -r '.releaseMode // "snapshot"' "${RELEASE_METADATA_FILE}")}"
NEXT_VERSION="${NEXT_VERSION:-$(yq -r '.nextVersion // "bump-patch"' "${RELEASE_METADATA_FILE}")}"
GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTENSION_GDC_ARTIFACTS_VERSION="$(yq -r '.extensionGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
MCM_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTERNAL_DNS_ARTIFACTS_VERSION="$(yq -r '.externalDNSArtifactsVersion' "${RELEASE_METADATA_FILE}")"
CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"

EXT_NEEDS_RELEASE="$(yq -r '.components.gardenerExtensionProviderGDC.needsRelease // true' "${RELEASE_METADATA_FILE}")"
MCM_NEEDS_RELEASE="$(yq -r '.components.machineControllerManagerProviderGDC.needsRelease // true' "${RELEASE_METADATA_FILE}")"
CCM_NEEDS_RELEASE="$(yq -r '.components.cloudProviderGDC.needsRelease // true' "${RELEASE_METADATA_FILE}")"

EXT_LAST_TAG="$(yq -r '.components.gardenerExtensionProviderGDC.lastReleaseTag // ""' "${RELEASE_METADATA_FILE}")"
MCM_LAST_TAG="$(yq -r '.components.machineControllerManagerProviderGDC.lastReleaseTag // ""' "${RELEASE_METADATA_FILE}")"
CCM_LAST_TAG="$(yq -r '.components.cloudProviderGDC.lastReleaseTag // ""' "${RELEASE_METADATA_FILE}")"

IMAGES=()

if [[ "${RELEASE_MODE}" == "snapshot" || "${EXT_NEEDS_RELEASE}" == "true" ]]; then
  IMAGES+=(
    "gardener-extension-provider-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
    "gardener-extension-admission-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
  )
else
  echo "Skipping Gardener release registry promotion for gardener-extension-provider-gdc: no code changes since ${EXT_LAST_TAG}."
fi

if [[ "${RELEASE_MODE}" == "snapshot" || "${MCM_NEEDS_RELEASE}" == "true" ]]; then
  IMAGES+=(
    "machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
  )
else
  echo "Skipping Gardener release registry promotion for machine-controller-manager-provider-gdc: no code changes since ${MCM_LAST_TAG}."
fi

if [[ "${RELEASE_MODE}" == "snapshot" || "${CCM_NEEDS_RELEASE}" == "true" ]]; then
  IMAGES+=(
    "cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
  )
else
  echo "Skipping Gardener release registry promotion for cloud-provider-gdc: no code changes since ${CCM_LAST_TAG}."
fi

# Publish tested `external-dnsman2-gdch` image to `${PUBLIC_REGISTRY}` (`snapshots` or `releases/gardener/gardener-gdc-ci`)
# so consumers have the exact certified version alongside the GDC release artifacts.
IMAGES+=(
  "external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}"
)

PUBLISHED_IMAGES_JSON="[]"

for item in "${IMAGES[@]}"; do
  name="${item%%:*}"
  version_tag="${item##*:}"
  src_image="${GHCR_REGISTRY}/${name}:${version_tag}"
  dst_image="${PUBLIC_REGISTRY}/${name}:${version_tag}"

  echo "Promoting ${src_image} -> ${dst_image}..."
  docker pull "${src_image}"
  docker tag "${src_image}" "${dst_image}"
  docker push "${dst_image}"

  PUBLISHED_IMAGES_JSON="$(jq --arg name "${name}" --arg image "${dst_image}" --arg version "${version_tag}" \
    '. + [{"component": $name, "image": $image, "version": $version}]' <<< "${PUBLISHED_IMAGES_JSON}")"
done

# Promote packaged GDC OCI Helm charts to Gardener Registry (only if snapshot mode or extension-provider-gdc changed)
CHARTS_OUT_DIR="${RELEASE_ARTIFACTS_DIR}/charts"
PUBLISHED_CHARTS_JSON="[]"
if [[ "${RELEASE_MODE}" == "snapshot" || "${EXT_NEEDS_RELEASE}" == "true" ]]; then
  if [[ -d "${CHARTS_OUT_DIR}" ]]; then
    for chart_tgz in "${CHARTS_OUT_DIR}"/*.tgz; do
      [[ -e "${chart_tgz}" ]] || continue
      chart_file="$(basename "${chart_tgz}")"
      echo "Promoting Helm chart ${chart_file} -> oci://${PUBLIC_REGISTRY}/charts..."
      if [[ -f "${HOME}/.docker/config.json" ]]; then
        helm push --registry-config "${HOME}/.docker/config.json" "${chart_tgz}" "oci://${PUBLIC_REGISTRY}/charts"
      else
        helm push "${chart_tgz}" "oci://${PUBLIC_REGISTRY}/charts"
      fi
      PUBLISHED_CHARTS_JSON="$(jq --arg chart "oci://${PUBLIC_REGISTRY}/charts/${chart_file%.tgz}" \
        '. + [$chart]' <<< "${PUBLISHED_CHARTS_JSON}")"
    done
  fi
else
  echo "Skipping Gardener release registry Helm chart promotion: no code changes in gardener-extension-provider-gdc since ${EXT_LAST_TAG}."
fi

compute_next_dev_version() {
  local ver="${1#v}"
  local op="$2"
  local major minor patch
  IFS='.' read -r major minor patch <<< "${ver%%-*}"
  case "${op}" in
    bump-patch)
      echo "v${major}.${minor}.$((patch + 1))-dev"
      ;;
    bump-minor)
      echo "v${major}.$((minor + 1)).0-dev"
      ;;
    bump-major)
      echo "v$((major + 1)).0.0-dev"
      ;;
    noop)
      echo ""
      ;;
    *)
      echo "::error::Unsupported next_version operation: ${op}" >&2
      return 1
      ;;
  esac
}

tag_and_bump_gdc_repository() {
  local repo_url="$1"
  local ref="$2"
  local release_ver="$3"
  local next_op="$4"

  local repo_slug="${repo_url#https://github.com/}"
  repo_slug="${repo_slug%.git}"

  local push_token="${GDC_RELEASE_GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [[ -z "${push_token}" ]]; then
    echo "::error::GDC_RELEASE_GITHUB_TOKEN (or GH_TOKEN) is required to tag and bump ${repo_slug} in release mode." >&2
    return 1
  fi

  local auth_url="https://x-access-token:${push_token}@github.com/${repo_slug}.git"
  local work_dir
  work_dir="$(mktemp -d)"

  echo "Tagging ${repo_slug} with release ${release_ver} (next_version=${next_op})..."
  git clone "${auth_url}" "${work_dir}" >/dev/null 2>&1

  local target_branch="main"
  if git -C "${work_dir}" ls-remote --exit-code --heads origin "${ref}" >/dev/null 2>&1; then
    target_branch="${ref}"
  fi
  git -C "${work_dir}" checkout "${target_branch}" >/dev/null 2>&1
  git -C "${work_dir}" config user.name "github-actions[bot]"
  git -C "${work_dir}" config user.email "41898282+github-actions[bot]@users.noreply.github.com"

  # 1. Commit `release vX.Y.Z` (stripping `-dev` in VERSION), tag `vX.Y.Z`, and push
  echo "${release_ver}" > "${work_dir}/VERSION"
  git -C "${work_dir}" add VERSION
  if ! git -C "${work_dir}" diff --cached --quiet; then
    git -C "${work_dir}" commit -s -m "release ${release_ver}"
  fi
  git -C "${work_dir}" tag -f "${release_ver}"
  git -C "${work_dir}" push origin "HEAD:refs/heads/${target_branch}" "refs/tags/${release_ver}"

  # Create GitHub Release if not already present
  if ! GH_TOKEN="${push_token}" gh release view "${release_ver}" --repo "${repo_slug}" >/dev/null 2>&1; then
    GH_TOKEN="${push_token}" gh release create "${release_ver}" \
      --repo "${repo_slug}" \
      --title "${release_ver}" \
      --generate-notes
  fi

  # 2. Bump `VERSION` to next `-dev` version and push directly to target branch
  if [[ "${next_op}" != "noop" ]]; then
    local next_dev_ver
    next_dev_ver="$(compute_next_dev_version "${release_ver}" "${next_op}")"
    if [[ -n "${next_dev_ver}" ]]; then
      echo "${next_dev_ver}" > "${work_dir}/VERSION"
      git -C "${work_dir}" add VERSION
      if ! git -C "${work_dir}" diff --cached --quiet; then
        git -C "${work_dir}" commit -s -m "next version: ${next_dev_ver}"
        git -C "${work_dir}" push origin "HEAD:refs/heads/${target_branch}"
      fi
    fi
  fi

  rm -rf "${work_dir}"
}

if [[ "${RELEASE_MODE}" == "release" ]]; then
  echo "=== Creating Release Git Tags & Bumping VERSION in Changed GDC Repositories ==="
  EXT_REPO="$(yq -r '.components.gardenerExtensionProviderGDC.repository' "${RELEASE_METADATA_FILE}")"
  EXT_REF="$(yq -r '.components.gardenerExtensionProviderGDC.ref' "${RELEASE_METADATA_FILE}")"
  MCM_REPO="$(yq -r '.components.machineControllerManagerProviderGDC.repository' "${RELEASE_METADATA_FILE}")"
  MCM_REF="$(yq -r '.components.machineControllerManagerProviderGDC.ref' "${RELEASE_METADATA_FILE}")"
  CCM_REPO="$(yq -r '.components.cloudProviderGDC.repository' "${RELEASE_METADATA_FILE}")"
  CCM_REF="$(yq -r '.components.cloudProviderGDC.ref' "${RELEASE_METADATA_FILE}")"

  if [[ "${EXT_NEEDS_RELEASE}" == "true" ]]; then
    tag_and_bump_gdc_repository "${EXT_REPO}" "${EXT_REF}" "${EXTENSION_GDC_ARTIFACTS_VERSION}" "${NEXT_VERSION}"
  else
    echo "Skipping Git tag and VERSION bump for ${EXT_REPO}: no code changes since ${EXT_LAST_TAG}."
  fi

  if [[ "${MCM_NEEDS_RELEASE}" == "true" ]]; then
    tag_and_bump_gdc_repository "${MCM_REPO}" "${MCM_REF}" "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" "${NEXT_VERSION}"
  else
    echo "Skipping Git tag and VERSION bump for ${MCM_REPO}: no code changes since ${MCM_LAST_TAG}."
  fi

  if [[ "${CCM_NEEDS_RELEASE}" == "true" ]]; then
    tag_and_bump_gdc_repository "${CCM_REPO}" "${CCM_REF}" "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" "${NEXT_VERSION}"
  else
    echo "Skipping Git tag and VERSION bump for ${CCM_REPO}: no code changes since ${CCM_LAST_TAG}."
  fi
fi

jq -n \
  --arg releaseMode "${RELEASE_MODE}" \
  --arg releaseVersion "${GARDENER_ARTIFACTS_VERSION}" \
  --arg certifiedAt "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
  --arg workflowRunId "${GITHUB_RUN_ID:-local}" \
  --arg publicRegistry "${PUBLIC_REGISTRY}" \
  --argjson images "${PUBLISHED_IMAGES_JSON}" \
  --argjson charts "${PUBLISHED_CHARTS_JSON}" \
  '{
    releaseMode: $releaseMode,
    releaseVersion: $releaseVersion,
    certifiedAt: $certifiedAt,
    workflowRunId: $workflowRunId,
    publicRegistry: $publicRegistry,
    images: $images,
    charts: $charts
  }' > "${CERTIFIED_RELEASE_MANIFEST}"

echo "Certified artifacts promoted to ${PUBLIC_REGISTRY}. Manifest written to ${CERTIFIED_RELEASE_MANIFEST}:"
cat "${CERTIFIED_RELEASE_MANIFEST}"
