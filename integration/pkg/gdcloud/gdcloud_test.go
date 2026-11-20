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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExec(t *testing.T) {
	tests := []struct {
		name                  string
		args                  []string
		mockedGdcloudLocation func() string
		wantOutput            []byte
		wantErrMsg            string
	}{
		{
			name:                  "Successful command execution",
			args:                  []string{"gdcloud version: 1.14.3-gdch.9425-21"},
			mockedGdcloudLocation: func() string { return "echo" },
			wantOutput:            []byte("gdcloud version: 1.14.3-gdch.9425-21\n"),
			wantErrMsg:            "",
		},
		{
			name:                  "Command returns an error",
			args:                  []string{"-c", "exit 1"},
			mockedGdcloudLocation: func() string { return "sh" },
			wantOutput:            nil,
			wantErrMsg:            "exit status 1",
		},
		{
			name:                  "Binary not found",
			args:                  []string{"invalid-arg"},
			mockedGdcloudLocation: func() string { return "nonexistent-gdcloud-binary-xyz" },
			wantOutput:            nil,
			wantErrMsg:            "cannot locate gdcloud binary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			originalLocationFunc := gdcloudLocation
			gdcloudLocation = tt.mockedGdcloudLocation
			defer func() { gdcloudLocation = originalLocationFunc }()

			c := &TestingClient{configDir: "/tmp/fake"}
			gotOutput, gotErr := c.Exec(tt.args...)

			if (gotErr != nil) && !strings.Contains(gotErr.Error(), tt.wantErrMsg) {
				t.Errorf("Exec() error = %v, wantErr %v", gotErr, tt.wantErrMsg)
				return
			}
			if !bytes.Equal(gotOutput, tt.wantOutput) {
				t.Errorf("Exec() gotOutput = %q, want %q", gotOutput, tt.wantOutput)
			}
		})
	}
}

func TestNewTestingClient(t *testing.T) {
	t.Setenv("SSL_CERT_FILE", "/existing/ca.pem")
	if err := os.Unsetenv("SSL_CERT_FILE"); err != nil {
		t.Fatalf("failed to unset SSL_CERT_FILE: %v", err)
	}
	t.Setenv("GDCLOUD_PLUGINS_BASE_DIR", "")
	if err := os.Unsetenv("GDCLOUD_PLUGINS_BASE_DIR"); err != nil {
		t.Fatalf("failed to unset GDCLOUD_PLUGINS_BASE_DIR: %v", err)
	}

	tmpDir := t.TempDir()
	caFile := filepath.Join(tmpDir, "gdc-ca.pem")
	wantCAContent := "-----BEGIN CERTIFICATE-----\nFAKE-GDC-CA\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(caFile, []byte(wantCAContent), 0600); err != nil {
		t.Fatalf("failed to write fake CA file: %v", err)
	}

	logFile := filepath.Join(tmpDir, "gdcloud-calls.log")
	mockScript := filepath.Join(tmpDir, "mock-gdcloud.sh")
	scriptContent := "#!/bin/sh\necho \"SSL_CERT_FILE=${SSL_CERT_FILE} GDCLOUD_PLUGINS_BASE_DIR=${GDCLOUD_PLUGINS_BASE_DIR} ARGS=$*\" >> \"" + logFile + "\"\n"
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write mock gdcloud script: %v", err)
	}

	originalLocationFunc := gdcloudLocation
	gdcloudLocation = func() string { return mockScript }
	defer func() { gdcloudLocation = originalLocationFunc }()

	client, err := NewTestingClient(caFile, "/tmp/fake-sa.json", "https://console.example.com")
	if err != nil {
		t.Fatalf("NewTestingClient() unexpected error: %v", err)
	}

	if got := os.Getenv("SSL_CERT_FILE"); got != client.caDataPath {
		t.Errorf("SSL_CERT_FILE = %q, want %q", got, client.caDataPath)
	}

	bundleContent, err := os.ReadFile(client.caDataPath)
	if err != nil {
		t.Fatalf("failed to read generated CA bundle %q: %v", client.caDataPath, err)
	}
	if !strings.Contains(string(bundleContent), wantCAContent) {
		t.Errorf("CA bundle %q missing GDC CA content", client.caDataPath)
	}

	wantPluginsDir := filepath.Join(client.ConfigDir(), "plugins")
	if info, err := os.Stat(wantPluginsDir); err != nil || !info.IsDir() {
		t.Errorf("expected fallback plugins dir %q to exist, stat err: %v", wantPluginsDir, err)
	}

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read gdcloud calls log: %v", err)
	}
	gotLog := string(logBytes)
	prefix := "SSL_CERT_FILE=" + client.caDataPath + " GDCLOUD_PLUGINS_BASE_DIR=" + wantPluginsDir + " ARGS="
	wantCalls := []string{
		prefix + "config set core/disable_prompts true",
		prefix + "config set auth/login_config_cert_path " + caFile,
		prefix + "config set core/organization_console_url https://console.example.com",
		prefix + "auth activate-service-account --key-file=/tmp/fake-sa.json",
	}
	for _, want := range wantCalls {
		if !strings.Contains(gotLog, want) {
			t.Errorf("NewTestingClient() missing expected call %q in log:\n%s", want, gotLog)
		}
	}

	if err := client.Cleanup(); err != nil {
		t.Fatalf("Cleanup() unexpected error: %v", err)
	}
	if val, ok := os.LookupEnv("SSL_CERT_FILE"); ok {
		t.Errorf("expected SSL_CERT_FILE to be unset after Cleanup(), got %q", val)
	}
}

