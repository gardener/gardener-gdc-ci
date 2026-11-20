// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gdcloud

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const systemCABundlePath = "/etc/ssl/certs/ca-certificates.crt"

var userHomeDir = os.UserHomeDir

var gdcloudLocation = func() string {
	if loc := os.Getenv("GDCLOUD_PATH"); loc != "" {
		if info, err := os.Stat(loc); err == nil && info.IsDir() {
			// GDCLOUD_PATH names the gdcloud SDK root. Always resolve to its binary so that a wrong directory
			// fails with an error naming the missing <root>/bin/gdcloud instead of an exec error on the directory.
			return filepath.Join(loc, "bin", "gdcloud")
		}
		return loc
	}
	return "gdcloud"
}

// TestingClient provides a hermetic wrapper around the gdcloud CLI.
// It manages an isolated execution environment for safe concurrent testing.
type TestingClient struct {
	configDir       string
	caDataPath      string
	prevSSLCertFile string
	hadSSLCertFile  bool
	setSSLCertFile  bool
}

// ConfigDir returns the path to the hermetic configuration directory managed by this client.
func (c *TestingClient) ConfigDir() string {
	return c.configDir
}

// NewTestingClient initializes a new hermetic gdcloud CLI client.
// It creates a temporary directory for gdcloud configuration and configures it with
// the provided CA data path, service account key file path, and console URL.
func NewTestingClient(caDataPath, serviceaccountPath string, consoleURL string) (client *TestingClient, err error) {
	// Create a temporary HOME directory to ensure gdcloud does not mutate the user's ~/.config/gdcloud
	configDir, err := os.MkdirTemp("", "gdcloud-test-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory for gdcloud config: %w", err)
	}

	prevSSLCertFile, hadSSLCertFile := os.LookupEnv("SSL_CERT_FILE")
	setSSLCertFile := false

	defer func() {
		if err != nil {
			if setSSLCertFile {
				restoreSSLCertFile(prevSSLCertFile, hadSSLCertFile)
			}
			os.RemoveAll(configDir)
		}
	}()

	caBundlePath := buildCABundle(configDir, caDataPath)
	if caBundlePath != "" {
		_ = os.Setenv("SSL_CERT_FILE", caBundlePath)
		setSSLCertFile = true
	}

	c := &TestingClient{
		configDir:       configDir,
		caDataPath:      caBundlePath,
		prevSSLCertFile: prevSSLCertFile,
		hadSSLCertFile:  hadSSLCertFile,
		setSSLCertFile:  setSSLCertFile,
	}

	if result, err := c.Exec("config", "set", "core/disable_prompts", "true"); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("config", "set", "auth/login_config_cert_path", caDataPath); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("config", "set", "core/organization_console_url", consoleURL); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("auth", "activate-service-account", "--key-file="+serviceaccountPath); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	return c, nil
}

