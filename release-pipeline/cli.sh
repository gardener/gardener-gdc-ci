#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0
#
# cli.sh is the unified command-line entry point for inspecting and managing
# Gardener GDC release pipeline test environments locally.
# Supported actions:
#   - status (default): Queries GDC Virtual Garden runtime clusters and GDC Soil
#     user clusters to display active release test jobs and available idle clusters.
#   - fetch-kubeconfigs: Delegates to refresh-kubeconfigs.sh to populate
#     /tmp/kubeconfigs/ for all configured GDC clusters, Virtual Gardens, and Shoots.
#   - teardown: Delegates to teardown.sh to clean up test resources and release
#     cluster locks for a given gardener-artifacts version.

set -euo pipefail

# Ensure the gdcloud CLI and its k8s auth plugin (gdcloud-k8s-auth-plugin) are
# discoverable in PATH on developer workstations when not installed globally.
if ! command -v gdcloud >/dev/null 2>&1; then
  for gdcloud_bin_dir in "${HOME}/Downloads/google-distributed-cloud-hosted-cli/bin" "${HOME}/google-distributed-cloud-hosted-cli/bin" "/usr/local/bin/google-distributed-cloud-hosted-cli/bin"; do
    if [[ -d "${gdcloud_bin_dir}" && ":${PATH}:" != *":${gdcloud_bin_dir}:"* ]]; then
      export PATH="${gdcloud_bin_dir}:${PATH}"
      break
    fi
  done
fi

# Annotation key used by the release pipeline to lock/claim a Garden or Shoot
# cluster for a specific gardener-artifacts version.
COMMIT_HASH_ANNOTATION="integration.release.sapbtp.gardener/commit-hash"

CLI_TMP_DIR=""
cleanup_cli_tmp() {
  if [[ -n "${CLI_TMP_DIR:-}" && -d "${CLI_TMP_DIR}" ]]; then
    rm -rf "${CLI_TMP_DIR}"
  fi
}
trap cleanup_cli_tmp EXIT

usage() {
  cat << 'EOF'
Usage:
  ./release-pipeline/cli.sh [--action=<status|fetch-kubeconfigs|teardown>] [--config=<path>] [--virtual-garden-provider=<gdc|gke>] [extra_args...]

Usage examples:

1. Get status (default action):
   ./release-pipeline/cli.sh
   ./release-pipeline/cli.sh --action=status

2. Fetch / refresh GDC kubeconfigs into /tmp/kubeconfigs/:
   ./release-pipeline/cli.sh --action=fetch-kubeconfigs

3. Specify a custom configuration file:
   ./release-pipeline/cli.sh --action=status --config=/path/to/config.yaml
   (Default config: pipeline-configurations/staging-release-pipeline-configuration.yaml)

4. Teardown an active test environment for a specific gardener-artifacts version:
   ./release-pipeline/cli.sh --action=teardown --gardener-artifacts-version=v0.5.3-dev-1234567
   ./release-pipeline/cli.sh --action=teardown --gardener-artifacts-version=v0.5.3-dev-1234567 --config=/path/to/config.yaml

Note: Any additional action-specific arguments are passed directly to the underlying scripts.
EOF
}

validate_virtual_garden_provider() {
  local provider="$1"
  case "$provider" in
    gdc|gke)
      ;;
    *)
      echo "Error: unsupported --virtual-garden-provider '${provider}' (supported: 'gdc', 'gke')" >&2
      exit 1
      ;;
  esac
}

# sanitize_hash extracts the short commit hash suffix from a version string
# (e.g., "v0.5.3-dev-8e432a2" -> "8e432a2"), or replaces dots with hyphens when
# no hyphen is present (e.g., "v0.5.3" -> "v0-5-3"). Used to match Seed and
# Shoot names created by a test run (e.g., "seed-<hash>", "sh-<hash>").
sanitize_hash() {
  local version="$1"
  if [[ "$version" == *-* ]]; then
    echo "${version##*-}"
  else
    echo "${version//./-}"
  fi
}

