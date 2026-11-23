# Release Pipeline CLI Helpers

This directory provides local command-line utilities for inspecting and managing the Gardener GDC release pipeline test environments (GDC Virtual Garden runtime clusters, Virtual Gardens, GDC Soil User Clusters, Seeds, and Shoots).

---

## Scripts Overview

| Script | Description |
| :--- | :--- |
| [`cli.sh`](./cli.sh) | Unified entry point for checking cluster status, refreshing kubeconfigs, or tearing down an active test environment. |
| [`refresh-kubeconfigs.sh`](./refresh-kubeconfigs.sh) | Fetches and refreshes kubeconfigs for all configured GDC clusters (Global API, Management API, Virtual Garden User Clusters, Soil User Clusters), Virtual Gardens, and Shoot clusters into `/tmp/kubeconfigs/`. |
| [`teardown.sh`](./teardown.sh) | Runs the `integration/release/teardown` test suite for a specific `--gardener-artifacts-version` to clean up Shoot, Seed, and Virtual Garden resources and release cluster leases. |

---

## Prerequisites

1. **`kubectl`**, **`yq`**, and **`jq`** (used by `cli.sh` and `refresh-kubeconfigs.sh` to query clusters and render tables).
2. **`gdcloud` CLI** (with `gdcloud-k8s-auth-plugin`) logged in to the target GDC organization (`~/.config/gdcloud`):
   ```bash
   gdcloud auth login
   ```
   - Installed in `PATH` (or under `~/Downloads/google-distributed-cloud-hosted-cli/bin` / `~/google-distributed-cloud-hosted-cli/bin`).
3. **Go** (`go >= 1.25`, required only for `--action=teardown`).

### How `cli.sh` Resolves Cluster Access

- **`status` and `fetch-kubeconfigs` (Pure Shell)**:
  - Uses your existing local `gdcloud` CLI session directly (`~/.config/gdcloud`) — no GDC service account keys or Go compilation required.
  - When `/tmp/kubeconfigs/<cluster>-kubeconfig` files already exist and are reachable (for example, after running `./release-pipeline/cli.sh --action=fetch-kubeconfigs`), `cli.sh --action=status` reuses them directly for faster execution and automatically refreshes any missing or stale kubeconfigs via `gdcloud`.
- **`teardown`**:
  - Runs the `integration/release/teardown` Go test suite. When `GDC_RELEASE_SERVICE_ACCOUNT_KEY` or `GDC_SERVICE_ACCOUNT_FILE` is set, it authenticates with that service account; otherwise it clones your active local `gdcloud` CLI session (`~/.config/gdcloud` and `~/.ais`) into a hermetic temporary config directory without modifying your workstation state.

---

## Usage (`cli.sh`)

Run `./release-pipeline/cli.sh` from the repository root:

```bash
./release-pipeline/cli.sh [--action=<status|fetch-kubeconfigs|teardown>] [--config=<path>] [--virtual-garden-provider=<gdc|gke>] [extra_args...]
```

If `--config` is omitted, it defaults to [`pipeline-configurations/staging-release-pipeline-configuration.yaml`](../pipeline-configurations/staging-release-pipeline-configuration.yaml). `--virtual-garden-provider` defaults to `gdc`.

### 1. View Cluster & Release Job Status (Default Action)

Lists active release test jobs (and their leased clusters, seeds, and shoots) alongside all available GDC Virtual Garden runtime clusters and Soil Shoot clusters in the pool:

```bash
./release-pipeline/cli.sh
# or explicitly:
./release-pipeline/cli.sh --action=status
```

> [!NOTE]
> The staging pool has a single Virtual Garden host (`gdc.virtualGardenClusters`), so only one release run can be active at a time. While a run holds the host (for example the nightly), a local or manual run of `scripts/release/*` with another artifacts version fails in Stage 3 with `no idle or matching Garden cluster found`. Check the status first, or add another host to the pool.

To use a custom pipeline configuration file:

```bash
./release-pipeline/cli.sh --action=status --config=/path/to/config.yaml
```

### 2. Fetch / Refresh All Kubeconfigs

Downloads kubeconfigs for all GDC Global/Management/User clusters (`gdc.virtualGardenClusters` and `gdc.userClusters`), Virtual Gardens, and Shoot clusters defined in the pipeline configuration into `/tmp/kubeconfigs/` (with `0600` permissions):

```bash
./release-pipeline/cli.sh --action=fetch-kubeconfigs
```

Generated kubeconfigs are written to `/tmp/kubeconfigs/`:
- `/tmp/kubeconfigs/<org>-global-kubeconfig`
- `/tmp/kubeconfigs/management-api-<zone>-kubeconfig`
- `/tmp/kubeconfigs/<user-cluster>-kubeconfig` (e.g., `gardener-grover-4-kubeconfig`, `gardener-grover-3-kubeconfig`)
- `/tmp/kubeconfigs/<user-cluster>-virtual-garden-kubeconfig` (e.g., `gardener-grover-4-virtual-garden-kubeconfig`, `gardener-grover-3-virtual-garden-kubeconfig`)
- `/tmp/kubeconfigs/<shoot-name>-kubeconfig` (e.g., `sh-grv-1-a-kubeconfig`)

### 3. Teardown an Active Test Environment

Cleans up all Gardener test resources (Shoots, Parent Subnets, Seeds, and cluster lease annotations) associated with a specific `gardener-artifacts` version:

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
