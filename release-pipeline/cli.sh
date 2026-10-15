#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0
#
# cli.sh is the unified command-line entry point for inspecting and managing
# Gardener GDC release pipeline test environments locally.
# Supported actions:
#   - status (default): Queries GKE runtime clusters and GDC user clusters to
#     display active release test jobs and available idle clusters.
#   - fetch-kubeconfigs: Delegates to refresh-kubeconfigs.sh to populate
#     /tmp/kubeconfigs/ for all configured clusters.
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

usage() {
  cat << 'EOF'
Usage examples:

1. Get status (default action):
   ./release-pipeline/cli.sh
   ./release-pipeline/cli.sh --action=status

2. Fetch kubeconfigs:
   ./release-pipeline/cli.sh --action=fetch-kubeconfigs

3. Specify a custom configuration file:
   ./release-pipeline/cli.sh --action=status --config=/path/to/config.yaml
   (Default config: pipeline-configurations/staging-release-pipeline-configuration.yaml)

4. Teardown an active test environment for a specific gardener-artifacts version:
   ./release-pipeline/cli.sh --action=teardown --gardener-artifacts-version=v1.0 --config=/path/to/config.yaml

Note: Any additional arguments are passed directly to the underlying scripts.
EOF
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
  printf '%s' "$b64" | base64 -d > "$output_path"
}

