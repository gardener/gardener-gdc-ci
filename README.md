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

- **Automated Nightly Schedule**: Runs every weeknight (Monday–Friday at `02:00 UTC`, `cron: '0 2 * * 1-5'`).
- **Manual Dispatch (`workflow_dispatch`)**: Restricted to approved Google and SAP maintainers listed in [`OWNERS_ALIASES`](OWNERS_ALIASES) / [`CODEOWNERS`](CODEOWNERS). Unauthorized manual triggers are automatically blocked by the `authorize` gate job.

## Repository Structure

```text
├── .github/workflows/
│   ├── ci.yaml                    # Presubmit lint, format, unit tests, and binary build
│   └── release-pipeline.yaml      # Nightly (Mon-Fri) & maintainer-triggered release pipeline
├── dashboard/                     # GitHub Pages Release Pipeline Dashboard
├── docs/                          # Design documentation & operational runbooks
├── hack/
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
