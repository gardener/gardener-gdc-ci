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

package shoot

import (
	"context"
	"flag"
	"net/url"
	"strings"
	"testing"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	securityv1alpha1 "github.com/gardener/gardener/pkg/apis/security/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/subnet"
)

var (
	gardenerArtifactsVersion       = flag.String("gardener-artifacts-version", "", "The short artifacts version for gardener repo")
	externalDNSArtifactsVersion    = flag.String("external-dns-artifacts-version", "", "The short artifacts version for external-dns-management repo")
	externalDNSManagementImageName = flag.String("external-dns-management-image-name", "external-dns-management-gdch", "The name of the external DNS management image")
	releaseConfigurationFilePath   = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	virtualGardenProvider          = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	gardenProjectName           = "garden"
	waitForShootCreationTimeout = 45 * time.Minute
	// subnetPrefixLength is set to /27 (32 IPs).
	// Rationale for /27:
	// 1. Capacity: A /27 subnet provides 32 IPs, which splitCIDR divides into /28 (16 IPs)
	//    per zone. This is plenty for the test worker VM pools (typically 3 nodes).
	// 2. Coexistence: The shared parent subnet group (release-pipeline-parent-subnet-group)
	//    is backed by a /26 CIDR (64 IPs total). Allocating /27 leaves 32 IPs in the pool.
	// Why /26 backfired:
	// - Allocating /26 consumed the entire 64-IP parent pool (100% utilization).
	// - Subsequent parallel test stages (e.g. 5.3 LoadBalancer services and 5.4 DNS lifecycle)
	//   dynamically request branch subnets (such as /31 for ILBs) from the same parent pool.
	// - With /26, IPAM failed with "No resource could allocate a /31 CIDR", causing LB IP
	//   allocation timeouts and cascading test failures.
	subnetPrefixLength        = 27
	waitForSubnetReadyTimeout = 5 * time.Minute
)

func TestCreateShootCluster(t *testing.T) {
	validateFlags(t)

	t.Logf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releasePipelineCfg, err := releaseConfig.LoadReleaseTestConfig(
		*releaseConfigurationFilePath,
		*gardenerArtifactsVersion,
		releaseConfig.WithVirtualGardenProvider(*virtualGardenProvider),
	)
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releasePipelineCfg != nil && releasePipelineCfg.GDCClient != nil {
		defer releasePipelineCfg.GDCClient.Cleanup()
	}

	// create shoot's parent subnet
	ctx := context.Background()
	t.Logf("Ensuring shoot parent subnet for commit %s", *gardenerArtifactsVersion)
	subnetName := gardener.GetShootParentSubnetName(*gardenerArtifactsVersion)
	shootNodeCIDR, err := subnet.EnsureSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnet.SubnetOptions{
		Namespace:         releasePipelineCfg.GDC.Project,
		Name:              subnetName,
		ParentSubnetGroup: releasePipelineCfg.GDC.ParentPrivateSubnetGroup,
		PrefixLength:      subnetPrefixLength,
	}, waitForSubnetReadyTimeout, t)
	if err != nil {
		t.Fatalf("failed to ensure subnet %s", err)
	}

	// Create Image Pull Secret
	gardenClient := releasePipelineCfg.TestShoot.VirtualGarden.Client
	t.Log("Creating image pull secret in virtual garden")
	if err := gardener.CreateImagePullSecret(ctx, gardenClient, releasePipelineCfg.ImagePullCredentials); err != nil {
		t.Fatalf("failed to create image pull secret: %v", err)
	}

	// Deploy Networking-Cilium extension
	t.Log("Deploying Networking-Cilium extension")
	if err := gardener.DeployExtension(ctx, gardenClient, newCiliumExtensionConfig(t, releasePipelineCfg)); err != nil {
		t.Fatalf("failed to deploy Cilium extension: %v", err)
	}

	// Deploy OS-Gardenlinux extension
	t.Log("Deploying OS-Gardenlinux extension")
	if err := gardener.DeployExtension(ctx, gardenClient, newGardenlinuxExtensionConfig(t, releasePipelineCfg)); err != nil {
		t.Fatalf("failed to deploy Gardenlinux extension: %v", err)
	}

	// Deploy External DNS extension
	t.Log("Deploying External DNS extension")
	if err := gardener.DeployExtension(ctx, gardenClient, newExternalDNSExtensionConfig(t, releasePipelineCfg, *externalDNSArtifactsVersion, *externalDNSManagementImageName)); err != nil {
		t.Fatalf("failed to deploy external DNS extension: %v", err)
	}

	// Create Gardener project
	t.Logf("Creating Gardener project %s", gardenProjectName)
	createGardenerProject(ctx, gardenClient, gardenProjectName, t)

	// Add required labels to the garden namespace
	t.Log("Labeling garden namespace")
	labelGardenNamespace(ctx, gardenClient, t)

	// Create Gardener's SecretBinding to reference the GDC cloudprovider credentials
	t.Log("Creating SecretBinding for GDC credentials")
	createCredentialsBinding(ctx, gardenClient, t)

	// Create Cloud profile
	t.Logf("Creating CloudProfile %s", cloudprofileName)
	createCloudprofile(ctx, gardenClient, releasePipelineCfg, t)

	// Create Shoot cluster
	t.Logf("Creating Shoot cluster %s", releasePipelineCfg.TestShoot.Name)
	createShoot(ctx, gardenClient, releasePipelineCfg, shootNodeCIDR, *gardenerArtifactsVersion, t)
}

