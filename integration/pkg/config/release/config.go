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
	"context"
	"fmt"
	"os"
	"strings"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdcloud"
)

// LoadOptions contains configuration options for loading the release test config.
type LoadOptions struct {
	AllocateNewClusters   bool
	VirtualGardenProvider string
	OnlyGarden            bool
	AllowMissingGarden    bool
}

// Option is a function that configures LoadOptions.
type Option func(*LoadOptions)

// WithClusterAllocation returns an Option that enables the allocation of new clusters.
func WithClusterAllocation() Option {
	return func(o *LoadOptions) {
		o.AllocateNewClusters = true
	}
}

// WithVirtualGardenProvider returns an Option that configures the Virtual Garden provider.
func WithVirtualGardenProvider(provider string) Option {
	return func(o *LoadOptions) {
		o.VirtualGardenProvider = provider
	}
}

// WithOnlyGarden returns an Option that skips seed/shoot matching for garden-only provisioning.
func WithOnlyGarden() Option {
	return func(o *LoadOptions) {
		o.OnlyGarden = true
	}
}

// WithAllowMissingGarden returns an Option that allows loading the config even if no Garden cluster is found.
func WithAllowMissingGarden() Option {
	return func(o *LoadOptions) {
		o.AllowMissingGarden = true
	}
}

// optionsKey is the key used to store LoadOptions in context.Context.
type optionsKey struct{}

// LoadReleaseTestConfig orchestrates the loading and processing of the integration test configuration.
// It performs the following steps:
// 1. Reads and unmarshals the pipeline configuration from the provided YAML file.
// 2. Initializes the GDC environment by fetching CA data (if necessary) and calling gdcloudInit.
// 3. Selects a Garden cluster to be used for the test run.
// 4. Selects a Shoot cluster to be used as a Seed, including its network configuration.
// 5. Creates a global API client.
// 6. Fetches harbor pull credentials.
// Finally, it assembles and returns a fully initialized ReleaseTestConfig struct.
func LoadReleaseTestConfig(releaseConfigurationFilePath, gardenerArtifactsVersion string, opts ...Option) (*config.ReleaseTestConfig, error) {
	options := &LoadOptions{}
	for _, o := range opts {
		o(options)
	}

	ctx := context.WithValue(context.Background(), optionsKey{}, options)

	pipelineCfg, err := loadPipelineConfig(releaseConfigurationFilePath)
	if err != nil {
		return nil, err
	}

	gdcClient, err := prepareGDC(ctx, pipelineCfg)
	if err != nil {
		return nil, err
	}

	var (
		virtualGardenConfig   *config.VirtualGardenConfig
		runtimeClient         client.WithWatch
		runtimeClusterName    string
		gardenCandidate       *operatorv1alpha1.Garden
		gardenNeedsAnnotation bool
	)
	if options.VirtualGardenProvider == "gdc" {
		virtualGardenConfig, runtimeClient, runtimeClusterName, gardenCandidate, gardenNeedsAnnotation, err = findGardenClusterFromGDCUserClustersPool(ctx, pipelineCfg.GDC.UserClusters, gardenerArtifactsVersion, gdcClient)
	} else {
		virtualGardenConfig, runtimeClient, runtimeClusterName, gardenCandidate, gardenNeedsAnnotation, err = findGardenClusterFromGKEClustersPool(ctx, pipelineCfg.GCP.GKEClusters, gardenerArtifactsVersion)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get matching garden cluster: %w", err)
	}

	var seedConfig *config.SeedConfig
	if !options.OnlyGarden {
		if pipelineCfg.GDC.UseUserClusterAsSeed && options.VirtualGardenProvider == "gdc" {
			podCIDR, svcCIDR := ResolveClusterCIDRs(ctx, runtimeClient)
			seedConfig = &config.SeedConfig{
				Name: "seed-" + GetCommitHashOrSanitize(gardenerArtifactsVersion),
				HostCluster: &config.HostCluster{
					UserClusterName:   runtimeClusterName,
					UserClusterClient: runtimeClient,
				},
				Network: &config.NetworkConfiguration{
					PodsCIDR:     podCIDR,
					ServicesCIDR: svcCIDR,
				},
			}
		} else {
			var shootCandidate *gardencorev1beta1.Shoot
			var shootGardenClient client.WithWatch
			var shootNeedsAnnotation bool
			seedConfig, shootCandidate, shootGardenClient, shootNeedsAnnotation, err = findShootClusterToHostSeed(ctx, pipelineCfg.GDC.UserClusters, gardenerArtifactsVersion, gdcClient)
			if err != nil {
				return nil, fmt.Errorf("failed to find matching seed cluster: %w", err)
			} else if shootNeedsAnnotation {
				if err := annotateResource(ctx, shootGardenClient, shootCandidate, "shoot", gardenerArtifactsVersion); err != nil {
					return nil, err
				}
			}
		}
	}

	// Proceed to annotate garden if necessary
	if gardenNeedsAnnotation && gardenCandidate != nil {
		if err := annotateResource(ctx, runtimeClient, gardenCandidate, "garden cluster", gardenerArtifactsVersion); err != nil {
			return nil, err
		}
	}

	globalClient, err := config.GetGlobalClient(gdcClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get global client: %w", err)
	}

	managementClients := make(map[string]client.WithWatch)
	for _, zone := range pipelineCfg.GDC.Zones {
		c, err := config.GetManagementClient(gdcClient, pipelineCfg.GDC.Org, zone)
		if err != nil {
			return nil, fmt.Errorf("failed to get management client for zone %s: %w", zone, err)
		}
		managementClients[zone] = c
	}

	pullCredentials, err := config.AccessLatestSecret(ctx, "harbor-docker-config")
	if err != nil {
		return nil, fmt.Errorf("failed to fetch harbor docker config secret: %w", err)
	}

	releaseTestConfig := &config.ReleaseTestConfig{
		GDCClient: gdcClient,
		TestShoot: &config.ShootConfig{
			Name:          fmt.Sprintf("sh-%s", GetCommitHashOrSanitize(gardenerArtifactsVersion)),
			Namespace:     gardenv1beta1constants.GardenNamespace,
			VirtualGarden: virtualGardenConfig,
		},
		Seed:                 seedConfig,
		RuntimeClusterClient: runtimeClient,
		RuntimeClusterName:   runtimeClusterName,
		ImagePullCredentials: pullCredentials,
		GlobalAPIClient:      globalClient,
		ManagementClients:    managementClients,
		GCP:                  &pipelineCfg.GCP,
		GDC:                  &pipelineCfg.GDC,
		Gardener:             &pipelineCfg.Gardener,
	}

	return releaseTestConfig, nil
}

