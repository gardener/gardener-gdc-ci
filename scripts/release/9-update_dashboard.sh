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

echo "=== Stage 10: Update Release Pipeline Dashboard & Job Summary ==="

DASHBOARD_DATA_FILE="${DASHBOARD_DATA_FILE:-${REPO_ROOT}/dashboard/data/runs.json}"
mkdir -p "$(dirname "${DASHBOARD_DATA_FILE}")"

if [[ ! -f "${DASHBOARD_DATA_FILE}" ]]; then
  echo "[]" > "${DASHBOARD_DATA_FILE}"
fi

RUN_ID="${GITHUB_RUN_ID:-local-$(date +%s)}"
RUN_NUMBER="${GITHUB_RUN_NUMBER:-1}"
TRIGGER_EVENT="${GITHUB_EVENT_NAME:-manual}"
ACTOR="${GITHUB_ACTOR:-local}"
RUN_URL="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-gardener/gardener-gdc-ci}/actions/runs/${RUN_ID}"
TIMESTAMP="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"

STAGE_BUILD="${STAGE_BUILD_STATUS:-unknown}"
STAGE_DEPLOY_GARDEN="${STAGE_DEPLOY_GARDEN_STATUS:-unknown}"
STAGE_DEPLOY_SEED="${STAGE_DEPLOY_SEED_STATUS:-unknown}"
STAGE_DEPLOY_SHOOT="${STAGE_DEPLOY_SHOOT_STATUS:-unknown}"
STAGE_TEST_SHOOT="${STAGE_TEST_SHOOT_STATUS:-unknown}"
STAGE_TEARDOWN="${STAGE_TEARDOWN_STATUS:-unknown}"
STAGE_PUBLISH="${STAGE_PUBLISH_STATUS:-skipped}"
STAGE_OCM="${STAGE_OCM_STATUS:-skipped}"
OVERALL_STATUS="${OVERALL_PIPELINE_STATUS:-failure}"

format_duration_seconds() {
  local total_secs="$1"
  if [[ -z "${total_secs}" || "${total_secs}" -lt 0 ]]; then
    echo "N/A"
    return
  fi
  local hours=$((total_secs / 3600))
  local mins=$(((total_secs % 3600) / 60))
  local secs=$((total_secs % 60))
  if [[ "${hours}" -gt 0 ]]; then
    echo "${hours}h ${mins}m ${secs}s"
  elif [[ "${mins}" -gt 0 ]]; then
    echo "${mins}m ${secs}s"
  else
    echo "${secs}s"
  fi
}

compute_pipeline_duration() {
  if [[ -n "${PIPELINE_DURATION:-}" && "${PIPELINE_DURATION}" != "N/A" ]]; then
    echo "${PIPELINE_DURATION}"
    return
  fi

  local start_epoch="${PIPELINE_START_EPOCH:-}"
  if [[ -z "${start_epoch}" && -f "${RELEASE_METADATA_FILE}" ]]; then
    start_epoch="$(yq -r '.startedAtEpoch // ""' "${RELEASE_METADATA_FILE}" 2>/dev/null || true)"
  fi

  if [[ -z "${start_epoch}" && "${RUN_ID}" =~ ^[0-9]+$ ]] && command -v gh >/dev/null 2>&1; then
    local run_started_at
    run_started_at="$(gh api "repos/${GITHUB_REPOSITORY:-gardener/gardener-gdc-ci}/actions/runs/${RUN_ID}" --jq '.run_started_at // empty' 2>/dev/null || true)"
    if [[ -n "${run_started_at}" ]]; then
      start_epoch="$(date -u -d "${run_started_at}" +%s 2>/dev/null || true)"
    fi
  fi

  if [[ "${start_epoch}" =~ ^[0-9]+$ ]]; then
    local now_epoch
    now_epoch="$(date -u +%s)"
    format_duration_seconds "$((now_epoch - start_epoch))"
  else
    echo "N/A"
  fi
}

DURATION="$(compute_pipeline_duration)"

