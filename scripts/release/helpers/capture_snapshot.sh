#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/constants.sh"

# capture_snapshot collects cluster state from Garden, Seed, and Shoot clusters
# when a release pipeline stage fails.
capture_snapshot() {
  local stage_name="${1:-unknown-stage}"
  local run_id="${GITHUB_RUN_ID:-local}"
  local dest_dir="release-pipeline-snapshots/${run_id}/${stage_name}"

  echo "Capturing cluster diagnostic snapshot for failed stage '${stage_name}' into gs://gardener-ci-pipeline/${dest_dir}..."
  if [[ ! -f "${UPDATED_PIPELINE_CONFIG_FILE}" ]]; then
    echo "Updated pipeline config (${UPDATED_PIPELINE_CONFIG_FILE}) not found; skipping cluster snapshot."
    return 0
  fi

  local snapshotter_cmd=(go run "${REPO_ROOT}/integration/cmd/gardener-release-snapshotter")
  if [[ -x "${REPO_ROOT}/bin/gardener-release-snapshotter" ]]; then
    snapshotter_cmd=("${REPO_ROOT}/bin/gardener-release-snapshotter")
  fi

  local artifacts_ver="${GARDENER_ARTIFACTS_VERSION:-}"
  if [[ -z "${artifacts_ver}" && -f "${RELEASE_METADATA_FILE}" ]]; then
    artifacts_ver="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
  fi

  "${snapshotter_cmd[@]}" \
    --release-configuration-file-path="${UPDATED_PIPELINE_CONFIG_FILE}" \
    --snapshot-config-path="${BASE_SNAPSHOT_CONFIG_FILE}" \
    --gardener-artifacts-version="${artifacts_ver}" \
    --dest-dir="${dest_dir}" || \
    echo "Warning: Failed to capture full diagnostic snapshot for stage '${stage_name}'."
}
