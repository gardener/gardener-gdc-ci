#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=scripts/release/helpers/constants.sh
source "${SCRIPT_DIR}/helpers/constants.sh"
# shellcheck source=scripts/release/helpers/imagevector.sh
source "${SCRIPT_DIR}/helpers/imagevector.sh"

echo "=== Stage 8: Promote Certified Artifacts from GHCR to Gardener Registry ==="

if [[ ! -f "${RELEASE_METADATA_FILE}" ]]; then
  echo "::error::Release metadata file (${RELEASE_METADATA_FILE}) not found." >&2
  exit 1
fi

RELEASE_MODE="${RELEASE_MODE:-$(yq -r '.releaseMode // "snapshot"' "${RELEASE_METADATA_FILE}")}"
NEXT_VERSION="${NEXT_VERSION:-$(yq -r '.nextVersion // "bump-patch"' "${RELEASE_METADATA_FILE}")}"
GARDENER_ARTIFACTS_VERSION="$(yq -r '.gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTENSION_GDC_ARTIFACTS_VERSION="$(yq -r '.extensionGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
MCM_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.mcmProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"
EXTERNAL_DNS_ARTIFACTS_VERSION="$(yq -r '.externalDNSArtifactsVersion' "${RELEASE_METADATA_FILE}")"
CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION="$(yq -r '.cloudProviderGDCArtifactsVersion // .gardenerArtifactsVersion' "${RELEASE_METADATA_FILE}")"

EXT_NEEDS_RELEASE="$(yq -r '.components.gardenerExtensionProviderGDC.needsRelease != false' "${RELEASE_METADATA_FILE}")"
MCM_NEEDS_RELEASE="$(yq -r '.components.machineControllerManagerProviderGDC.needsRelease != false' "${RELEASE_METADATA_FILE}")"
CCM_NEEDS_RELEASE="$(yq -r '.components.cloudProviderGDC.needsRelease != false' "${RELEASE_METADATA_FILE}")"

EXT_LAST_TAG="$(yq -r '.components.gardenerExtensionProviderGDC.lastReleaseTag // ""' "${RELEASE_METADATA_FILE}")"
MCM_LAST_TAG="$(yq -r '.components.machineControllerManagerProviderGDC.lastReleaseTag // ""' "${RELEASE_METADATA_FILE}")"
CCM_LAST_TAG="$(yq -r '.components.cloudProviderGDC.lastReleaseTag // ""' "${RELEASE_METADATA_FILE}")"
EXT_COMMIT_SHA="$(yq -r '.components.gardenerExtensionProviderGDC.commitSHA // ""' "${RELEASE_METADATA_FILE}")"
EXT_REPO="$(yq -r '.components.gardenerExtensionProviderGDC.repository' "${RELEASE_METADATA_FILE}")"
EXT_REF="$(yq -r '.components.gardenerExtensionProviderGDC.ref' "${RELEASE_METADATA_FILE}")"
MCM_COMMIT_SHA="$(yq -r '.components.machineControllerManagerProviderGDC.commitSHA // ""' "${RELEASE_METADATA_FILE}")"
MCM_REPO="$(yq -r '.components.machineControllerManagerProviderGDC.repository' "${RELEASE_METADATA_FILE}")"
MCM_REF="$(yq -r '.components.machineControllerManagerProviderGDC.ref' "${RELEASE_METADATA_FILE}")"
DNS_COMMIT_SHA="$(yq -r '.components.externalDNSManagement.commitSHA // ""' "${RELEASE_METADATA_FILE}")"
DNS_REPO="$(yq -r '.components.externalDNSManagement.repository' "${RELEASE_METADATA_FILE}")"
DNS_REF="$(yq -r '.components.externalDNSManagement.ref' "${RELEASE_METADATA_FILE}")"
CCM_COMMIT_SHA="$(yq -r '.components.cloudProviderGDC.commitSHA // ""' "${RELEASE_METADATA_FILE}")"
CCM_REPO="$(yq -r '.components.cloudProviderGDC.repository' "${RELEASE_METADATA_FILE}")"
CCM_REF="$(yq -r '.components.cloudProviderGDC.ref' "${RELEASE_METADATA_FILE}")"
EXT_CHART_VERSION="${EXTENSION_GDC_ARTIFACTS_VERSION#v}"

# Optional OCM attachments must belong to this run, including local reruns.
rm -f "${RELEASE_WORK_DIR}/gdc-release-notes.md" "${RELEASE_ARTIFACTS_DIR}/gosec-report.sarif"

IMAGES=()

# In `snapshot` mode, promote the candidate extension images directly from GHCR.
# In `release` mode, extension images are compiled and pushed after reapplying the
# updated `imagevector/images.yaml` onto the exact tested commit SHA (`EXT_COMMIT_SHA`).
if [[ "${RELEASE_MODE}" == "snapshot" ]]; then
  IMAGES+=(
    "gardener-extension-provider-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
    "gardener-extension-admission-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
  )
elif [[ "${EXT_NEEDS_RELEASE}" != "true" ]]; then
  echo "Skipping Gardener release registry promotion for gardener-extension-provider-gdc: no code changes since ${EXT_LAST_TAG}."
fi

if [[ "${RELEASE_MODE}" == "snapshot" || "${MCM_NEEDS_RELEASE}" == "true" ]]; then
  IMAGES+=(
    "machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
  )
else
  echo "Skipping Gardener release registry promotion for machine-controller-manager-provider-gdc: no code changes since ${MCM_LAST_TAG}."
fi

if [[ "${RELEASE_MODE}" == "snapshot" || "${CCM_NEEDS_RELEASE}" == "true" ]]; then
  IMAGES+=(
    "cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
  )
else
  echo "Skipping Gardener release registry promotion for cloud-provider-gdc: no code changes since ${CCM_LAST_TAG}."
fi

# Publish tested `external-dnsman2-gdch` image to `${PUBLIC_REGISTRY}` (`snapshots` or `releases/gardener/gardener-gdc-ci`)
# so consumers have the exact certified version alongside the GDC release artifacts.
IMAGES+=(
  "external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}"
)

PUBLISHED_IMAGES_JSON="[]"

for item in "${IMAGES[@]}"; do
  name="${item%%:*}"
  version_tag="${item##*:}"
  src_image="${GHCR_REGISTRY}/${name}:${version_tag}"
  dst_image="${PUBLIC_REGISTRY}/${name}:${version_tag}"

  echo "Promoting ${src_image} -> ${dst_image}..."
  docker pull "${src_image}"
  docker tag "${src_image}" "${dst_image}"
  docker push "${dst_image}"

  PUBLISHED_IMAGES_JSON="$(jq --arg name "${name}" --arg image "${dst_image}" --arg version "${version_tag}" \
    '. + [{"component": $name, "image": $image, "version": $version}]' <<< "${PUBLISHED_IMAGES_JSON}")"
done

# Step 5: Since Helm charts are not compile-time embedded, continue sourcing them from GHCR / Stage 1
# candidate artifacts (`${CHARTS_OUT_DIR}`) to publish to the Gardener Release Registry.
CHARTS_OUT_DIR="${RELEASE_ARTIFACTS_DIR}/charts"
PUBLISHED_CHARTS_JSON="[]"
if [[ "${RELEASE_MODE}" == "snapshot" || "${EXT_NEEDS_RELEASE}" == "true" ]]; then
  if [[ -d "${CHARTS_OUT_DIR}" ]]; then
    for chart_tgz in "${CHARTS_OUT_DIR}"/*.tgz; do
      [[ -e "${chart_tgz}" ]] || continue
      chart_file="$(basename "${chart_tgz}")"
      echo "Promoting Helm chart ${chart_file} -> oci://${PUBLIC_REGISTRY}/charts..."
      if [[ -f "${HOME}/.docker/config.json" ]]; then
        helm push --registry-config "${HOME}/.docker/config.json" "${chart_tgz}" "oci://${PUBLIC_REGISTRY}/charts"
      else
        helm push "${chart_tgz}" "oci://${PUBLIC_REGISTRY}/charts"
      fi
      PUBLISHED_CHARTS_JSON="$(jq --arg chart "oci://${PUBLIC_REGISTRY}/charts/${chart_file%.tgz}" \
        '. + [$chart]' <<< "${PUBLISHED_CHARTS_JSON}")"
    done
  fi
else
  echo "Skipping Gardener release registry Helm chart promotion: no code changes in gardener-extension-provider-gdc since ${EXT_LAST_TAG}."
fi

compute_next_dev_version() {
  local ver="${1#v}"
  local op="$2"
  local major minor patch
  IFS='.' read -r major minor patch <<< "${ver%%-*}"
  case "${op}" in
    bump-patch)
      echo "v${major}.${minor}.$((patch + 1))-dev"
      ;;
    bump-minor)
      echo "v${major}.$((minor + 1)).0-dev"
      ;;
    bump-major)
      echo "v$((major + 1)).0.0-dev"
      ;;
    noop)
      echo ""
      ;;
    *)
      echo "::error::Unsupported next_version operation: ${op}" >&2
      return 1
      ;;
  esac
}

generate_subcomponent_notes_for_range() {
  local repo_url="$1"
  local from_tag="$2"
  local to_ver="$3"
  local subcomponent_notes_out="$4"

  local repo_slug="${repo_url#https://github.com/}"
  repo_slug="${repo_slug%.git}"
  local push_token="${GDC_RELEASE_GITHUB_TOKEN:-${GH_TOKEN:-}}"
  local tmp_dir
  tmp_dir="$(mktemp -d)"

  if git clone --tags "${repo_url}" "${tmp_dir}" >/dev/null 2>&1; then
    if git -C "${tmp_dir}" rev-parse -q --verify "refs/tags/${to_ver}" >/dev/null 2>&1; then
      git -C "${tmp_dir}" checkout "refs/tags/${to_ver}" >/dev/null 2>&1 || true
    fi
    GDC_RELEASE_GITHUB_TOKEN="${push_token}" python3 "${SCRIPT_DIR}/helpers/generate_release_notes.py" \
      --repo-dir "${tmp_dir}" \
      --repo-slug "${repo_slug}" \
      --version "${to_ver}" \
      --from-tag "${from_tag}" \
      --output "${tmp_dir}/full-notes.md" \
      --subcomponent-output "${subcomponent_notes_out}" || true
  fi
  rm -rf "${tmp_dir}"
}

tag_and_bump_gdc_repository() {
  local repo_url="$1"
  local ref="$2"
  local release_ver="$3"
  local next_op="$4"
  local last_tag="$5"
  local subcomponent_notes_out="$6"
  local persist_notes_out="$7"
  shift 7
  local extra_notes_args=("$@")

  local repo_slug="${repo_url#https://github.com/}"
  repo_slug="${repo_slug%.git}"

  local push_token="${GDC_RELEASE_GITHUB_TOKEN:-${GH_TOKEN:-}}"
  if [[ -z "${push_token}" ]]; then
    echo "::error::GDC_RELEASE_GITHUB_TOKEN (or GH_TOKEN) is required to tag and bump ${repo_slug} in release mode." >&2
    return 1
  fi

  local auth_url="https://x-access-token:${push_token}@github.com/${repo_slug}.git"
  local work_dir
  work_dir="$(mktemp -d)"

  echo "Tagging ${repo_slug} with release ${release_ver} (last_tag=${last_tag:-none}, next_version=${next_op})..."
  git clone "${auth_url}" "${work_dir}" >/dev/null 2>&1

  local target_branch="main"
  if git -C "${work_dir}" ls-remote --exit-code --heads origin "${ref}" >/dev/null 2>&1; then
    target_branch="${ref}"
  fi
  git -C "${work_dir}" checkout "${target_branch}" >/dev/null 2>&1
  git -C "${work_dir}" config user.name "github-actions[bot]"
  git -C "${work_dir}" config user.email "41898282+github-actions[bot]@users.noreply.github.com"
  git -C "${work_dir}" config commit.gpgsign false
  git -C "${work_dir}" config tag.gpgSign false
  git -C "${work_dir}" config tag.forceSignAnnotated false

  local effective_from_tag="${last_tag}"
  if [[ "${effective_from_tag}" == "${release_ver}" ]]; then
    effective_from_tag="$(git -C "${work_dir}" tag --list 'v*' --sort=-v:refname | grep -vxF "${release_ver}" | head -n 1 || true)"
  fi

  # If releasing gardener-extension-provider-gdc, update imagevector/images.yaml so the
  # release commit and main branch record the certified MCM and CCM versions.
  if [[ -f "${work_dir}/imagevector/images.yaml" ]]; then
    local prev_mcm_tag prev_ccm_tag prev_imagevector_file="${work_dir}/imagevector/images.yaml"
    if [[ -n "${effective_from_tag}" ]] && git -C "${work_dir}" show "${effective_from_tag}:imagevector/images.yaml" > "${work_dir}/prev-images.yaml" 2>/dev/null; then
      prev_imagevector_file="${work_dir}/prev-images.yaml"
    fi
    prev_mcm_tag="$(read_imagevector_tag "${prev_imagevector_file}" "machine-controller-manager-provider-gdch")"
    prev_ccm_tag="$(read_imagevector_tag "${prev_imagevector_file}" "cloud-controller-manager")"
    rm -f "${work_dir}/prev-images.yaml"
    update_extension_imagevector "${work_dir}/imagevector/images.yaml" \
      "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
      "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" \
      "${PUBLIC_REGISTRY}"
    extra_notes_args+=(
      "--image-update" "machine-controller-manager-provider-gdch:${prev_mcm_tag}:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
      "--image-update" "cloud-controller-manager:${prev_ccm_tag}:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
    )

    # If a subcomponent was already tagged in a previous partial attempt (so MCM_NEEDS_RELEASE or
    # CCM_NEEDS_RELEASE was false on retry, but imagevector/images.yaml is still advancing from
    # prev_tag to the new version), generate its subcomponent notes on demand for idempotency.
    if [[ -n "${MCM_SUBCOMPONENT_NOTES:-}" && ! -f "${MCM_SUBCOMPONENT_NOTES}" && -n "${prev_mcm_tag}" && "${prev_mcm_tag}" != "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" ]]; then
      generate_subcomponent_notes_for_range "${MCM_REPO}" "${prev_mcm_tag}" "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" "${MCM_SUBCOMPONENT_NOTES}"
      if [[ -f "${MCM_SUBCOMPONENT_NOTES}" ]]; then
        extra_notes_args+=("--subcomponent-notes" "${MCM_SUBCOMPONENT_NOTES}")
      fi
    fi
    if [[ -n "${CCM_SUBCOMPONENT_NOTES:-}" && ! -f "${CCM_SUBCOMPONENT_NOTES}" && -n "${prev_ccm_tag}" && "${prev_ccm_tag}" != "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" ]]; then
      generate_subcomponent_notes_for_range "${CCM_REPO}" "${prev_ccm_tag}" "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" "${CCM_SUBCOMPONENT_NOTES}"
      if [[ -f "${CCM_SUBCOMPONENT_NOTES}" ]]; then
        extra_notes_args+=("--subcomponent-notes" "${CCM_SUBCOMPONENT_NOTES}")
      fi
    fi
  fi

  # Generate SAP Gardener-formatted release notes before creating the release commit
  local notes_file="${work_dir}/release-notes.md"
  local sub_args=()
  if [[ -n "${subcomponent_notes_out}" ]]; then
    sub_args=("--subcomponent-output" "${subcomponent_notes_out}")
  fi
  GDC_RELEASE_GITHUB_TOKEN="${push_token}" python3 "${SCRIPT_DIR}/helpers/generate_release_notes.py" \
    --repo-dir "${work_dir}" \
    --repo-slug "${repo_slug}" \
    --version "${release_ver}" \
    --from-tag "${effective_from_tag}" \
    --output "${notes_file}" \
    "${sub_args[@]}" \
    "${extra_notes_args[@]}"

  # Persist the aggregated release notes outside the temp dir so later pipeline stages
  # (e.g. embedding them into the OCM component descriptor) can consume them.
  if [[ -n "${persist_notes_out}" ]]; then
    cp "${notes_file}" "${persist_notes_out}"
  fi

  # 1. Commit `release vX.Y.Z` (stripping `-dev` in VERSION + updated imagevector/images.yaml if present), tag `vX.Y.Z`, and push.
  # Idempotent on retry: if `refs/tags/${release_ver}` already exists on origin with identical code/imagevector state,
  # skip creating a duplicate release commit on top of the post-release `-dev` branch commit.
  local tag_already_matches="false"
  if git -C "${work_dir}" rev-parse -q --verify "refs/tags/${release_ver}" >/dev/null 2>&1; then
    if git -C "${work_dir}" diff --quiet "refs/tags/${release_ver}" -- . ':!VERSION'; then
      tag_already_matches="true"
    fi
  fi

  if [[ "${tag_already_matches}" == "true" ]]; then
    echo "Tag ${release_ver} already exists on ${repo_slug} with matching content; skipping duplicate tag push."
  else
    echo "${release_ver}" > "${work_dir}/VERSION"
    git -C "${work_dir}" add VERSION
    if [[ -f "${work_dir}/imagevector/images.yaml" ]]; then
      git -C "${work_dir}" add imagevector/images.yaml
    fi
    if ! git -C "${work_dir}" diff --cached --quiet; then
      git -C "${work_dir}" commit -s -m "release ${release_ver}"
    fi
    git -C "${work_dir}" tag -f "${release_ver}"
    git -C "${work_dir}" push origin "HEAD:refs/heads/${target_branch}" "+refs/tags/${release_ver}"
  fi

  # Create or update GitHub Release with Gardener-structured release notes
  if ! GH_TOKEN="${push_token}" gh release view "${release_ver}" --repo "${repo_slug}" >/dev/null 2>&1; then
    GH_TOKEN="${push_token}" gh release create "${release_ver}" \
      --repo "${repo_slug}" \
      --title "${release_ver}" \
      --notes-file "${notes_file}"
  else
    GH_TOKEN="${push_token}" gh release edit "${release_ver}" \
      --repo "${repo_slug}" \
      --title "${release_ver}" \
      --notes-file "${notes_file}"
  fi

  # 2. Bump `VERSION` to next `-dev` version and push directly to target branch
  if [[ "${next_op}" != "noop" ]]; then
    local next_dev_ver
    next_dev_ver="$(compute_next_dev_version "${release_ver}" "${next_op}")"
    if [[ -n "${next_dev_ver}" ]]; then
      echo "${next_dev_ver}" > "${work_dir}/VERSION"
      git -C "${work_dir}" add VERSION
      if ! git -C "${work_dir}" diff --cached --quiet; then
        git -C "${work_dir}" commit -s -m "next version: ${next_dev_ver}"
        git -C "${work_dir}" push origin "HEAD:refs/heads/${target_branch}"
      fi
    fi
  fi

  rm -rf "${work_dir}"
}

# Run gosec SAST on the extension source tree (already cloned at the certified commit)
# and persist the SARIF report so it can be attached to the OCM component descriptor.
# Blocking: unsuppressed SAST findings or scan failures abort the release.
# Use a pinned gosec version supporting Go 1.26 with Gardener's canonical sast.sh.
run_gosec_sast_scan() {
  local src_dir="$1"
  local report_out="$2"
  local gosec_version="v2.29.0"
  local gosec_bin_dir="${RELEASE_WORK_DIR}/sast-bin"

  # Resolve dependencies before locating the module's scan script or running gosec.
  if ! ( cd "${src_dir}" && go mod download ); then
    echo "::error::Could not resolve scan dependencies for gosec SAST scan." >&2
    return 1
  fi
  local gardener_hack_dir
  if ! gardener_hack_dir="$(cd "${src_dir}" && go list -m -f '{{.Dir}}' github.com/gardener/gardener)"; then
    echo "::error::Could not locate Gardener module for gosec SAST scan." >&2
    return 1
  fi
  gardener_hack_dir="${gardener_hack_dir}/hack"
  if [[ ! -f "${gardener_hack_dir}/sast.sh" ]]; then
    echo "::error::Gardener sast.sh not found at ${gardener_hack_dir}." >&2
    return 1
  fi

  echo "Installing gosec (${gosec_version}) for SAST scan of gardener-extension-provider-gdc..."
  if ! GOBIN="${gosec_bin_dir}" go install "github.com/securego/gosec/v2/cmd/gosec@${gosec_version}"; then
    echo "::error::Could not install gosec (${gosec_version}) for SAST scan." >&2
    return 1
  fi

  echo "Running gosec SAST scan on gardener-extension-provider-gdc source..."
  # sast.sh invokes `gosec` from PATH and exits non-zero when unsuppressed findings are detected.
  if ! ( cd "${src_dir}" && PATH="${gosec_bin_dir}:${PATH}" bash "${gardener_hack_dir}/sast.sh" \
      --gosec-report true --report-dir "$(dirname "${report_out}")" --report-fname "$(basename "${report_out}")" ); then
    echo "::error::gosec SAST scan failed or reported unsuppressed security findings in gardener-extension-provider-gdc." >&2
    return 1
  fi

  if [[ ! -s "${report_out}" ]]; then
    echo "::error::gosec did not produce a SARIF report at ${report_out}." >&2
    return 1
  fi

  local unsuppressed_count
  unsuppressed_count="$(jq '[.runs[0].results[]? | select((.suppressions // []) | length == 0)] | length' "${report_out}")"
  if [[ "${unsuppressed_count}" -gt 0 ]]; then
    echo "::error::gosec SAST scan reported ${unsuppressed_count} unsuppressed finding(s) in ${report_out}." >&2
    return 1
  fi

  echo "gosec SAST scan passed with 0 unsuppressed findings. Report written to ${report_out}."
}

build_and_publish_extension_release_images() {
  local repo_url="$1"
  local exact_sha="$2"
  local release_ver="$3"

  local ext_build_dir
  ext_build_dir="$(mktemp -d)"

  echo "Cloning ${repo_url} at exact certified commit ${exact_sha} to build release images with updated imagevector..."
  git clone "${repo_url}" "${ext_build_dir}" >/dev/null 2>&1
  git -C "${ext_build_dir}" checkout "${exact_sha}" >/dev/null 2>&1

  echo "Applying imagevector/images.yaml updates (MCM=${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}, CCM=${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}) on commit ${exact_sha}..."
  update_extension_imagevector "${ext_build_dir}/imagevector/images.yaml" \
    "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
    "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" \
    "${PUBLIC_REGISTRY}"
  echo "${release_ver}" > "${ext_build_dir}/VERSION"

  # Scan the release source before building or publishing its images.
  run_gosec_sast_scan "${ext_build_dir}" "${RELEASE_ARTIFACTS_DIR}/gosec-report.sarif"

  echo "Compiling and building gardener-extension-provider-gdc Docker images (${release_ver})..."
  REGISTRY="${PUBLIC_REGISTRY}" IMAGE_TAG="${release_ver}" make -C "${ext_build_dir}" docker-images

  for ext_img in "gardener-extension-provider-gdch" "gardener-extension-admission-gdch"; do
    local dst_image="${PUBLIC_REGISTRY}/${ext_img}:${release_ver}"
    echo "Pushing compiled release image ${dst_image} to Gardener Release Registry..."
    docker push "${dst_image}"
    PUBLISHED_IMAGES_JSON="$(jq --arg name "${ext_img}" --arg image "${dst_image}" --arg version "${release_ver}" \
      '. + [{"component": $name, "image": $image, "version": $version}]' <<< "${PUBLISHED_IMAGES_JSON}")"
  done

  rm -rf "${ext_build_dir}"
}

if [[ "${RELEASE_MODE}" == "release" ]]; then
  echo "=== Building Release Extension Images, Updating imagevector/images.yaml, Tagging & Bumping VERSION in Changed GDC Repositories ==="
  MCM_SUBCOMPONENT_NOTES="${RELEASE_WORK_DIR}/mcm-subcomponent-notes.md"
  CCM_SUBCOMPONENT_NOTES="${RELEASE_WORK_DIR}/ccm-subcomponent-notes.md"
  EXT_RELEASE_NOTES="${RELEASE_WORK_DIR}/gdc-release-notes.md"
  rm -f "${MCM_SUBCOMPONENT_NOTES}" "${CCM_SUBCOMPONENT_NOTES}"

  # 1. Release machine-controller-manager-provider-gdc first (if changed)
  if [[ "${MCM_NEEDS_RELEASE}" == "true" ]]; then
    tag_and_bump_gdc_repository \
      "${MCM_REPO}" \
      "${MCM_REF}" \
      "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
      "${NEXT_VERSION}" \
      "${MCM_LAST_TAG}" \
      "${MCM_SUBCOMPONENT_NOTES}" \
      "" \
      --oci-image "machine-controller-manager-provider-gdch=${PUBLIC_REGISTRY}/machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
  else
    echo "Skipping Git tag and VERSION bump for ${MCM_REPO}: no code changes since ${MCM_LAST_TAG}."
  fi

  # 2. Release cloud-provider-gdc second (if changed)
  if [[ "${CCM_NEEDS_RELEASE}" == "true" ]]; then
    tag_and_bump_gdc_repository \
      "${CCM_REPO}" \
      "${CCM_REF}" \
      "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" \
      "${NEXT_VERSION}" \
      "${CCM_LAST_TAG}" \
      "${CCM_SUBCOMPONENT_NOTES}" \
      "" \
      --oci-image "cloud-controller-manager-gdch=${PUBLIC_REGISTRY}/cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
  else
    echo "Skipping Git tag and VERSION bump for ${CCM_REPO}: no code changes since ${CCM_LAST_TAG}."
  fi

  # 3. Release gardener-extension-provider-gdc last (if changed directly or transitively via MCM/CCM)
  if [[ "${EXT_NEEDS_RELEASE}" == "true" ]]; then
    # First: Clone extension-provider at exact tested commit SHA, apply imagevector/images.yaml,
    # compile & build Docker images, and push them directly to Gardener Release Registry
    build_and_publish_extension_release_images \
      "${EXT_REPO}" \
      "${EXT_COMMIT_SHA}" \
      "${EXTENSION_GDC_ARTIFACTS_VERSION}"

    EXT_NOTES_ARGS=(
      --helm-chart "admission-gdch-application-helm=${PUBLIC_REGISTRY}/charts/admission-gdch-application-helm:${EXT_CHART_VERSION}"
      --helm-chart "admission-gdch-runtime-helm=${PUBLIC_REGISTRY}/charts/admission-gdch-runtime-helm:${EXT_CHART_VERSION}"
      --helm-chart "gardener-extension-provider-gdch-helm=${PUBLIC_REGISTRY}/charts/gardener-extension-provider-gdch-helm:${EXT_CHART_VERSION}"
      --oci-image "gardener-extension-admission-gdch=${PUBLIC_REGISTRY}/gardener-extension-admission-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
      --oci-image "gardener-extension-provider-gdch=${PUBLIC_REGISTRY}/gardener-extension-provider-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}"
      --oci-image "machine-controller-manager-provider-gdch=${PUBLIC_REGISTRY}/machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}"
      --oci-image "cloud-controller-manager-gdch=${PUBLIC_REGISTRY}/cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}"
      --oci-image "external-dnsman2-gdch=${PUBLIC_REGISTRY}/external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}"
    )
    if [[ -f "${MCM_SUBCOMPONENT_NOTES}" ]]; then
      EXT_NOTES_ARGS+=(--subcomponent-notes "${MCM_SUBCOMPONENT_NOTES}")
    fi
    if [[ -f "${CCM_SUBCOMPONENT_NOTES}" ]]; then
      EXT_NOTES_ARGS+=(--subcomponent-notes "${CCM_SUBCOMPONENT_NOTES}")
    fi

    # Final step: Update main branch directly with the desired change in imagevector/images.yaml,
    # commit & tag release, create GitHub Release with cascaded release notes, and bump VERSION to next -dev
    tag_and_bump_gdc_repository \
      "${EXT_REPO}" \
      "${EXT_REF}" \
      "${EXTENSION_GDC_ARTIFACTS_VERSION}" \
      "${NEXT_VERSION}" \
      "${EXT_LAST_TAG}" \
      "" \
      "${EXT_RELEASE_NOTES}" \
      "${EXT_NOTES_ARGS[@]}"
  else
    echo "Skipping Git tag, image build, and VERSION bump for ${EXT_REPO}: no code changes since ${EXT_LAST_TAG}."
  fi

  echo "Skipping Git tag and VERSION update for ${EXTERNAL_DNS_REPO}: upstream repository is read-only."
fi

jq -n \
  --arg releaseMode "${RELEASE_MODE}" \
  --arg releaseVersion "${GARDENER_ARTIFACTS_VERSION}" \
  --arg certifiedAt "$(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
  --arg workflowRunId "${GITHUB_RUN_ID:-local}" \
  --arg publicRegistry "${PUBLIC_REGISTRY}" \
  --arg extRepo "${EXT_REPO}" \
  --arg extRef "${EXT_REF}" \
  --arg extCommitSHA "${EXT_COMMIT_SHA}" \
  --arg extVersion "${EXTENSION_GDC_ARTIFACTS_VERSION}" \
  --arg mcmRepo "${MCM_REPO}" \
  --arg mcmRef "${MCM_REF}" \
  --arg mcmCommitSHA "${MCM_COMMIT_SHA}" \
  --arg mcmVersion "${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  --arg dnsRepo "${DNS_REPO}" \
  --arg dnsRef "${DNS_REF}" \
  --arg dnsCommitSHA "${DNS_COMMIT_SHA}" \
  --arg dnsVersion "${EXTERNAL_DNS_ARTIFACTS_VERSION}" \
  --arg ccmRepo "${CCM_REPO}" \
  --arg ccmRef "${CCM_REF}" \
  --arg ccmCommitSHA "${CCM_COMMIT_SHA}" \
  --arg ccmVersion "${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  --arg extImage "${PUBLIC_REGISTRY}/gardener-extension-provider-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}" \
  --arg admissionImage "${PUBLIC_REGISTRY}/gardener-extension-admission-gdch:${EXTENSION_GDC_ARTIFACTS_VERSION}" \
  --arg mcmImage "${PUBLIC_REGISTRY}/machine-controller-manager-provider-gdch:${MCM_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  --arg ccmImage "${PUBLIC_REGISTRY}/cloud-controller-manager-gdch:${CLOUD_PROVIDER_GDC_ARTIFACTS_VERSION}" \
  --arg dnsImage "${PUBLIC_REGISTRY}/external-dnsman2-gdch:${EXTERNAL_DNS_ARTIFACTS_VERSION}" \
  --arg providerChart "oci://${PUBLIC_REGISTRY}/charts/gardener-extension-provider-gdch-helm:${EXT_CHART_VERSION}" \
  --arg admissionApplicationChart "oci://${PUBLIC_REGISTRY}/charts/admission-gdch-application-helm:${EXT_CHART_VERSION}" \
  --arg admissionRuntimeChart "oci://${PUBLIC_REGISTRY}/charts/admission-gdch-runtime-helm:${EXT_CHART_VERSION}" \
  --argjson images "${PUBLISHED_IMAGES_JSON}" \
  --argjson charts "${PUBLISHED_CHARTS_JSON}" \
  '{
    releaseMode: $releaseMode,
    releaseVersion: $releaseVersion,
    certifiedAt: $certifiedAt,
    workflowRunId: $workflowRunId,
    publicRegistry: $publicRegistry,
    promotedImages: $images,
    promotedCharts: $charts,
    sources: [
      {name: "gardener-extension-provider-gdc", repository: $extRepo, ref: $extRef, commitSHA: $extCommitSHA, version: $extVersion},
      {name: "machine-controller-manager-provider-gdc", repository: $mcmRepo, ref: $mcmRef, commitSHA: $mcmCommitSHA, version: $mcmVersion},
      {name: "external-dns-management", repository: $dnsRepo, ref: $dnsRef, commitSHA: $dnsCommitSHA, version: $dnsVersion},
      {name: "cloud-provider-gdc", repository: $ccmRepo, ref: $ccmRef, commitSHA: $ccmCommitSHA, version: $ccmVersion}
    ],
    images: [
      {component: "gardener-extension-provider-gdch", image: $extImage, version: $extVersion},
      {component: "gardener-extension-admission-gdch", image: $admissionImage, version: $extVersion},
      {component: "machine-controller-manager-provider-gdch", image: $mcmImage, version: $mcmVersion},
      {component: "cloud-controller-manager-gdch", image: $ccmImage, version: $ccmVersion},
      {component: "external-dnsman2-gdch", image: $dnsImage, version: $dnsVersion}
    ],
    charts: [$providerChart, $admissionApplicationChart, $admissionRuntimeChart]
  }' > "${CERTIFIED_RELEASE_MANIFEST}"

echo "Certified artifacts promoted to ${PUBLIC_REGISTRY}. Manifest written to ${CERTIFIED_RELEASE_MANIFEST}:"
cat "${CERTIFIED_RELEASE_MANIFEST}"