RELEASE_MODE_VAL="${RELEASE_MODE:-snapshot}"
ARTIFACTS_VERSION="unknown"
EXT_VERSION="N/A"
MCM_GDC_VERSION="N/A"
CCM_GDC_VERSION="N/A"
DNS_VERSION="N/A"
EXT_SHA="N/A"
MCM_GDC_SHA="N/A"
DNS_SHA="N/A"
CCM_GDC_SHA="N/A"
if [[ -f "${RELEASE_METADATA_FILE}" ]]; then
  RELEASE_MODE_VAL="$(yq -r '.releaseMode // "snapshot"' "${RELEASE_METADATA_FILE}")"
  ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion // "unknown"' "${RELEASE_METADATA_FILE}")"
  EXT_VERSION="$(yq -r '.extensionGDCArtifactsVersion // .components.gardenerExtensionProviderGDC.version // .gardenerArtifactsVersion // "N/A"' "${RELEASE_METADATA_FILE}")"
  MCM_GDC_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .components.machineControllerManagerProviderGDC.version // .gardenerArtifactsVersion // "N/A"' "${RELEASE_METADATA_FILE}")"
  CCM_GDC_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .components.cloudProviderGDC.version // .gardenerArtifactsVersion // "N/A"' "${RELEASE_METADATA_FILE}")"
  DNS_VERSION="$(yq -r '.externalDNSArtifactsVersion // .components.externalDNSManagement.version // "N/A"' "${RELEASE_METADATA_FILE}")"
  EXT_SHA="$(yq -r '.components.gardenerExtensionProviderGDC.commitSHA // "N/A"' "${RELEASE_METADATA_FILE}")"
  MCM_GDC_SHA="$(yq -r '.components.machineControllerManagerProviderGDC.commitSHA // "N/A"' "${RELEASE_METADATA_FILE}")"
  DNS_SHA="$(yq -r '.components.externalDNSManagement.commitSHA // "N/A"' "${RELEASE_METADATA_FILE}")"
  CCM_GDC_SHA="$(yq -r '.components.cloudProviderGDC.commitSHA // "N/A"' "${RELEASE_METADATA_FILE}")"
fi

NEW_ENTRY="$(jq -n \
  --arg runId "${RUN_ID}" \
  --arg runNumber "${RUN_NUMBER}" \
  --arg timestamp "${TIMESTAMP}" \
  --arg trigger "${TRIGGER_EVENT}" \
  --arg releaseMode "${RELEASE_MODE_VAL}" \
  --arg actor "${ACTOR}" \
  --arg status "${OVERALL_STATUS}" \
  --arg duration "${DURATION}" \
  --arg artifactsVersion "${ARTIFACTS_VERSION}" \
  --arg extVersion "${EXT_VERSION}" \
  --arg mcmGdcVersion "${MCM_GDC_VERSION}" \
  --arg ccmGdcVersion "${CCM_GDC_VERSION}" \
  --arg dnsVersion "${DNS_VERSION}" \
  --arg extSha "${EXT_SHA}" \
  --arg mcmGdcSha "${MCM_GDC_SHA}" \
  --arg dnsSha "${DNS_SHA}" \
  --arg ccmGdcSha "${CCM_GDC_SHA}" \
  --arg runUrl "${RUN_URL}" \
  --arg publicRegistry "${PUBLIC_REGISTRY}" \
  --arg stageBuild "${STAGE_BUILD}" \
  --arg stageGarden "${STAGE_DEPLOY_GARDEN}" \
  --arg stageSeed "${STAGE_DEPLOY_SEED}" \
  --arg stageShoot "${STAGE_DEPLOY_SHOOT}" \
  --arg stageTest "${STAGE_TEST_SHOOT}" \
  --arg stageTeardown "${STAGE_TEARDOWN}" \
  --arg stagePublish "${STAGE_PUBLISH}" \
  --arg stageOCM "${STAGE_OCM}" \
  '{
    runId: $runId,
    runNumber: $runNumber,
    timestamp: $timestamp,
    trigger: $trigger,
    releaseMode: $releaseMode,
    actor: $actor,
    status: $status,
    duration: $duration,
    artifactsVersion: $artifactsVersion,
    runUrl: $runUrl,
    publicRegistry: $publicRegistry,
    componentVersions: {
      extensionProviderGDC: $extVersion,
      mcmProviderGDC: $mcmGdcVersion,
      cloudProviderGDC: $ccmGdcVersion,
      externalDNS: $dnsVersion
    },
    components: {
      extensionProviderGDC: $extSha,
      mcmProviderGDC: $mcmGdcSha,
      externalDNS: $dnsSha,
      cloudProviderGDC: $ccmGdcSha
    },
    stages: {
      build: $stageBuild,
      deployGarden: $stageGarden,
      deploySeed: $stageSeed,
      deployShoot: $stageShoot,
      testShoot: $stageTest,
      teardown: $stageTeardown,
      publish: $stagePublish,
      ocm: $stageOCM
    }
  }')"

# Fetch latest runs.json from gh-pages if available so history persists across runs
if git ls-remote --exit-code --heads origin gh-pages >/dev/null 2>&1; then
  git fetch origin gh-pages:refs/remotes/origin/gh-pages >/dev/null 2>&1 || true
  if git show origin/gh-pages:data/runs.json > "${DASHBOARD_DATA_FILE}.remote" 2>/dev/null; then
    mv "${DASHBOARD_DATA_FILE}.remote" "${DASHBOARD_DATA_FILE}"
  fi
fi

