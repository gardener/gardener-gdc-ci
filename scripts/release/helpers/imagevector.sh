#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

# Updates `imagevector/images.yaml` inside a `gardener-extension-provider-gdc` checkout
# so that `machine-controller-manager-provider-gdch` and `cloud-controller-manager`
# point to the canonical Gardener Release Registry and the certified SemVer tags.
update_extension_imagevector() {
  local images_yaml="$1"
  local mcm_tag="$2"
  local ccm_tag="$3"
  local release_registry="${4:-europe-docker.pkg.dev/gardener-project/releases/gardener/gardener-gdc-ci}"

  if [[ ! -f "${images_yaml}" ]]; then
    echo "::error::imagevector file not found at ${images_yaml}" >&2
    return 1
  fi

  MCM_TAG="${mcm_tag}" \
  CCM_TAG="${ccm_tag}" \
  MCM_REPO="${release_registry}/machine-controller-manager-provider-gdch" \
  CCM_REPO="${release_registry}/cloud-controller-manager-gdch" \
  yq -i '
    (.images[] | select(.name == "cloud-controller-manager")) |= (
      .sourceRepository = "github.com/GoogleCloudPlatform/cloud-provider-gdc" |
      .repository = strenv(CCM_REPO) |
      .tag = strenv(CCM_TAG)
    ) |
    (.images[] | select(.name == "machine-controller-manager-provider-gdch")) |= (
      .sourceRepository = "github.com/gardener/machine-controller-manager-provider-gdc" |
      .repository = strenv(MCM_REPO) |
      .tag = strenv(MCM_TAG)
    )
  ' "${images_yaml}"
}

# Reads the current tag for an image entry in `imagevector/images.yaml`.
read_imagevector_tag() {
  local images_yaml="$1"
  local image_name="$2"
  if [[ ! -f "${images_yaml}" ]]; then
    echo ""
    return 0
  fi
  IMAGE_NAME="${image_name}" yq -r '.images[] | select(.name == strenv(IMAGE_NAME)) | .tag // ""' "${images_yaml}"
}
