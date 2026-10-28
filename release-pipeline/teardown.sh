#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

usage() {
  echo "Usage: $0 --gardener-artifacts-version=<version> --config=<path> [extra-go-test-flags...]"
  exit 1
}

GARDENER_ARTIFACTS_VERSION=""
CONFIG=""
EXTRA_ARGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      ;;
    --gardener-artifacts-version)
      if [[ $# -lt 2 ]]; then
        echo "Error: --gardener-artifacts-version requires a value"
        usage
      fi
      GARDENER_ARTIFACTS_VERSION="$2"
      shift 2
      ;;
    --gardener-artifacts-version=*)
      GARDENER_ARTIFACTS_VERSION="${1#*=}"
      shift
      ;;
    --config)
      if [[ $# -lt 2 ]]; then
        echo "Error: --config requires a value"
        usage
      fi
      CONFIG="$2"
      shift 2
      ;;
    --config=*)
      CONFIG="${1#*=}"
      shift
      ;;
    *)
      EXTRA_ARGS+=("$1")
      shift
      ;;
  esac
done

if [[ -z "$GARDENER_ARTIFACTS_VERSION" ]]; then
  echo "Error: --gardener-artifacts-version is required"
  usage
fi

if [[ -z "$CONFIG" ]]; then
  echo "Error: --config is required"
  usage
fi

if [[ -z "${HOME:-}" ]]; then
  HOME=$(pwd)
  export HOME
fi

if ! command -v gdcloud >/dev/null 2>&1; then
  for gdcloud_bin_dir in "${HOME}/Downloads/google-distributed-cloud-hosted-cli/bin" "${HOME}/google-distributed-cloud-hosted-cli/bin" "/usr/local/bin/google-distributed-cloud-hosted-cli/bin"; do
    if [[ -d "${gdcloud_bin_dir}" && ":${PATH}:" != *":${gdcloud_bin_dir}:"* ]]; then
      export PATH="${gdcloud_bin_dir}:${PATH}"
      break
    fi
  done
fi

if [[ -z "${GDC_RELEASE_SERVICE_ACCOUNT_KEY:-}" && -z "${GDC_SERVICE_ACCOUNT_FILE:-}" ]]; then
  GDC_RELEASE_SERVICE_ACCOUNT_KEY="$(gcloud secrets versions access latest --project="${GDC_GCP_PROJECT_ID:-gardener-ci}" --secret=staging-gdc-service-account-keys)"
  export GDC_RELEASE_SERVICE_ACCOUNT_KEY
fi

if [[ -z "${GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON:-}" && -z "${HARBOR_DOCKER_CONFIG_FILE:-}" ]]; then
  GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON="$(gcloud secrets versions access latest --project="${GDC_GCP_PROJECT_ID:-gardener-ci}" --secret=staging-harbor-docker-config-credentials)"
  export GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON
fi

echo "Triggering teardown test..."
echo "Version: $GARDENER_ARTIFACTS_VERSION"
echo "Config: $CONFIG"

go test -v -timeout=65m "${REPO_ROOT}/integration/release/teardown" \
  -args \
  --gardener-artifacts-version="$GARDENER_ARTIFACTS_VERSION" \
  --release-configuration-file-path="$CONFIG" \
  "${EXTRA_ARGS[@]}"
