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
	t.Setenv("SSL_CERT_FILE", os.Getenv("SSL_CERT_FILE"))
	tmpDir := t.TempDir()
	caFile := filepath.Join(tmpDir, "gdc-ca.pem")
	wantCAContent := "-----BEGIN CERTIFICATE-----\nFAKE-GDC-CA\n-----END CERTIFICATE-----\n"
	if err := os.WriteFile(caFile, []byte(wantCAContent), 0600); err != nil {
		t.Fatalf("failed to write fake CA file: %v", err)
	}

	logFile := filepath.Join(tmpDir, "gdcloud-calls.log")
	mockScript := filepath.Join(tmpDir, "mock-gdcloud.sh")
	scriptContent := "#!/bin/sh\necho \"SSL_CERT_FILE=${SSL_CERT_FILE} ARGS=$*\" >> \"" + logFile + "\"\n"
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
	defer client.Cleanup()

	bundleContent, err := os.ReadFile(client.caDataPath)
	if err != nil {
		t.Fatalf("failed to read generated CA bundle %q: %v", client.caDataPath, err)
	}
	if !strings.Contains(string(bundleContent), wantCAContent) {
		t.Errorf("CA bundle %q missing GDC CA content", client.caDataPath)
	}

	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed to read gdcloud calls log: %v", err)
	}
	gotLog := string(logBytes)
	wantCalls := []string{
		"SSL_CERT_FILE=" + client.caDataPath + " ARGS=config set core/disable_prompts true",
		"SSL_CERT_FILE=" + client.caDataPath + " ARGS=config set auth/login_config_cert_path " + caFile,
		"SSL_CERT_FILE=" + client.caDataPath + " ARGS=config set core/organization_console_url https://console.example.com",
		"SSL_CERT_FILE=" + client.caDataPath + " ARGS=auth activate-service-account --key-file=/tmp/fake-sa.json",
	}
	for _, want := range wantCalls {
		if !strings.Contains(gotLog, want) {
			t.Errorf("NewTestingClient() missing expected call %q in log:\n%s", want, gotLog)
		}
	}
}
