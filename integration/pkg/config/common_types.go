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

package config

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdcloud"
)

// GCPConfig holds the configurations for GCP resources
type GCPConfig struct {
	// GKEClusters is a list of GKE clusters available for the pipeline.
	GKEClusters []RuntimeClusterConfig `yaml:"gkeClusters"`
}

// GDCConfig holds the configurations for GDC air-gapped resources
type GDCConfig struct {
	// Project is the primary GDC project where the Gardener deployment
	// and core infrastructure reside.
	Project string `yaml:"project"`

	// Org is the name of GDC organization.
	Org string `yaml:"org"`

	// Region is the name of GDC region.
	Region string `yaml:"region"`

	// Zones is a list of GDC zones.
	// This list is used to configure the availability zones for the Multi-Zone shoot cluster created by the pipeline.
	Zones []string `yaml:"zones"`

	// UserClusters is a list of GDC user clusters available for the pipeline.
	// These userClusters must host Gardener landscapes whose Shoot clusters will be used to host the seed cluster created by the pipeline.
	UserClusters []RuntimeClusterConfig `yaml:"userClusters"`

	// GlobalAPIURL is the URL of the GDC Global API.
	// i.e
	// - "https://global-api.gdc1.us-west16-c.s.gpcdemolabs.com" in codev Lab
	// - "https://global-api.gdc1.us-west6-c.staging.gpcdemolabs.com" in staging Lab
	GlobalAPIURL string `yaml:"globalAPIURL"`

	// ManagementAPIURL is the URL of the GDC Zonal Management API.
	// This is the zone that hosts the zonal backup bucket.
	// i.e
	// - "https://management-kube.apiserver.gdc1.us-west16-c.s.gpcdemolabs.com" in codev Lab
	// - "https://management-kube.apiserver.gdc1.us-west6-c.staging.gpcdemolabs.com" in staging Lab
	ManagementAPIURL string `yaml:"managementAPIURL"`

	// ConsoleURL is the URL of the GDC Console. Used for GDC authentication.
	// Both Global and Zonal Console URLs are acceptable.
	// i.e
	// - "https://console.gdc1.us-west16-c.s.gpcdemolabs.com" in codev Lab
	// - "https://console.gdc1.us-west6-c.staging.gpcdemolabs.com" in staging Lab
	ConsoleURL string `yaml:"consoleURL"`

	// HarborRegistryURL is the full URL of the Harbor registry.
	// It should be in the format $HARBOR_URL/$HARBOR_PROJECT.
	// i.e
	// - "harbor-1-google-garden.gdc1.us-west16-c.s.gpcdemolabs.com/sap-gardener"
	HarborRegistryURL string `yaml:"harborRegistryURL"`

	// LabDomainURL is the URL of the Lab domain.
	// i.e.
	// - "s.gpcdemolabs.com" in codev Lab
	// - "staging.gpcdemolabs.com" in staging Lab
	LabDomainURL string `yaml:"labDomainURL"`

	// ManagedDNSDomainName is the domain name of managed DNS zone in the specified GDC Project.
	ManagedDNSDomainName string `yaml:"managedDNSDomainName"`

	// CAData is the base64 encoded CA data for GDC.
	CAData string `yaml:"caData"`

	// ParentPrivateSubnetGroup is the parent subnet group that hosts the subnets for shoot VMs created by the pipeline.
	ParentPrivateSubnetGroup string `yaml:"parentPrivateSubnetGroup"`

	// ParentPublicSubnetGroup is the parent subnet group that hosts the public subnets for load balancers.
	ParentPublicSubnetGroup string `yaml:"parentPublicSubnetGroup"`

	// DualZoneBucketLocation is the location for dual-zone buckets in GDC.
	DualZoneBucketLocation string `yaml:"dualZoneBucketLocation"`

	// SecondaryProject is a secondary GDC project used to host VMs for
	// validating cross-project access and connectivity.
	SecondaryProject string `yaml:"secondaryProject"`

	// UseUserClusterAsSeed configures using the GDC Standard User Cluster as the host
	// for the Seed cluster instead of a remote Shoot.
	UseUserClusterAsSeed bool `yaml:"useUserClusterAsSeed"`
}

// RuntimeClusterConfig holds the configuration of a single GKE cluster
type RuntimeClusterConfig struct {
	// Name is the name of the cluster.
	// This can be GKE cluster name or GDC user cluster name.
	Name string `yaml:"name"`

	// Zone is the zone where the cluster is located.
	// This can be GKE zone or GDC zone.
	Zone string `yaml:"zone"`

	// Project is the project where the cluster is located.
	// This can be GKE project or GDC project.
	Project string `yaml:"project"`
}

