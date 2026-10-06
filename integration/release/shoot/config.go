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
	"encoding/json"
	"fmt"
	"net/netip"
	"testing"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
)

// harborRegistry holds the host and full path for a Harbor instance.
type harborRegistry struct {
	// host is the base address of the Harbor instance (e.g., "harbor.example.com").
	host string
	// fullPath is the complete URL including the project (e.g., "harbor.example.com/my-project").
	fullPath string
}

const (
	credentialsBindingName = "seed-binding"
	cloudprofileName       = "gdch-staging"
	gdcMachineType         = "n3-standard-2-gdc"
)

// newShootSpec constructs the spec for the Shoot cluster using the provided configuration.
func newShootSpec(t *testing.T, cfg *config.ReleaseTestConfig, shootNodeCIDR, gardenerArtifactsVersion string) gardencorev1beta1.ShootSpec {
	t.Logf("Creating Shoot spec for Gardener-GDC with zones: %v", cfg.GDC.Zones)
	var workerPools []gardencorev1beta1.Worker
	var infraConfigZones []map[string]interface{}
	shootNodeCIDRs, err := splitCIDR(shootNodeCIDR)
	if err != nil {
		t.Fatalf("failed to split CIDR: %v", err)
	}
	for i, zone := range cfg.GDC.Zones {
		zoneIdentifier := string(zone[len(zone)-1])
		t.Logf("Adding worker pool %s for zone %s", fmt.Sprintf("%s-%s", "pool-", zoneIdentifier), zone)
		workerPools = append(workerPools, gardencorev1beta1.Worker{
			Name:    fmt.Sprintf("%s-%s", "pool-", zoneIdentifier),
			Minimum: 2,
			Maximum: 2,
			Machine: gardencorev1beta1.Machine{
				Type:         gdcMachineType,
				Architecture: ptr.To("amd64"),
				Image: &gardencorev1beta1.ShootMachineImage{
					Name:    "gardenlinux",
					Version: ptr.To("v1"),
				},
			},
			Volume: &gardencorev1beta1.Volume{
				VolumeSize: "160Gi",
				Type:       ptr.To("pd-standard"),
			},
			Zones: []string{zone},
		})
		infraConfigZones = append(infraConfigZones, map[string]interface{}{
			"name": zone,
			"CIDR": shootNodeCIDRs[i],
		})
	}
	t.Logf("Infrastructure config: parentSubnet=%s, parentSubnetProject=%s, nodeCIDR=%s", gardener.GetShootParentSubnetName(gardenerArtifactsVersion), cfg.GDC.Project, shootNodeCIDR)
	return gardencorev1beta1.ShootSpec{
		ControlPlane: &gardencorev1beta1.ControlPlane{
			HighAvailability: &gardencorev1beta1.HighAvailability{
				FailureTolerance: gardencorev1beta1.FailureTolerance{
					Type: gardencorev1beta1.FailureToleranceTypeNode,
				},
			},
		},
		Extensions: []gardencorev1beta1.Extension{
			{
				Type: "shoot-dns-service",
			},
		},
		CloudProfile: &gardencorev1beta1.CloudProfileReference{
			Kind: "CloudProfile",
			Name: string(cloudprofileName),
		},
		DNS: &gardencorev1beta1.DNS{
			Domain: ptr.To(cfg.TestShoot.Name + "." + gardenv1beta1constants.GardenNamespace + "." + cfg.GDC.ManagedDNSDomainName),
		},
		Kubernetes: gardencorev1beta1.Kubernetes{
			Version: cfg.Gardener.KubernetesVersion,
			VerticalPodAutoscaler: &gardencorev1beta1.VerticalPodAutoscaler{
				Enabled: true,
			},
		},
		Networking: &gardencorev1beta1.Networking{
			IPFamilies: []gardencorev1beta1.IPFamily{
				gardencorev1beta1.IPFamilyIPv4,
			},
			Nodes:    ptr.To(shootNodeCIDR),
			Pods:     ptr.To("10.240.0.0/16"),
			Services: ptr.To("100.64.0.0/13"),
			Type:     ptr.To("cilium"),
		},
		Provider: gardencorev1beta1.Provider{
			Type: "gdch",
			InfrastructureConfig: &runtime.RawExtension{
				Raw: encode(map[string]interface{}{
					"apiVersion": "gdch.provider.extensions.gardener.gdc.goog/v1alpha1",
					"kind":       "InfrastructureConfig",
					"networks": map[string]interface{}{
						"nodeCIDR":            shootNodeCIDR,
						"parentSubnet":        gardener.GetShootParentSubnetName(gardenerArtifactsVersion),
						"parentSubnetProject": cfg.GDC.Project,
						"zones":               infraConfigZones,
					},
				}, t),
			},
			Workers: workerPools,
		},
		Region:                 cfg.GDC.Region,
		Purpose:                ptr.To(gardencorev1beta1.ShootPurposeTesting),
		CredentialsBindingName: ptr.To(credentialsBindingName),
		SeedName:               ptr.To(cfg.Seed.Name),
	}
}

