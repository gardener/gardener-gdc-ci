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
# shellcheck source=scripts/ci-common.sh
source "${REPO_ROOT}/scripts/ci-common.sh"

echo "=== Stage 6: Run Shoot Functional & CNCF Conformance Test Suites ==="

GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
VIRTUAL_GARDEN_PROVIDER="${VIRTUAL_GARDEN_PROVIDER:-gke}"
SKIP_CONFORMANCE="${SKIP_CONFORMANCE:-false}"

# Ensure Sonobuoy CLI is installed for conformance testing
if [[ "${SKIP_CONFORMANCE}" != "true" ]]; then
  install_sonobuoy_cli
fi

SUITES=(
  "shootscaling:40m"
  "storage:60m"
  "networking:30m"
  "loadbalancer:30m"
  "dns:30m"
  "etcdbackup:30m"
)

FAILED_SUITES=()

for entry in "${SUITES[@]}"; do
  suite="${entry%%:*}"
  timeout="${entry##*:}"
  echo "--- Running Shoot E2E Suite: ${suite} (timeout=${timeout}) ---"
  if ! go test -v -timeout="${timeout}" "${REPO_ROOT}/integration/release/${suite}" \
    -args \
    --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
    --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}" \
    --virtual-garden-provider="${VIRTUAL_GARDEN_PROVIDER}"; then
    echo "Warning: Shoot test suite '${suite}' failed on first attempt. Retrying after 30s..."
    sleep 30
    if ! go test -v -timeout="${timeout}" "${REPO_ROOT}/integration/release/${suite}" \
      -args \
      --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
      --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}" \
      --virtual-garden-provider="${VIRTUAL_GARDEN_PROVIDER}"; then
      echo "::error::Shoot test suite '${suite}' failed."
      FAILED_SUITES+=("${suite}")
    fi
  fi
done

if [[ "${SKIP_CONFORMANCE}" != "true" ]]; then
  echo "--- Running CNCF Kubernetes Conformance Suite (Sonobuoy) ---"
  if ! go test -v -timeout=120m "${REPO_ROOT}/integration/release/conformance" \
    -args \
    --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
    --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}"; then
    echo "Warning: Shoot test suite 'conformance' failed on first attempt. Retrying after 30s..."
    sleep 30
    if ! go test -v -timeout=120m "${REPO_ROOT}/integration/release/conformance" \
      -args \
      --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
      --gardener-artifacts-version="${GARDENER_ARTIFACTS_VERSION}"; then
      echo "::error::Shoot test suite 'conformance' failed."
      FAILED_SUITES+=("conformance")
    fi
  fi
fi

if [[ "${#FAILED_SUITES[@]}" -gt 0 ]]; then
  echo "::error::Failed Shoot E2E suites: ${FAILED_SUITES[*]}"
  capture_snapshot "6-test-shoot"
  exit 1
fi

echo "All Shoot functional and conformance test suites passed!"
