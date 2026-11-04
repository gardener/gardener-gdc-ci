#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

# verify_authorized_maintainer ensures that workflow_dispatch runs execute only from 'main' and
# can only be triggered by approved Google and SAP maintainers listed in OWNERS_ALIASES (as
# committed on origin/main when available), and resolves the execution mode for the single
# weekday schedule ('0 10 * * 1-5', 02:00 PST): on a biweekly release Wednesday (even Unix epoch
# week) the run is the official release and no separate snapshot runs that day; on every other
# weekday it is the nightly snapshot.
verify_authorized_maintainer() {
  local owners_file="${1:-OWNERS_ALIASES}"
  if [[ "${GITHUB_EVENT_NAME:-}" == "schedule" ]]; then
    local current_epoch="${CURRENT_EPOCH:-$(date -u +%s)}"
    local epoch_week=$(( current_epoch / 604800 ))
    local day_of_week=$(( ((current_epoch / 86400) + 3) % 7 + 1 ))  # Mon=1 .. Sun=7
    local release_mode="snapshot"
    if (( day_of_week == 3 && epoch_week % 2 == 0 )); then
      release_mode="release"
      echo "Schedule triggered on a biweekly release Wednesday (epoch_week=${epoch_week}); running the official release instead of the nightly snapshot."
    else
      echo "Schedule triggered on a regular weekday (epoch_week=${epoch_week}, day_of_week=${day_of_week}); running the nightly snapshot."
    fi
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
      echo "release_mode=${release_mode}" >> "${GITHUB_OUTPUT}"
    fi
  fi

  if [[ "${GITHUB_EVENT_NAME:-}" != "workflow_dispatch" ]]; then
    echo "Trigger event is '${GITHUB_EVENT_NAME:-local}'; maintainer check not required."
    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
      echo "should_run=true" >> "${GITHUB_OUTPUT}"
    fi
    return 0
  fi

  if [[ "${GITHUB_REF_NAME:-}" != "main" ]]; then
    echo "::error::The release pipeline only runs merged code; run it from 'main' (got '${GITHUB_REF_NAME:-}')." >&2
    exit 1
  fi

  local actor="${GITHUB_ACTOR:-}"
  if [[ -z "${actor}" ]]; then
    echo "::error::GITHUB_ACTOR is empty on ${GITHUB_EVENT_NAME:-unknown}." >&2
    exit 1
  fi

  local owners_content=""
  if git rev-parse --verify --quiet "origin/main:${owners_file}" >/dev/null 2>&1; then
    owners_content="$(git show "origin/main:${owners_file}")"
  elif [[ -f "${owners_file}" ]]; then
    owners_content="$(cat "${owners_file}")"
  else
    echo "::error::Owners file '${owners_file}' not found." >&2
    exit 1
  fi

  if ! printf '%s\n' "${owners_content}" | grep -E "^[[:space:]]*-[[:space:]]*${actor}[[:space:]]*$" >/dev/null 2>&1; then
    echo "::error::Unauthorized user @${actor}. Only approved Google and SAP maintainers listed in ${owners_file} on main may trigger the release pipeline." >&2
    exit 1
  fi

  echo "Authorized maintainer @${actor} verified via ${owners_file}."

  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "should_run=true" >> "${GITHUB_OUTPUT}"
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
    echo "::error::GDC Service Account key is required (file '${sa_file}' is missing or empty). Verify that GDC_RELEASE_SERVICE_ACCOUNT_KEY is configured in the gdc-staging environment secrets." >&2
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

  local sudo_cmd=""
  if [[ "${EUID:-$(id -u)}" -ne 0 ]] && command -v sudo >/dev/null 2>&1; then
    sudo_cmd="sudo"
  fi

  local gdcloud_root="/usr/local/bin/google-distributed-cloud-hosted-cli"
  if [[ -x "${gdcloud_root}/bin/gdcloud" && -x "${gdcloud_root}/bin/gdcloud-k8s-auth-plugin" ]]; then
    echo "gdcloud CLI already installed at ${gdcloud_root}/bin/gdcloud"
    if [[ ! -d "${gdcloud_root}/plugins" || ! -w "${gdcloud_root}/plugins" ]]; then
      ${sudo_cmd} mkdir -p "${gdcloud_root}/plugins"
      ${sudo_cmd} chmod 0777 "${gdcloud_root}/plugins"
    fi
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

  echo "Extracting gdcloud CLI to /usr/local/bin and installing gdcloud-k8s-auth-plugin..."
  ${sudo_cmd} tar -xzf "${tarball}" -C /usr/local/bin/
  ${sudo_cmd} "${gdcloud_root}/bin/gdcloud" components install gdcloud-k8s-auth-plugin
  ${sudo_cmd} mkdir -p "${gdcloud_root}/plugins"
  ${sudo_cmd} chmod 0777 "${gdcloud_root}/plugins"
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
