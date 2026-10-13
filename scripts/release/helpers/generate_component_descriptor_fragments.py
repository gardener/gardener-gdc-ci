#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Create OCM source/resource fragments for a certified GDC release."""

import argparse
import json
from pathlib import Path
from typing import Any, Dict


CERTIFICATION_LABEL = {
    "name": "gardener.cloud/purposes",
    "value": ["release", "e2e-certification"],
}

LOCAL_IMAGE_NAMES = {
    "gardener-extension-provider-gdch",
    "gardener-extension-admission-gdch",
}
DEPENDENCY_IMAGE_NAMES = {
    "machine-controller-manager-provider-gdch",
    "cloud-controller-manager-gdch",
}
COMPONENT_IMAGE_NAMES = LOCAL_IMAGE_NAMES | DEPENDENCY_IMAGE_NAMES
IMAGE_VECTOR_NAMES = {
    "machine-controller-manager-provider-gdch": "machine-controller-manager-provider-gdch",
    "cloud-controller-manager-gdch": "cloud-controller-manager",
}
COMPONENT_SOURCE_NAMES = {
    "gardener-extension-provider-gdc",
    "machine-controller-manager-provider-gdc",
    "cloud-provider-gdc",
}


def source_fragment(source: Dict[str, str]) -> Dict[str, Any]:
    is_main = source["name"] == "gardener-extension-provider-gdc"
    return {
        "name": "main-source" if is_main else source["name"],
        "version": source["version"],
        "type": "git",
        "access": {
            "type": "github",
            "repoUrl": source["repository"].removeprefix("https://").removesuffix(".git"),
            "ref": source["ref"],
            "commit": source["commitSHA"],
        },
        "labels": [{
            "name": "cloud.gardener/cicd/source",
            "value": {"repository-classification": "main"},
        }] if is_main else [],
    }


def resource_labels() -> list[Dict[str, Any]]:
    return [CERTIFICATION_LABEL]


def image_resource(image: Dict[str, str]) -> Dict[str, Any]:
    name = image["component"]
    version = image["version"]
    labels = resource_labels()
    if name in IMAGE_VECTOR_NAMES:
        labels.append({
            "name": "imagevector.gardener.cloud/name",
            "value": IMAGE_VECTOR_NAMES[name],
        })
    return {
        "name": name,
        "version": version,
        "type": "ociImage",
        "extraIdentity": {"version": version},
        "relation": "local" if name in LOCAL_IMAGE_NAMES else "external",
        "access": {
            "type": "ociImage",
            "imageReference": image["image"],
        },
        "labels": labels,
    }


def chart_resource(chart_ref: str) -> Dict[str, Any]:
    ref = chart_ref.removeprefix("oci://")
    name_and_version = ref.rsplit("/", 1)[-1]
    name, version = name_and_version.rsplit(":", 1)
    return {
        "name": name,
        "version": version,
        "type": "helmChart",
        "extraIdentity": {"type": "helmChart"},
        "relation": "local",
        "access": {
            "type": "ociRegistry",
            "imageReference": ref,
        },
        "labels": resource_labels(),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--resources", type=Path, required=True)
    parser.add_argument("--sources", type=Path, required=True)
    args = parser.parse_args()

    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    resources = [
        image_resource(image)
        for image in manifest.get("images", [])
        if image["component"] in COMPONENT_IMAGE_NAMES
    ]
    resources.extend(chart_resource(chart) for chart in manifest.get("charts", []))
    if not resources:
        raise SystemExit("certified release manifest contains no resources")

    args.resources.write_text(json.dumps(resources, indent=2) + "\n", encoding="utf-8")
    sources = [
        source_fragment(source)
        for source in manifest.get("sources", [])
        if source["name"] in COMPONENT_SOURCE_NAMES
    ]
    if not any(source["name"] == "main-source" for source in sources):
        raise SystemExit("certified release manifest contains no main source")
    args.sources.write_text(json.dumps(sources, indent=2) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
