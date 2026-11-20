#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_CONFIG="${REPO_ROOT}/pipeline-configurations/staging-release-pipeline-configuration.yaml"

usage() {
  echo "Usage: $0 --gardener-artifacts-version=<version> [--config=<path>] [--virtual-garden-provider=<gdc|gke>] [--num-shoots=<n>]"
}

GARDENER_ARTIFACTS_VERSION=""
CONFIG="$DEFAULT_CONFIG"
VIRTUAL_GARDEN_PROVIDER="${VIRTUAL_GARDEN_PROVIDER:-gdc}"
NUM_SHOOTS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --gardener-artifacts-version)
      if [[ $# -lt 2 ]]; then
        echo "Error: --gardener-artifacts-version requires a value" >&2
        usage >&2
        exit 1
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
        echo "Error: --config requires a value" >&2
        usage >&2
        exit 1
      fi
      CONFIG="$2"
      shift 2
      ;;
    --config=*)
      CONFIG="${1#*=}"
      shift
      ;;
    --virtual-garden-provider)
      if [[ $# -lt 2 ]]; then
        echo "Error: --virtual-garden-provider requires a value" >&2
        usage >&2
        exit 1
      fi
      VIRTUAL_GARDEN_PROVIDER="$2"
      shift 2
      ;;
    --virtual-garden-provider=*)
      VIRTUAL_GARDEN_PROVIDER="${1#*=}"
      shift
      ;;
    --num-shoots)
      if [[ $# -lt 2 ]]; then
        echo "Error: --num-shoots requires a value" >&2
        usage >&2
        exit 1
      fi
      NUM_SHOOTS="$2"
      shift 2
      ;;
    --num-shoots=*)
      NUM_SHOOTS="${1#*=}"
      shift
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "$GARDENER_ARTIFACTS_VERSION" ]]; then
  echo "Error: --gardener-artifacts-version is required" >&2
  usage >&2
  exit 1
fi

if [[ -z "$CONFIG" || ! -f "$CONFIG" ]]; then
  echo "Error: configuration file not found: $CONFIG" >&2
  usage >&2
  exit 1
fi

case "$VIRTUAL_GARDEN_PROVIDER" in
  gdc|gke)
    ;;
  *)
    echo "Error: unsupported --virtual-garden-provider '${VIRTUAL_GARDEN_PROVIDER}' (supported: 'gdc', 'gke')" >&2
    exit 1
    ;;
esac

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

echo "Triggering teardown test..."
echo "Version: $GARDENER_ARTIFACTS_VERSION"
echo "Config: $CONFIG"
echo "Virtual Garden Provider: $VIRTUAL_GARDEN_PROVIDER"

TEST_ARGS=(
  --gardener-artifacts-version="$GARDENER_ARTIFACTS_VERSION"
  --release-configuration-file-path="$CONFIG"
  --virtual-garden-provider="$VIRTUAL_GARDEN_PROVIDER"
)
if [[ -n "$NUM_SHOOTS" ]]; then
  TEST_ARGS+=(--num-shoots="$NUM_SHOOTS")
fi

go test -v -timeout=65m "${REPO_ROOT}/integration/release/teardown" \
  -args \
  "${TEST_ARGS[@]}"
