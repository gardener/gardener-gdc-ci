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

package sonobuoy

import (
	"bytes"
	"strings"
	"testing"
)

func TestExec(t *testing.T) {
	tests := []struct {
		name                   string
		args                   []string
		mockedSonobuoyLocation func() string
		wantOutput             []byte
		wantErrMsg             string
	}{
		{
			name:                   "Successful command execution",
			args:                   []string{"Sonobuoy Version: v0.57.3"},
			mockedSonobuoyLocation: func() string { return "echo" },
			wantOutput:             []byte("Sonobuoy Version"),
			wantErrMsg:             "",
		},
		{
			name:                   "Command returns an error",
			args:                   []string{"-c", "exit 1"},
			mockedSonobuoyLocation: func() string { return "/bin/sh" },
			wantOutput:             nil,
			wantErrMsg:             "exit status 1",
		},
		{
			name:                   "Binary not found",
			args:                   []string{"invalid-arg"},
			mockedSonobuoyLocation: func() string { return "" },
			wantOutput:             nil,
			wantErrMsg:             "cannot locate sonobuoy binary",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mock the location function for the duration of this test case.
			originalLocationFunc := sonobuoyLocation
			sonobuoyLocation = tt.mockedSonobuoyLocation
			defer func() { sonobuoyLocation = originalLocationFunc }() // Restore after test

			gotOutput, gotErr := Exec(tt.args...)
			if tt.wantErrMsg != "" {
				if gotErr == nil {
					t.Fatalf("Exec() succeeded; want error containing %q", tt.wantErrMsg)
				}
				if !strings.Contains(gotErr.Error(), tt.wantErrMsg) {
					t.Errorf("Exec() error = %q, want error containing %q", gotErr, tt.wantErrMsg)
				}
				return
			}

			if gotErr != nil {
				t.Fatalf("Exec() failed unexpectedly: %v", gotErr)
			}

			if !bytes.Contains(gotOutput, tt.wantOutput) {
				t.Errorf("Exec() gotOutput = %q, want output containing %q", gotOutput, tt.wantOutput)
			}
		})
	}
}
