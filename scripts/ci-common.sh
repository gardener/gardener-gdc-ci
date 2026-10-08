#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# verify_authorized_maintainer ensures that workflow_dispatch and /test-release issue_comment runs
# can only be triggered by approved Google and SAP maintainers listed in OWNERS_ALIASES.
verify_authorized_maintainer() {
  local owners_file="${1:-OWNERS_ALIASES}"
  if [[ "${GITHUB_EVENT_NAME:-}" != "workflow_dispatch" && "${GITHUB_EVENT_NAME:-}" != "issue_comment" && "${GITHUB_EVENT_NAME:-}" != "pull_request" ]]; then
    echo "Trigger event is '${GITHUB_EVENT_NAME:-local}'; maintainer check not required."
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
      echo "should_run=true" >> "${GITHUB_OUTPUT}"
      echo "pr_number=${PR_NUMBER:-}" >> "${GITHUB_OUTPUT}"
    fi
    return 0
  fi

  local actor="${GITHUB_ACTOR:-}"
  if [[ "${GITHUB_EVENT_NAME:-}" == "issue_comment" && -f "${GITHUB_EVENT_PATH:-}" ]]; then
    local is_pr comment_body
    is_pr="$(jq -r '.issue.pull_request.url // empty' "${GITHUB_EVENT_PATH}")"
    comment_body="$(jq -r '.comment.body // empty' "${GITHUB_EVENT_PATH}")"
    actor="$(jq -r '.comment.user.login // empty' "${GITHUB_EVENT_PATH}")"
    PR_NUMBER="$(jq -r '.issue.number // empty' "${GITHUB_EVENT_PATH}")"
    export PR_NUMBER

    if [[ -z "${is_pr}" ]] || ! printf '%s\n' "${comment_body}" | tr -d '\r' | grep -Eq '^[[:space:]]*/test-release([[:space:]]|$)'; then
      echo "Comment does not contain a '/test-release' command line on a pull request. Skipping."
      if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
        echo "should_run=false" >> "${GITHUB_OUTPUT}"
      fi
      return 0
    fi
  fi

  if [[ -z "${actor}" ]]; then
    echo "::error::GITHUB_ACTOR is empty on ${GITHUB_EVENT_NAME:-unknown}." >&2
    exit 1
  fi

  if ! grep -E "^[[:space:]]*-[[:space:]]*${actor}[[:space:]]*$" "${owners_file}" >/dev/null 2>&1; then
    echo "::error::Unauthorized user @${actor}. Only approved Google and SAP maintainers listed in ${owners_file} may trigger the release pipeline." >&2
    exit 1
  fi

  echo "Authorized maintainer @${actor} verified via ${owners_file}."

  if [[ "${GITHUB_EVENT_NAME:-}" == "issue_comment" && -f "${GITHUB_EVENT_PATH:-}" ]]; then
    local comment_id
    comment_id="$(jq -r '.comment.id // empty' "${GITHUB_EVENT_PATH}")"
    if [[ -n "${comment_id}" && -n "${GH_TOKEN:-}" && -n "${GITHUB_REPOSITORY:-}" ]]; then
      gh api --method POST "repos/${GITHUB_REPOSITORY}/issues/comments/${comment_id}/reactions" \
        -f content='rocket' >/dev/null 2>&1 || true
    fi
  fi

  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "should_run=true" >> "${GITHUB_OUTPUT}"
    echo "pr_number=${PR_NUMBER:-}" >> "${GITHUB_OUTPUT}"
  fi
}

# start_pr_check_run creates an in_progress GitHub Check Run on PR_HEAD_SHA when
# triggered via '/test-release' or workflow_dispatch with pr_number.
start_pr_check_run() {
  local check_name="${1:-E2E Release Certification & Promotion}"
  if [[ -z "${PR_HEAD_SHA:-}" || -z "${GH_TOKEN:-}" || -z "${GITHUB_REPOSITORY:-}" ]]; then
    return 0
  fi

  local run_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID:-}"
  echo "Creating check-run '${check_name}' on PR #${PR_NUMBER:-} commit ${PR_HEAD_SHA}..."
  PR_CHECK_RUN_ID="$(gh api --method POST "repos/${GITHUB_REPOSITORY}/check-runs" \
    -f name="${check_name}" \
    -f head_sha="${PR_HEAD_SHA}" \
    -f status="in_progress" \
    -f details_url="${run_url}" \
    -f "output[title]=Running via ${GITHUB_EVENT_NAME:-manual trigger}" \
    -f "output[summary]=Triggered by @${GITHUB_ACTOR:-maintainer} (${GITHUB_EVENT_NAME:-manual}) for PR #${PR_NUMBER:-} (${run_url})." \
    --jq '.id' 2>/dev/null || true)"
  export PR_CHECK_RUN_ID
}