# run_status inspects all GKE runtime clusters and GDC user clusters defined in
# the pipeline configuration YAML ($1), correlates active test runs by their
# commit-hash annotation, and prints the Active Release Jobs and Available
# Clusters tables.
run_status() {
  local config_file="$1"
  if [[ ! -f "$config_file" ]]; then
    echo "Error: configuration file not found: $config_file" >&2
    exit 1
  fi

  echo "Loading configuration..."
  # Create a temporary directory for any ephemeral kubeconfigs fetched during
  # this status check and clean it up automatically on script exit.
  local tmp_dir
  tmp_dir="$(mktemp -d /tmp/gardener-cli-status.XXXXXX)"
  trap "rm -rf '${tmp_dir}'" EXIT

  # Accumulators:
  # - available_json: JSON array of [name, type, hosted_by] rows for idle clusters.
  # - jobs_json: JSON object keyed by gardenerArtifactsVersion tracking active jobs.
  local available_json="[]"
  local jobs_json="{}"

  echo "Fetching Cluster Status..."

  # ---------------------------------------------------------------------------
  # 1. Inspect GCP GKE Runtime Clusters (Virtual Garden Hosts)
  # ---------------------------------------------------------------------------
  local gke_count
  gke_count=$(yq eval '.gcp.gkeClusters | length' "$config_file")
  if [[ "$gke_count" -gt 0 && "$gke_count" != "null" ]]; then
    for (( i=0; i<gke_count; i++ )); do
      local cluster_name zone project
      cluster_name=$(yq eval ".gcp.gkeClusters[$i].name" "$config_file")
      zone=$(yq eval ".gcp.gkeClusters[$i].zone" "$config_file")
      project=$(yq eval ".gcp.gkeClusters[$i].project" "$config_file")

      # Reuse cached kubeconfig from /tmp/kubeconfigs/ if present; otherwise
      # fetch credentials into the temporary directory via gcloud.
      local kubeconfig="/tmp/kubeconfigs/${cluster_name}-kubeconfig"
      if [[ ! -s "$kubeconfig" ]]; then
        kubeconfig="${tmp_dir}/${cluster_name}-kubeconfig"
        KUBECONFIG="$kubeconfig" gcloud container clusters get-credentials "$cluster_name" --region "$zone" --project "$project" >/dev/null 2>&1
      fi

      local hosted_by
      hosted_by=$(printf "GKE: %s\n%s\n%s" "$cluster_name" "$project" "$zone")

      # List Garden CRs (gardens.operator.gardener.cloud) on the GKE runtime cluster.
      local gardens_json
      gardens_json=$(kubectl --kubeconfig "$kubeconfig" --request-timeout=15s get gardens.operator.gardener.cloud -A -o json 2>/dev/null || echo '{"items":[]}')

      local item_count
      item_count=$(jq '.items | length' <<< "$gardens_json")
      for (( g=0; g<item_count; g++ )); do
        local garden_name version
        garden_name=$(jq -r ".items[$g].metadata.name" <<< "$gardens_json")
        version=$(jq -r ".items[$g].metadata.annotations[\"${COMMIT_HASH_ANNOTATION}\"] // \"\"" <<< "$gardens_json")

        # An unannotated Garden CR means the GKE runtime cluster is idle and available.
        if [[ -z "$version" ]]; then
          available_json=$(jq -c \
            --arg name "$cluster_name" \
            --arg type "GKE (Virtual Garden Host)" \
            --arg hosted "$hosted_by" \
            '. + [[$name, $type, $hosted]]' <<< "$available_json")
          continue
        fi

        # An annotated Garden CR indicates an active release job. Connect to its
        # Virtual Garden API server to inspect any Seeds and Shoots created by the job.
        local created_seeds="" created_shoots=""
        local vg_kubeconfig="${tmp_dir}/${cluster_name}-vg-kubeconfig"
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
    done
  fi

  # ---------------------------------------------------------------------------
  # 2. Inspect GDC User Clusters (Hosting Soil Shoot Clusters for Seeds)
  # ---------------------------------------------------------------------------
  local uc_count
  uc_count=$(yq eval '.gdc.userClusters | length' "$config_file")
  if [[ "$uc_count" -gt 0 && "$uc_count" != "null" ]]; then
    for (( i=0; i<uc_count; i++ )); do
      local uc_name uc_zone uc_project
      uc_name=$(yq eval ".gdc.userClusters[$i].name" "$config_file")
      uc_zone=$(yq eval ".gdc.userClusters[$i].zone" "$config_file")
      uc_project=$(yq eval ".gdc.userClusters[$i].project" "$config_file")

      # Reuse cached GDC user cluster kubeconfig if present; otherwise fetch
      # credentials via gdcloud using the developer's local gdcloud session.
      local uc_kubeconfig="/tmp/kubeconfigs/${uc_name}-kubeconfig"
      if [[ ! -s "$uc_kubeconfig" ]]; then
        uc_kubeconfig="${tmp_dir}/${uc_name}-kubeconfig"
        KUBECONFIG="$uc_kubeconfig" gdcloud clusters get-credentials "$uc_name" --standard --project "$uc_project" --zone "$uc_zone" >/dev/null 2>&1 || continue
      fi

      # Extract the Virtual Garden kubeconfig hosted on the GDC user cluster
      # (falling back to a cached virtual-garden kubeconfig if available).
      local vg_kubeconfig="${tmp_dir}/${uc_name}-vg-kubeconfig"
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
      shoots_json=$(kubectl --kubeconfig "$vg_kubeconfig" --request-timeout=15s get shoots -A -o json 2>/dev/null || continue)

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
EXTRA_ARGS=()

while [[ $# -gt 0 ]]; do
  case "$1" in
    -h|--help)
      usage
      exit 0
      ;;
    --action)
      ACTION="$2"
      shift 2
      ;;
    --action=*)
      ACTION="${1#*=}"
      shift
      ;;
    --config)
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

case "$ACTION" in
  status)
    run_status "$CONFIG"
    ;;
  fetch-kubeconfigs)
    "${REPO_ROOT}/release-pipeline/refresh-kubeconfigs.sh" --config "$CONFIG" "${EXTRA_ARGS[@]}"
    ;;
  teardown)
    "${REPO_ROOT}/release-pipeline/teardown.sh" --config="$CONFIG" "${EXTRA_ARGS[@]}"
    ;;
  *)
    echo "Unknown action: $ACTION" >&2
    usage
    exit 1
    ;;
esac
