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

DASHBOARD_DIR="${DASHBOARD_DIR:-${REPO_ROOT}/dashboard}"
DASHBOARD_DATA_FILE="${DASHBOARD_DATA_FILE:-${DASHBOARD_DIR}/data/runs.json}"
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
STAGE_SECURITY_SCAN="${STAGE_SECURITY_SCAN_STATUS:-skipped}"
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

DASHBOARD_REPO="${GITHUB_REPOSITORY:-gardener/gardener-gdc-ci}"
DASHBOARD_PAGES_BRANCH="${DASHBOARD_PAGES_BRANCH:-gh-pages}"
DASHBOARD_PUBLISH_MAX_ATTEMPTS="${DASHBOARD_PUBLISH_MAX_ATTEMPTS:-3}"

fetch_remote_runs_json() {
  local target_file="$1"
  local tmp_remote="${target_file}.remote"
  rm -f "${tmp_remote}"

  if [[ -n "${GH_TOKEN:-}" ]] && command -v gh >/dev/null 2>&1; then
    if GH_TOKEN="${GH_TOKEN}" gh api \
      -H "Accept: application/vnd.github.raw+json" \
      "repos/${DASHBOARD_REPO}/contents/data/runs.json?ref=${DASHBOARD_PAGES_BRANCH}" \
      > "${tmp_remote}" 2>/dev/null && jq -e 'type == "array"' "${tmp_remote}" >/dev/null 2>&1; then
      mv "${tmp_remote}" "${target_file}"
      return 0
    fi
    rm -f "${tmp_remote}"
  fi

  if git ls-remote --exit-code --heads origin "${DASHBOARD_PAGES_BRANCH}" >/dev/null 2>&1; then
    git fetch origin "${DASHBOARD_PAGES_BRANCH}:refs/remotes/origin/${DASHBOARD_PAGES_BRANCH}" >/dev/null 2>&1 || true
    if git show "origin/${DASHBOARD_PAGES_BRANCH}:data/runs.json" > "${tmp_remote}" 2>/dev/null && jq -e 'type == "array"' "${tmp_remote}" >/dev/null 2>&1; then
      mv "${tmp_remote}" "${target_file}"
      return 0
    fi
    rm -f "${tmp_remote}"
  fi

  return 1
}

merge_dashboard_entry() {
  local target_file="$1"
  local entry_json="$2"
  jq --argjson entry "${entry_json}" \
    '[$entry] + map(select(.runId != $entry.runId)) | sort_by(-((.runNumber | tonumber?) // 0)) | .[:100]' \
    "${target_file}" > "${target_file}.tmp"
  mv "${target_file}.tmp" "${target_file}"
}

build_create_commit_payload() {
  local payload_file="$1"
  local repo="$2"
  local branch="$3"
  local expected_head_oid="$4"
  local headline="$5"
  local dashboard_dir="$6"
  local data_file="$7"

  python3 - "${payload_file}" "${repo}" "${branch}" "${expected_head_oid}" "${headline}" "${dashboard_dir}" "${data_file}" <<'PY'
import base64
import json
import os
import sys

payload_file, repo, branch, expected_head_oid, headline, dashboard_dir, data_file = sys.argv[1:8]

files_by_path: dict[str, bytes] = {".nojekyll": b""}
if os.path.isdir(dashboard_dir):
    for root, _, filenames in os.walk(dashboard_dir):
        for name in filenames:
            if name.endswith((".tmp", ".remote")):
                continue
            abs_path = os.path.join(root, name)
            rel_path = os.path.relpath(abs_path, dashboard_dir).replace(os.sep, "/")
            with open(abs_path, "rb") as f:
                files_by_path[rel_path] = f.read()

with open(data_file, "rb") as f:
    files_by_path["data/runs.json"] = f.read()

additions = [
    {"path": rel_path, "contents": base64.b64encode(files_by_path[rel_path]).decode("ascii")}
    for rel_path in sorted(files_by_path)
]

mutation = """mutation($input: CreateCommitOnBranchInput!) {
  createCommitOnBranch(input: $input) {
    commit {
      oid
      url
      signature {
        isValid
      }
    }
  }
}"""

payload = {
    "query": mutation,
    "variables": {
        "input": {
            "branch": {
                "repositoryNameWithOwner": repo,
                "branchName": branch,
            },
            "message": {
                "headline": headline,
            },
            "fileChanges": {
                "additions": additions,
            },
            "expectedHeadOid": expected_head_oid,
        }
    },
}

with open(payload_file, "w", encoding="utf-8") as f:
    json.dump(payload, f)
PY
}

