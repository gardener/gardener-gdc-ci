#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
export REPO_ROOT

# Working directory for release pipeline state & logs
export RELEASE_WORK_DIR="${RELEASE_WORK_DIR:-/tmp/gardener-gdc-release}"
export RELEASE_ARTIFACTS_DIR="${RELEASE_ARTIFACTS_DIR:-${RELEASE_WORK_DIR}/artifacts}"
mkdir -p "${RELEASE_WORK_DIR}" "${RELEASE_ARTIFACTS_DIR}"

# Configuration files
export PIPELINE_CONFIG_FILE="${PIPELINE_CONFIG_FILE:-${REPO_ROOT}/pipeline-configurations/staging-release-pipeline-configuration.yaml}"
export BASE_SNAPSHOT_CONFIG_FILE="${BASE_SNAPSHOT_CONFIG_FILE:-${REPO_ROOT}/snapshot-configurations/base-snapshot-configuration.yaml}"
export RELEASE_METADATA_FILE="${RELEASE_METADATA_FILE:-${RELEASE_WORK_DIR}/release-metadata.yaml}"
export UPDATED_PIPELINE_CONFIG_FILE="${UPDATED_PIPELINE_CONFIG_FILE:-${RELEASE_WORK_DIR}/updated-pipeline-configuration.yaml}"
export CERTIFIED_RELEASE_MANIFEST="${CERTIFIED_RELEASE_MANIFEST:-${RELEASE_ARTIFACTS_DIR}/certified-release-manifest.json}"

# Registries:
# 1. GHCR_REGISTRY stores the initial candidate artifacts built by the pipeline (GDC Staging pulls directly from GHCR).
# 2. PUBLIC_REGISTRY defaults to europe-docker.pkg.dev/gardener-project/public (matching local Makefile build tags in Stage 1)
#    and is overridden in Stage 8 by gardener/cc-utils params@v1 (snapshots on PRs, releases on scheduled/manual runs).
export GHCR_REGISTRY="${GHCR_REGISTRY:-ghcr.io/gardener/gardener-gdc-ci}"
export PUBLIC_REGISTRY="${PUBLIC_REGISTRY:-europe-docker.pkg.dev/gardener-project/public}"

# GitHub source repositories (no Git-on-Borg mirrors)
export EXTENSION_GDC_REPO="${EXTENSION_GDC_REPO:-https://github.com/gardener/gardener-extension-provider-gdc.git}"
export EXTENSION_GDC_REF="${EXTENSION_GDC_REF:-main}"

export MCM_PROVIDER_GDC_REPO="${MCM_PROVIDER_GDC_REPO:-https://github.com/gardener/machine-controller-manager-provider-gdc.git}"
export MCM_PROVIDER_GDC_REF="${MCM_PROVIDER_GDC_REF:-main}"

export EXTERNAL_DNS_REPO="${EXTERNAL_DNS_REPO:-https://github.com/gardener/external-dns-management.git}"
export EXTERNAL_DNS_REF="${EXTERNAL_DNS_REF:-master}"

export CLOUD_PROVIDER_GDC_REPO="${CLOUD_PROVIDER_GDC_REPO:-https://github.com/GoogleCloudPlatform/cloud-provider-gdc.git}"
export CLOUD_PROVIDER_GDC_REF="${CLOUD_PROVIDER_GDC_REF:-main}"

# When running in GitHub Actions, merge ~/.docker/config.json (which contains the ghcr.io login)
# with GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON so cluster imagePullSecrets can pull from both GHCR and Harbor.
if [[ -f "${HOME}/.docker/config.json" && -n "${GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON:-}" ]] && command -v jq >/dev/null 2>&1; then
  GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON="$(jq -c -s 'reduce .[] as $item ({}; . * $item)' "${HOME}/.docker/config.json" <(printf '%s' "${GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON}"))"
  export GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON
fi
