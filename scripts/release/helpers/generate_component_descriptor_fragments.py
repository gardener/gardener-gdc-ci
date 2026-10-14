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
DEPENDENCY_SOURCE_IMAGES = {
    "machine-controller-manager-provider-gdc": "machine-controller-manager-provider-gdch",
    "cloud-provider-gdc": "cloud-controller-manager-gdch",
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


def imagevector_resources(manifest: Dict[str, Any], vector: Dict[str, Any]):
    certified_images = {
        image["component"]: image for image in manifest["images"]
        if image["component"] in DEPENDENCY_IMAGE_NAMES
    }
    source_images = {
        source["repository"].removeprefix("https://").removesuffix(".git"): certified_images[name]
        for source in manifest["sources"]
        if (name := DEPENDENCY_SOURCE_IMAGES.get(source["name"])) in certified_images
    }
    matched_dependencies = set()
    for entry in vector["images"]:
        certified = source_images.get(entry.get("sourceRepository"))
        if certified is None:
            certified = certified_images.get(entry["repository"].rsplit("/", 1)[-1])
        # Only export the publicly accessible dependencies certified by this pipeline.
        # CSI images currently point to inaccessible registries and must stay out of OCM.
        if certified is None:
            continue
        resource = image_resource(certified)
        matched_dependencies.add(certified["component"])
        labels = {label["name"]: label for label in entry.get("labels", [])}
        purposes = labels.get("gardener.cloud/purposes", CERTIFICATION_LABEL)
        labels["gardener.cloud/purposes"] = {
            **purposes,
            "value": list(dict.fromkeys(purposes["value"] + CERTIFICATION_LABEL["value"])),
        }
        for suffix, value in (
            ("name", entry["name"]),
            ("repository", resource["access"]["imageReference"].rsplit(":", 1)[0]),
            ("source-repository", entry.get("sourceRepository")),
            ("target-version", entry.get("targetVersion")),
        ):
            if value is not None:
                key = f"imagevector.gardener.cloud/{suffix}"
                labels[key] = {"name": key, "value": value}
        resource["labels"] = list(labels.values())
        resource["extraIdentity"].update(entry.get("extraIdentity", {}))
        if "targetVersion" in entry:
            resource["extraIdentity"]["targetversion"] = entry["targetVersion"]
        yield resource
    missing = certified_images.keys() - matched_dependencies
    if missing:
        raise SystemExit(f"extension imagevector is missing certified dependencies: {sorted(missing)}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--resources", type=Path, required=True)
    parser.add_argument("--sources", type=Path, required=True)
    parser.add_argument("--imagevector", type=Path, required=True,
                        help="extension imagevector converted to JSON")
    args = parser.parse_args()

    manifest = json.loads(args.manifest.read_text(encoding="utf-8"))
    resources = [
        image_resource(image)
        for image in manifest.get("images", [])
        if image["component"] in LOCAL_IMAGE_NAMES
    ]
    resources.extend(imagevector_resources(
        manifest, json.loads(args.imagevector.read_text(encoding="utf-8")),
    ))
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
