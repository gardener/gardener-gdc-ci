#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

"""Unit tests for retry_with_backoff and gdcloud_can_install_plugins in scripts/ci-common.sh."""

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


class CICommonGdcloudPluginsTest(unittest.TestCase):

    def _can_install_plugins(self, gdcloud_bin: str) -> bool:
        full_script = f'set -euo pipefail\nsource "{CI_COMMON_SH}"\ngdcloud_can_install_plugins "{gdcloud_bin}"\n'
        res = subprocess.run(["bash", "-c", full_script], capture_output=True, text=True, check=False)
        self.assertIn(res.returncode, (0, 1), msg=res.stderr)
        return res.returncode == 0

    def _make_binary(self, path: str) -> str:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "w", encoding="utf-8") as f:
            f.write("#!/usr/bin/env bash\n")
        os.chmod(path, stat.S_IRWXU)
        return path

    def test_resolves_symlink_before_deriving_sdk_root(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            sdk_bin = self._make_binary(os.path.join(tmpdir, "sdk", "bin", "gdcloud"))
            os.makedirs(os.path.join(tmpdir, "sdk", "plugins"))
            link = os.path.join(tmpdir, "usr", "local", "bin", "gdcloud")
            os.makedirs(os.path.dirname(link))
            os.symlink(sdk_bin, link)

            self.assertTrue(self._can_install_plugins(link))
            # The directory next to the symlink's parent must be neither consulted nor created.
            self.assertFalse(os.path.exists(os.path.join(tmpdir, "usr", "local", "plugins")))

    def test_rejects_binary_outside_of_bin_directory_without_creating_plugins(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            gdcloud_bin = self._make_binary(os.path.join(tmpdir, "tools", "gdcloud"))

            self.assertFalse(self._can_install_plugins(gdcloud_bin))
            self.assertFalse(os.path.exists(os.path.join(tmpdir, "plugins")))
            self.assertFalse(os.path.exists(os.path.join(tmpdir, "tools", "plugins")))

    def test_accepts_writable_sdk_root_without_creating_plugins(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            gdcloud_bin = self._make_binary(os.path.join(tmpdir, "sdk", "bin", "gdcloud"))

            self.assertTrue(self._can_install_plugins(gdcloud_bin))
            self.assertFalse(os.path.exists(os.path.join(tmpdir, "sdk", "plugins")))

    @unittest.skipIf(os.geteuid() == 0, "root can write to read-only directories")
    def test_rejects_read_only_plugins_directory(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            gdcloud_bin = self._make_binary(os.path.join(tmpdir, "sdk", "bin", "gdcloud"))
            plugins_dir = os.path.join(tmpdir, "sdk", "plugins")
            os.makedirs(plugins_dir)
            os.chmod(plugins_dir, stat.S_IRUSR | stat.S_IXUSR)
            try:
                self.assertFalse(self._can_install_plugins(gdcloud_bin))
            finally:
                os.chmod(plugins_dir, stat.S_IRWXU)


if __name__ == "__main__":
    unittest.main()
