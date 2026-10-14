#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

descriptor_path="${1:?component descriptor path is required}"
archive_path="${2:?descriptor archive path is required}"

# Replace the CI repository source with the certified component sources. Only suppress
# main-source analysis when the descriptor actually contains the replacement SAST report.
yq -i '
  ([.component.sources[] | select(.name == "main-source") | .labels[]?] |
    map(select(.name != "cloud.gardener.cnudie/dso/scanning-hints/source_analysis/v1"))) as $labels |
  .component.sources = load(strenv(OCM_SOURCES_FILE)) |
  (.component.sources[] | select(.name == "main-source")).labels =
    (($labels + (.component.sources[] | select(.name == "main-source") | .labels)) | unique_by(.name))
' "${descriptor_path}"
if [[ "$(yq '.component.resources[] | select(.name == "gosec-report") | .name' "${descriptor_path}")" == "gosec-report" ]]; then
  yq -i '(.component.sources[] | select(.name == "main-source")).labels += [{"name": "cloud.gardener.cnudie/dso/scanning-hints/source_analysis/v1", "value": {"policy": "skip", "comment": "we use gosec for sast scanning. See attached gosec-report resource."}}]' "${descriptor_path}"
fi
tar czf "${archive_path}" -C "$(dirname "${descriptor_path}")" component-descriptor.yaml