func createShoot(ctx context.Context, gardenClient client.WithWatch, cfg *config.ReleaseTestConfig, shootNodeCIDR, gardenerArtifactsVersion string, t *testing.T) {
	shoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.TestShoot.Name,
			Namespace: gardenv1beta1constants.GardenNamespace,
		},
	}

	shootSpec := newShootSpec(t, cfg, shootNodeCIDR, gardenerArtifactsVersion)

	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, shoot, func() error {
		shoot.Spec = shootSpec
		return nil
	})

	if err != nil {
		t.Fatalf("failed to create or update shoot %q/%q: %v", shoot.Namespace, shoot.Name, err)
	}
	shootKey := client.ObjectKey{Name: cfg.TestShoot.Name, Namespace: gardenv1beta1constants.GardenNamespace}
	err = gardener.WaitForShootReconciliation(ctx, gardenClient, shootKey, waitForShootCreationTimeout)
	if err != nil {
		t.Fatalf("shoot \"%q/%q\" reconciliation failed or timed out: %v", shoot.Namespace, shoot.Name, err)
	}
}

func createCloudprofile(ctx context.Context, gardenClient client.WithWatch, cfg *config.ReleaseTestConfig, t *testing.T) {
	cloudprofile := &gardencorev1beta1.CloudProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: cloudprofileName,
		},
	}
	harborRegistry := parseHarborUrl(cfg.GDC.HarborRegistryURL, t)
	cloudProfileSpec := newCloudProfileSpec(t, cfg, harborRegistry)

	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, cloudprofile, func() error {
		cloudprofile.Spec = cloudProfileSpec
		return nil
	})

	if err != nil {
		t.Fatalf("failed to create or update CloudProfile: %q: %v", cloudprofileName, err)
	}
}

// validateFlags checks that all required command-line flags have been provided.
func validateFlags(t *testing.T) error {
	// A map of flag names to their string pointer values for easy validation.
	requiredFlags := map[string]*string{
		"gardener-artifacts-version":      gardenerArtifactsVersion,
		"external-dns-artifacts-version":  externalDNSArtifactsVersion,
		"release-configuration-file-path": releaseConfigurationFilePath,
	}

	for name, value := range requiredFlags {
		if *value == "" {
			t.Fatalf("--%s is a required flag", name)
		}
	}

	return nil
}

func parseHarborUrl(harborRegistryURL string, t *testing.T) *harborRegistry {
	parsedURL, err := url.Parse(harborRegistryURL)
	if err != nil {
		t.Fatalf("failed to parse harborRegistryURL %q: %v; URL must be in the format 'harbor.example.com/my-project'", harborRegistryURL, err)
	}

	// If no scheme specified, Prepend the default scheme and re-parse
	if parsedURL.Scheme == "" {
		parsedURL, err = url.Parse("https://" + harborRegistryURL)
	}

	if parsedURL.Host == "" {
		t.Fatalf("Harbor URL is not specified")
	}

	if parsedURL.Path == "" {
		t.Fatalf("Harbor project is not specified")
	}

	fullPathWithoutTrailingSlash := strings.TrimSuffix(parsedURL.Host+parsedURL.Path, "/")
	return &harborRegistry{
		host:     parsedURL.Host,
		fullPath: fullPathWithoutTrailingSlash,
	}
}

func labelGardenNamespace(ctx context.Context, gardenClient client.Client, t *testing.T) {
	ns := &corev1.Namespace{}
	if err := gardenClient.Get(ctx, client.ObjectKey{Name: "garden"}, ns); err != nil {
		t.Fatalf("failed to get garden namespace: %v", err)
	}
	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	ns.Labels["project.gardener.cloud/name"] = "garden"
	ns.Labels["gardener.cloud/role"] = "project"
	if err := gardenClient.Update(ctx, ns); err != nil {
		t.Fatalf("failed to update garden namespace: %v", err)
	}
}

func createCredentialsBinding(ctx context.Context, gardenClient client.Client, t *testing.T) {
	credentialsBinding := &securityv1alpha1.CredentialsBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsBindingName,
			Namespace: gardenProjectName,
		},
		Provider: securityv1alpha1.CredentialsBindingProvider{
			Type: "gdch",
		},
		CredentialsRef: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Secret",
			Name:       releaseConfig.GDCCredentialsSecretName,
			Namespace:  gardenProjectName,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, credentialsBinding, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("failed to create credentials binding: %v", err)
	}
}

func createGardenerProject(ctx context.Context, gardenClient client.WithWatch, projectName string, t *testing.T) {
	project := &gardencorev1beta1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectName,
			Namespace: gardenv1beta1constants.GardenNamespace,
		},
		Spec: gardencorev1beta1.ProjectSpec{
			Namespace: ptr.To(gardenv1beta1constants.GardenNamespace),
		},
	}
	err := gardenClient.Create(ctx, project)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("failed to create gardener project: %v", err)
	}
}