// loadPipelineConfig reads a YAML file from the given path and unmarshals it
// into a pipelineConfig struct.
func loadPipelineConfig(releaseConfigurationFilePath string) (*pipelineConfig, error) {
	yamlFile, err := os.ReadFile(releaseConfigurationFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file at %s: %w", releaseConfigurationFilePath, err)
	}

	pipelineCfg := &pipelineConfig{}
	if err := yaml.Unmarshal(yamlFile, pipelineCfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML content: %w", err)
	}
	return pipelineCfg, nil
}

// prepareGDC ensures the GDC environment is ready. It fetches the GDC CA data if it's
// not already present in the configuration and then initializes the gdcloud environment.
func prepareGDC(ctx context.Context, pipelineCfg *pipelineConfig) (*gdcloud.TestingClient, error) {
	if pipelineCfg.GDC.CAData == "" {
		caData, err := config.FetchGDCCAData(pipelineCfg.GDC.ConsoleURL)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch CA data: %w", err)
		}
		pipelineCfg.GDC.CAData = caData
	}

	gdcClient, err := gdcloudInit(ctx, pipelineCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize gdcloud: %w", err)
	}
	return gdcClient, nil
}

func ResolveClusterCIDRs(ctx context.Context, runtimeClient client.Client) (string, string) {
	podCIDR := "10.244.0.0/16" // fallback default
	svcCIDR := "10.96.0.0/12"  // fallback default

	if runtimeClient == nil {
		return podCIDR, svcCIDR
	}

	// 1. Try to parse from kubeadm-config ConfigMap in kube-system
	cm := &corev1.ConfigMap{}
	if err := runtimeClient.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "kubeadm-config"}, cm); err == nil {
		if clusterConfiguration, ok := cm.Data["ClusterConfiguration"]; ok {
			lines := strings.Split(clusterConfiguration, "\n")
			for _, line := range lines {
				if strings.Contains(line, "podSubnet:") {
					parts := strings.Split(line, ":")
					if len(parts) == 2 {
						podCIDR = strings.TrimSpace(parts[1])
					}
				}
				if strings.Contains(line, "serviceSubnet:") {
					parts := strings.Split(line, ":")
					if len(parts) == 2 {
						svcCIDR = strings.TrimSpace(parts[1])
					}
				}
			}
			return podCIDR, svcCIDR
		}
	}

	// 2. Fallback: try to read first Node's spec
	nodeList := &corev1.NodeList{}
	if err := runtimeClient.List(ctx, nodeList); err == nil && len(nodeList.Items) > 0 {
		nodePodCIDR := nodeList.Items[0].Spec.PodCIDR
		if nodePodCIDR == "" && len(nodeList.Items[0].Spec.PodCIDRs) > 0 {
			nodePodCIDR = nodeList.Items[0].Spec.PodCIDRs[0]
		}
		if nodePodCIDR != "" {
			podCIDR = DeriveParentCIDR(nodePodCIDR)
		}
	}

	return podCIDR, svcCIDR
}

func DeriveParentCIDR(cidr string) string {
	ipParts := strings.Split(cidr, "/")
	if len(ipParts) != 2 {
		return cidr
	}
	ip := ipParts[0]
	mask := ipParts[1]
	if mask != "24" {
		return cidr
	}
	octets := strings.Split(ip, ".")
	if len(octets) != 4 {
		return cidr
	}
	var thirdOctet int
	fmt.Sscanf(octets[2], "%d", &thirdOctet)
	parentThirdOctet := thirdOctet & 0xF0
	return fmt.Sprintf("%s.%s.%d.0/20", octets[0], octets[1], parentThirdOctet)
}
