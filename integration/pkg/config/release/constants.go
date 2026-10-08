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

package release

import (
	"flag"
	"os"
)

const (
	// DefaultCandidateRegistryURL is the default GitHub Container Registry path for candidate release artifacts.
	DefaultCandidateRegistryURL = "ghcr.io/gardener/gardener-gdc-ci"

	// GDCCredentialsSecretName is the name of the secret containing the GDC credentials.
	GDCCredentialsSecretName = "gdch-cred"

	// CommitHashAnnotation is the annotation used to identify the Garden or Shoot cluster to be used for the test run.
	CommitHashAnnotation = "integration.release.sapbtp.gardener/commit-hash"

	// SeedBucketNameAnnotation stores the Seed backup bucket name (<seed.UID>) on the host Shoot CR
	// so teardown retries can still identify and verify deletion of the GDC Bucket after the Seed CR is gone.
	SeedBucketNameAnnotation = "integration.release.sapbtp.gardener/seed-bucket-name"

	// TestImageName is the name of the container image used in Shoot Cluster tests.
	TestImageName = "nginx:alpine"

	// CloudControllerManagerImageName is the name of the cloud controller manager image.
	CloudControllerManagerImageName = "cloud-controller-manager-gdch"

	// MachineControllerManagerProviderImageName is the name of the machine controller manager provider image.
	MachineControllerManagerProviderImageName = "machine-controller-manager-provider-gdch"

	// ExtensionProviderGdchHelmChartName is the name of the extension provider GDCH Helm chart.
	ExtensionProviderGdchHelmChartName = "gardener-extension-provider-gdch-helm"

	// ExtensionProviderGdchImageName is the name of the extension provider GDCH image.
	ExtensionProviderGdchImageName = "gardener-extension-provider-gdch"
)

// CandidateRegistryURL returns the candidate artifact registry URL (GHCR),
// reading GHCR_REGISTRY if set or defaulting to DefaultCandidateRegistryURL.
func CandidateRegistryURL() string {
	if reg := os.Getenv("GHCR_REGISTRY"); reg != "" {
		return reg
	}
	return DefaultCandidateRegistryURL
}

func init() {
	// Register dummy flags to prevent test binaries from failing when invoked with
	// agtest runner's common flags.
	flag.String("env-id", "", "dummy flag for env-id")
	flag.String("surface-type", "", "dummy flag for surface-type")
	flag.String("global-org-console-url", "", "dummy flag for global-org-console-url")
	flag.String("zonal-org-console-url", "", "dummy flag for zonal-org-console-url")
	flag.String("zone", "", "dummy flag for zone")
	flag.String("org", "", "dummy flag for org")
	flag.String("gdcloud-cli-path", "", "dummy flag for gdcloud-cli-path")
	flag.String("pa-sa-key-path", "", "dummy flag for pa-sa-key-path")
	flag.String("allocate-new-user-cluster", "", "dummy flag for allocate-new-user-cluster")
}