func TestNewTestingClientRestoresSSLCertFile(t *testing.T) {
	const prevVal = "/custom/previous-ca.pem"
	t.Setenv("SSL_CERT_FILE", prevVal)

	tmpDir := t.TempDir()
	caFile := filepath.Join(tmpDir, "gdc-ca.pem")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"), 0600); err != nil {
		t.Fatalf("failed to write fake CA file: %v", err)
	}

	// 1. Verify error path in NewTestingClient restores previous SSL_CERT_FILE.
	originalLocationFunc := gdcloudLocation
	defer func() { gdcloudLocation = originalLocationFunc }()
	gdcloudLocation = func() string { return "nonexistent-gdcloud-binary-xyz" }

	if _, err := NewTestingClient(caFile, "/tmp/fake-sa.json", "https://console.example.com"); err == nil {
		t.Fatalf("expected NewTestingClient() to fail when binary is missing")
	}
	if got := os.Getenv("SSL_CERT_FILE"); got != prevVal {
		t.Errorf("SSL_CERT_FILE after failed NewTestingClient() = %q, want %q", got, prevVal)
	}

	// 2. Verify writable SDK plugins directory does not override GDCLOUD_PLUGINS_BASE_DIR,
	// and Cleanup() restores previous SSL_CERT_FILE.
	t.Setenv("GDCLOUD_PLUGINS_BASE_DIR", "")
	if err := os.Unsetenv("GDCLOUD_PLUGINS_BASE_DIR"); err != nil {
		t.Fatalf("failed to unset GDCLOUD_PLUGINS_BASE_DIR: %v", err)
	}
	sdkBinDir := filepath.Join(tmpDir, "sdk", "bin")
	sdkPluginsDir := filepath.Join(tmpDir, "sdk", "plugins")
	if err := os.MkdirAll(sdkBinDir, 0755); err != nil {
		t.Fatalf("failed to create sdk bin dir: %v", err)
	}
	if err := os.MkdirAll(sdkPluginsDir, 0755); err != nil {
		t.Fatalf("failed to create sdk plugins dir: %v", err)
	}
	logFile := filepath.Join(tmpDir, "sdk-calls.log")
	mockScript := filepath.Join(sdkBinDir, "gdcloud")
	scriptContent := "#!/bin/sh\necho \"GDCLOUD_PLUGINS_BASE_DIR=${GDCLOUD_PLUGINS_BASE_DIR}\" >> \"" + logFile + "\"\n"
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write mock gdcloud script: %v", err)
	}
	gdcloudLocation = func() string { return mockScript }

	client, err := NewTestingClient(caFile, "/tmp/fake-sa.json", "https://console.example.com")
	if err != nil {
		t.Fatalf("NewTestingClient() unexpected error: %v", err)
	}
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read sdk calls log: %v", err)
	}
	if !strings.Contains(string(logBytes), "GDCLOUD_PLUGINS_BASE_DIR=\n") {
		t.Errorf("expected GDCLOUD_PLUGINS_BASE_DIR to remain unset when SDK plugins dir is writable, got log:\n%s", string(logBytes))
	}

	if err := client.Cleanup(); err != nil {
		t.Fatalf("Cleanup() unexpected error: %v", err)
	}
	if got := os.Getenv("SSL_CERT_FILE"); got != prevVal {
		t.Errorf("SSL_CERT_FILE after Cleanup() = %q, want %q", got, prevVal)
	}
}

