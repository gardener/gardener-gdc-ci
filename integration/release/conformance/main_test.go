// Copyright 2025 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package conformance

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/loader"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gcs"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/sonobuoy"
)

const (
	// Upload destination GCS bucket Name
	gcsBucketName = "gardener-ci-pipeline"
	// Directory within the bucket to store results
	gcsResultsDir = "shoot-conformance-results"
	// fixed path for junit output of sonobuoy test result
	e2eResultsPath = "plugins/e2e/results/global/junit_01.xml"
)

type junitTestSuites struct {
	XMLName  xml.Name       `xml:"testsuites"`
	Failures int            `xml:"failures,attr"`
	Tests    int            `xml:"tests,attr"`
	Suite    junitTestSuite `xml:"testsuite"`
}

type junitTestSuite struct {
	XMLName   xml.Name        `xml:"testsuite"`
	Name      string          `xml:"name,attr"`
	Tests     int             `xml:"tests,attr"`
	Failures  int             `xml:"failures,attr"`
	TestCases []junitTestCase `xml:"testcase"`
}

type junitTestCase struct {
	XMLName xml.Name      `xml:"testcase"`
	Name    string        `xml:"name,attr"`
	Status  string        `xml:"status,attr"`
	Time    string        `xml:"time,attr"`
	Failure *junitFailure `xml:"failure"` // Pointer, nil if the test passed.
}

// JUnitFailure maps to the <failure> element.
type junitFailure struct {
	XMLName     xml.Name `xml:"failure"`
	MessageAttr string   `xml:"message,attr"`
	// the actual error message text inside the tag.
	MessageBody string `xml:",chardata"`
}

var (
	nonAlphanumericRegex = regexp.MustCompile(`[^a-zA-Z0-9-_]+`)

	gardenerArtifactsVersion        = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath    = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	continuousConfigurationFilePath = flag.String("continuous-configuration-file-path", "", "the path to the continuous configuration file")
)

// sanitizePathSegment replaces characters unsuitable for GCS paths with hyphens.
func sanitizePathSegment(segment string) string {
	return nonAlphanumericRegex.ReplaceAllString(segment, "-")
}