# complete_pr_check_run updates the GitHub Check Run on PR_HEAD_SHA with the final conclusion.
complete_pr_check_run() {
  local status_str="${1:-failure}"
  if [[ -z "${PR_CHECK_RUN_ID:-}" || -z "${GH_TOKEN:-}" || -z "${GITHUB_REPOSITORY:-}" ]]; then
    return 0
  fi

  local conclusion="failure"
  local title="Release pipeline failed"
  if [[ "${status_str}" == "success" ]]; then
    conclusion="success"
    title="Release pipeline passed"
  elif [[ "${status_str}" == "cancelled" ]]; then
    conclusion="cancelled"
    title="Release pipeline cancelled"
  fi

  local run_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID:-}"
  echo "Updating check-run ${PR_CHECK_RUN_ID} on PR #${PR_NUMBER:-} with conclusion=${conclusion}..."
  gh api --method PATCH "repos/${GITHUB_REPOSITORY}/check-runs/${PR_CHECK_RUN_ID}" \
    -f status="completed" \
    -f conclusion="${conclusion}" \
    -f details_url="${run_url}" \
    -f "output[title]=${title}" \
    -f "output[summary]=Manual ${GITHUB_EVENT_NAME:-workflow_dispatch} run completed with ${conclusion} (${run_url})." >/dev/null 2>&1 || \
    echo "Warning: Failed to update check-run ${PR_CHECK_RUN_ID} on PR #${PR_NUMBER:-}."
}

# checkout_pr_if_requested fetches and checks out PR_NUMBER, merges origin/main,
# and starts a GitHub Check Run on the PR's head SHA.
checkout_pr_if_requested() {
  if [[ -z "${PR_NUMBER:-}" ]]; then
    return 0
  fi

  git fetch origin main
  local base_branch_sha
  base_branch_sha="$(git rev-parse origin/main)"

  echo "Fetching and checking out PR #${PR_NUMBER}..."
  git fetch origin "pull/${PR_NUMBER}/head:pr-${PR_NUMBER}"
  PR_HEAD_SHA="$(git rev-parse "pr-${PR_NUMBER}")"
  export PR_HEAD_SHA

  start_pr_check_run "${CHECK_NAME:-E2E Release Certification & Promotion}"

  git checkout "pr-${PR_NUMBER}"
  echo "Merging base branch (${base_branch_sha:0:7}) into PR #${PR_NUMBER} (${PR_HEAD_SHA:0:7})..."
  if ! git -c user.name="github-actions[bot]" -c user.email="github-actions[bot]@users.noreply.github.com" \
    merge --no-edit "${base_branch_sha}"; then
    echo "::error::Failed to merge base branch (${base_branch_sha:0:7}) into PR #${PR_NUMBER} (${PR_HEAD_SHA:0:7}). Please rebase the PR onto main."
    complete_pr_check_run "failure"
    exit 1
  fi

  if [[ -n "${GITHUB_ENV:-}" ]]; then
    {
      echo "PR_NUMBER=${PR_NUMBER}"
      echo "PR_HEAD_SHA=${PR_HEAD_SHA}"
      [[ -n "${PR_CHECK_RUN_ID:-}" ]] && echo "PR_CHECK_RUN_ID=${PR_CHECK_RUN_ID}"
    } >> "${GITHUB_ENV}"
  fi
}

