#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for retry_with_backoff in scripts/ci-common.sh."""

import os
import stat
import subprocess
import tempfile
import unittest


REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
CI_COMMON_SH = os.path.join(REPO_ROOT, "scripts", "ci-common.sh")


class CICommonRetryTest(unittest.TestCase):

    def _run_bash(self, script_body: str, extra_env: dict[str, str] | None = None) -> subprocess.CompletedProcess[str]:
        env = os.environ.copy()
        env["RETRY_INITIAL_DELAY_SECONDS"] = "0"
        if extra_env:
            env.update(extra_env)
        full_script = f'set -euo pipefail\nsource "{CI_COMMON_SH}"\n{script_body}\n'
        return subprocess.run(
            ["bash", "-c", full_script],
            capture_output=True,
            text=True,
            env=env,
            check=False,
        )

    def test_succeeds_immediately_on_first_attempt(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            counter_file = os.path.join(tmpdir, "count")
            mock_cmd = os.path.join(tmpdir, "mock_cmd.sh")
            with open(mock_cmd, "w", encoding="utf-8") as f:
                f.write(
                    "#!/usr/bin/env bash\n"
                    f'count=$(cat "{counter_file}" 2>/dev/null || echo 0)\n'
                    f'echo $((count + 1)) > "{counter_file}"\n'
                    "exit 0\n"
                )
            os.chmod(mock_cmd, stat.S_IRWXU)

            res = self._run_bash(f'retry_with_backoff "{mock_cmd}" arg1')
            self.assertEqual(res.returncode, 0, msg=res.stderr)
            with open(counter_file, encoding="utf-8") as f:
                self.assertEqual(int(f.read().strip()), 1)
            self.assertNotIn("::warning::", res.stderr)

    def test_retries_transient_ghcr_tag_not_found_and_succeeds(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            counter_file = os.path.join(tmpdir, "count")
            mock_helm = os.path.join(tmpdir, "mock_helm.sh")
            with open(mock_helm, "w", encoding="utf-8") as f:
                f.write(
                    "#!/usr/bin/env bash\n"
                    f'count=$(cat "{counter_file}" 2>/dev/null || echo 0)\n'
                    "next=$((count + 1))\n"
                    f'echo "${{next}}" > "{counter_file}"\n'
                    "if (( next < 3 )); then\n"
                    '  echo \'Error: failed to perform "Tag" on destination: sha256:9a553cc6: not found\' >&2\n'
                    "  exit 1\n"
                    "fi\n"
                    'echo "Pushed: ghcr.io/gardener/gardener-gdc-ci/private-cloud/gardenlet:1.154.1"\n'
                    "exit 0\n"
                )
            os.chmod(mock_helm, stat.S_IRWXU)

            res = self._run_bash(
                f'retry_with_backoff "{mock_helm}" push gardenlet-1.154.1.tgz oci://ghcr.io/gardener/gardener-gdc-ci/private-cloud',
                extra_env={"RETRY_MAX_ATTEMPTS": "4"},
            )
            self.assertEqual(res.returncode, 0, msg=res.stderr)
            with open(counter_file, encoding="utf-8") as f:
                self.assertEqual(int(f.read().strip()), 3)
            self.assertIn("Attempt 1/4 failed", res.stderr)
            self.assertIn("Attempt 2/4 failed", res.stderr)
            self.assertIn("Pushed: ghcr.io/", res.stdout)

    def test_propagates_exit_code_after_exhausting_max_attempts(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            counter_file = os.path.join(tmpdir, "count")
            mock_fail = os.path.join(tmpdir, "mock_fail.sh")
            with open(mock_fail, "w", encoding="utf-8") as f:
                f.write(
                    "#!/usr/bin/env bash\n"
                    f'count=$(cat "{counter_file}" 2>/dev/null || echo 0)\n'
                    f'echo $((count + 1)) > "{counter_file}"\n'
                    "exit 42\n"
                )
            os.chmod(mock_fail, stat.S_IRWXU)

            res = self._run_bash(
                f'retry_with_backoff "{mock_fail}" push',
                extra_env={"RETRY_MAX_ATTEMPTS": "3"},
            )
            self.assertEqual(res.returncode, 42)
            with open(counter_file, encoding="utf-8") as f:
                self.assertEqual(int(f.read().strip()), 3)
            self.assertIn("::error::Command failed after 3 attempt(s) (exit 42)", res.stderr)

    def test_ci_common_does_not_use_world_writable_plugin_permissions(self):
        with open(CI_COMMON_SH, encoding="utf-8") as f:
            content = f.read()
        self.assertNotIn("chmod 0777", content)
        self.assertNotIn("--no-check-certificate", content)
        self.assertNotIn("(-k)", content)
        self.assertIn('--cacert "${ca_file}"', content)
        self.assertIn("--proto '=https' --proto-redir '=https'", content)

    def test_setup_gdc_credentials_verifies_tls_and_supports_bootstrap_ca(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            bin_dir = os.path.join(tmpdir, "bin")
            os.makedirs(bin_dir)
            args_log = os.path.join(tmpdir, "wget-args.txt")
            ca_copy = os.path.join(tmpdir, "bootstrap-ca-seen.pem")
            mock_wget = os.path.join(bin_dir, "wget")
            with open(mock_wget, "w", encoding="utf-8") as f:
                f.write(
                    "#!/usr/bin/env bash\n"
                    f'printf "%s\\n" "$@" > "{args_log}"\n'
                    'out=""\n'
                    'for arg in "$@"; do\n'
                    '  case "${arg}" in\n'
                    f'    --ca-certificate=*) cp "${{arg#--ca-certificate=}}" "{ca_copy}" ;;\n'
                    '  esac\n'
                    'done\n'
                    'while [[ $# -gt 0 ]]; do\n'
                    '  if [[ "$1" == "-O" ]]; then out="$2"; shift 2; else shift; fi\n'
                    'done\n'
                    'printf "FAKE-CA\\n" > "${out}"\n'
                )
            os.chmod(mock_wget, stat.S_IRWXU)

            sa_file = os.path.join(tmpdir, "sa.json")
            ca_file = os.path.join(tmpdir, "ca.crt")
            res = self._run_bash(
                f'setup_gdc_credentials "{sa_file}" "{ca_file}" "https://console.example.test" \'{{"name":"sa"}}\'',
                extra_env={
                    "PATH": f"{bin_dir}:{os.environ['PATH']}",
                    "GDC_BOOTSTRAP_CA_PEM": "-----BEGIN CERTIFICATE-----\nBOOT\n-----END CERTIFICATE-----",
                },
            )
            self.assertEqual(res.returncode, 0, msg=res.stderr)
            with open(args_log, encoding="utf-8") as f:
                args = f.read()
            self.assertNotIn("--no-check-certificate", args)
            self.assertIn("--ca-certificate=", args)
            with open(ca_copy, encoding="utf-8") as f:
                self.assertIn("BOOT", f.read())

    def test_install_gdcloud_cli_fails_without_ca_bundle(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            sa_file = os.path.join(tmpdir, "sa.json")
            ca_file = os.path.join(tmpdir, "empty-ca.crt")
            with open(sa_file, "w", encoding="utf-8") as f:
                f.write('{"name":"sa"}')
            open(ca_file, "w", encoding="utf-8").close()

            res = self._run_bash(
                f'install_gdcloud_cli "{sa_file}" "{ca_file}" "https://mgmt.example.test"',
            )
            self.assertNotEqual(res.returncode, 0)
            self.assertIn("missing or empty", res.stderr)


if __name__ == "__main__":
    unittest.main()