// newCloudProfile constructs the spec for Gardener-GDC CloudProfile using provided configurations.
func newCloudProfileSpec(t *testing.T, cfg *config.ReleaseTestConfig, harborRegistry *harborRegistry) gardencorev1beta1.CloudProfileSpec {
	t.Logf("Creating CloudProfile spec for Gardener-GDC with zones: %v", cfg.GDC.Zones)
	var availabilityZones []gardencorev1beta1.AvailabilityZone
	var orgConfigZones []map[string]interface{}
	for _, zoneName := range cfg.GDC.Zones {
		availabilityZones = append(availabilityZones, gardencorev1beta1.AvailabilityZone{Name: zoneName})
		orgConfigZones = append(orgConfigZones, map[string]interface{}{
			"name":              zoneName,
			"managementAPI":     fmt.Sprintf("https://management-kube.apiserver.%s.%s.%s", cfg.GDC.Org, zoneName, cfg.GDC.LabDomainURL),
			"infrastructureAPI": fmt.Sprintf("https://infra-kube.apiserver.%s.%s.%s", cfg.GDC.Org, zoneName, cfg.GDC.LabDomainURL),
		})
	}
	t.Logf("CloudProfile: region=%s, machineType=%s, shootKubernetesVersion=%s", cfg.GDC.Region, gdcMachineType, cfg.Gardener.KubernetesVersion)
	return gardencorev1beta1.CloudProfileSpec{
		Type: "gdch",
		Kubernetes: gardencorev1beta1.KubernetesSettings{
			Versions: []gardencorev1beta1.ExpirableVersion{
				{
					Version:        cfg.Gardener.KubernetesVersion,
					Classification: ptr.To(gardencorev1beta1.ClassificationSupported),
				},
			},
		},
		MachineImages: []gardencorev1beta1.MachineImage{
			{
				Name:           "gardenlinux",
				UpdateStrategy: ptr.To(gardencorev1beta1.UpdateStrategyMajor),
				Versions: []gardencorev1beta1.MachineImageVersion{
					{
						ExpirableVersion: gardencorev1beta1.ExpirableVersion{
							Version:        "v1",
							Classification: ptr.To(gardencorev1beta1.ClassificationSupported),
						},
						CRI: []gardencorev1beta1.CRI{
							{
								Name: gardencorev1beta1.CRINameContainerD,
								ContainerRuntimes: []gardencorev1beta1.ContainerRuntime{
									{Type: "gvisor"},
								},
							},
						},
						Architectures: []string{"amd64"},
					},
				},
			},
		},
		MachineTypes: []gardencorev1beta1.MachineType{
			{
				Name:         gdcMachineType,
				CPU:          resource.MustParse("2"),
				GPU:          resource.MustParse("0"),
				Memory:       resource.MustParse("7Gi"),
				Usable:       ptr.To(true),
				Architecture: ptr.To("amd64"),
			},
		},
		Regions: []gardencorev1beta1.Region{
			{
				Name:  cfg.GDC.Region,
				Zones: availabilityZones,
			},
		},
		VolumeTypes: []gardencorev1beta1.VolumeType{
			{
				Name:    "pd-standard",
				Class:   "standard",
				MinSize: ptr.To(resource.MustParse("20Gi")),
				Usable:  ptr.To(true),
			},
		},
		ProviderConfig: &runtime.RawExtension{
			Raw: encode(map[string]interface{}{
				"apiVersion": "gdch.provider.extensions.gardener.gdc.goog/v1alpha1",
				"kind":       "CloudProfileConfig",
				"machineImages": []map[string]interface{}{
					{
						"name":    "gardenlinux",
						"project": cfg.GDC.Project,
						"versions": []map[string]interface{}{
							{
								"version":      "v1",
								"image":        "gardenlinux-gdch",
								"architecture": "amd64",
							},
						},
					},
				},
				"orgConfig": map[string]interface{}{
					"caData":              cfg.GDC.CAData,
					"globalManagementAPI": cfg.GDC.GlobalAPIURL,
					"orgName":             cfg.GDC.Org,
					"registryURL":         harborRegistry.host,
					"zones":               orgConfigZones,
				},
			}, t),
		},
	}
}

