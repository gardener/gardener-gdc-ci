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

echo "=== Stage 3: Select Virtual Garden & Deploy Operator Extension ==="

GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
MCM_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"

if ! go test -v -timeout=10m "${REPO_ROOT}/integration/release/garden" \
  -args \
  --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
  --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}" \
  --mcm-artifacts-version="${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  --ccm-artifacts-version="${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"; then
  capture_snapshot "3-deploy-gardener"
  exit 1
fi