func TestGdcloudLocation(t *testing.T) {
	sdkRoot := t.TempDir()
	sdkBinary := filepath.Join(sdkRoot, "bin", "gdcloud")
	if err := os.MkdirAll(filepath.Dir(sdkBinary), 0755); err != nil {
		t.Fatalf("Failed to create SDK bin directory: %v", err)
	}
	if err := os.WriteFile(sdkBinary, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatalf("Failed to create SDK binary: %v", err)
	}
	emptyDir := t.TempDir()

	tests := []struct {
		name        string
		gdcloudPath string
		want        string
	}{
		{
			name:        "unset falls back to PATH lookup",
			gdcloudPath: "",
			want:        "gdcloud",
		},
		{
			name:        "binary path is used as is",
			gdcloudPath: sdkBinary,
			want:        sdkBinary,
		},
		{
			name:        "nonexistent path is used as is",
			gdcloudPath: filepath.Join(emptyDir, "missing", "gdcloud"),
			want:        filepath.Join(emptyDir, "missing", "gdcloud"),
		},
		{
			name:        "SDK root directory resolves to its binary",
			gdcloudPath: sdkRoot,
			want:        sdkBinary,
		},
		{
			name:        "directory without the binary still points at the expected binary",
			gdcloudPath: emptyDir,
			want:        filepath.Join(emptyDir, "bin", "gdcloud"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GDCLOUD_PATH", tt.gdcloudPath)
			if got := gdcloudLocation(); got != tt.want {
				t.Errorf("gdcloudLocation() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExec_DirectoryWithoutBinaryNamesMissingBinary(t *testing.T) {
	emptyDir := t.TempDir()
	t.Setenv("GDCLOUD_PATH", emptyDir)

	c := &TestingClient{configDir: t.TempDir()}
	_, err := c.Exec("version")
	if err == nil {
		t.Fatal("Exec() succeeded, want an error for a directory without bin/gdcloud")
	}
	wantBinary := filepath.Join(emptyDir, "bin", "gdcloud")
	if !strings.Contains(err.Error(), "cannot locate gdcloud binary") || !strings.Contains(err.Error(), wantBinary) {
		t.Errorf("Exec() error = %q, want it to name the missing binary %q", err, wantBinary)
	}
}

func TestNewTestingClientFromLocalSession(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	if err := os.Unsetenv("XDG_CONFIG_HOME"); err != nil {
		t.Fatalf("failed to unset XDG_CONFIG_HOME: %v", err)
	}
	t.Setenv("GDCLOUD_PLUGINS_BASE_DIR", "")
	if err := os.Unsetenv("GDCLOUD_PLUGINS_BASE_DIR"); err != nil {
		t.Fatalf("failed to unset GDCLOUD_PLUGINS_BASE_DIR: %v", err)
	}

	fakeHome := t.TempDir()
	origHomeDir := userHomeDir
	userHomeDir = func() (string, error) { return fakeHome, nil }
	defer func() { userHomeDir = origHomeDir }()

	caFile := filepath.Join(t.TempDir(), "gdc-ca.pem")
	if err := os.WriteFile(caFile, []byte("-----BEGIN CERTIFICATE-----\nFAKE\n-----END CERTIFICATE-----\n"), 0600); err != nil {
		t.Fatalf("failed to write fake CA file: %v", err)
	}

	// 1. Missing ~/.config/gdcloud returns an actionable error.
	if _, err := NewTestingClientFromLocalSession(caFile, "https://console.example.com"); err == nil || !strings.Contains(err.Error(), "no local gdcloud session found") {
		t.Fatalf("expected missing local gdcloud session error, got %v", err)
	}

	// 2. Populated ~/.config/gdcloud and ~/.ais are cloned into the hermetic configDir without calling activate-service-account.
	srcGDCloudDir := filepath.Join(fakeHome, ".config", "gdcloud")
	srcAISDir := filepath.Join(fakeHome, ".ais", "gdcloud")
	if err := os.MkdirAll(srcGDCloudDir, 0700); err != nil {
		t.Fatalf("failed to create fake gdcloud dir: %v", err)
	}
	if err := os.MkdirAll(srcAISDir, 0700); err != nil {
		t.Fatalf("failed to create fake ais dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcGDCloudDir, "active_config"), []byte("default"), 0600); err != nil {
		t.Fatalf("failed to write active_config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(srcAISDir, "login_token_store.json"), []byte(`{"token":"test"}`), 0600); err != nil {
		t.Fatalf("failed to write login_token_store.json: %v", err)
	}

	logFile := filepath.Join(t.TempDir(), "gdcloud-local-calls.log")
	mockScript := filepath.Join(t.TempDir(), "mock-gdcloud.sh")
	scriptContent := "#!/bin/sh\necho \"ARGS=$*\" >> \"" + logFile + "\"\n"
	if err := os.WriteFile(mockScript, []byte(scriptContent), 0755); err != nil {
		t.Fatalf("failed to write mock gdcloud script: %v", err)
	}

	origLoc := gdcloudLocation
	gdcloudLocation = func() string { return mockScript }
	defer func() { gdcloudLocation = origLoc }()

	client, err := NewTestingClientFromLocalSession(caFile, "https://console.example.com")
	if err != nil {
		t.Fatalf("NewTestingClientFromLocalSession() unexpected error: %v", err)
	}
	defer client.Cleanup()

	if _, err := os.Stat(filepath.Join(client.ConfigDir(), "gdcloud", "active_config")); err != nil {
		t.Errorf("expected cloned gdcloud/active_config in %q: %v", client.ConfigDir(), err)
	}
	if _, err := os.Stat(filepath.Join(client.ConfigDir(), ".ais", "gdcloud", "login_token_store.json")); err != nil {
		t.Errorf("expected cloned .ais/gdcloud/login_token_store.json in %q: %v", client.ConfigDir(), err)
	}

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read gdcloud calls log: %v", err)
	}
	gotLog := string(logBytes)
	if strings.Contains(gotLog, "activate-service-account") {
		t.Errorf("NewTestingClientFromLocalSession() must not call activate-service-account, got log:\n%s", gotLog)
	}
	for _, want := range []string{
		"ARGS=config set core/disable_prompts true",
		"ARGS=config set auth/login_config_cert_path " + caFile,
		"ARGS=config set core/organization_console_url https://console.example.com",
	} {
		if !strings.Contains(gotLog, want) {
			t.Errorf("NewTestingClientFromLocalSession() missing expected call %q in log:\n%s", want, gotLog)
		}
	}
}