func TestShootConformance(t *testing.T) {
	ctx := context.Background()
	t.Logf("TestShootConformance started.")

	releasePipelineCfg, err := loader.LoadConfig(loader.LoadConfigOptions{
		ReleaseConfigPath:    *releaseConfigurationFilePath,
		ContinuousConfigPath: *continuousConfigurationFilePath,
		GardenerVersion:      *gardenerArtifactsVersion,
	})
	if err != nil {
		t.Fatalf("failed to load configuration: %v", err)
	}

	if releasePipelineCfg != nil && releasePipelineCfg.GDCClient != nil {
		defer releasePipelineCfg.GDCClient.Cleanup()
	}
	// Define the Shoot we are targeting.
	shootKey := client.ObjectKey{
		Namespace: releasePipelineCfg.TestShoot.Namespace,
		Name:      releasePipelineCfg.TestShoot.Name,
	}

	// Call the new function to dynamically get the Shoot's kubeconfig.
	t.Logf("Requesting kubeconfig for Shoot %q in namespace %q", shootKey.Name, shootKey.Namespace)
	shootKubeconfigBytes, err := gardener.GetShootKubeconfig(ctx, releasePipelineCfg.TestShoot.VirtualGarden.Client, shootKey)
	if err != nil {
		t.Fatalf("Failed to get Shoot kubeconfig: %v", err)
	}

	// create a temp directory for storing test related files (kubeconfig, test results etc.)
	// this directory will be cleaned up after test is done
	tempDir := t.TempDir()
	testRunId := sanitizePathSegment(filepath.Base(tempDir))
	t.Logf("Using Test Run ID: %s (derived from %s)", testRunId, tempDir)

	shootKubeconfigPath := filepath.Join(tempDir, "shoot.config")
	t.Logf("Writing Shoot kubeconfig to %s", shootKubeconfigPath)
	if err := os.WriteFile(shootKubeconfigPath, shootKubeconfigBytes, 0600); err != nil {
		t.Fatalf("Failed to write Shoot kubeconfig to temporary file: %v", err)
	}
	t.Logf("Successfully wrote Shoot kubeconfig to: %s", shootKubeconfigPath)

	// Cleanup for Sonobuoy resources on the cluster
	t.Cleanup(func() {
		t.Logf("Cleanup: Deleting Sonobuoy resources from cluster...")
		deleteArgs := []string{
			"delete",
			"--all",
			"--wait",
			"--kubeconfig", shootKubeconfigPath,
		}

		output, err := sonobuoy.Exec(deleteArgs...)
		t.Logf("Sonobuoy delete output:\n%s", string(output))
		if err != nil {
			t.Errorf("Failed to cleanup Sonobuoy resources from cluster: %v", err)
		} else {
			t.Logf("Sonobuoy cluster cleanup finished successfully.")
		}
	})

	t.Run("RunSonobuoyConformance", func(t *testing.T) {
		t.Logf("Subtest RunSonobuoyConformance started.")

		skipTests := []string{
			"AdmissionWebhook.*should honor timeout",
			"\\[Slow\\]",
			"\\[Serial\\]",
			"\\[Disruptive\\]",
			"CronJob",
			"HostPort validates that there is no conflict between pods with same hostPort but different hostIP and protocol",
			"Services should serve endpoints on same port and different protocols",
			"Aggregator Should be able to support the 1.17 Sample API Server using the current Aggregator",
		}
		skipRegex := strings.Join(skipTests, "|")
		t.Log("Running Sonobuoy conformance tests (this may take a while)")
		args := []string{
			"run",
			"--kubeconfig", shootKubeconfigPath,
			"--mode", "certified-conformance", // Run the full conformance suite
			"--plugin-env", "e2e.E2E_PARALLEL=true",
			"--plugin-env", fmt.Sprintf("e2e.E2E_SKIP=%s", skipRegex),
			"--wait",
		}
		output, err := sonobuoy.Exec(args...)
		t.Logf("Executing Sonobuoy run: %s", strings.Join(args, " "))
		t.Logf("Sonobuoy run output:\n%s", string(output))
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				t.Errorf("Sonobuoy run command failed with exit code %d", exitErr.ExitCode())
			}
			t.Fatalf("Sonobuoy run failed: %v", err)
		}
		t.Logf("Sonobuoy run command finished successfully.")

		t.Log("Retrieving Sonobuoy results")
		retrieveArgs := []string{
			"retrieve",
			"--kubeconfig", shootKubeconfigPath,
		}
		t.Logf("Executing Sonobuoy retrieve: %s", strings.Join(retrieveArgs, " "))
		retrieveOutput, err := sonobuoy.Exec(retrieveArgs...)
		if err != nil {
			t.Fatalf("Failed to retrieve Sonobuoy results: %v", err)
		}
		t.Logf("Sonobuoy retrieve raw output: %q", string(retrieveOutput))

		retrievedFile := strings.TrimSpace(string(retrieveOutput))
		if retrievedFile == "" {
			t.Fatal("Sonobuoy retrieve returned an empty filename")
		}

		// The retrieved file is placed in the current working directory by sonobuoy.
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("Failed to get current working directory: %v", err)
		}
		originalResultsPath := filepath.Join(cwd, retrievedFile)
		t.Logf("Original Sonobuoy results tarball path: %s", originalResultsPath)

		if _, err := os.Stat(originalResultsPath); err != nil {
			t.Fatalf("Sonobuoy results tarball not found at %s: %v", originalResultsPath, err)
		}
		timestamp := time.Now().UTC().Format("20060102T150405Z")
		destFileName := fmt.Sprintf("conf-%s-%s.tar.gz", timestamp, releasePipelineCfg.TestShoot.Name)

		// Construct the GCS object path
		gcsDestObjectPath := filepath.Join(gcsResultsDir, destFileName)

		// Upload the results file to GCS
		t.Logf("Uploading results to GCS bucket %s", gcsBucketName)
		uploadFileToGCS(t, gcsBucketName, gcsDestObjectPath, originalResultsPath, testRunId, releasePipelineCfg.TestShoot.Name)

		t.Logf("Conformance test results tarball uploaded to: gs://%s/%s", gcsBucketName, gcsDestObjectPath)

		t.Log("Verifying conformance test results")
		if err := verifySonobuoyResults(t, originalResultsPath); err != nil {
			t.Fatalf("Sonobuoy results verification failed: %v", err)
		}
		t.Log("Sonobuoy results verification successful: all tests passed.")
	})
	t.Logf("TestShootConformance finished.")
}

