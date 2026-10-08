# Gardener GDC CI & Release Pipeline (`gardener-gdc-ci`)

[![Release Pipeline](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml/badge.svg)](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml)
[![CI](https://github.com/gardener/gardener-gdc-ci/actions/workflows/ci.yaml/badge.svg)](https://github.com/gardener/gardener-gdc-ci/actions/workflows/ci.yaml)

## Goal

`gardener-gdc-ci` is the end-to-end qualification and release certification pipeline for Gardener on Google Distributed Cloud (GDC) air-gapped. Its goal is to ensure that all GDC Gardener provider components work together as a verified stack before promoting container images and Helm charts to the Gardener artifact registry and cutting official SemVer releases.

## What It Does

The pipeline qualifies the following source repositories together in a live GDC Staging environment:

- [`gardener/gardener-extension-provider-gdc`](https://github.com/gardener/gardener-extension-provider-gdc)
- [`gardener/machine-controller-manager-provider-gdc`](https://github.com/gardener/machine-controller-manager-provider-gdc)
- [`GoogleCloudPlatform/cloud-provider-gdc`](https://github.com/GoogleCloudPlatform/cloud-provider-gdc)
- [`gardener/external-dns-management`](https://github.com/gardener/external-dns-management)

Each pipeline run executes the following end-to-end qualification flow:

1. **Build Candidate Artifacts**: Builds container images and OCI Helm charts from the target Git refs of each component repository.
2. **Deploy Gardener Control Plane & Seed**: Deploys a Virtual Garden control plane and registers a GDC Seed cluster (`gardenlet`).
3. **Provision & Validate Tenant Shoot Cluster**: Provisions a tenant Shoot cluster on GDC and executes functional test suites (worker scaling, persistent storage, pod/node networking, load balancers, DNS lifecycle, and etcd backup/restore) followed by the CNCF Kubernetes Conformance suite (`sonobuoy`).
4. **Teardown Environment**: Cleans up all Shoot, Seed, and Virtual Garden test resources (capturing diagnostic snapshots automatically if any stage fails).
5. **Publish Certified Artifacts & Tag Releases**: Promotes verified images and Helm charts to the Gardener registry (`snapshots` or `releases`) and, on official release runs, creates the Git release tags and bumps component `VERSION` files to the next `-dev` version.

---

## How to Trigger the Pipeline

The **[Gardener GDC Release Pipeline](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml)** runs automatically on a schedule and can also be triggered on demand by approved Google and SAP maintainers listed in [`OWNERS_ALIASES`](OWNERS_ALIASES) / [`CODEOWNERS`](CODEOWNERS).

### 1. Automated Weeknight Schedule
Runs automatically every weeknight (**Monday–Friday at `02:00 UTC`**) in `snapshot` mode against the default branches of all component repositories.

### 2. Pull Request Comment (`/test-release`)
To run the full E2E pipeline against an open Pull Request in `gardener/gardener-gdc-ci`, comment on the PR:
```text
/test-release
```

### 3. GitHub Actions UI (`workflow_dispatch`)
Navigate to **[Actions → Gardener GDC Release Pipeline](https://github.com/gardener/gardener-gdc-ci/actions/workflows/release-pipeline.yaml)**, click **Run workflow**, and configure the desired inputs:

| Input | Default | Description |
| :--- | :--- | :--- |
| `release_mode` | `snapshot` | Execution mode: `snapshot` (nightly/pre-release) or `release` (official SemVer release) |
| `next_version` | `bump-patch` | Next `-dev` version bump after a successful official release (`bump-patch`, `bump-minor`, `bump-major`, or `noop`) |
| `pr_number` | `""` | Optional Pull Request number in `gardener-gdc-ci` to test |
| `extension_gdc_ref` | `main` | Git ref (branch, tag, or SHA) for `gardener/gardener-extension-provider-gdc` |
| `mcm_provider_gdc_ref` | `main` | Git ref (branch, tag, or SHA) for `gardener/machine-controller-manager-provider-gdc` |
| `external_dns_ref` | `master` | Git ref (branch, tag, or SHA) for `gardener/external-dns-management` |
| `cloud_provider_gdc_ref` | `main` | Git ref (branch, tag, or SHA) for `GoogleCloudPlatform/cloud-provider-gdc` |
| `skip_conformance` | `false` | Skip the Sonobuoy CNCF conformance suite for faster validation runs |
| `publish_to_public_registry` | `true` | Publish certified artifacts to the Gardener registry on success |

### 4. GitHub CLI (`gh`)
```bash
# Trigger a snapshot qualification run
gh workflow run release-pipeline.yaml \
  --repo gardener/gardener-gdc-ci \
  --ref main \
  -f release_mode=snapshot

# Trigger an official SemVer release and bump patch version to next -dev
gh workflow run release-pipeline.yaml \
  --repo gardener/gardener-gdc-ci \
  --ref main \
  -f release_mode=release \
  -f next_version=bump-patch

# Trigger a run for a specific Pull Request
gh workflow run release-pipeline.yaml \
  --repo gardener/gardener-gdc-ci \
  --ref main \
  -f pr_number=<PR_NUMBER>
```

---

## Monitoring & Release Dashboard

- **Release Pipeline Dashboard ([`https://gardener.github.io/gardener-gdc-ci/`](https://gardener.github.io/gardener-gdc-ci/))**: Displays pass rates, latest certified versions, stage-by-stage status badges, component commit SHAs, and historical run results.
- **GitHub Actions Job Summary**: Every workflow run publishes a summary table under the run's **Summary** tab with stage outcomes, component SHAs, artifact versions, and target registries.

---

## Produced Artifacts & Versioning

Artifact versions are derived from the root `VERSION` file in each component repository (`v0.5.3-dev` in the GDC repositories; `v0.53.0-dev` in `external-dns-management`):

- **`snapshot` Mode (Nightly / PR / Pre-Release):**
  - Uses `<VERSION>-<short_sha>` (for example, `v0.5.3-dev-8e432a2`, and `0.5.3-dev-8e432a2` with the leading `v` stripped for Helm charts).
  - Publishes to **`europe-docker.pkg.dev/gardener-project/snapshots/gardener/gardener-gdc-ci`**.
- **`release` Mode (Official Release):**
  - Strips `-dev` from `VERSION` (for example, `v0.5.3`, and `0.5.3` for Helm charts) for GDC repositories that have code changes since their last published release tag.
  - Publishes to **`europe-docker.pkg.dev/gardener-project/releases/gardener/gardener-gdc-ci`**, creates the corresponding Git release tag and GitHub Release on changed GDC repositories, and bumps their `VERSION` file according to `next_version`.

### Published Container Images & Helm Charts

| Artifact Name | Type | Source Repository | `snapshot` Version | `release` Version |
| :--- | :--- | :--- | :--- | :--- |
| `gardener-extension-provider-gdch` | Image | `gardener/gardener-extension-provider-gdc` | `<VERSION>-<ext_sha>` (e.g. `v0.5.3-dev-8e432a2`) | `${VERSION%-dev}` (e.g. `v0.5.3`) |
| `gardener-extension-admission-gdch` | Image | `gardener/gardener-extension-provider-gdc` | `<VERSION>-<ext_sha>` (e.g. `v0.5.3-dev-8e432a2`) | `${VERSION%-dev}` (e.g. `v0.5.3`) |
| `machine-controller-manager-provider-gdch` | Image | `gardener/machine-controller-manager-provider-gdc` | `<VERSION>-<mcm_sha>` (e.g. `v0.5.3-dev-3f9a1c4`) | `${VERSION%-dev}` (e.g. `v0.5.3`) |
| `cloud-controller-manager-gdch` | Image | `GoogleCloudPlatform/cloud-provider-gdc` | `<VERSION>-<ccm_sha>` (e.g. `v0.5.3-dev-7b2d4e1`) | `${VERSION%-dev}` (e.g. `v0.5.3`) |
| `external-dnsman2-gdch` | Image | `gardener/external-dns-management` | `<VERSION>-<dns_sha>` (e.g. `v0.53.0-dev-a1b2c3d`) | `<VERSION>-<dns_sha>` (e.g. `v0.53.0-dev-a1b2c3d`) |
| `gardener-extension-provider-gdch-helm` | Helm Chart | `gardener/gardener-extension-provider-gdc` | `${VERSION#v}-<ext_sha>` (e.g. `0.5.3-dev-8e432a2`) | `${RELEASE_VER#v}` (e.g. `0.5.3`) |
| `admission-gdch-application-helm` | Helm Chart | `gardener/gardener-extension-provider-gdc` | `${VERSION#v}-<ext_sha>` (e.g. `0.5.3-dev-8e432a2`) | `${RELEASE_VER#v}` (e.g. `0.5.3`) |
| `admission-gdch-runtime-helm` | Helm Chart | `gardener/gardener-extension-provider-gdc` | `${VERSION#v}-<ext_sha>` (e.g. `0.5.3-dev-8e432a2`) | `${RELEASE_VER#v}` (e.g. `0.5.3`) |