# Publish updated dashboard and runs.json to gh-pages via GitHub's GraphQL
# createCommitOnBranch API so commits are automatically signed by GitHub and
# satisfy organization-level required_signatures rulesets.
publish_dashboard_to_gh_pages() {
  local headline="$1"
  local entry_json="$2"

  if ! command -v gh >/dev/null 2>&1; then
    echo "::warning::gh CLI not found; skipping ${DASHBOARD_PAGES_BRANCH} dashboard publish." >&2
    return 0
  fi

  local payload_file
  payload_file="$(mktemp)"

  local attempt last_err=""
  for ((attempt = 1; attempt <= DASHBOARD_PUBLISH_MAX_ATTEMPTS; attempt++)); do
    if (( attempt > 1 )); then
      fetch_remote_runs_json "${DASHBOARD_DATA_FILE}" || true
      merge_dashboard_entry "${DASHBOARD_DATA_FILE}" "${entry_json}"
    fi

    local head_oid
    if ! head_oid="$(GH_TOKEN="${GH_TOKEN}" gh api "repos/${DASHBOARD_REPO}/git/ref/heads/${DASHBOARD_PAGES_BRANCH}" --jq '.object.sha' 2>&1)"; then
      last_err="resolve ${DASHBOARD_PAGES_BRANCH} HEAD SHA: ${head_oid}"
      echo "::warning::Attempt ${attempt}/${DASHBOARD_PUBLISH_MAX_ATTEMPTS} failed (${last_err})" >&2
      continue
    fi

    build_create_commit_payload \
      "${payload_file}" \
      "${DASHBOARD_REPO}" \
      "${DASHBOARD_PAGES_BRANCH}" \
      "${head_oid}" \
      "${headline}" \
      "${DASHBOARD_DIR}" \
      "${DASHBOARD_DATA_FILE}"

    local resp
    if resp="$(GH_TOKEN="${GH_TOKEN}" gh api graphql --input "${payload_file}" 2>&1)"; then
      local commit_oid sig_valid
      commit_oid="$(jq -r '.data.createCommitOnBranch.commit.oid // "unknown"' <<< "${resp}" 2>/dev/null || echo "unknown")"
      sig_valid="$(jq -r '.data.createCommitOnBranch.commit.signature.isValid // "unknown"' <<< "${resp}" 2>/dev/null || echo "unknown")"
      echo "Published signed dashboard commit ${commit_oid} to ${DASHBOARD_PAGES_BRANCH} (signature valid: ${sig_valid})."
      rm -f "${payload_file}"

      local pages_resp
      if ! pages_resp="$(GH_TOKEN="${GH_TOKEN}" gh api -X POST "repos/${DASHBOARD_REPO}/pages/builds" 2>&1)"; then
        echo "::warning::Failed to trigger GitHub Pages build for ${DASHBOARD_REPO}: ${pages_resp}" >&2
      fi
      return 0
    fi

    last_err="${resp}"
    echo "::warning::Attempt ${attempt}/${DASHBOARD_PUBLISH_MAX_ATTEMPTS} to publish signed dashboard commit failed: ${last_err}" >&2
  done

  rm -f "${payload_file}"
  echo "::warning::Failed to publish dashboard to ${DASHBOARD_PAGES_BRANCH} after ${DASHBOARD_PUBLISH_MAX_ATTEMPTS} attempt(s): ${last_err}" >&2
  return 0
}

# Fetch latest runs.json from gh-pages if available so history persists across runs
fetch_remote_runs_json "${DASHBOARD_DATA_FILE}" || true

# Prepend the latest run (replacing any prior in_progress entry for the same runId), sort by runNumber descending, and retain the most recent 100 runs
merge_dashboard_entry "${DASHBOARD_DATA_FILE}" "${NEW_ENTRY}"

# Publish updated dashboard and runs.json to gh-pages branch when running on main
if [[ "${GITHUB_REF:-}" == "refs/heads/main" && -n "${GH_TOKEN:-}" ]]; then
  publish_dashboard_to_gh_pages \
    "chore(dashboard): record ${RELEASE_MODE_VAL} run #${RUN_NUMBER} (${OVERALL_STATUS})" \
    "${NEW_ENTRY}"
fi

# Write GitHub Actions Job Summary if running in GitHub Actions (final stage only)
if [[ -n "${GITHUB_STEP_SUMMARY:-}" && "${DASHBOARD_SKIP_SUMMARY:-false}" != "true" ]]; then
  TRIGGER_SUMMARY="\`${TRIGGER_EVENT}\` (by \`@${ACTOR}\`)"
  if [[ "${TRIGGER_EVENT}" == "schedule" ]]; then
    TRIGGER_SUMMARY="\`${TRIGGER_EVENT}\`"
  fi
  cat >> "${GITHUB_STEP_SUMMARY}" <<EOF
## Gardener GDC Release Pipeline Summary (Run #${RUN_NUMBER})

| Field | Value |
| :--- | :--- |
| **Overall Status** | \`${OVERALL_STATUS}\` |
| **Duration** | \`${DURATION}\` |
| **Trigger** | ${TRIGGER_SUMMARY} |
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
| 1b. Automated Security Gate (SAST, Vulns, Images, Helm) | \`${STAGE_SECURITY_SCAN}\` |
| 3. Deploy Virtual Garden (\`garden\`) | \`${STAGE_DEPLOY_GARDEN}\` |
| 4. Deploy GDC Seed (\`seed\`) | \`${STAGE_DEPLOY_SEED}\` |
| 5. Provision GDC Shoot (\`shoot\`) | \`${STAGE_DEPLOY_SHOOT}\` |
| 6. Shoot E2E & Conformance (\`sonobuoy\`) | \`${STAGE_TEST_SHOOT}\` |
| 7. Teardown Stack (\`teardown\`) | \`${STAGE_TEARDOWN}\` |
| 8. Promote Certified Artifacts to Public Registry | \`${STAGE_PUBLISH}\` |
| 9. Publish OCM Component Descriptor | \`${STAGE_OCM}\` |
EOF
fi

echo "Dashboard data updated at ${DASHBOARD_DATA_FILE}."