// newCiliumExtensionConfig creates the configuration for the Cilium extension.
func newCiliumExtensionConfig(t *testing.T, cfg *config.ReleaseTestConfig) *gardener.ExtensionConfig {
	t.Logf("Creating Cilium extension config with version %s", cfg.Gardener.CiliumExtension.Version)
	return &gardener.ExtensionConfig{
		Name:                "networking-cilium",
		HelmChartRef:        &cfg.Gardener.CiliumExtension.HelmChartRef,
		ImagePullSecretName: gardener.ImagePullSecretName,
		Resources: []gardencorev1beta1.ControllerResource{{
			Kind:    "Network",
			Type:    "cilium",
			Primary: ptr.To(true),
		}},
		Values: map[string]interface{}{
			"image": map[string]interface{}{
				"tag":        cfg.Gardener.CiliumExtension.Version,
				"pullPolicy": "Always",
			},
		},
	}
}

// newGardenlinuxExtensionConfig creates the configuration for the Gardenlinux OS extension.
func newGardenlinuxExtensionConfig(t *testing.T, cfg *config.ReleaseTestConfig) *gardener.ExtensionConfig {
	t.Logf("Creating Gardenlinux extension config with version %s", cfg.Gardener.GardenlinuxExtension.Version)
	return &gardener.ExtensionConfig{
		Name:                "os-gardenlinux",
		HelmChartRef:        &cfg.Gardener.GardenlinuxExtension.HelmChartRef,
		ImagePullSecretName: gardener.ImagePullSecretName,
		Resources: []gardencorev1beta1.ControllerResource{{
			Kind:    "OperatingSystemConfig",
			Type:    "gardenlinux",
			Primary: ptr.To(true),
		}},
		Values: map[string]interface{}{
			"image": map[string]interface{}{
				"tag":        cfg.Gardener.GardenlinuxExtension.Version,
				"pullPolicy": "Always",
			},
		},
	}
}