# print_json_table renders a JSON array of header strings ($1) and a JSON array
# of row arrays ($2) into a bordered ASCII table. Supports multi-line cells (\n)
# and strips ANSI color escape codes when calculating column widths so colored
# status labels align properly.
print_json_table() {
  local headers_json="$1"
  local rows_json="$2"

  jq -r -n --argjson headers "$headers_json" --argjson rows "$rows_json" '
    # Compute visible string length excluding ANSI color escape sequences.
    def visible_len(s):
      (s | gsub("\u001b\\[[0-9;]*m"; "") | length);

    # Pad string on the right with spaces up to the target visible column width.
    def pad_right(s; width):
      s + (" " * ([0, width - visible_len(s)] | max));

    # Combine headers and data rows to compute the max width of each column.
    ([ $headers ] + $rows) as $all_rows |
    (
      [ range(0; ($headers | length)) as $col |
        ([ $all_rows[] | (.[$col] // "") | split("\n")[] | visible_len(.) ] | max)
      ]
    ) as $widths |
    # Build horizontal border separator line (e.g., "+------+------+").
    (
      "+" + ([ $widths[] | ("-" * (. + 2)) ] | join("+")) + "+"
    ) as $sep |

    # Print header row followed by each data row (expanding multi-line cells).
    $sep,
    ("| " + ([ range(0; ($headers | length)) as $i | pad_right($headers[$i]; $widths[$i]) ] | join(" | ")) + " |"),
    $sep,
    (
      $rows[] as $row |
      ([ $row[] | split("\n") ]) as $split_cells |
      ([ $split_cells[] | length ] | max) as $max_h |
      (
        range(0; ([1, $max_h] | max)) as $line_idx |
        "| " + (
          [ range(0; ($headers | length)) as $col |
            pad_right(($split_cells[$col][$line_idx] // ""); $widths[$col])
          ] | join(" | ")
        ) + " |"
      ),
      $sep
    )
  '
}

# extract_virtual_garden_kubeconfig reads the base64-encoded Virtual Garden
# kubeconfig from the "gardener" Secret in the "garden" namespace of a runtime
# cluster ($1) and writes the decoded kubeconfig to $2.
extract_virtual_garden_kubeconfig() {
  local runtime_kubeconfig="$1"
  local output_path="$2"

  local b64
  b64=$(kubectl --kubeconfig "$runtime_kubeconfig" --request-timeout=15s get secret -n garden gardener -o jsonpath='{.data.kubeconfig}' 2>/dev/null || true)
  if [[ -z "$b64" ]]; then
    return 1
  fi
  (
    umask 077
    printf '%s' "$b64" | base64 -d > "$output_path"
  )
}

# resolve_gdc_user_cluster_kubeconfig returns a working kubeconfig path for a
# GDC user cluster, reusing /tmp/kubeconfigs/<name>-kubeconfig when valid and
# otherwise fetching fresh credentials into $CLI_TMP_DIR via gdcloud.
resolve_gdc_user_cluster_kubeconfig() {
  local uc_name="$1"
  local uc_project="$2"
  local uc_zone="$3"

  local cached_kc="/tmp/kubeconfigs/${uc_name}-kubeconfig"
  if [[ -s "$cached_kc" ]] && kubectl --kubeconfig "$cached_kc" --request-timeout=10s get ns default >/dev/null 2>&1; then
    printf '%s' "$cached_kc"
    return 0
  fi

  local tmp_kc="${CLI_TMP_DIR}/${uc_name}-kubeconfig"
  if (umask 077 && KUBECONFIG="$tmp_kc" gdcloud clusters get-credentials "$uc_name" --standard --project "$uc_project" --zone "$uc_zone" >/dev/null 2>&1); then
    printf '%s' "$tmp_kc"
    return 0
  fi

  return 1
}

# run_status inspects the configured GDC Virtual Garden runtime clusters (or GKE
# runtime clusters when --virtual-garden-provider=gke) and GDC Soil user clusters
# defined in the pipeline configuration YAML ($1), correlates active test runs by
# their commit-hash annotation, and prints the Active Release Jobs and Available
# Clusters tables.
run_status() {
  local config_file="$1"
  local vg_provider="$2"

  if [[ ! -f "$config_file" ]]; then
    echo "Error: configuration file not found: $config_file" >&2
    exit 1
  fi

  echo "Loading configuration..."
  CLI_TMP_DIR="$(mktemp -d /tmp/gardener-cli-status.XXXXXX)"

  # Accumulators:
  # - available_json: JSON array of [name, type, hosted_by] rows for idle clusters.
  # - jobs_json: JSON object keyed by gardenerArtifactsVersion tracking active jobs.
  local available_json="[]"
  local jobs_json="{}"

  echo "Fetching Cluster Status..."

  # Helper: inspect_vg_host_cluster <cluster_name> <kubeconfig> <type_label> <hosted_by>
  inspect_vg_host_cluster() {
    local cluster_name="$1"
    local kubeconfig="$2"
    local type_label="$3"
    local hosted_by="$4"

    local gardens_json
    gardens_json=$(kubectl --kubeconfig "$kubeconfig" --request-timeout=15s get gardens.operator.gardener.cloud -A -o json 2>/dev/null || echo '{"items":[]}')

    local item_count
    item_count=$(jq '.items | length' <<< "$gardens_json")
    for (( g=0; g<item_count; g++ )); do
      local garden_name version
      garden_name=$(jq -r ".items[$g].metadata.name" <<< "$gardens_json")
      version=$(jq -r ".items[$g].metadata.annotations[\"${COMMIT_HASH_ANNOTATION}\"] // \"\"" <<< "$gardens_json")

      # An unannotated Garden CR means the runtime cluster is idle and available.
      if [[ -z "$version" ]]; then
        available_json=$(jq -c \
          --arg name "$cluster_name" \
          --arg type "$type_label" \
          --arg hosted "$hosted_by" \
          '. + [[$name, $type, $hosted]]' <<< "$available_json")
        continue
      fi

      # An annotated Garden CR indicates an active release job. Connect to its
      # Virtual Garden API server to inspect any Seeds and Shoots created by the job.
      local created_seeds="" created_shoots=""
      local vg_kubeconfig="${CLI_TMP_DIR}/${cluster_name}-vg-kubeconfig"
      if extract_virtual_garden_kubeconfig "$kubeconfig" "$vg_kubeconfig"; then
        local sanitized_hash
        sanitized_hash="$(sanitize_hash "$version")"

        # Query Seed resources on the Virtual Garden matching the job's version/hash.
        local seeds_raw shoots_raw
        seeds_raw=$(kubectl --kubeconfig "$vg_kubeconfig" --request-timeout=15s get seeds -o json 2>/dev/null || echo '{"items":[]}')
        created_seeds=$(jq -r \
          --arg ver "$version" \
          --arg hash "$sanitized_hash" \
          --arg ann "$COMMIT_HASH_ANNOTATION" '
          def seed_status:
            if .status.lastOperation == null then "PENDING"
            elif .status.lastOperation.progress != 100 then "\u001b[31mNOT READY\u001b[0m"
            elif ([.status.conditions[]? | select(.status == "False")] | length) > 0 then "\u001b[31mNOT READY\u001b[0m"
            else "\u001b[32mREADY\u001b[0m"
            end;
          [
            .items[]
            | select((.metadata.name | contains($hash)) or (.metadata.annotations[$ann] == $ver))
            | "\(.metadata.name) [ \(seed_status) ]"
          ] | join("\n")
        ' <<< "$seeds_raw")

        # Query Shoot resources on the Virtual Garden matching the job's version/hash.
        shoots_raw=$(kubectl --kubeconfig "$vg_kubeconfig" --request-timeout=15s get shoots -A -o json 2>/dev/null || echo '{"items":[]}')
        created_shoots=$(jq -r \
          --arg ver "$version" \
          --arg hash "$sanitized_hash" \
          --arg ann "$COMMIT_HASH_ANNOTATION" '
          def shoot_status:
            if .status.lastOperation == null then "PENDING"
            else
              (.status.lastOperation.state // "") as $st |
              (.status.lastOperation.type // "") as $tp |
              (.status.lastOperation.progress // 0) as $pr |
              (if $st == "Succeeded" then "\u001b[32mSUCCEEDED\u001b[0m"
               elif $st == "Processing" then "\u001b[34mPROCESSING\u001b[0m"
               elif ($st == "Error" or $st == "Failed") then "\u001b[31m\($st)\u001b[0m"
               else $st end) + " (\($tp) \($pr)%)"
            end;
          [
            .items[]
            | select((.metadata.name | contains($hash)) or (.metadata.annotations[$ann] == $ver))
            | "\(.metadata.name) [ \(shoot_status) ]"
          ] | join("\n")
        ' <<< "$shoots_raw")
      fi

      # Record or update the active job entry keyed by gardenerArtifactsVersion.
      jobs_json=$(jq -c \
        --arg ver "$version" \
        --arg vg "$garden_name" \
        --arg rt "$hosted_by" \
        --arg seeds "$created_seeds" \
        --arg shoots "$created_shoots" '
        .[$ver] = ((.[$ver] // {
          version: $ver,
          runtimeCluster: "",
          virtualGarden: "",
          soilCluster: "",
          createdSeeds: "",
          createdShoots: ""
        }) | .virtualGarden = $vg | .runtimeCluster = $rt | .createdSeeds = $seeds | .createdShoots = $shoots)
      ' <<< "$jobs_json")
    done
  }

  local use_uc_as_seed
  use_uc_as_seed=$(yq eval '(.gdc.useUserClusterAsSeed // false)' "$config_file")

  # ---------------------------------------------------------------------------
  # 1. Inspect Virtual Garden Runtime Clusters
  # ---------------------------------------------------------------------------
  if [[ "$vg_provider" == "gdc" ]]; then
    local vg_yq_path=".gdc.virtualGardenClusters"
    local gdc_vg_count
    gdc_vg_count=$(yq eval '(.gdc.virtualGardenClusters // []) | length' "$config_file")
    if [[ ( "$gdc_vg_count" -eq 0 || "$gdc_vg_count" == "null" ) && "$use_uc_as_seed" == "true" ]]; then
      vg_yq_path=".gdc.userClusters"
      gdc_vg_count=$(yq eval '(.gdc.userClusters // []) | length' "$config_file")
    fi

    if [[ "$gdc_vg_count" -gt 0 && "$gdc_vg_count" != "null" ]]; then
      for (( i=0; i<gdc_vg_count; i++ )); do
        local vg_uc_name vg_uc_zone vg_uc_project vg_uc_kubeconfig
        vg_uc_name=$(yq eval "${vg_yq_path}[$i].name" "$config_file")
        vg_uc_zone=$(yq eval "${vg_yq_path}[$i].zone" "$config_file")
        vg_uc_project=$(yq eval "${vg_yq_path}[$i].project" "$config_file")

        if ! vg_uc_kubeconfig=$(resolve_gdc_user_cluster_kubeconfig "$vg_uc_name" "$vg_uc_project" "$vg_uc_zone"); then
          echo "Warning: could not get credentials for GDC Virtual Garden host cluster '$vg_uc_name'; skipping it (is the local gdcloud session still valid?)." >&2
          continue
        fi

        local gdc_vg_hosted_by
        gdc_vg_hosted_by=$(printf "GDC: %s\n%s\n%s" "$vg_uc_name" "$vg_uc_project" "$vg_uc_zone")
        inspect_vg_host_cluster "$vg_uc_name" "$vg_uc_kubeconfig" "GDC (Virtual Garden Host)" "$gdc_vg_hosted_by"
      done
    fi
  else
    local gke_count
    gke_count=$(yq eval '(.gcp.gkeClusters // []) | length' "$config_file")
    if [[ "$gke_count" -gt 0 && "$gke_count" != "null" ]]; then
      for (( i=0; i<gke_count; i++ )); do
        local cluster_name zone project kubeconfig
        cluster_name=$(yq eval ".gcp.gkeClusters[$i].name" "$config_file")
        zone=$(yq eval ".gcp.gkeClusters[$i].zone" "$config_file")
        project=$(yq eval ".gcp.gkeClusters[$i].project" "$config_file")

        kubeconfig="/tmp/kubeconfigs/${cluster_name}-kubeconfig"
        if [[ ! -s "$kubeconfig" ]] || ! kubectl --kubeconfig "$kubeconfig" --request-timeout=10s get ns default >/dev/null 2>&1; then
          kubeconfig="${CLI_TMP_DIR}/${cluster_name}-kubeconfig"
          (umask 077 && KUBECONFIG="$kubeconfig" gcloud container clusters get-credentials "$cluster_name" --region "$zone" --project "$project" >/dev/null 2>&1)
        fi

        local hosted_by
        hosted_by=$(printf "GKE: %s\n%s\n%s" "$cluster_name" "$project" "$zone")
        inspect_vg_host_cluster "$cluster_name" "$kubeconfig" "GKE (Virtual Garden Host)" "$hosted_by"
      done
    fi
  fi

  # ---------------------------------------------------------------------------
  # 2. Inspect GDC User Clusters (Hosting Soil Shoot Clusters for Seeds)
  # ---------------------------------------------------------------------------
  local uc_count
  uc_count=$(yq eval '(.gdc.userClusters // []) | length' "$config_file")
  if [[ "$use_uc_as_seed" != "true" && "$uc_count" -gt 0 && "$uc_count" != "null" ]]; then
    for (( i=0; i<uc_count; i++ )); do
      local uc_name uc_zone uc_project uc_kubeconfig
      uc_name=$(yq eval ".gdc.userClusters[$i].name" "$config_file")
      uc_zone=$(yq eval ".gdc.userClusters[$i].zone" "$config_file")
      uc_project=$(yq eval ".gdc.userClusters[$i].project" "$config_file")

      if ! uc_kubeconfig=$(resolve_gdc_user_cluster_kubeconfig "$uc_name" "$uc_project" "$uc_zone"); then
        echo "Warning: could not get credentials for GDC user cluster '$uc_name'; skipping it (is the local gdcloud session still valid?)." >&2
        continue
      fi

      # Extract a fresh Virtual Garden kubeconfig from the GDC user cluster
      # (falling back to a cached virtual-garden kubeconfig only if reachable).
      local vg_kubeconfig="${CLI_TMP_DIR}/${uc_name}-vg-kubeconfig"
      if ! extract_virtual_garden_kubeconfig "$uc_kubeconfig" "$vg_kubeconfig"; then
        if [[ -s "/tmp/kubeconfigs/${uc_name}-virtual-garden-kubeconfig" ]]; then
          vg_kubeconfig="/tmp/kubeconfigs/${uc_name}-virtual-garden-kubeconfig"
        else
          continue
        fi
      fi

      # List Shoot clusters on the GDC Virtual Garden (these Shoots act as Soil
      # clusters that host Gardener Seeds during release pipeline runs).
      local shoots_json
      if ! shoots_json=$(kubectl --kubeconfig "$vg_kubeconfig" --request-timeout=15s get shoots -A -o json 2>/dev/null); then
        continue
      fi

      local gdc_hosted_by
      gdc_hosted_by=$(printf "GDC: %s\n%s\n%s" "$uc_name" "$uc_project" "$uc_zone")

      local shoot_count
      shoot_count=$(jq '.items | length' <<< "$shoots_json")
      for (( s=0; s<shoot_count; s++ )); do
        local shoot_name shoot_ver shoot_status_str
        shoot_name=$(jq -r ".items[$s].metadata.name" <<< "$shoots_json")
        shoot_ver=$(jq -r ".items[$s].metadata.annotations[\"${COMMIT_HASH_ANNOTATION}\"] // \"\"" <<< "$shoots_json")

        # An unannotated Shoot is idle and available to host a Seed.
        if [[ -z "$shoot_ver" ]]; then
          local avail_hosted
          avail_hosted=$(printf "%s\n(on %s)" "$uc_name" "$gdc_hosted_by")
          available_json=$(jq -c \
            --arg name "$shoot_name" \
            --arg type "Shoot (Seed Host)" \
            --arg hosted "$avail_hosted" \
            '. + [[$name, $type, $hosted]]' <<< "$available_json")
          continue
        fi

        # Format the status of the claimed Soil Shoot and attach it to the
        # corresponding active release job entry.
        shoot_status_str=$(jq -r "
          .items[$s] |
          if .status.lastOperation == null then \"PENDING\"
          else
            (.status.lastOperation.state // \"\") as \$st |
            (.status.lastOperation.type // \"\") as \$tp |
            (.status.lastOperation.progress // 0) as \$pr |
            (if \$st == \"Succeeded\" then \"\u001b[32mSUCCEEDED\u001b[0m\"
             elif \$st == \"Processing\" then \"\u001b[34mPROCESSING\u001b[0m\"
             elif (\$st == \"Error\" or \$st == \"Failed\") then \"\u001b[31m\(\$st)\u001b[0m\"
             else \$st end) + \" (\(\$tp) \(\$pr)%)\"
          end
        " <<< "$shoots_json")

        local host_info
        host_info=$(printf "%s [ %s ]\n(on %s)" "$shoot_name" "$shoot_status_str" "$gdc_hosted_by")

        jobs_json=$(jq -c \
          --arg ver "$shoot_ver" \
          --arg soil "$host_info" '
          if has($ver) then
            .[$ver].soilCluster = (if .[$ver].soilCluster == "" then $soil else (.[$ver].soilCluster + "\n" + $soil) end)
          else . end
        ' <<< "$jobs_json")
      done
    done
  fi

  # ---------------------------------------------------------------------------
  # 3. Render Output Tables
  # ---------------------------------------------------------------------------
  echo ""
  echo "Listing Active Release Jobs..."
  local active_rows_json
  active_rows_json=$(jq -c '[ .[] | [.version, .runtimeCluster, .virtualGarden, .soilCluster, .createdSeeds, .createdShoots] ]' <<< "$jobs_json")
  if [[ $(jq 'length' <<< "$active_rows_json") -eq 0 ]]; then
    echo "No active jobs found."
  else
    print_json_table \
      '["ARTIFACTS VERSION", "RUNTIME CLUSTER", "VIRTUAL GARDEN", "SOIL CLUSTER (SEED HOST)", "CREATED SEEDS", "CREATED SHOOTS"]' \
      "$active_rows_json"
  fi

  echo ""
  echo "Listing Available Clusters..."
  if [[ $(jq 'length' <<< "$available_json") -eq 0 ]]; then
    echo "No available clusters found."
  else
    print_json_table \
      '["NAME", "TYPE", "HOSTED_BY"]' \
      "$available_json"
  fi
}

# -----------------------------------------------------------------------------
# CLI Argument Parsing & Action Dispatch
# -----------------------------------------------------------------------------
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEFAULT_CONFIG="${REPO_ROOT}/pipeline-configurations/staging-release-pipeline-configuration.yaml"

ACTION="status"
CONFIG="$DEFAULT_CONFIG"
VIRTUAL_GARDEN_PROVIDER="${VIRTUAL_GARDEN_PROVIDER:-gdc}"
SHOW_HELP="false"
EXTRA_ARGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      SHOW_HELP="true"
      shift
      ;;
    --action)
      if [[ $# -lt 2 ]]; then
        echo "Error: --action requires a value" >&2
        usage
        exit 1
      fi
      ACTION="$2"
      shift 2
      ;;
    --action=*)
      ACTION="${1#*=}"
      shift
      ;;
    --config)
      if [[ $# -lt 2 ]]; then
        echo "Error: --config requires a value" >&2
        usage
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
        usage
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
      EXTRA_ARGS+=("$1")
      shift
      ;;
  esac
done

validate_virtual_garden_provider "$VIRTUAL_GARDEN_PROVIDER"

if [[ "$SHOW_HELP" == "true" ]]; then
  usage
  exit 0
fi

case "$ACTION" in
  status)
    if [[ ${#EXTRA_ARGS[@]} -gt 0 ]]; then
      echo "Error: unknown argument(s) for --action=status: ${EXTRA_ARGS[*]}" >&2
      usage
      exit 1
    fi
    run_status "$CONFIG" "$VIRTUAL_GARDEN_PROVIDER"
    ;;
  fetch-kubeconfigs)
    "${REPO_ROOT}/release-pipeline/refresh-kubeconfigs.sh" \
      --config="$CONFIG" \
      --virtual-garden-provider="$VIRTUAL_GARDEN_PROVIDER" \
      "${EXTRA_ARGS[@]}"
    ;;
  teardown)
    "${REPO_ROOT}/release-pipeline/teardown.sh" \
      --config="$CONFIG" \
      --virtual-garden-provider="$VIRTUAL_GARDEN_PROVIDER" \
      "${EXTRA_ARGS[@]}"
    ;;
  *)
    echo "Unknown action: $ACTION" >&2
    usage
    exit 1
    ;;
esac
