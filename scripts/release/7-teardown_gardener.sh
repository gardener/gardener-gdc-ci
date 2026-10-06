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

echo "=== Stage 7: Teardown Release Test Environment (Shoot -> Seed -> Release Garden Lock) ==="

if [[ ! -f "${UPDATED_PIPELINE_CONFIG_FILE}" || ! -f "${RELEASE_METADATA_FILE}" ]]; then
  echo "Pipeline configuration or release metadata not found; nothing to tear down."
  exit 0
fi

GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
VIRTUAL_GARDEN_PROVIDER="${VIRTUAL_GARDEN_PROVIDER:-gke}"

TEARDOWN_SUCCESS=false
for attempt in 1 2 3; do
  echo "Running Gardener teardown (attempt ${attempt}/3)..."
  if go test -v -timeout=65m "${REPO_ROOT}/integration/release/teardown" \
    -args \
    --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
    --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}" \
    --virtual-garden-provider="${VIRTUAL_GARDEN_PROVIDER}"; then
    TEARDOWN_SUCCESS=true
    break
  fi
  if [[ "${attempt}" -lt 3 ]]; then
    echo "Warning: Teardown attempt ${attempt} failed. Retrying after 30s..."
    sleep 30
  fi
done

if [[ "${TEARDOWN_SUCCESS}" != "true" ]]; then
  capture_snapshot "7-teardown-gardener"
  exit 1
fi