// newExternalDNSExtensionConfig creates the configuration for the Shoot DNS extension.
func newExternalDNSExtensionConfig(t *testing.T, cfg *config.ReleaseTestConfig, externalDNSArtifactsVersion, externalDNSManagementImageName string) *gardener.ExtensionConfig {
	t.Logf("Creating External DNS extension config with version %s and dns manager version %s", cfg.Gardener.ExternalDNSExtension.Version, externalDNSArtifactsVersion)
	dnsControllerManagerRepository := fmt.Sprintf("%s/%s", releaseConfig.CandidateRegistryURL(), externalDNSManagementImageName)

	useNextGen := true
	imageNameOverwrite := "dns-controller-manager-next-generation"
	if externalDNSManagementImageName == "external-dns-management-gdch" {
		useNextGen = false
		imageNameOverwrite = "dns-controller-manager"
	}

	return &gardener.ExtensionConfig{
		Name:                "extension-shoot-dns-service",
		HelmChartRef:        &cfg.Gardener.ExternalDNSExtension.HelmChartRef,
		ImagePullSecretName: gardener.ImagePullSecretName,
		Resources: []gardencorev1beta1.ControllerResource{{
			Kind: "Extension",
			Type: "shoot-dns-service",
		}},
		Values: map[string]interface{}{
			"useNextGenerationController": useNextGen,
			"image": map[string]interface{}{
				"tag":        cfg.Gardener.ExternalDNSExtension.Version,
				"pullPolicy": "Always",
			},
			// When useNextGen is true, next-gen controllers are managed dynamically per shoot
			// via imageVectorOverwrite. Set deploy to false to avoid deploying an obsolete
			// standalone controller with incompatible legacy CLI flags on the seed cluster.
			"dnsControllerManager": map[string]interface{}{
				"deploy":     !useNextGen,
				"createCRDs": true,
				"image": map[string]interface{}{
					"repository": dnsControllerManagerRepository,
					"tag":        externalDNSArtifactsVersion,
					"pullPolicy": "Always",
				},
			},
			"imageVectorOverwrite": `
images:
- name: ` + imageNameOverwrite + `
  repository: ` + dnsControllerManagerRepository + `
  tag: ` + externalDNSArtifactsVersion,
		},
	}
}

// encode is a helper function to marshal a map into a JSON byte slice.
func encode(obj map[string]interface{}, t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("failed to encode object: %v", err)
	}
	return data
}

// splitCIDR takes an IPv4 CIDR string and splits it into three smaller,
// non-overlapping CIDR blocks. Each resulting block will have at least 4 IPs.
// The input CIDR must have a prefix of /28 or smaller to succeed.
// The three subnets are half,quarter,quarter of the original subnet
// Example: 10.201.2.0/28 --> [10.201.2.0/29, 10.201.2.8/30, 10.201.2.12/30]
func splitCIDR(cidr string) ([]string, error) {
	// Parse the CIDR
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid CIDR: %v", err)
	}

	// Validate that it's an IPv4 address.
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("IPv4 address required")
	}

	// Check if the CIDR is large enough to be split.
	// To get 3 subnets with at least 4 IPs each (/30), the original
	// must be a /28 or smaller (e.g., /27, /24).
	if prefix.Bits() > 28 {
		return nil, fmt.Errorf("CIDR prefix must be /30 or smaller, got /%d", prefix.Bits())
	}

	prefix = prefix.Masked()
	halfPrefixLen := prefix.Bits() + 1
	quarterPrefixLen := prefix.Bits() + 2

	// Calculate how many IPs are in the first (half) subnet
	halfSize := 1 << (32 - halfPrefixLen)

	// Calculate how many IPs are in the second/third (quarter) subnets
	quarterSize := 1 << (32 - quarterPrefixLen)

	subnetList := make([]string, 3)

	// The first half subnet
	addr1 := prefix.Addr()
	subnetList[0] = netip.PrefixFrom(addr1, halfPrefixLen).String()

	// Second Subnet: Find its start by looping through all IPs in the first subnet
	addr2 := addr1
	for i := 0; i < halfSize; i++ {
		addr2 = addr2.Next()
	}
	subnetList[1] = netip.PrefixFrom(addr2, quarterPrefixLen).String()

	// Third subnet: Find its start by looping through all IPs in the second subnet
	addr3 := addr2
	for i := 0; i < quarterSize; i++ {
		addr3 = addr3.Next()
	}
	subnetList[2] = netip.PrefixFrom(addr3, quarterPrefixLen).String()

	return subnetList, nil
}