// verifySonobuoyResults opens the results tarball, finds the JUnit results file,
// and checks it for any test failures. It returns an error if failures are found.
func verifySonobuoyResults(t *testing.T, resultPath string) error {
	t.Helper()

	file, err := os.Open(resultPath)
	if err != nil {
		return fmt.Errorf("could not open results tarball %s: %w", resultPath, err)
	}
	defer file.Close()

	// Create a gzip reader
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("could not create gzip reader: %w", err)
	}
	defer gzipReader.Close()

	// create tar file specific reader
	tarReader := tar.NewReader(gzipReader)

	var fileFound bool
	var junitData []byte

	// Loop through every file and directory within the tar archive.
	for {
		// Read the next file/dir from the tar archive.
		header, err := tarReader.Next()
		if err == io.EOF {
			break // End of archive
		}
		if err != nil {
			return fmt.Errorf("error reading tar header: %w", err)
		}

		// Sanitize the filename from the header to prevent path traversal issues.
		cleanedName := filepath.Clean(header.Name)
		// Check if the current file is the one we're looking for.
		if cleanedName == e2eResultsPath {
			fileFound = true
			t.Logf("Found results file in tarball: %s", header.Name)

			if header.Typeflag != tar.TypeReg {
				return fmt.Errorf("expected results file %s to be a regular file, got type %v", e2eResultsPath, header.Typeflag)
			}

			junitData, err = io.ReadAll(tarReader)
			if err != nil {
				return fmt.Errorf("failed to read results file content: %w", err)
			}
			break
		}
	}

	if !fileFound {
		return fmt.Errorf("could not find results file (%s) in the Sonobuoy tarball", e2eResultsPath)
	}

	var suites junitTestSuites
	if err := xml.Unmarshal(junitData, &suites); err != nil {
		return fmt.Errorf("failed to parse JUnit XML: %w", err)
	}

	t.Logf("Parsed results: Tests=%d, Failures=%d", suites.Tests, suites.Failures)

	// check top-level failure count
	if suites.Failures > 0 {
		t.Logf("--- FAILED E2E TESTS ---")
		// Loop through the test cases within the single suite
		for _, testCase := range suites.Suite.TestCases {
			// Check if the Failure field is populated
			if testCase.Failure != nil {
				t.Logf("Test Case: %s", testCase.Name)
				failureMessage := strings.TrimSpace(testCase.Failure.MessageBody)
				t.Logf("  Failure Message: %s", failureMessage)
				t.Logf("--------------------------")
			}
		}
		return fmt.Errorf("conformance run finished with %d failed tests", suites.Failures)
	}
	return nil
}

// uploadFileToGCS uploads a local file to the specified GCS bucket and object path.
func uploadFileToGCS(t *testing.T, bucketName, objectName, localFilePath, testRunId, shootName string) {
	t.Helper()
	ctx := context.Background()

	options := gcs.UploadOptions{
		BucketName:    bucketName,
		ObjectName:    objectName,
		LocalFilePath: localFilePath,
		Metadata: map[string]string{
			"uploaded-by":       "TestShootConformance",
			"original-filename": filepath.Base(localFilePath),
			"test-run-id":       testRunId,
			"shoot-name":        shootName,
		},
	}

	if err := gcs.UploadFile(ctx, options); err != nil {
		t.Fatalf("Failed to upload file to GCS: %v", err)
	}
	t.Logf("Successfully uploaded to gs://%s/%s", bucketName, objectName)
}
