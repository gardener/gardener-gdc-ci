#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/helpers/constants.sh"
# shellcheck source=scripts/release/helpers/capture_snapshot.sh
source "${SCRIPT_DIR}/helpers/capture_snapshot.sh"

echo "=== Stage 4: Deploy GDC Seed Cluster (Gardenlet) ==="

GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
MCM_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"

if ! go test -v -timeout=65m "${REPO_ROOT}/integration/release/seed" \
  -args \
  --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
  --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}" \
  --mcm-artifacts-version="${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  --ccm-artifacts-version="${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"; then
  capture_snapshot "4-deploy-seed"
  exit 1
fi
