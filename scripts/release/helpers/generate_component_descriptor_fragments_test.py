#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

import json
import os
import runpy
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("generate_component_descriptor_fragments.py")


class GenerateComponentDescriptorFragmentsTest(unittest.TestCase):
    def test_imagevector_preserves_distinct_target_version_mappings(self):
        generate = runpy.run_path(str(SCRIPT))["imagevector_resources"]
        manifest = {
            "images": [{"component": "cloud-controller-manager-gdch",
                        "version": "v1.2.3", "image": "certified.example/ccm:v1.2.3"}],
            "sources": [{"name": "cloud-provider-gdc",
                         "repository": "https://github.com/GoogleCloudPlatform/cloud-provider-gdc.git"}],
        }
        resources = list(generate(manifest, {"images": [
            {"name": "cloud-controller-manager", "repository": "upstream.example/ccm",
             "sourceRepository": "github.com/GoogleCloudPlatform/cloud-provider-gdc",
             "tag": "v0.1.0", "targetVersion": target}
            for target in ("1.34.x", ">= 1.35")
        ]}))
        self.assertEqual(len(resources), 2)
        self.assertNotEqual(resources[0]["extraIdentity"], resources[1]["extraIdentity"])
        for resource in resources:
            self.assertEqual(resource["access"]["imageReference"], "certified.example/ccm:v1.2.3")
            self.assertIn({"name": "imagevector.gardener.cloud/source-repository",
                           "value": "github.com/GoogleCloudPlatform/cloud-provider-gdc"}, resource["labels"])

    def test_scan_failures_and_findings_abort_release(self):
        publish_script = SCRIPT.parent.parent / "8-publish_release_artifacts.sh"
        scan_function = publish_script.read_text().split("run_gosec_sast_scan() {", 1)[1].split(
            "\nbuild_and_publish_extension_release_images()", 1
        )[0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "hack").mkdir()
            for failure in ("download", "list", "install", "sast_exit", "unsuppressed_sarif"):
                with self.subTest(failure=failure):
                    report_file = root / "report.sarif"
                    report_file.unlink(missing_ok=True)
                    if failure == "sast_exit":
                        (root / "hack" / "sast.sh").write_text("exit 1\n")
                    elif failure == "unsuppressed_sarif":
                        (root / "hack" / "sast.sh").write_text(
                            'cat << \'EOF\' > "$4/$6"\n'
                            '{"runs":[{"results":[{"ruleId":"G304","message":{"text":"unsuppressed"}}]}]}\n'
                            'EOF\n'
                        )
                    else:
                        (root / "hack" / "sast.sh").write_text("exit 0\n")

                    result = subprocess.run([
                        "bash", "-c", """
set -euo pipefail
go() {
  case "$1 $2" in
    'mod download') [[ "$FAILURE" != download ]] ;;
    'list -m') [[ "$FAILURE" != list ]] && printf '%s\\n' "$SCAN_DIR" ;;
    'install github.com/securego/gosec/v2/cmd/gosec@v2.29.0') [[ "$FAILURE" != install ]] ;;
    *) return 99 ;;
  esac
}
run_gosec_sast_scan() {""" + scan_function + """
run_gosec_sast_scan "$SCAN_DIR" "$SCAN_DIR/report.sarif"
printf 'release-continues\\n'
""",
                    ], env={**os.environ, "FAILURE": failure, "SCAN_DIR": str(root),
                            "RELEASE_WORK_DIR": str(root)}, capture_output=True, text=True, check=False)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertNotIn("release-continues", result.stdout)
                    self.assertIn("::error::", result.stderr)

    def test_scan_passes_when_only_suppressed_findings_exist(self):
        publish_script = SCRIPT.parent.parent / "8-publish_release_artifacts.sh"
        scan_function = publish_script.read_text().split("run_gosec_sast_scan() {", 1)[1].split(
            "\nbuild_and_publish_extension_release_images()", 1
        )[0]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "hack").mkdir()
            (root / "hack" / "sast.sh").write_text(
                'cat << \'EOF\' > "$4/$6"\n'
                '{"runs":[{"results":[{"ruleId":"G115","suppressions":[{"kind":"inSource"}]}]}]}\n'
                'EOF\n'
            )
            result = subprocess.run([
                "bash", "-c", """
set -euo pipefail
go() {
  case "$1 $2" in
    'mod download') return 0 ;;
    'list -m') printf '%s\\n' "$SCAN_DIR" ;;
    'install github.com/securego/gosec/v2/cmd/gosec@v2.29.0') return 0 ;;
    *) return 99 ;;
  esac
}
run_gosec_sast_scan() {""" + scan_function + """
run_gosec_sast_scan "$SCAN_DIR" "$SCAN_DIR/report.sarif"
printf 'release-continues\\n'
""",
            ], env={**os.environ, "SCAN_DIR": str(root),
                    "RELEASE_WORK_DIR": str(root)}, capture_output=True, text=True, check=True)
            self.assertIn("release-continues", result.stdout)
            self.assertIn("gosec SAST scan passed with 0 unsuppressed findings.", result.stdout)

    def test_generates_local_and_external_image_and_chart_fragments(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            manifest = root / "manifest.json"
            resources = root / "resources.json"
            sources = root / "sources.json"
            vector = root / "imagevector.json"
            vector.write_text(json.dumps({"images": [
                {
                    "name": "machine-controller-manager-provider-gdch",
                    "repository": "old.example/machine-controller-manager-provider-gdch",
                    "tag": "v0.1.0",
                },
                {
                    "name": "cloud-controller-manager",
                    "repository": "old.example/cloud-controller-manager-gdch",
                    "tag": "v0.1.0",
                    "sourceRepository": "github.com/gardener/cloud-provider-gdc",
                    "targetVersion": ">= 1.34",
                    "labels": [{"name": "gardener.cloud/cve-categorisation",
                                "value": {"network_exposure": "protected"}, "signing": False}],
                },
                {
                    "name": "csi-driver",
                    "repository": "private.example/csi-driver",
                    "tag": "v2.0.0",
                    "sourceRepository": "example.com/csi-driver",
                },
            ]}))
            manifest.write_text(
                json.dumps(
                    {
                        "sources": [
                            {
                                "name": name,
                                "repository": f"https://github.com/gardener/{name}.git",
                                "ref": "main",
                                "commitSHA": "abc1234",
                                "version": "v1.2.3",
                            }
                            for name in (
                                "gardener-extension-provider-gdc",
                                "machine-controller-manager-provider-gdc",
                                "cloud-provider-gdc",
                                "external-dns-management",
                            )
                        ],
                        "images": [
                            {
                                "component": "gardener-extension-provider-gdch",
                                "version": "v1.2.3",
                                "image": "registry.example/provider:v1.2.3",
                            },
                            {
                                "component": "machine-controller-manager-provider-gdch",
                                "version": "v1.2.3",
                                "image": "registry.example/image:v1.2.3",
                            },
                            {
                                "component": "cloud-controller-manager-gdch",
                                "version": "v1.2.3",
                                "image": "registry.example/ccm:v1.2.3",
                            },
                            {
                                "component": "external-dnsman2-gdch",
                                "version": "v0.53.0",
                                "image": "registry.example/external-dns:v0.53.0",
                            },
                        ],
                        "charts": [
                            "oci://registry.example/charts/example-chart:1.2.3"
                        ],
                    }
                ),
                encoding="utf-8",
            )

            subprocess.run(
                [
                    "python3",
                    str(SCRIPT),
                    "--manifest",
                    str(manifest),
                    "--resources",
                    str(resources),
                    "--sources",
                    str(sources),
                    "--imagevector",
                    str(vector),
                ],
                check=True,
            )

            generated_resources = json.loads(resources.read_text(encoding="utf-8"))
            self.assertEqual(len(generated_resources), 4)
            self.assertEqual(generated_resources[0]["relation"], "local")
            self.assertEqual(generated_resources[1]["relation"], "external")
            self.assertEqual(generated_resources[3]["type"], "helmChart")
            self.assertEqual(generated_resources[3]["version"], "1.2.3")
            ccm = generated_resources[2]
            self.assertEqual(ccm["access"]["imageReference"], "registry.example/ccm:v1.2.3")
            self.assertIn({"name": "imagevector.gardener.cloud/target-version", "value": ">= 1.34"}, ccm["labels"])
            self.assertIn({"name": "gardener.cloud/cve-categorisation",
                           "value": {"network_exposure": "protected"}, "signing": False}, ccm["labels"])
            self.assertNotIn("csi-driver", [resource["name"] for resource in generated_resources])
            image_vector_names = {
                resource["name"]: label["value"]
                for resource in generated_resources
                for label in resource["labels"]
                if label["name"] == "imagevector.gardener.cloud/name"
            }
            self.assertEqual(image_vector_names, {
                "machine-controller-manager-provider-gdch": "machine-controller-manager-provider-gdch",
                "cloud-controller-manager-gdch": "cloud-controller-manager",
            })
            self.assertNotIn("external-dnsman2-gdch", [resource["name"] for resource in generated_resources])
            generated_sources = json.loads(sources.read_text(encoding="utf-8"))
            self.assertEqual([source["name"] for source in generated_sources], [
                "main-source",
                "machine-controller-manager-provider-gdc",
                "cloud-provider-gdc",
            ])
            self.assertEqual(generated_sources[0]["access"], {
                "type": "github",
                "repoUrl": "github.com/gardener/gardener-extension-provider-gdc",
                "ref": "main",
                "commit": "abc1234",
            })

    @unittest.skipUnless(shutil.which("yq"), "finalization requires yq")
    def test_finalization_only_skips_main_source_scanning_with_attached_report(self):
        for has_report in (False, True):
            with self.subTest(has_report=has_report), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                descriptor = root / "component-descriptor.yaml"
                sources = root / "sources.json"
                sources.write_text(json.dumps([
                    {"name": "main-source", "labels": []},
                    {"name": "dependency-source", "labels": []},
                ]))
                descriptor.write_text(json.dumps({"component": {
                    "sources": [{"name": "main-source", "labels": [
                        {"name": "example.com/custom", "value": "preserved"},
                        {"name": "cloud.gardener.cnudie/dso/scanning-hints/source_analysis/v1",
                         "value": {"policy": "skip"}},
                    ]}],
                    "resources": [{"name": "gosec-report"}] if has_report else [],
                }}))
                subprocess.run([
                    "bash", str(SCRIPT.with_name("finalize_component_descriptor.sh")),
                    str(descriptor), str(root / "descriptor.tar.gz"),
                ], env={**os.environ, "OCM_SOURCES_FILE": str(sources)}, check=True)
                finalized = json.loads(subprocess.check_output([
                    "yq", "-o=json", ".component.sources", str(descriptor),
                ]))
                self.assertEqual([source["name"] for source in finalized], [
                    "main-source", "dependency-source",
                ])
                self.assertEqual(finalized[1]["labels"], [])
                self.assertIn({"name": "example.com/custom", "value": "preserved"}, finalized[0]["labels"])
                scan_labels = [label for label in finalized[0]["labels"]
                               if label["name"] == "cloud.gardener.cnudie/dso/scanning-hints/source_analysis/v1"]
                if has_report:
                    self.assertEqual(scan_labels[0]["value"]["policy"], "skip")
                else:
                    self.assertEqual(scan_labels, [])


if __name__ == "__main__":
    unittest.main()