# mask_secret_value masks every non-empty line of a multi-line secret in GitHub Actions logs.
mask_secret_value() {
  local xtrace_was_set=false
  if [[ "$-" == *x* ]]; then
    xtrace_was_set=true
    set +x
  fi

  local secret_val="${1:-}"
  if [[ -n "${secret_val}" && "${GITHUB_ACTIONS:-}" == "true" ]]; then
    while IFS= read -r line; do
      line="${line#"${line%%[![:space:]]*}"}"
      line="${line%"${line##*[![:space:]]}"}"
      if [[ -n "${line}" && "${line}" != "{" && "${line}" != "}" ]]; then
        echo "::add-mask::${line}"
      fi
    done <<< "${secret_val}"
  fi

  if [[ "${xtrace_was_set}" == "true" ]]; then
    set -x
  fi
}

# setup_harbor_docker_config merges GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON into
# ~/.docker/config.json so docker and helm can push/pull from GDC Staging Harbor.
setup_harbor_docker_config() {
  local xtrace_was_set=false
  if [[ "$-" == *x* ]]; then
    xtrace_was_set=true
    set +x
  fi

  local harbor_cfg="${1:-${GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON:-}}"
  if [[ -n "${harbor_cfg}" ]]; then
    mask_secret_value "${harbor_cfg}"
    mkdir -p "${HOME}/.docker"
    local docker_cfg_path="${HOME}/.docker/config.json"
    local tmp_secret
    tmp_secret="$(mktemp /tmp/harbor_docker_cfg.XXXXXX)"
    printf '%s' "${harbor_cfg}" > "${tmp_secret}"
    if [[ -f "${docker_cfg_path}" ]]; then
      jq -s 'reduce .[] as $item ({}; . * $item)' "${docker_cfg_path}" "${tmp_secret}" > "${docker_cfg_path}.tmp"
      mv "${docker_cfg_path}.tmp" "${docker_cfg_path}"
    else
      mv "${tmp_secret}" "${docker_cfg_path}"
    fi
    rm -f "${tmp_secret}"
    chmod 600 "${docker_cfg_path}"
    mkdir -p "${HOME}/.config/helm/registry"
    cp "${docker_cfg_path}" "${HOME}/.config/helm/registry/config.json"
    chmod 600 "${HOME}/.config/helm/registry/config.json"
  fi

  if [[ "${xtrace_was_set}" == "true" ]]; then
    set -x
  fi
}

# setup_gdc_credentials writes the GDC service account key to disk (if provided
# via environment variable), masks all lines in GitHub Actions logs, and
# downloads the GDC Root CA certificate.
setup_gdc_credentials() {
  local xtrace_was_set=false
  if [[ "$-" == *x* ]]; then
    xtrace_was_set=true
    set +x
  fi

  local sa_file="$1"
  local ca_file="$2"
  local console_url="$3"
  local sa_key="${4:-${GDC_RELEASE_SERVICE_ACCOUNT_KEY:-}}"

  if [[ -n "${sa_key}" ]]; then
    mask_secret_value "${sa_key}"
    (
      umask 077
      printf '%s' "${sa_key}" > "${sa_file}"
    )
  fi

  if [[ "${xtrace_was_set}" == "true" ]]; then
    set -x
  fi

  if [[ ! -s "${sa_file}" ]]; then
    echo "::error::GDC Service Account key is required (file '${sa_file}' is missing or empty). Verify that GDC_RELEASE_SERVICE_ACCOUNT_KEY is configured in repository secrets." >&2
    exit 1
  fi

  echo "Fetching GDC Root CA from ${console_url}/.well-known/certificate-authority..."
  if ! wget "${console_url}/.well-known/certificate-authority" \
    --no-check-certificate -q -O "${ca_file}"; then
    echo "::error::Failed to fetch GDC Root CA from ${console_url}" >&2
    exit 1
  fi
}

# install_gdcloud_cli downloads and installs the gdcloud CLI and
# gdcloud-k8s-auth-plugin from the GDC management API server's CLIBundleMetadata.
install_gdcloud_cli() {
  local sa_file="$1"
  local ca_file="$2"
  local mgmt_url="$3"
  local gdcloud_version="${4:-}"
  local token_helper_bin="${5:-./bin/token-helper}"

  local gdcloud_root="/usr/local/bin/google-distributed-cloud-hosted-cli"
  if [[ -x "${gdcloud_root}/bin/gdcloud" && -x "${gdcloud_root}/bin/gdcloud-k8s-auth-plugin" ]]; then
    echo "gdcloud CLI already installed at ${gdcloud_root}/bin/gdcloud"
    export PATH="${gdcloud_root}/bin:${PATH}"
    export GDCLOUD_PATH="${gdcloud_root}/bin/gdcloud"
    if [[ -n "${GITHUB_PATH:-}" ]]; then
      echo "${gdcloud_root}/bin" >> "${GITHUB_PATH}"
    fi
    if [[ -n "${GITHUB_ENV:-}" ]]; then
      echo "GDCLOUD_PATH=${GDCLOUD_PATH}" >> "${GITHUB_ENV}"
    fi
    return 0
  fi

  echo "Minting STS token using GDC ServiceAccount to query CLIBundleMetadata..."
  local sts_token
  if [[ -x "${token_helper_bin}" ]]; then
    sts_token=$("${token_helper_bin}" \
      --service-account-file="${sa_file}" \
      --ca-cert-file="${ca_file}" \
      --audience="${mgmt_url}")
  else
    sts_token=$(go run ./integration/cmd/token-helper \
      --service-account-file="${sa_file}" \
      --ca-cert-file="${ca_file}" \
      --audience="${mgmt_url}")
  fi

  echo "Resolving gdcloud CLI bundle URL (version: ${gdcloud_version:-latest}) from ${mgmt_url}..."
  local serving_url
  serving_url=$(curl -ksSL -H "Authorization: Bearer ${sts_token}" \
    "${mgmt_url}/apis/artifactview.private.gdc.goog/v1alpha1/namespaces/ui-system/clibundlemetadata" | \
    jq -r --arg ver "${gdcloud_version}" '
      [.items[]
       | select(.platform.os == "linux" and .platform.architecture == "amd64")
       | select($ver == "" or (.commonMetadata.artifactVersion | contains($ver)))
      ]
      | sort_by(.metadata.creationTimestamp)
      | last
      | .commonMetadata.servingURL // empty
    ')

  if [[ -z "${serving_url}" ]]; then
    echo "::error::Failed to resolve gdcloud CLI servingURL for version '${gdcloud_version}'." >&2
    exit 1
  fi

  local tarball="/tmp/gdcloud_cli_linux.tar.gz"
  echo "Downloading gdcloud CLI from ${serving_url}..."
  curl -kL --fail --retry 3 "${serving_url}?uncompressed=false" -o "${tarball}"

  local sudo_cmd=""
  if [[ "${EUID:-$(id -u)}" -ne 0 ]] && command -v sudo >/dev/null 2>&1; then
    sudo_cmd="sudo"
  fi

  echo "Extracting gdcloud CLI to /usr/local/bin and installing gdcloud-k8s-auth-plugin..."
  ${sudo_cmd} tar -xzf "${tarball}" -C /usr/local/bin/
  ${sudo_cmd} "${gdcloud_root}/bin/gdcloud" components install gdcloud-k8s-auth-plugin
  rm -f "${tarball}"

  export PATH="${gdcloud_root}/bin:${PATH}"
  export GDCLOUD_PATH="${gdcloud_root}/bin/gdcloud"
  if [[ -n "${GITHUB_PATH:-}" ]]; then
    echo "${gdcloud_root}/bin" >> "${GITHUB_PATH}"
  fi
  if [[ -n "${GITHUB_ENV:-}" ]]; then
    echo "GDCLOUD_PATH=${GDCLOUD_PATH}" >> "${GITHUB_ENV}"
  fi
  "${GDCLOUD_PATH}" version
}

# install_sonobuoy_cli downloads and installs the Sonobuoy CLI binary used for
# CNCF Kubernetes conformance testing on the Shoot cluster.
install_sonobuoy_cli() {
  local version="${1:-0.57.3}"
  if command -v sonobuoy >/dev/null 2>&1; then
    export SONOBUOY_PATH
    SONOBUOY_PATH="$(command -v sonobuoy)"
    if [[ -n "${GITHUB_ENV:-}" ]]; then
      echo "SONOBUOY_PATH=${SONOBUOY_PATH}" >> "${GITHUB_ENV}"
    fi
    return 0
  fi

  local tarball="/tmp/sonobuoy_${version}_linux_amd64.tar.gz"
  local url="https://github.com/vmware-tanzu/sonobuoy/releases/download/v${version}/sonobuoy_${version}_linux_amd64.tar.gz"
  echo "Downloading Sonobuoy v${version} from ${url}..."
  curl -sSL --fail --retry 3 "${url}" -o "${tarball}"

  local sudo_cmd=""
  if [[ "${EUID:-$(id -u)}" -ne 0 ]] && command -v sudo >/dev/null 2>&1; then
    sudo_cmd="sudo"
  fi

  ${sudo_cmd} tar -xzf "${tarball}" -C /usr/local/bin sonobuoy
  ${sudo_cmd} chmod +x /usr/local/bin/sonobuoy
  rm -f "${tarball}"

  export SONOBUOY_PATH="/usr/local/bin/sonobuoy"
  if [[ -n "${GITHUB_ENV:-}" ]]; then
    echo "SONOBUOY_PATH=${SONOBUOY_PATH}" >> "${GITHUB_ENV}"
  fi
  "${SONOBUOY_PATH}" version --short || true
}