// VirtualGardenConfig contains Garden cluster specific configuration.
type VirtualGardenConfig struct {
	// Client is the Kubernetes client for the garden cluster.
	Client client.WithWatch
	// CAData is the CA data of the garden cluster.
	CAData []byte
	// Host is the host of the garden cluster.
	Host string
}

// SeedConfig contains Seed cluster specific configuration.
type SeedConfig struct {
	// Name is the name of the seed cluster.
	Name string

	// HostCluster holds the configuration of the cluster that hosts the Seed cluster.
	// This can be a Shoot cluster or GDC user cluster.
	HostCluster *HostCluster

	// Network holds the network configuration of the Seed cluster.
	Network *NetworkConfiguration
}

// HostCluster contains the configuration of the host cluster.
type HostCluster struct {
	// If the host cluster is a Shoot cluster, this field must be set.
	Shoot *ShootConfig

	// UserClusterName is the name of the GDC user cluster hosting the seed directly.
	UserClusterName string
	// UserClusterClient is the client for the GDC user cluster hosting the seed directly.
	UserClusterClient client.WithWatch
}

// ReleaseTestConfig holds the configuration needed by a single release test
type ReleaseTestConfig struct {
	// GDCClient holds the testing client.
	GDCClient *gdcloud.TestingClient

	// TestShoot is the configuration for the shoot cluster used in the release test.
	TestShoot *ShootConfig

	// Clients
	RuntimeClusterClient client.WithWatch
	RuntimeClusterName   string
	GlobalAPIClient      client.WithWatch
	ManagementClients    map[string]client.WithWatch

	// Test Resources
	ImagePullCredentials []byte

	// Infrastructure Configuration
	GCP *GCPConfig
	GDC *GDCConfig

	// Remote Seed Configuration
	Seed *SeedConfig

	// Gardener holds Gardener specific configurations.
	Gardener *GardenerConfig
}

// GardenerConfig holds gardener specific configurations
type GardenerConfig struct {
	// GardenerVersion is the version of Gardener to use.
	GardenerVersion string `yaml:"gardenerVersion"`

	// OperatorChartRepository is the OCI registry or local path for gardener-operator Helm chart.
	OperatorChartRepository string `yaml:"operatorChartRepository"`

	// KubernetesVersion is the kubernetes version to use for seed and shoots clusters.
	KubernetesVersion string `yaml:"kubernetesVersion"`

	// CiliumExtension is the configuration for the Cilium extension.
	CiliumExtension *ExtensionConfig `yaml:"ciliumExtension"`

	// GardenlinuxExtension is the configuration for the Gardenlinux extension.
	GardenlinuxExtension *ExtensionConfig `yaml:"gardenlinuxExtension"`

	// ExternalDNSExtension is the configuration for the ExternalDNS extension.
	ExternalDNSExtension *ExtensionConfig `yaml:"externalDNSExtension"`

	// CSIImages holds the configuration for the CSI images.
	CSIImages *CSIImagesConfig `yaml:"csiImages"`
}

// CSIImagesConfig holds the configuration for the CSI images.
type CSIImagesConfig struct {
	CSIDriver              *ImageConfig `yaml:"csiDriver"`
	CSIProvisioner         *ImageConfig `yaml:"csiProvisioner"`
	CSIAttacher            *ImageConfig `yaml:"csiAttacher"`
	CSILivenessProbe       *ImageConfig `yaml:"csiLivenessProbe"`
	CSINodeDriverRegistrar *ImageConfig `yaml:"csiNodeDriverRegistrar"`
	CSISnapshotter         *ImageConfig `yaml:"csiSnapshotter"`
	CSISnapshotController  *ImageConfig `yaml:"csiSnapshotController"`
	CSIResizer             *ImageConfig `yaml:"csiResizer"`
}

// ImageConfig holds the repository and tag for a container image.
type ImageConfig struct {
	Repository string `yaml:"repository"`
	Tag        string `yaml:"tag"`
}

// ExtensionConfig defines the standard configuration for a Gardener extension.
type ExtensionConfig struct {
	// HelmChartRef is the reference to the Helm chart for the extension.
	HelmChartRef string `yaml:"helmChartRef"`

	// Version is the version of the extension.
	Version string `yaml:"version"`
}

// NetworkConfiguration holds the networking CIDRs for a cluster
type NetworkConfiguration struct {
	PodsCIDR     string
	ServicesCIDR string
}

// ShootConfig contains Shoot cluster specific configuration.
type ShootConfig struct {
	// Name is the name of the shoot cluster.
	Name string

	// Namespace is the namespace of the shoot cluster.
	Namespace string

	// VirtualGarden holds the configuration of the virtual garden cluster that hosts the shoot cluster.
	VirtualGarden *VirtualGardenConfig
}
