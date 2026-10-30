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

echo "=== Stage 5: Provision Shoot Cluster on GDC ==="

GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTERNAL_DNS_ARTIFACTS_VERSION="$(yq -r '.externalDNSArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTERNAL_DNS_MANAGEMENT_IMAGE_NAME="${EXTERNAL_DNS_MANAGEMENT_IMAGE_NAME:-external-dnsman2-gdch}"
VIRTUAL_GARDEN_PROVIDER="${VIRTUAL_GARDEN_PROVIDER:-gke}"
SUBNET_PREFIX_LENGTH="${SUBNET_PREFIX_LENGTH:-27}"
NUM_SHOOTS="${NUM_SHOOTS:-1}"
WORKER_POOL_MINIMUM="${WORKER_POOL_MINIMUM:-4}"
WORKER_POOL_MAXIMUM="${WORKER_POOL_MAXIMUM:-4}"

if ! go test -v -timeout=55m "${REPO_ROOT}/integration/release/shoot" \
  -args \
  --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
  --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}" \
  --external-dns-artifacts-version="${EXTERNAL_DNS_ARTIFACTS_VERSION}" \
  --external-dns-management-image-name="${EXTERNAL_DNS_MANAGEMENT_IMAGE_NAME}" \
  --virtual-garden-provider="${VIRTUAL_GARDEN_PROVIDER}" \
  --subnet-prefix-length="${SUBNET_PREFIX_LENGTH}" \
  --num-shoots="${NUM_SHOOTS}" \
  --worker-pool-minimum="${WORKER_POOL_MINIMUM}" \
  --worker-pool-maximum="${WORKER_POOL_MAXIMUM}"; then
  capture_snapshot "5-deploy-shoot"
  exit 1
fi
