// Copyright 2026 Google LLC
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

package conformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/gcs"
)

func TestPersistConformanceResults(t *testing.T) {
	const (
		resultsContent = "sonobuoy-results"
		destFileName   = "conf-20260101T000000Z-sh-test.tar.gz"
	)

	resultsPath := filepath.Join(t.TempDir(), "sonobuoy.tar.gz")
	if err := os.WriteFile(resultsPath, []byte(resultsContent), 0600); err != nil {
		t.Fatalf("Failed to write results tarball: %v", err)
	}

	// A regular file where the artifacts directory should be makes the local copy fail.
	unusableArtifactsDir := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(unusableArtifactsDir, nil, 0600); err != nil {
		t.Fatalf("Failed to create blocking file: %v", err)
	}

	tests := []struct {
		name          string
		artifactsDir  string
		bucket        string
		uploadErr     error
		wantLocalCopy bool
		wantUpload    bool
		wantErrs      []string
	}{
		{
			name: "no destination configured leaves the tarball in place",
		},
		{
			name:          "local artifacts directory only",
			artifactsDir:  t.TempDir(),
			wantLocalCopy: true,
		},
		{
			name:          "local artifacts directory and bucket",
			artifactsDir:  t.TempDir(),
			bucket:        "private-bucket",
			wantLocalCopy: true,
			wantUpload:    true,
		},
		{
			name:         "unusable local artifacts directory is an error",
			artifactsDir: unusableArtifactsDir,
			wantErrs:     []string{"failed to save results"},
		},
		{
			name:          "failed upload is an error even when the local copy succeeded",
			artifactsDir:  t.TempDir(),
			bucket:        "private-bucket",
			uploadErr:     fmt.Errorf("simulated upload failure"),
			wantLocalCopy: true,
			wantUpload:    true,
			wantErrs:      []string{"failed to upload results to gs://private-bucket/", "simulated upload failure"},
		},
		{
			name:         "both destinations failing reports both",
			artifactsDir: unusableArtifactsDir,
			bucket:       "private-bucket",
			uploadErr:    fmt.Errorf("simulated upload failure"),
			wantUpload:   true,
			wantErrs:     []string{"failed to save results", "simulated upload failure"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(artifactsDirEnv, tc.artifactsDir)
			t.Setenv(diagnosticsBucketEnv, tc.bucket)

			var uploads []gcs.UploadOptions
			origUploadFile := uploadFile
			uploadFile = func(_ context.Context, options gcs.UploadOptions) error {
				uploads = append(uploads, options)
				return tc.uploadErr
			}
			t.Cleanup(func() { uploadFile = origUploadFile })

			err := persistConformanceResults(context.Background(), t.Logf, resultsPath, destFileName, "run-1", "sh-test")

			if len(tc.wantErrs) == 0 && err != nil {
				t.Fatalf("persistConformanceResults() returned unexpected error: %v", err)
			}
			if len(tc.wantErrs) > 0 && err == nil {
				t.Fatalf("persistConformanceResults() = nil, want error containing %q", tc.wantErrs)
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("persistConformanceResults() error = %q, want it to contain %q", err, want)
				}
			}

			if tc.wantLocalCopy {
				localCopy := filepath.Join(tc.artifactsDir, "diagnostics", "conformance", destFileName)
				data, readErr := os.ReadFile(localCopy)
				if readErr != nil {
					t.Fatalf("Expected a local copy of the results tarball: %v", readErr)
				}
				if string(data) != resultsContent {
					t.Errorf("Local copy content = %q, want %q", data, resultsContent)
				}
			}

			if !tc.wantUpload {
				if len(uploads) != 0 {
					t.Errorf("Expected no upload, got %+v", uploads)
				}
				return
			}
			if len(uploads) != 1 {
				t.Fatalf("Expected exactly one upload, got %d", len(uploads))
			}
			wantObject := gcsResultsDir + "/" + destFileName
			if uploads[0].BucketName != tc.bucket || uploads[0].ObjectName != wantObject || uploads[0].LocalFilePath != resultsPath {
				t.Errorf("Unexpected upload %+v, want bucket %q, object %q, file %q", uploads[0], tc.bucket, wantObject, resultsPath)
			}
		})
	}
}