// NewTestingClientFromLocalSession initializes a hermetic gdcloud CLI client cloned from the
// developer's existing local gdcloud session (~/.config/gdcloud and ~/.ais) so local CLI workflows
// (such as teardown and status) work without service account keys or mutating global gdcloud state.
func NewTestingClientFromLocalSession(caDataPath, consoleURL string) (client *TestingClient, err error) {
	homeDir, err := userHomeDir()
	if err != nil || homeDir == "" {
		return nil, fmt.Errorf("failed to resolve user home directory for local gdcloud session: %w", err)
	}
	xdgConfigHome := os.Getenv("XDG_CONFIG_HOME")
	if xdgConfigHome == "" {
		xdgConfigHome = filepath.Join(homeDir, ".config")
	}
	localGDCloudDir := filepath.Join(xdgConfigHome, "gdcloud")
	if info, statErr := os.Stat(localGDCloudDir); statErr != nil || !info.IsDir() {
		return nil, fmt.Errorf("no local gdcloud session found at %q (run 'gdcloud auth login' or set GDC_RELEASE_SERVICE_ACCOUNT_KEY / GDC_SERVICE_ACCOUNT_FILE)", localGDCloudDir)
	}

	configDir, err := os.MkdirTemp("", "gdcloud-test-*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp directory for gdcloud config: %w", err)
	}

	prevSSLCertFile, hadSSLCertFile := os.LookupEnv("SSL_CERT_FILE")
	setSSLCertFile := false

	defer func() {
		if err != nil {
			if setSSLCertFile {
				restoreSSLCertFile(prevSSLCertFile, hadSSLCertFile)
			}
			os.RemoveAll(configDir)
		}
	}()

	if err := copyDirRecursive(localGDCloudDir, filepath.Join(configDir, "gdcloud")); err != nil {
		return nil, fmt.Errorf("failed to clone local gdcloud config directory %q: %w", localGDCloudDir, err)
	}
	localAISDir := filepath.Join(homeDir, ".ais")
	if info, statErr := os.Stat(localAISDir); statErr == nil && info.IsDir() {
		if err := copyDirRecursive(localAISDir, filepath.Join(configDir, ".ais")); err != nil {
			return nil, fmt.Errorf("failed to clone local gdcloud token store %q: %w", localAISDir, err)
		}
	}

	caBundlePath := buildCABundle(configDir, caDataPath)
	if caBundlePath != "" {
		_ = os.Setenv("SSL_CERT_FILE", caBundlePath)
		setSSLCertFile = true
	}

	c := &TestingClient{
		configDir:       configDir,
		caDataPath:      caBundlePath,
		prevSSLCertFile: prevSSLCertFile,
		hadSSLCertFile:  hadSSLCertFile,
		setSSLCertFile:  setSSLCertFile,
	}

	if result, err := c.Exec("config", "set", "core/disable_prompts", "true"); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("config", "set", "auth/login_config_cert_path", caDataPath); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	if result, err := c.Exec("config", "set", "core/organization_console_url", consoleURL); err != nil {
		return nil, fmt.Errorf("%s, cannot execute gdcloud command: %w", result, err)
	}
	return c, nil
}

func copyDirRecursive(srcDir, dstDir string) error {
	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dstDir, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		switch {
		case mode.IsDir():
			return os.MkdirAll(targetPath, mode.Perm()|0700)
		case mode&os.ModeSymlink != 0:
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(linkTarget, targetPath)
		case mode.IsRegular():
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				_ = out.Close()
				return err
			}
			return out.Close()
		default:
			return nil
		}
	})
}

// buildCABundle combines the system CA bundle (if present) with the GDC CA certificate at caDataPath
// so that SSL_CERT_FILE trusts both standard public CAs and the GDC environment's private Root CA
// (e.g., when gdcloud auto-downloads gdcloud-iam-plugin from the GDC Harbor registry).
func buildCABundle(configDir, caDataPath string) string {
	if caDataPath == "" {
		return ""
	}
	gdcCA, err := os.ReadFile(caDataPath)
	if err != nil {
		return caDataPath
	}

	var combined []byte
	if sysCA, err := os.ReadFile(systemCABundlePath); err == nil && len(sysCA) > 0 {
		combined = append(combined, sysCA...)
		if combined[len(combined)-1] != '\n' {
			combined = append(combined, '\n')
		}
	}
	combined = append(combined, gdcCA...)

	bundlePath := filepath.Join(configDir, "ca-bundle.crt")
	if err := os.WriteFile(bundlePath, combined, 0600); err != nil {
		return caDataPath
	}
	return bundlePath
}

// Cleanup restores SSL_CERT_FILE to its previous state and removes the temporary
// configuration directory used by the testing client.
func (c *TestingClient) Cleanup() error {
	if c.setSSLCertFile {
		restoreSSLCertFile(c.prevSSLCertFile, c.hadSSLCertFile)
		c.setSSLCertFile = false
	}
	return os.RemoveAll(c.configDir)
}

func restoreSSLCertFile(prev string, hadPrev bool) {
	if hadPrev {
		_ = os.Setenv("SSL_CERT_FILE", prev)
	} else {
		_ = os.Unsetenv("SSL_CERT_FILE")
	}
}

