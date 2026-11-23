#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_CONFIG="${REPO_ROOT}/pipeline-configurations/staging-release-pipeline-configuration.yaml"

if ! command -v gdcloud >/dev/null 2>&1; then
  for gdcloud_bin_dir in "${HOME}/Downloads/google-distributed-cloud-hosted-cli/bin" "${HOME}/google-distributed-cloud-hosted-cli/bin" "/usr/local/bin/google-distributed-cloud-hosted-cli/bin"; do
    if [[ -d "${gdcloud_bin_dir}" && ":${PATH}:" != *":${gdcloud_bin_dir}:"* ]]; then
      export PATH="${gdcloud_bin_dir}:${PATH}"
      break
    fi
  done
fi

usage() {
  echo "Usage: $0 [--config=<config_path>] [--virtual-garden-provider=<gdc|gke>]"
}

log_success() {
  local message="$1"
  local GREEN='\033[0;32m'
  local BOLD='\033[1m'
  local RESET='\033[0m'
  local length=${#message}
  local border_length=$((length + 6))
  local border=""
  local i
  for ((i=0; i<border_length; i++)); do border="${border}="; done
  echo -e "${GREEN}${BOLD}"
  echo "$border"
  echo "|  $(printf '%*s' "-$length" "$message")  |"
  echo "$border"
  echo -e "${RESET}"
}

log_info() {
  local message="$1"
  local BLUE='\033[0;34m'
  local RESET='\033[0m'
  echo -e "${BLUE}${message}${RESET}"
}

log_failure() {
  local message="$1"
  local RED='\033[0;31m'
  local BOLD='\033[1m'
  local RESET='\033[0m'
  local length=${#message}
  local border_length=$((length + 6))
  local border=""
  local i
  for ((i=0; i<border_length; i++)); do border="${border}="; done
  echo -e "${RED}${BOLD}" >&2
  echo "$border" >&2
  echo "|  $(printf '%*s' "-$length" "$message")  |" >&2
  echo "$border" >&2
  echo -e "${RESET}" >&2
}

CONFIG_FILE="$DEFAULT_CONFIG"
VIRTUAL_GARDEN_PROVIDER="${VIRTUAL_GARDEN_PROVIDER:-gdc}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --config)
      if [[ $# -lt 2 ]]; then
        log_failure "Error: --config requires a value"
        usage >&2
        exit 1
      fi
      CONFIG_FILE="$2"
      shift 2
      ;;
    --config=*)
      CONFIG_FILE="${1#*=}"
      shift
      ;;
    --virtual-garden-provider)
      if [[ $# -lt 2 ]]; then
        log_failure "Error: --virtual-garden-provider requires a value"
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
    *)
      log_failure "Unknown argument: $1"
      usage >&2
      exit 1
      ;;
  esac
done

case "$VIRTUAL_GARDEN_PROVIDER" in
  gdc|gke)
    ;;
  *)
    log_failure "Unsupported --virtual-garden-provider '${VIRTUAL_GARDEN_PROVIDER}' (supported: 'gdc', 'gke')"
    exit 1
    ;;
esac

if [[ -z "$CONFIG_FILE" || ! -f "$CONFIG_FILE" ]]; then
  log_failure "Configuration file not found: $CONFIG_FILE"
  usage >&2
  exit 1
fi

export_shoot_kubeconfig() {
  local virtual_garden_kubeconfig="$1"
  local shoot_name="$2"
  local output_kubeconfig="$3"
  local gardener_project_name="garden"

  kubectl --kubeconfig "$virtual_garden_kubeconfig" create -f \
    <(printf '{"spec":{"expirationSeconds":60000}}') \
    --raw "/apis/core.gardener.cloud/v1beta1/namespaces/$gardener_project_name/shoots/$shoot_name/adminkubeconfig" \
    | jq -r ".status.kubeconfig" | base64 -d > "$output_kubeconfig"
}

echo "Reading configuration from $CONFIG_FILE"

mkdir -p /tmp/kubeconfigs
umask 077

log_kubeconfig_refresh() {
  local kubeconfig_path="$1"
  local description="$2"
  log_info "$description: $kubeconfig_path"
}

refresh_virtual_garden_and_shoots() {
  local runtime_kubeconfig="$1"
  local cluster_name="$2"

  local virtual_garden_kubeconfig="/tmp/kubeconfigs/${cluster_name}-virtual-garden-kubeconfig"
  if kubectl --kubeconfig "$runtime_kubeconfig" get secret -n garden gardener >/dev/null 2>&1; then
    if kubectl --kubeconfig "$runtime_kubeconfig" get secret -n garden gardener -ojsonpath='{.data.kubeconfig}' | base64 -d > "$virtual_garden_kubeconfig"; then
      log_kubeconfig_refresh "$virtual_garden_kubeconfig" "Virtual Garden kubeconfig"

      local shoots
      shoots=$(kubectl --kubeconfig "$virtual_garden_kubeconfig" -n garden get shoots -o jsonpath='{.items[*].metadata.name}')
      for shoot_name in $shoots; do
        local shoot_kubeconfig="/tmp/kubeconfigs/${shoot_name}-kubeconfig"
        if export_shoot_kubeconfig "$virtual_garden_kubeconfig" "$shoot_name" "$shoot_kubeconfig"; then
          log_kubeconfig_refresh "$shoot_kubeconfig" "Shoot kubeconfig ($shoot_name)"
        else
          log_failure "Failed to export shoot kubeconfig for $shoot_name"
        fi
      done
    else
      log_failure "Failed to extract virtual garden kubeconfig for $cluster_name"
    fi
  else
    echo "No virtual garden secret found for $cluster_name."
  fi
}

# 1. Refresh GCP GKE Clusters only when VIRTUAL_GARDEN_PROVIDER=gke
if [[ "$VIRTUAL_GARDEN_PROVIDER" == "gke" ]]; then
  GKE_CLUSTERS_COUNT=$(yq eval '(.gcp.gkeClusters // []) | length' "$CONFIG_FILE")
  if [[ "$GKE_CLUSTERS_COUNT" -gt 0 && "$GKE_CLUSTERS_COUNT" != "null" ]]; then
    log_success "Refreshing GCP GKE Clusters"
    for (( i=0; i<GKE_CLUSTERS_COUNT; i++ )); do
      GKE_CLUSTER_NAME=$(yq eval ".gcp.gkeClusters[$i].name" "$CONFIG_FILE")
      GKE_REGION=$(yq eval ".gcp.gkeClusters[$i].zone" "$CONFIG_FILE")
      GKE_PROJECT=$(yq eval ".gcp.gkeClusters[$i].project" "$CONFIG_FILE")

      echo "Processing GKE Cluster: $GKE_CLUSTER_NAME in $GKE_REGION ($GKE_PROJECT)"

      export KUBECONFIG="/tmp/kubeconfigs/${GKE_CLUSTER_NAME}-kubeconfig"
      gcloud container clusters get-credentials "$GKE_CLUSTER_NAME" --region "$GKE_REGION" --project "$GKE_PROJECT" >&2
      log_kubeconfig_refresh "$KUBECONFIG" "GKE Cluster kubeconfig"

      refresh_virtual_garden_and_shoots "$KUBECONFIG" "$GKE_CLUSTER_NAME"
    done
  fi
fi

# 2. Refresh GDC Clusters
log_success "Refreshing GDC Clusters"

ORG_NAME=$(yq eval '.gdc.org' "$CONFIG_FILE")

export KUBECONFIG="/tmp/kubeconfigs/${ORG_NAME}-global-kubeconfig"
gdcloud config set zone "" >/dev/null 2>&1 || true
gdcloud clusters get-credentials global-api >&2
log_kubeconfig_refresh "$KUBECONFIG" "GDC Global API kubeconfig"

declare -A REFRESHED_MGMT_ZONES=()
declare -A REFRESHED_USER_CLUSTERS=()

refresh_management_api_zone() {
  local zone="$1"
  if [[ -z "$zone" || "$zone" == "null" || -n "${REFRESHED_MGMT_ZONES[$zone]:-}" ]]; then
    return 0
  fi
  export KUBECONFIG="/tmp/kubeconfigs/management-api-${zone}-kubeconfig"
  gdcloud clusters get-credentials "${ORG_NAME}-admin" --zone="${zone}" >&2
  log_kubeconfig_refresh "$KUBECONFIG" "Management API kubeconfig ($zone)"
  REFRESHED_MGMT_ZONES["$zone"]=1
}

ZONES_COUNT=$(yq eval '(.gdc.zones // []) | length' "$CONFIG_FILE")
if [[ "$ZONES_COUNT" -gt 0 && "$ZONES_COUNT" != "null" ]]; then
  for (( z=0; z<ZONES_COUNT; z++ )); do
    zone_name=$(yq eval ".gdc.zones[$z]" "$CONFIG_FILE")
    refresh_management_api_zone "$zone_name"
  done
fi

refresh_gdc_user_cluster_list() {
  local yq_path="$1"
  local count
  count=$(yq eval "(${yq_path} // []) | length" "$CONFIG_FILE")
  if [[ "$count" -gt 0 && "$count" != "null" ]]; then
    for (( i=0; i<count; i++ )); do
      local uc_name uc_zone uc_project cluster_key
      uc_name=$(yq eval "${yq_path}[$i].name" "$CONFIG_FILE")
      uc_zone=$(yq eval "${yq_path}[$i].zone" "$CONFIG_FILE")
      uc_project=$(yq eval "${yq_path}[$i].project" "$CONFIG_FILE")
      cluster_key="${uc_project}/${uc_zone}/${uc_name}"

      if [[ -n "${REFRESHED_USER_CLUSTERS[$cluster_key]:-}" ]]; then
        continue
      fi
      REFRESHED_USER_CLUSTERS["$cluster_key"]=1

      echo "Processing GDC User Cluster: $uc_name in $uc_zone ($uc_project)"

      refresh_management_api_zone "$uc_zone"

      export KUBECONFIG="/tmp/kubeconfigs/${uc_name}-kubeconfig"
      gdcloud clusters get-credentials "$uc_name" --standard --project "$uc_project" --zone "$uc_zone" >&2
      log_kubeconfig_refresh "$KUBECONFIG" "User Cluster kubeconfig"

      refresh_virtual_garden_and_shoots "$KUBECONFIG" "$uc_name"
    done
  fi
}

if [[ "$VIRTUAL_GARDEN_PROVIDER" == "gdc" ]]; then
  refresh_gdc_user_cluster_list ".gdc.virtualGardenClusters"
fi
refresh_gdc_user_cluster_list ".gdc.userClusters"

echo "All kubeconfigs refreshed successfully."
