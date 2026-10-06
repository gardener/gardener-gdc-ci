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

echo "=== Stage 2: Prepare Pipeline Configuration & Bootstrap GDC CLI ==="

CONSOLE_URL="$(yq -r '.gdc.consoleURL' "${PIPELINE_CONFIG_FILE}")"
MGMT_API_URL="$(yq -r '.gdc.managementAPIURL' "${PIPELINE_CONFIG_FILE}")"

SA_FILE="${RELEASE_WORK_DIR}/gdc-sa-key.json"
CA_FILE="${RELEASE_WORK_DIR}/gdc-ca.crt"

setup_harbor_docker_config "${GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON:-}"
setup_gdc_credentials "${SA_FILE}" "${CA_FILE}" "${CONSOLE_URL}" "${GDC_RELEASE_SERVICE_ACCOUNT_KEY:-}"
export GDC_SERVICE_ACCOUNT_FILE="${SA_FILE}"
if [[ -n "${GITHUB_ENV:-}" ]]; then
  echo "GDC_SERVICE_ACCOUNT_FILE=${SA_FILE}" >> "${GITHUB_ENV}"
fi

install_gdcloud_cli "${SA_FILE}" "${CA_FILE}" "${MGMT_API_URL}" "${GDCLOUD_VERSION:-}"

# Fetch GDC CA Data and populate updated-pipeline-configuration.yaml
CA_DATA_B64="$(base64 -w 0 < "${CA_FILE}")"
cp "${PIPELINE_CONFIG_FILE}" "${UPDATED_PIPELINE_CONFIG_FILE}"
yq -i ".gdc.caData = \"${CA_DATA_B64}\"" "${UPDATED_PIPELINE_CONFIG_FILE}"

# Authenticate to all candidate GKE Virtual Garden runtime clusters so LoadReleaseTestConfig
# can inspect existing Garden resources and allocate a free runtime cluster.
GKE_COUNT="$(yq '.gcp.gkeClusters | length' "${UPDATED_PIPELINE_CONFIG_FILE}")"
for ((i = 0; i < GKE_COUNT; i++)); do
  CLUSTER_NAME="$(yq -r ".gcp.gkeClusters[${i}].name" "${UPDATED_PIPELINE_CONFIG_FILE}")"
  CLUSTER_ZONE="$(yq -r ".gcp.gkeClusters[${i}].zone" "${UPDATED_PIPELINE_CONFIG_FILE}")"
  CLUSTER_PROJECT="$(yq -r ".gcp.gkeClusters[${i}].project" "${UPDATED_PIPELINE_CONFIG_FILE}")"
  echo "Fetching GKE kubeconfig for runtime cluster ${CLUSTER_NAME} (${CLUSTER_PROJECT}/${CLUSTER_ZONE})..."
  KUBECONFIG="/tmp/${CLUSTER_NAME}-kubeconfig" gcloud container clusters get-credentials "${CLUSTER_NAME}" \
    --location="${CLUSTER_ZONE}" \
    --project="${CLUSTER_PROJECT}"
done

# Display current cluster pool status via gardener-release-cli
make -C "${REPO_ROOT}" build-local
"${REPO_ROOT}/bin/gardener-release-cli" status --config="${UPDATED_PIPELINE_CONFIG_FILE}" || true

echo "Prepared pipeline configuration at ${UPDATED_PIPELINE_CONFIG_FILE}"