# Prepend the latest run, deduplicate by runId, sort by runNumber descending, and retain the most recent 100 runs
jq --argjson entry "${NEW_ENTRY}" '[$entry] + . | unique_by(.runId) | sort_by(-((.runNumber | tonumber?) // 0)) | .[:100]' "${DASHBOARD_DATA_FILE}" > "${DASHBOARD_DATA_FILE}.tmp"
mv "${DASHBOARD_DATA_FILE}.tmp" "${DASHBOARD_DATA_FILE}"

# Publish updated dashboard and runs.json to gh-pages branch when running on main
if [[ "${GITHUB_REF:-}" == "refs/heads/main" && -z "${PR_NUMBER:-}" && -n "${GH_TOKEN:-}" ]]; then
  PAGES_WORK_DIR="$(mktemp -d)"
  if git clone --branch gh-pages "https://x-access-token:${GH_TOKEN}@github.com/${GITHUB_REPOSITORY:-gardener/gardener-gdc-ci}.git" "${PAGES_WORK_DIR}" >/dev/null 2>&1; then
    cp -r "${REPO_ROOT}/dashboard/"* "${PAGES_WORK_DIR}/"
    touch "${PAGES_WORK_DIR}/.nojekyll"
    git -C "${PAGES_WORK_DIR}" config user.name "github-actions[bot]"
    git -C "${PAGES_WORK_DIR}" config user.email "41898282+github-actions[bot]@users.noreply.github.com"
    git -C "${PAGES_WORK_DIR}" add .
    if ! git -C "${PAGES_WORK_DIR}" diff --cached --quiet; then
      git -C "${PAGES_WORK_DIR}" commit -m "chore(dashboard): record ${RELEASE_MODE_VAL} run #${RUN_NUMBER}" >/dev/null 2>&1
      git -C "${PAGES_WORK_DIR}" push origin gh-pages >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "${PAGES_WORK_DIR}"
fi

# Write GitHub Actions Job Summary if running in GitHub Actions
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
  cat >> "${GITHUB_STEP_SUMMARY}" <<EOF
## Gardener GDC Release Pipeline Summary (Run #${RUN_NUMBER})

| Field | Value |
| :--- | :--- |
| **Overall Status** | \`${OVERALL_STATUS}\` |
| **Duration** | \`${DURATION}\` |
| **Trigger** | \`${TRIGGER_EVENT}\` (by \`@${ACTOR}\`) |
| **Release Mode** | \`${RELEASE_MODE_VAL}\` |
| **Artifacts Version(Ext / MCM / CCM / DNS)** | \`${EXT_VERSION}\` / \`${MCM_GDC_VERSION}\` / \`${CCM_GDC_VERSION}\` / \`${DNS_VERSION}\` |
| **Extension Provider GDC SHA** | \`${EXT_SHA}\` |
| **MCM Provider GDC SHA** | \`${MCM_GDC_SHA}\` |
| **External DNS Management SHA** | \`${DNS_SHA}\` |
| **Cloud Provider GDC SHA** | \`${CCM_GDC_SHA}\` |
| **Candidate Registry** | \`${GHCR_REGISTRY}\` |
| **Public Registry** | \`${PUBLIC_REGISTRY}\` |
| **Release Dashboard** | [https://gardener.github.io/gardener-gdc-ci/](https://gardener.github.io/gardener-gdc-ci/) |
EOF
  if [[ -n "${OCM_DESCRIPTOR_ARTIFACT_URL:-}" ]]; then
    printf '| **OCM Component Descriptor** | [Download descriptor](%s) |\n' "${OCM_DESCRIPTOR_ARTIFACT_URL}" >> "${GITHUB_STEP_SUMMARY}"
  fi
  cat >> "${GITHUB_STEP_SUMMARY}" <<EOF

### Stage Progress

| Stage | Status |
| :--- | :--- |
| 1. Build & Push Candidate Artifacts (GHCR) | \`${STAGE_BUILD}\` |
| 3. Deploy Virtual Garden (\`garden\`) | \`${STAGE_DEPLOY_GARDEN}\` |
| 4. Deploy GDC Seed (\`seed\`) | \`${STAGE_DEPLOY_SEED}\` |
| 5. Provision GDC Shoot (\`shoot\`) | \`${STAGE_DEPLOY_SHOOT}\` |
| 6. Shoot E2E & Conformance (\`sonobuoy\`) | \`${STAGE_TEST_SHOOT}\` |
| 7. Teardown Stack (\`teardown\`) | \`${STAGE_TEARDOWN}\` |
| 8. Promote Certified Artifacts to Public Registry | \`${STAGE_PUBLISH}\` |
| 9. Publish OCM Component Descriptor | \`${STAGE_OCM}\` |
EOF
fi

complete_pr_check_run "${OVERALL_STATUS}"

echo "Dashboard data updated at ${DASHBOARD_DATA_FILE}."
