# Release Pipeline CLI Helpers

This directory provides local command-line utilities for inspecting and managing the Gardener GDC release pipeline test environments (GKE runtime clusters, Virtual Gardens, GDC User Clusters, Seeds, and Shoots).

---

## Scripts Overview

| Script | Description |
| :--- | :--- |
| [`cli.sh`](./cli.sh) | Unified entry point for checking cluster status, refreshing kubeconfigs, or tearing down an active test environment. |
| [`refresh-kubeconfigs.sh`](./refresh-kubeconfigs.sh) | Fetches and refreshes kubeconfigs for all configured GKE runtime clusters, GDC clusters (Global API, Management API, User Clusters), Virtual Gardens, and Shoot clusters into `/tmp/kubeconfigs/`. |
| [`teardown.sh`](./teardown.sh) | Runs the `integration/release/teardown` test suite for a specific `--gardener-artifacts-version` to clean up Shoot, Seed, and Virtual Garden resources and release cluster leases. |

---

## Prerequisites

1. **`kubectl`**, **`yq`**, and **`jq`** (used by `cli.sh` and `refresh-kubeconfigs.sh` to query clusters and render tables).
2. **Google Cloud SDK (`gcloud`)** authenticated with access to the `dilipkumardk-gardener-lab` GCP project (and `gardener-ci` when running `teardown`):
   ```bash
   gcloud auth login
   ```
3. **`gdcloud` CLI** (with `gdcloud-k8s-auth-plugin`) logged in to the target GDC organization (`~/.config/gdcloud`):
   - Installed in `PATH` (or under `~/Downloads/google-distributed-cloud-hosted-cli/bin` / `~/google-distributed-cloud-hosted-cli/bin`).
4. **Go** (`go >= 1.25`, required only for `--action=teardown`).

### How `cli.sh` Resolves Cluster Access

- **`status` and `fetch-kubeconfigs` (Pure Shell)**:
  - Uses your existing local `gcloud` and `gdcloud` CLI sessions directly (`~/.config/gdcloud`) — no GDC service account keys or Go compilation required.
  - When `/tmp/kubeconfigs/<cluster>-kubeconfig` files already exist (for example, after running `./release-pipeline/cli.sh --action=fetch-kubeconfigs`), `cli.sh --action=status` reuses them directly for faster execution.
- **`teardown`**:
  - Runs the `integration/release/teardown` Go test suite. If `GDC_RELEASE_SERVICE_ACCOUNT_KEY` and `GDC_RELEASE_HARBOR_DOCKER_CONFIG_JSON` are not set in the environment, `teardown.sh` fetches them automatically from GCP Secret Manager (`gardener-ci` project).

---

## Usage (`cli.sh`)

Run `./release-pipeline/cli.sh` from the repository root:

```bash
./release-pipeline/cli.sh [--action=<status|fetch-kubeconfigs|teardown>] [--config=<path>] [extra_args...]
```

If `--config` is omitted, it defaults to [`pipeline-configurations/staging-release-pipeline-configuration.yaml`](../pipeline-configurations/staging-release-pipeline-configuration.yaml).

### 1. View Cluster & Release Job Status (Default Action)

Lists active release test jobs (and their leased clusters, seeds, and shoots) alongside all available GKE and GDC clusters in the pool:

```bash
./release-pipeline/cli.sh
# or explicitly:
./release-pipeline/cli.sh --action=status
```

To use a custom pipeline configuration file:

```bash
./release-pipeline/cli.sh --action=status --config=/path/to/config.yaml
```

### 2. Fetch / Refresh All Kubeconfigs

Downloads kubeconfigs for all GKE clusters, GDC Global/Management/User clusters, Virtual Gardens, and Shoot clusters defined in the pipeline configuration into `/tmp/kubeconfigs/`:

```bash
./release-pipeline/cli.sh --action=fetch-kubeconfigs
```

Generated kubeconfigs are written to `/tmp/kubeconfigs/`:
- `/tmp/kubeconfigs/<gke-cluster>-kubeconfig`
- `/tmp/kubeconfigs/<cluster>-virtual-garden-kubeconfig`
- `/tmp/kubeconfigs/<shoot-name>-kubeconfig`
- `/tmp/kubeconfigs/<org>-global-kubeconfig`
- `/tmp/kubeconfigs/management-api-<zone>-kubeconfig`
- `/tmp/kubeconfigs/<user-cluster>-kubeconfig`

### 3. Teardown an Active Test Environment

Cleans up all Gardener test resources (Shoots, Seeds, Virtual Garden) associated with a specific `gardener-artifacts` version and releases its cluster locks:

```bash
./release-pipeline/cli.sh --action=teardown --gardener-artifacts-version=v0.0.1-dev-12345
```

With a custom configuration file:

```bash
./release-pipeline/cli.sh \
  --action=teardown \
  --gardener-artifacts-version=v0.0.1-dev-12345 \
  --config=pipeline-configurations/staging-release-pipeline-configuration.yaml
```
