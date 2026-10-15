#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

if ! command -v gdcloud >/dev/null 2>&1; then
  for gdcloud_bin_dir in "${HOME}/Downloads/google-distributed-cloud-hosted-cli/bin" "${HOME}/google-distributed-cloud-hosted-cli/bin" "/usr/local/bin/google-distributed-cloud-hosted-cli/bin"; do
    if [[ -d "${gdcloud_bin_dir}" && ":${PATH}:" != *":${gdcloud_bin_dir}:"* ]]; then
      export PATH="${gdcloud_bin_dir}:${PATH}"
      break
    fi
  done
fi

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

CONFIG_FILE="${2:-}"

if [[ -z "$CONFIG_FILE" || ! -f "$CONFIG_FILE" ]]; then
  log_failure "Usage: $0 --config <config_path>"
  exit 1
fi

echo "Reading configuration from $CONFIG_FILE"

mkdir -p /tmp/kubeconfigs

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
          refresh_virtual_garden_and_shoots "$shoot_kubeconfig" "$shoot_name"
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

# 1. Refresh GCP GKE Clusters
log_success "Refreshing GCP GKE Clusters"

GKE_CLUSTERS_COUNT=$(yq eval '.gcp.gkeClusters | length' "$CONFIG_FILE")
if [[ "$GKE_CLUSTERS_COUNT" -gt 0 && "$GKE_CLUSTERS_COUNT" != "null" ]]; then
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
else
  log_failure "No GCP GKE clusters found in config."
fi

# 2. Refresh GDC Clusters
log_success "Refreshing GDC Clusters"

ORG_NAME=$(yq eval '.gdc.org' "$CONFIG_FILE")

export KUBECONFIG="/tmp/kubeconfigs/${ORG_NAME}-global-kubeconfig"
gdcloud config set zone "" >/dev/null 2>&1 || true
gdcloud clusters get-credentials global-api >&2
log_kubeconfig_refresh "$KUBECONFIG" "GDC Global API kubeconfig"

USER_CLUSTERS_COUNT=$(yq eval '.gdc.userClusters | length' "$CONFIG_FILE")
if [[ "$USER_CLUSTERS_COUNT" -gt 0 && "$USER_CLUSTERS_COUNT" != "null" ]]; then
  for (( i=0; i<USER_CLUSTERS_COUNT; i++ )); do
    VANILLA_USER_CLUSTER_NAME=$(yq eval ".gdc.userClusters[$i].name" "$CONFIG_FILE")
    VANILLA_USER_CLUSTER_ZONE=$(yq eval ".gdc.userClusters[$i].zone" "$CONFIG_FILE")
    PROJECT=$(yq eval ".gdc.userClusters[$i].project" "$CONFIG_FILE")

    echo "Processing GDC User Cluster: $VANILLA_USER_CLUSTER_NAME in $VANILLA_USER_CLUSTER_ZONE ($PROJECT)"

    export KUBECONFIG="/tmp/kubeconfigs/management-api-${VANILLA_USER_CLUSTER_ZONE}-kubeconfig"
    gdcloud clusters get-credentials "${ORG_NAME}-admin" --zone="${VANILLA_USER_CLUSTER_ZONE}" >&2
    log_kubeconfig_refresh "$KUBECONFIG" "Management API kubeconfig"

    export KUBECONFIG="/tmp/kubeconfigs/${VANILLA_USER_CLUSTER_NAME}-kubeconfig"
    gdcloud clusters get-credentials "$VANILLA_USER_CLUSTER_NAME" --standard --project "$PROJECT" --zone "$VANILLA_USER_CLUSTER_ZONE" >&2
    log_kubeconfig_refresh "$KUBECONFIG" "User Cluster kubeconfig"

    refresh_virtual_garden_and_shoots "$KUBECONFIG" "$VANILLA_USER_CLUSTER_NAME"
  done
else
  log_failure "No GDC User clusters found in config."
fi

echo "All kubeconfigs refreshed successfully."
