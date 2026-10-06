#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/helpers/constants.sh"

echo "=== Stage 8: Promote Certified Artifacts from GHCR to Gardener Public Registry ==="

if [[ ! -f "${RELEASE_METADATA_FILE}" ]]; then
  echo "::error::Release metadata file (${RELEASE_METADATA_FILE}) not found." >&2
  exit 1
fi

GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTENSION_GDC_ARTIFACTS_VERSION="$(yq -r '.extensionGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
MCM_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTERNAL_DNS_ARTIFACTS_VERSION="$(yq -r '.externalDNSArtifactsVersion' "${RELEASE_METADATA_FILE}")"
CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
RELEASE_VERSION="${RELEASE_VERSION:-v0.1.0-${GARDENER_ARTIFACTS_VERSION}}"

IMAGES=(
  "gardener-extension-provider-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
  "gardener-extension-admission-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
  "machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
  "external-dns-management-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}"
  "external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}"
  "cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
)

PUBLISHED_IMAGES_JSON="[]"

for item in "${IMAGES[@]}"; do
  name="${item%%:*}"
  candidate_tag="${item##*:}"
  src_image="${GHCR_REGISTRY}/${name}:${candidate_tag}"
  dst_image_sha="${PUBLIC_REGISTRY}/${name}:${candidate_tag}"
  dst_image_ver="${PUBLIC_REGISTRY}/${name}:${RELEASE_VERSION}"

  echo "Promoting ${src_image} -> ${dst_image_sha} and ${dst_image_ver}..."
  docker pull "${src_image}"
  docker tag "${src_image}" "${dst_image_sha}"
  docker tag "${src_image}" "${dst_image_ver}"
  docker push "${dst_image_sha}"
  docker push "${dst_image_ver}"

  PUBLISHED_IMAGES_JSON="$(jq --arg name "${name}" --arg shaTag "${dst_image_sha}" --arg verTag "${dst_image_ver}" \
    '. + [{"component": $name, "shaImage": $shaTag, "releaseImage": $verTag}]' <<< "${PUBLISHED_IMAGES_JSON}")"
done

# Promote packaged OCI Helm charts to Gardener Public Registry
CHARTS_OUT_DIR="${RELEASE_ARTIFACTS_DIR}/charts"
PUBLISHED_CHARTS_JSON="[]"
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

jq -n \
  --arg releaseVersion "${RELEASE_VERSION}" \
  --arg certifiedAt "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
  --arg workflowRunId "${GITHUB_RUN_ID:-local}" \
  --arg publicRegistry "${PUBLIC_REGISTRY}" \
  --argjson images "${PUBLISHED_IMAGES_JSON}" \
  --argjson charts "${PUBLISHED_CHARTS_JSON}" \
  '{
    releaseVersion: $releaseVersion,
    certifiedAt: $certifiedAt,
    workflowRunId: $workflowRunId,
    publicRegistry: $publicRegistry,
    images: $images,
    charts: $charts
  }' > "${CERTIFIED_RELEASE_MANIFEST}"

echo "Certified artifacts promoted to ${PUBLIC_REGISTRY}. Manifest written to ${CERTIFIED_RELEASE_MANIFEST}:"
cat "${CERTIFIED_RELEASE_MANIFEST}"