// resolvePluginsDir returns a writable directory path for GDCLOUD_PLUGINS_BASE_DIR
// when GDCLOUD_PLUGINS_BASE_DIR is not already set in the environment and the default
// gdcloud SDK plugins directory is not writable by the current user (e.g., when
// gdcloud is installed under /usr/local/bin by root).
func (c *TestingClient) resolvePluginsDir(executable string) string {
	if envDir := os.Getenv("GDCLOUD_PLUGINS_BASE_DIR"); envDir != "" {
		_ = os.MkdirAll(envDir, 0755)
		return ""
	}
	if hasWritableDefaultPluginsDir(executable) {
		return ""
	}
	if c.configDir == "" {
		return ""
	}
	if info, err := os.Stat(c.configDir); err != nil || !info.IsDir() {
		return ""
	}
	pluginsDir := filepath.Join(c.configDir, "plugins")
	if err := os.MkdirAll(pluginsDir, 0755); err != nil {
		return ""
	}
	return pluginsDir
}

// hasWritableDefaultPluginsDir reports whether the default gdcloud SDK plugins
// directory (<binDir>/plugins or <gdcloudRoot>/plugins) is writable by the current user.
func hasWritableDefaultPluginsDir(executable string) bool {
	realExec, err := filepath.EvalSymlinks(executable)
	if err != nil {
		realExec = executable
	}
	binDir := filepath.Dir(realExec)
	siblingPlugins := filepath.Join(binDir, "plugins")
	if info, err := os.Stat(siblingPlugins); err == nil && info.IsDir() {
		return isExistingDirWritable(siblingPlugins)
	}
	if filepath.Base(binDir) != "bin" {
		return false
	}
	gdcloudRoot := filepath.Dir(binDir)
	parentPlugins := filepath.Join(gdcloudRoot, "plugins")
	if info, err := os.Stat(parentPlugins); err == nil && info.IsDir() {
		return isExistingDirWritable(parentPlugins)
	}
	return isExistingDirWritable(gdcloudRoot)
}

func isExistingDirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".gdcloud-write-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// Exec executes a gdcloud CLI command using the testing client's hermetic configuration.
// It locates the gdcloud binary and runs the command with an isolated HOME directory to prevent global side effects.
func (c *TestingClient) Exec(args ...string) ([]byte, error) {
	loc := gdcloudLocation()
	if loc == "" {
		return nil, fmt.Errorf("cannot locate gdcloud binary")
	}
	executable, err := exec.LookPath(loc)
	if err != nil {
		return nil, fmt.Errorf("cannot locate gdcloud binary: %w", err)
	}

	pluginsDir := c.resolvePluginsDir(executable)

	maxAttempts := 1
	if len(args) > 0 && (args[0] == "auth" || args[0] == "clusters") {
		maxAttempts = 6
	}

	var result []byte
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cmd := exec.Command(executable, args...)
		// Override HOME and XDG_CONFIG_HOME to guarantee hermetic testing.
		// This forces gdcloud to write its configuration to our isolated test directory
		// instead of mutating the global ~/.config/gdcloud state. We override both
		// because some processes/libraries rigidly default to HOME while others
		// adhere strictly to XDG_CONFIG_HOME. exec.Cmd automatically uses the
		// last occurrence of duplicate environment keys.
		cmd.Env = append(os.Environ(),
			"HOME="+c.configDir,
			"XDG_CONFIG_HOME="+c.configDir,
		)
		if c.caDataPath != "" {
			cmd.Env = append(cmd.Env, "SSL_CERT_FILE="+c.caDataPath)
		}
		if pluginsDir != "" {
			cmd.Env = append(cmd.Env, "GDCLOUD_PLUGINS_BASE_DIR="+pluginsDir)
		}

		result, err = cmd.CombinedOutput()
		if err == nil {
			return result, nil
		}
		if strings.Contains(string(result), "unknown flag") || attempt == maxAttempts {
			break
		}
		time.Sleep(5 * time.Second)
	}
	return result, fmt.Errorf("cannot execute gdcloud command: %w", err)
}
