# Gardener GDC CI & Release Pipeline (`gardener-gdc-ci`)

[![Release Pipeline](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml/badge.svg)](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml)
[![CI](https://github.com/gardener/gardener-gdc-ci/actions/workflows/ci.yaml/badge.svg)](https://github.com/gardener/gardener-gdc-ci/actions/workflows/ci.yaml)

`gardener-gdc-ci` hosts the end-to-end release pipeline for certifying and releasing Gardener artifacts on Google Distributed Cloud air-gapped.

## Overview

The release pipeline validates the full Gardener GDC stack against the GDC Staging infrastructure (`staging.gpcdemolabs.com`) and GCP Virtual Garden runtime clusters before publishing certified release artifacts to the Gardener public registry (`europe-docker.pkg.dev/gardener-project/public`).

### Key Capabilities

1. **GitHub-Native Artifact Build (`1-build_and_push_artifacts.sh`)**: Builds candidate container images and Helm charts directly from GitHub repositories:
   - [`gardener/gardener-extension-provider-gdc`](https://github.com/gardener/gardener-extension-provider-gdc)
   - [`gardener/machine-controller-manager-provider-gdc`](https://github.com/gardener/machine-controller-manager-provider-gdc)
   - [`gardener/external-dns-management`](https://github.com/gardener/external-dns-management)
   - [`GoogleCloudPlatform/cloud-provider-gdc`](https://github.com/GoogleCloudPlatform/cloud-provider-gdc)
2. **Candidate Staging in GHCR**: Pushes initial candidate images and OCI Helm charts to GitHub Container Registry (`ghcr.io/gardener/gardener-gdc-ci/...`). Although GDC is air-gapped in production, the GDC Staging environment is configured with internet access for easier testing setup and pulls candidate artifacts directly from GHCR without needing to mirror to a GDC cluster registry.
3. **End-to-End Release Certification (`2-prepare_pipeline_configuration.sh` – `7-teardown_gardener.sh`)**:
   - Deploys the Gardener Operator & Virtual Garden on the assigned GKE runtime cluster (`staging-runtime-1..4`)
   - Deploys the GDC Seed (`gardenlet`) on the GDC Virtual User Cluster (`gardener-grover-3`)
   - Provisions a Shoot cluster on GDC and runs functional suites (`shootscaling`, `storage`, `networking`, `loadbalancer`, `dns`, `etcdbackup`) and Kubernetes CNCF `conformance` (`sonobuoy`)
   - Tears down Shoot, Seed, and Virtual Garden resources in an `always()` cleanup block
4. **Public Registry Promotion (`8-publish_release_artifacts.sh`)**: Upon full certification pass, re-tags and publishes certified images, Helm charts, and the release snapshot manifest to `europe-docker.pkg.dev/gardener-project/public`.
5. **Release Pipeline Dashboard (`9-update_dashboard.sh` & `dashboard/`)**: Publishes step-by-step execution history, duration, component SHAs, and links to certified snapshots on GitHub Pages and GitHub Actions Job Summaries.

## Triggering the Release Pipeline

The `Gardener GDC Release Pipeline` workflow ([`.github/workflows/release-pipeline.yaml`](.github/workflows/release-pipeline.yaml)) supports both scheduled nightly runs and on-demand manual runs:

- **Automated Weeknight Schedule (`schedule`)**: Runs automatically every weeknight (Monday–Friday at `02:00 UTC`, `cron: '0 2 * * 1-5'`).
- **Manual Triggers (Maintainer-Only)**: Restricted to approved Google and SAP maintainers listed in [`OWNERS_ALIASES`](OWNERS_ALIASES) / [`CODEOWNERS`](CODEOWNERS). Unauthorized triggers are automatically blocked by the `Authorize Trigger (Google & SAP Maintainers)` gate job before any secrets or infrastructure are accessed.

An authorized maintainer can manually trigger the release pipeline using any of the following methods:

1. **Pull Request Comment (`/test-release`):**
   To validate changes on an open Pull Request in `gardener/gardener-gdc-ci`, leave a comment on the PR:
   ```text
   /test-release
   ```
   When triggered via `/test-release` (or via `workflow_dispatch` with `pr_number`), the workflow reacts with `:rocket:`, checks out the PR branch, merges `origin/main`, runs the full E2E release pipeline, publishes snapshot artifacts to `europe-docker.pkg.dev/gardener-project/snapshots/gardener/gardener-gdc-ci`, and posts the `E2E Release Certification & Promotion` check-run status directly onto the PR's head commit.

2. **GitHub Actions UI (`workflow_dispatch`):**
   Navigate to **[Actions → Gardener GDC Release Pipeline](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml)**, click **Run workflow**, keep **Use workflow from: `Branch: main`**, configure any optional inputs below, and click **Run workflow**:

   | Input | Default | Description |
   | :--- | :--- | :--- |
   | `pr_number` | `""` | Optional Pull Request number to test (merges `main` into the PR branch and reports check-run on the PR) |
   | `extension_gdc_ref` | `main` | Git ref (branch, tag, or commit SHA) for `gardener/gardener-extension-provider-gdc` |
   | `mcm_provider_gdc_ref` | `main` | Git ref (branch, tag, or commit SHA) for `gardener/machine-controller-manager-provider-gdc` |
   | `external_dns_ref` | `master` | Git ref (branch, tag, or commit SHA) for `gardener/external-dns-management` |
   | `cloud_provider_gdc_ref` | `main` | Git ref (branch, tag, or commit SHA) for `GoogleCloudPlatform/cloud-provider-gdc` |
   | `skip_conformance` | `false` | Skip the Sonobuoy CNCF Kubernetes conformance suite for faster validation runs |
   | `publish_to_public_registry` | `true` | Promote certified artifacts to the Gardener public registry in Stage 8 on success |

3. **GitHub CLI (`gh`):**
   ```bash
   # Trigger a standard release certification run on main
   gh workflow run release-pipeline.yaml \
     --repo gardener/gardener-gdc-ci \
     --ref main

   # Trigger a run against a specific Pull Request
   gh workflow run release-pipeline.yaml \
     --repo gardener/gardener-gdc-ci \
     --ref main \
     -f pr_number=<PR_NUMBER>

   # Trigger a run with custom component refs or faster validation options
   gh workflow run release-pipeline.yaml \
     --repo gardener/gardener-gdc-ci \
     --ref main \
     -f extension_gdc_ref=main \
     -f mcm_provider_gdc_ref=main \
     -f external_dns_ref=master \
     -f cloud_provider_gdc_ref=main \
     -f skip_conformance=false \
     -f publish_to_public_registry=true
   ```

## Accessing the GitHub Dashboard & Viewing Job Status

You can monitor live and historical release pipeline executions in three ways:

1. **Interactive GitHub Pages Dashboard (`https://gardener.github.io/gardener-gdc-ci/`):**
   - Hosted from [`dashboard/index.html`](dashboard/index.html) and automatically updated on `main` branch runs by the `Publish Dashboard (GitHub Pages)` job.
   - Displays high-level KPI cards (total runs, pass rate, latest run status, latest certified version), live workflow status fetched from the GitHub Actions REST API, stage-by-stage badges (Stages 1–8), resolved component commit SHAs, and direct links to each workflow run.
   - *Note:* `Publish Dashboard (GitHub Pages)` is intentionally skipped on Pull Request runs so that experimental PR runs do not overwrite the `main` branch release history.

2. **GitHub Actions Job Summary (Available on Every Run, Including PRs):**
   - Open **[Actions → Gardener GDC Release Pipeline](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml)** and click on the target workflow run.
   - Select the **Summary** tab in the top-left pane of the run page and scroll down to **Gardener GDC Release Pipeline Summary**.
   - Stage 9 ([`scripts/release/9-update_dashboard.sh`](scripts/release/9-update_dashboard.sh)) always writes a complete Markdown summary table containing:
     - **Overall Status**, **Trigger / Actor**, and **Artifacts Version**
     - Resolved 7-character **Commit SHAs** for all four source repositories (`gardener-extension-provider-gdc`, `machine-controller-manager-provider-gdc`, `external-dns-management`, `cloud-provider-gdc`)
     - **Candidate Registry** (`ghcr.io/gardener/gardener-gdc-ci`) and **Public Registry** URLs
     - **Stage Progress** table for Stages 1 through 8

3. **Live Stage Logs & Pull Request Checks:**
   - Inside any workflow run, click the **E2E Release Certification & Promotion** job to expand real-time step logs for Stages 1–9.
   - When triggered on a Pull Request (via `/test-release` or `pr_number`), the pipeline also attaches an **E2E Release Certification & Promotion** check-run directly under the PR's **Checks** tab.

## Published Artifacts & Version Scheme

### Target Registries

In Stage 8 ([`scripts/release/8-publish_release_artifacts.sh`](scripts/release/8-publish_release_artifacts.sh)), the workflow authenticates keylessly via GitHub OIDC using `gardener/cc-utils/.github/actions/{params,oci-auth}@v1` and promotes certified artifacts from `ghcr.io/gardener/gardener-gdc-ci` to Google Artifact Registry (`europe-docker.pkg.dev/gardener-project`):

| Run Mode | Target Push Registry (`PUBLIC_REGISTRY`) | Public Read-Only Virtual Registry |
| :--- | :--- | :--- |
| **Release Runs** (`schedule` or `workflow_dispatch` on `main`) | `europe-docker.pkg.dev/gardener-project/releases/gardener/gardener-gdc-ci` | `europe-docker.pkg.dev/gardener-project/public/gardener/gardener-gdc-ci` |
| **Snapshot / PR Runs** (`/test-release` or `workflow_dispatch` with `pr_number`) | `europe-docker.pkg.dev/gardener-project/snapshots/gardener/gardener-gdc-ci` | `europe-docker.pkg.dev/gardener-project/public/gardener/gardener-gdc-ci` |

### Published Container Images

Each certified container image is pushed to `${PUBLIC_REGISTRY}/<image_name>` with **two tags** — its **component candidate tag** and the **unified release bundle tag** (`${RELEASE_VERSION}`):

| Image Name | Source Repository | Component Candidate Tag | Unified Release Bundle Tag |
| :--- | :--- | :--- | :--- |
| `gardener-extension-provider-gdch` | [`gardener/gardener-extension-provider-gdc`](https://github.com/gardener/gardener-extension-provider-gdc) | `v0.0.0-staging.1-<ext_sha>` | `v0.1.0-v0.0.0-staging.1-<ext_sha>` |
| `gardener-extension-admission-gdch` | [`gardener/gardener-extension-provider-gdc`](https://github.com/gardener/gardener-extension-provider-gdc) | `v0.0.0-staging.1-<ext_sha>` | `v0.1.0-v0.0.0-staging.1-<ext_sha>` |
| `machine-controller-manager-provider-gdch` | [`gardener/machine-controller-manager-provider-gdc`](https://github.com/gardener/machine-controller-manager-provider-gdc) | `v0.0.0-staging.1-<mcm_sha>` | `v0.1.0-v0.0.0-staging.1-<ext_sha>` |
| `external-dns-management-gdch` | [`gardener/external-dns-management`](https://github.com/gardener/external-dns-management) (`dns-controller-manager`) | `v0.23.0-staging.1-<dns_sha>` | `v0.1.0-v0.0.0-staging.1-<ext_sha>` |
| `external-dnsman2-gdch` | [`gardener/external-dns-management`](https://github.com/gardener/external-dns-management) (`dns-controller-manager-next-generation`) | `v0.23.0-staging.1-<dns_sha>` | `v0.1.0-v0.0.0-staging.1-<ext_sha>` |
| `cloud-controller-manager-gdch` | [`GoogleCloudPlatform/cloud-provider-gdc`](https://github.com/GoogleCloudPlatform/cloud-provider-gdc) | `v0.0.0-staging.1-<ccm_sha>` | `v0.1.0-v0.0.0-staging.1-<ext_sha>` |

> **Note on `csi-driver-gdch`:** `csi-driver-gdch` is **not** built or published by this pipeline to `ghcr.io` or `europe-docker.pkg.dev/gardener-project`. In production, `csi-driver-gdch` is delivered to SAP via GDC SAR; during staging E2E certification, it is pulled from GDC Staging Harbor as configured under `gardener.csiImages.csiDriver` in [`pipeline-configurations/staging-release-pipeline-configuration.yaml`](pipeline-configurations/staging-release-pipeline-configuration.yaml).

### Published OCI Helm Charts

Packaged Helm charts are promoted to `oci://${PUBLIC_REGISTRY}/charts/<chart_name>:<chart_version>`:

| Chart Name | Source Repository / Path | Chart Version |
| :--- | :--- | :--- |
| `gardener-extension-provider-gdch-helm` | `gardener/gardener-extension-provider-gdc` (`charts/extension-provider`) | `0.0.0-staging.1-<ext_sha>` |
| `admission-gdch-application-helm` | `gardener/gardener-extension-provider-gdc` (`charts/extension-admission/charts/application`) | `0.0.0-staging.1-<ext_sha>` |
| `admission-gdch-runtime-helm` | `gardener/gardener-extension-provider-gdc` (`charts/extension-admission/charts/runtime`) | `0.0.0-staging.1-<ext_sha>` |
| `gardenlet` | `gardener/gardener` (`charts/gardener/gardenlet`) | `<gardener_version>` (e.g., `1.149.3`) |
| `gardener-extension-shoot-dns-service` | `gardener/gardener-extension-shoot-dns-service` (`charts/gardener-extension-shoot-dns-service`) | `<shoot_dns_chart_version>` (e.g., `1.45.0-staging.1`) |
| `gardener-extension-networking-cilium` | `gardener/gardener-extension-networking-cilium` (`charts/gardener-extension-networking-cilium`) | `<cilium_chart_version>` (e.g., `1.38.0`) |
| `gardener-extension-os-gardenlinux` | `gardener/gardener-extension-os-gardenlinux` (`charts/gardener-extension-os-gardenlinux`) | `<gardenlinux_chart_version>` (e.g., `0.26.0`) |

### Version Scheme Explained

The release pipeline uses a two-tier versioning scheme across Stage 1 ([`1-build_and_push_artifacts.sh`](scripts/release/1-build_and_push_artifacts.sh)) and Stage 8 ([`8-publish_release_artifacts.sh`](scripts/release/8-publish_release_artifacts.sh)):

1. **Per-Component Candidate Version (`<base_semver>-staging.1-<7_char_commit_sha>`):**
   - **Container Images:** Tagged as `v0.0.0-staging.1-<sha>` (or `v0.23.0-staging.1-<sha>` for `external-dns-management`), where `<sha>` is the 7-character Git commit SHA (`git rev-parse --short=7 HEAD`) of that component's source repository.
   - **Helm Charts:** Packaged with the leading `v` stripped (`0.0.0-staging.1-<ext_sha>`) so `helm package` validates the version as strict SemVer 2.0.
   - **Why `<sha>` is placed after the final hyphen:**
     - **Per-Repo Traceability:** Each component image tag directly encodes the exact Git commit SHA built from its upstream repository.
     - **Deterministic Cluster & Lock Naming (`GetCommitHashOrSanitize`):** `gardener-release-cli` extracts the substring after the last hyphen (`<ext_sha>`, e.g., `8e432a2`) to construct unique, collision-free names for the E2E test run (`Seed/seed-<ext_sha>`, `Shoot/sh-<ext_sha>`, `release-subnet-v0.0.0-staging.1-<ext_sha>`) and to stamp the runtime cluster lock annotation (`integration.release.sapbtp.gardener/commit-hash: <ext_sha>`).

2. **Unified Release Bundle Tag (`RELEASE_VERSION`):**
   - Defaults to `v0.1.0-${GARDENER_ARTIFACTS_VERSION}` (for example, `v0.1.0-v0.0.0-staging.1-8e432a2`).
   - Applied as a secondary tag across all 6 certified component images during Stage 8 so downstream deployments can reference a single coordinated bundle tag representing a set of GDC Gardener components that passed E2E and CNCF conformance testing together.

## Repository Structure

```text
├── .github/workflows/
│   ├── ci.yaml                    # Presubmit lint, format, unit tests, and binary build
│   └── release-pipeline.yaml      # Nightly (Mon-Fri) & maintainer-triggered release pipeline
├── dashboard/                     # GitHub Pages Release Pipeline Dashboard
├── scripts/
│   ├── ci-common.sh               # Shared helpers (secret masking, gdcloud/sonobuoy setup)
│   └── release/                   # Stage 1-9 release orchestration scripts
├── integration/
│   ├── cmd/
│   │   ├── gardener-release-cli/         # Configuration renderer & runtime cluster selector
│   │   ├── gardener-release-snapshotter/ # Release snapshot generator & publisher
│   │   └── token-helper/                 # Standalone OAuth2 token helper for gdcloud bootstrap
│   ├── pkg/                       # Shared test libraries (auth, config, gardener, gdch, gdcloud, helm, k8s, sonobuoy, vm)
│   └── release/                   # Go E2E test suites (garden, seed, shoot, conformance, storage, networking, etc.)
├── pipeline-configurations/       # Base pipeline configuration templates for GDC Staging
└── snapshot-configurations/       # Base component version & repository snapshot configuration
```

## Local Development

```bash
# Format code and run linters (golangci-lint, go vet)
make check

# Run unit tests
make test

# Build release pipeline CLI binaries into bin/
make build-local
```
