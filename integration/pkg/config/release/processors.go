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
	"log"
	"os"
	"os/exec"
	"strings"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	gdchnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/clientcmd"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdcloud"
)

var (
	globalSchema     = runtime.NewScheme()
	managementSchema = runtime.NewScheme()
)

func init() {
	utilruntime.Must(ipamglobalv1.AddToScheme(globalSchema))
	utilruntime.Must(gdchnetworkingv1.AddToScheme(globalSchema))
	utilruntime.Must(globalnetworkingv1.AddToScheme(globalSchema))

	utilruntime.Must(vmv1.AddToScheme(managementSchema))
	utilruntime.Must(gdchnetworkingv1.AddToScheme(managementSchema))
}

// findGardenClusterFromGKEClustersPool searches the provided GKE clusters for a Garden deployment
// that matches the given artifacts version annotation.
//
// Search order:
//  1. If a Garden cluster with the target artifacts version annotation exists, return a client for it.
//  2. If no matching cluster is found and allocateNewClusters is true, return the first Garden cluster without any annotations
//     along with the garden object to be annotated later.
//  3. If neither case applies, return an error.
//
// Returns:
//   - A VirtualGardenConfig for the selected Garden cluster.
//   - A client.WithWatch for the selected Runtime cluster.
//   - The Garden object to be annotated (if applicable).
//   - A boolean indicating if the Garden object needs to be annotated.
//   - An error if no suitable cluster could be found.
func findGardenClusterFromGKEClustersPool(ctx context.Context, runtimeClusters []config.RuntimeClusterConfig, gardenerArtifactsVersion string) (*config.VirtualGardenConfig, client.WithWatch, string, *operatorv1alpha1.Garden, bool, error) {
	if len(runtimeClusters) == 0 {
		return nil, nil, "", nil, false, fmt.Errorf("no user clusters found")
	}

	opts, ok := ctx.Value(optionsKey{}).(*LoadOptions)
	if !ok {
		return nil, nil, "", nil, false, fmt.Errorf("failed to get options from context")
	}
	allocateNewClusters := opts.AllocateNewClusters

	clusterClients, err := getGKEClusterClients(runtimeClusters)
	if err != nil {
		return nil, nil, "", nil, false, fmt.Errorf("failed to get gke cluster clients: %w", err)
	}

	// Keep track of the first cluster found that has no annotations.
	var unannotatedClusterClient client.WithWatch
	var unannotatedGarden *operatorv1alpha1.Garden
	var unannotatedClusterName string

	for clusterName, clusterClient := range clusterClients {
		gardenList := &operatorv1alpha1.GardenList{}
		if err := clusterClient.List(ctx, gardenList); err != nil && !meta.IsNoMatchError(err) {
			return nil, nil, "", nil, false, fmt.Errorf("failed to list Garden resources in cluster %q: %w\n", clusterName, err)
		}

		if len(gardenList.Items) == 0 {
			if opts.AllowMissingGarden {
				if unannotatedClusterClient == nil {
					unannotatedClusterClient = clusterClient
					unannotatedClusterName = clusterName
				}
				continue
			}
			return nil, nil, "", nil, false, fmt.Errorf("The cluster %q does not have a Garden cluster deployment\n", clusterName)
		}

		garden := gardenList.Items[0]
		annotations := garden.GetAnnotations()

		if foundCommitHash, ok := annotations[CommitHashAnnotation]; ok {
			if foundCommitHash != gardenerArtifactsVersion {
				continue
			}
			// The garden CR with the matching artifacts version annotation is found, return its garden client
			virtualGardenConfig, err := config.GetGardenClient(ctx, clusterClient)
			if err != nil {
				if opts.AllowMissingGarden {
					log.Printf("Warning: failed to get garden client from runtime cluster %s: %v. Proceeding with nil VirtualGardenConfig as AllowMissingGarden is enabled.", clusterName, err)
					return nil, clusterClient, clusterName, nil, false, nil
				}
				return nil, nil, "", nil, false, fmt.Errorf("failed to get garden client from runtime cluster %s: %w", clusterName, err)
			}
			return virtualGardenConfig, clusterClient, clusterName, nil, false, nil
		}
		if unannotatedClusterClient == nil {
			unannotatedClusterClient = clusterClient
			unannotatedGarden = &garden
			unannotatedClusterName = clusterName
		}
	}

	// no garden CR with the matching artifacts version annotation was found,
	// return the unannotated Garden CR and its garden client
	if unannotatedClusterClient != nil && allocateNewClusters {
		log.Printf("No garden cluster found with artifacts version '%s'. Found idle cluster '%s'...", gardenerArtifactsVersion, unannotatedClusterName)

		virtualGardenConfig, err := config.GetGardenClient(ctx, unannotatedClusterClient)
		if err != nil {
			return nil, nil, "", nil, false, fmt.Errorf("failed to get garden client from idle cluster %s: %w", unannotatedClusterName, err)
		}
		return virtualGardenConfig, unannotatedClusterClient, unannotatedClusterName, unannotatedGarden, true, nil
	}

	if opts.AllowMissingGarden {
		log.Printf("Warning: no idle or matching Garden cluster found for artifacts version '%s'. Proceeding with nil VirtualGardenConfig as AllowMissingGarden is enabled.", gardenerArtifactsVersion)
		return nil, unannotatedClusterClient, unannotatedClusterName, nil, false, nil
	}

	// No runtime cluster with the matching artifacts version was found.
	return nil, nil, "", nil, false, fmt.Errorf("no idle or matching Garden cluster found for artifacts version '%s'", gardenerArtifactsVersion)
}

// annotateResource annotates the given object with the artifacts version.
func annotateResource(ctx context.Context, c client.Client, obj client.Object, resourceType, gardenerArtifactsVersion string) error {
	name := obj.GetName()
	if ns := obj.GetNamespace(); ns != "" {
		name = fmt.Sprintf("%s/%s", ns, name)
	}

	log.Printf("Annotating %s '%s' with artifacts version '%s'...", resourceType, name, gardenerArtifactsVersion)
	ann := obj.GetAnnotations()
	if ann == nil {
		ann = make(map[string]string)
	}
	ann[CommitHashAnnotation] = gardenerArtifactsVersion
	obj.SetAnnotations(ann)

	if err := c.Update(ctx, obj); err != nil {
		return fmt.Errorf("failed to annotate %s %s: %w", resourceType, name, err)
	}
	log.Printf("Successfully annotated %s '%s'", resourceType, name)
	return nil
}

// findShootClusterToHostSeed searches for a Shoot cluster to be used as a Seed cluster.
// It iterates through all configured GDC user clusters, attempting to find a Garden
// cluster in each one. For each Garden found, it calls findShootInGarden to find
// a suitable Shoot.
//
// Returns:
//   - A SeedConfig for the selected Shoot cluster to be used as a Seed.
//   - The Shoot object to be annotated (if applicable).
//   - The garden client needed to annotate the Shoot.
//   - A boolean indicating if the Shoot object needs to be annotated.
//   - An error if no suitable Shoot cluster could be found or if the process fails.
func findShootClusterToHostSeed(ctx context.Context, userClusters []config.RuntimeClusterConfig, gardenerArtifactsVersion string, gdcClient *gdcloud.TestingClient) (*config.SeedConfig, *gardencorev1beta1.Shoot, client.WithWatch, bool, error) {
	userClusterClients, err := config.GetGDCUserClusterClients(userClusters, gdcClient)
	if err != nil {
		return nil, nil, nil, false, fmt.Errorf("failed to get user cluster clients: %w", err)
	}

	for userClusterName, userClusterClient := range userClusterClients {
		virtualGardenConfig, err := config.GetGardenClient(ctx, userClusterClient)
		if err != nil {
			log.Printf("INFO: could not get garden client from runtime cluster %q, skipping: %v", userClusterName, err)
			continue
		}

		targetShoot, needsAnnotation, err := findShootInGarden(ctx, virtualGardenConfig.Client, gardenerArtifactsVersion, userClusterName)
		if err != nil {
			log.Printf("WARN: failed to select a shoot from garden in cluster %q: %v", userClusterName, err)
			continue
		}

		if targetShoot != nil {
			// Found our shoot, create a client for it and return.
			networkConfig, err := getShootNetworkConfig(ctx, virtualGardenConfig.Client, targetShoot)
			if err != nil {
				log.Printf("WARN: %v", err)
				continue
			}

			log.Printf("Successfully selected shoot '%s/%s' from garden in cluster '%s' to be used as seed cluster.", targetShoot.Namespace, targetShoot.Name, userClusterName)
			seedConfig := &config.SeedConfig{
				Name: "seed-" + GetCommitHashOrSanitize(gardenerArtifactsVersion),
				HostCluster: &config.HostCluster{
					Shoot: &config.ShootConfig{
						Name:          targetShoot.Name,
						Namespace:     targetShoot.Namespace,
						VirtualGarden: virtualGardenConfig,
					},
				},
				Network: networkConfig,
			}
			return seedConfig, targetShoot, virtualGardenConfig.Client, needsAnnotation, nil
		}
	}

	return nil, nil, nil, false, fmt.Errorf("no suitable shoot cluster found in any of the garden clusters for artifacts version '%s'", gardenerArtifactsVersion)
}

// getShootNetworkConfig retrieves the networking configuration (pods and services CIDRs)
// for a given Shoot cluster. It fetches the full Shoot object to access its spec.
func getShootNetworkConfig(ctx context.Context, gardenClient client.Client, shoot *gardencorev1beta1.Shoot) (*config.NetworkConfiguration, error) {
	fullShoot := &gardencorev1beta1.Shoot{}
	if err := gardenClient.Get(ctx, client.ObjectKeyFromObject(shoot), fullShoot); err != nil {
		return nil, fmt.Errorf("failed to get full shoot object for %s/%s: %w", shoot.Namespace, shoot.Name, err)
	}

	if fullShoot.Spec.Networking == nil || fullShoot.Spec.Networking.Pods == nil || fullShoot.Spec.Networking.Services == nil {
		return nil, fmt.Errorf("shoot %s/%s does not have networking CIDRs", shoot.Namespace, shoot.Name)
	}

	return &config.NetworkConfiguration{
		PodsCIDR:     *fullShoot.Spec.Networking.Pods,
		ServicesCIDR: *fullShoot.Spec.Networking.Services,
	}, nil
}

// findShootInGarden attempts to find a suitable shoot in a given garden to be used as a seed.
// The selection logic is as follows:
//  1. It first looks for a Shoot cluster that has the "integration.release.sapbtp.gardener/commit-hash"
//     annotation with a value matching the provided gardenerArtifactsVersion.
//  2. If no such Shoot is found and allocateNewClusters is true, it looks for the first Shoot cluster that does not have this annotation.
//
// It returns the selected shoot and a boolean indicating if it needs annotation.
// An error is returned if listing shoots fails.
func findShootInGarden(ctx context.Context, gardenClient client.WithWatch, gardenerArtifactsVersion, userClusterName string) (*gardencorev1beta1.Shoot, bool, error) {
	opts, ok := ctx.Value(optionsKey{}).(*LoadOptions)
	if !ok {
		return nil, false, fmt.Errorf("failed to get options from context")
	}
	allocateNewClusters := opts.AllocateNewClusters

	shootList := &gardencorev1beta1.ShootList{}
	if err := gardenClient.List(ctx, shootList); err != nil {
		return nil, false, fmt.Errorf("failed to list shoots for garden from cluster %q: %w", userClusterName, err)
	}

	if len(shootList.Items) == 0 {
		log.Printf("INFO: no shoot clusters found in garden from cluster %q", userClusterName)
		return nil, false, nil // Not an error, just no shoots here
	}

	var unannotatedShoot *gardencorev1beta1.Shoot
	var shootWithMatchingHash *gardencorev1beta1.Shoot

	for i := range shootList.Items {
		shoot := &shootList.Items[i]
		annotations := shoot.GetAnnotations()
		if foundCommitHash, ok := annotations[CommitHashAnnotation]; ok {
			if foundCommitHash == gardenerArtifactsVersion {
				shootWithMatchingHash = shoot
				break
			}
		} else if unannotatedShoot == nil {
			unannotatedShoot = shoot
		}
	}

	if shootWithMatchingHash != nil {
		log.Printf("Found shoot '%s/%s' with matching artifacts version '%s' in garden from cluster %s", shootWithMatchingHash.Namespace, shootWithMatchingHash.Name, gardenerArtifactsVersion, userClusterName)
		return shootWithMatchingHash, false, nil
	}

	if unannotatedShoot != nil && allocateNewClusters {
		log.Printf("No shoot found with artifacts version '%s' in garden from cluster %s. Found idle shoot '%s/%s'...", gardenerArtifactsVersion, userClusterName, unannotatedShoot.Namespace, unannotatedShoot.Name)
		return unannotatedShoot, true, nil
	}

	return nil, false, nil // No suitable shoot found in this garden
}

var gdcloudInit = func(ctx context.Context, pipelineCfg *pipelineConfig) (*gdcloud.TestingClient, error) {
	return config.GetGDCClient(ctx, pipelineCfg.GDC.CAData, pipelineCfg.GDC.ConsoleURL)
}

// getGKEClusterClients creates and returns clients for the specified GKE user clusters.
// This function is a variable to allow for mocking in unit tests.
var getGKEClusterClients = func(userClusters []config.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
	userClusterSchema := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(userClusterSchema); err != nil {
		return nil, fmt.Errorf("failed to register operatorv1alpha1 scheme: %w", err)
	}
	if err := corev1.AddToScheme(userClusterSchema); err != nil {
		return nil, fmt.Errorf("failed to register corev1 scheme: %w", err)
	}
	if err := config.RegisterGDCSchemes(userClusterSchema); err != nil {
		return nil, fmt.Errorf("failed to register GDC schemes: %w", err)
	}
	userClusterClients := make(map[string]client.WithWatch)
	for _, userCluster := range userClusters {
		userClusterClient, err := getGKEUserClusterClient(userCluster.Zone, userCluster.Project, userCluster.Name, userClusterSchema)
		if err != nil {
			return nil, fmt.Errorf("unable to get client for user cluster %s; %w\n", userCluster.Name, err)
		}
		userClusterClients[userCluster.Name] = userClusterClient
	}
	return userClusterClients, nil
}

// getGKEUserClusterClient fetches the credentials for a GKE cluster,
// builds a Kubernetes client config, and creates a new client with that config.
func getGKEUserClusterClient(zone, project, name string, schema *runtime.Scheme) (client.WithWatch, error) {
	kubeconfigPath := fmt.Sprintf("/tmp/%s-kubeconfig", name)
	cmd := exec.Command("gcloud", "container", "clusters", "get-credentials", name, "--region", zone, "--project", project)
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", kubeconfigPath))
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("failed to get gke cluster credentials for %s: %s, %w", name, string(output), err)
	}

	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("failed to build config from kubeconfig %s: %w", kubeconfigPath, err)
	}

	c, err := client.NewWithWatch(cfg, client.Options{Scheme: schema})
	if err != nil {
		return nil, fmt.Errorf("failed to create client from config: %w", err)
	}
	return c, nil
}

// GetCommitHashOrSanitize returns the hash from the version string or the sanitized version string if no hash is present.
func GetCommitHashOrSanitize(version string) string {
	// Check for the last hyphen to extract hash
	if i := strings.LastIndex(version, "-"); i >= 0 {
		return version[i+1:]
	}

	// Fallback: If no hyphen, replace dots with hyphens
	return strings.ReplaceAll(version, ".", "-")
}

// findGardenClusterFromGDCUserClustersPool searches the provided GDC User clusters for a Garden deployment
// that matches the given artifacts version annotation.
func findGardenClusterFromGDCUserClustersPool(ctx context.Context, runtimeClusters []config.RuntimeClusterConfig, gardenerArtifactsVersion string, gdcClient *gdcloud.TestingClient) (*config.VirtualGardenConfig, client.WithWatch, string, *operatorv1alpha1.Garden, bool, error) {
	if len(runtimeClusters) == 0 {
		return nil, nil, "", nil, false, fmt.Errorf("no user clusters found")
	}

	opts, ok := ctx.Value(optionsKey{}).(*LoadOptions)
	if !ok {
		return nil, nil, "", nil, false, fmt.Errorf("failed to get options from context")
	}
	allocateNewClusters := opts.AllocateNewClusters

	userClusterClients, err := config.GetGDCUserClusterClients(runtimeClusters, gdcClient)
	if err != nil {
		return nil, nil, "", nil, false, fmt.Errorf("failed to get user cluster clients: %w", err)
	}

	// Keep track of the first cluster found that has no annotations.
	var unannotatedClusterClient client.WithWatch
	var unannotatedGarden *operatorv1alpha1.Garden
	var unannotatedClusterName string

	for clusterName, clusterClient := range userClusterClients {
		gardenList := &operatorv1alpha1.GardenList{}
		if err := clusterClient.List(ctx, gardenList); err != nil && !meta.IsNoMatchError(err) {
			return nil, nil, "", nil, false, fmt.Errorf("failed to list Garden resources in cluster %q: %w\n", clusterName, err)
		}

		if len(gardenList.Items) == 0 {
			if (allocateNewClusters && opts.OnlyGarden) || opts.AllowMissingGarden {
				if unannotatedClusterClient == nil {
					unannotatedClusterClient = clusterClient
					unannotatedClusterName = clusterName
				}
				continue
			}
			return nil, nil, "", nil, false, fmt.Errorf("The cluster %q does not have a Garden cluster deployment\n", clusterName)
		}

		garden := gardenList.Items[0]
		annotations := garden.GetAnnotations()

		if foundCommitHash, ok := annotations[CommitHashAnnotation]; ok {
			if foundCommitHash != gardenerArtifactsVersion {
				continue
			}
			// The garden CR with the matching artifacts version annotation is found, return its garden client
			virtualGardenConfig, err := config.GetGardenClient(ctx, clusterClient)
			if err != nil {
				if opts.AllowMissingGarden {
					log.Printf("Warning: failed to get garden client from runtime cluster %s: %v. Proceeding with nil VirtualGardenConfig as AllowMissingGarden is enabled.", clusterName, err)
					return nil, clusterClient, clusterName, nil, false, nil
				}
				return nil, nil, "", nil, false, fmt.Errorf("failed to get garden client from runtime cluster %s: %w", clusterName, err)
			}
			return virtualGardenConfig, clusterClient, clusterName, nil, false, nil
		}
		if unannotatedClusterClient == nil {
			unannotatedClusterClient = clusterClient
			unannotatedGarden = &garden
			unannotatedClusterName = clusterName
		}
	}

	// no garden CR with the matching artifacts version annotation was found,
	// return the unannotated Garden CR and its garden client
	if unannotatedClusterClient != nil && allocateNewClusters {
		log.Printf("No garden cluster found with artifacts version '%s'. Found idle cluster '%s'...", gardenerArtifactsVersion, unannotatedClusterName)

		var virtualGardenConfig *config.VirtualGardenConfig
		if !opts.OnlyGarden && unannotatedGarden != nil {
			var err error
			virtualGardenConfig, err = config.GetGardenClient(ctx, unannotatedClusterClient)
			if err != nil {
				return nil, nil, "", nil, false, fmt.Errorf("failed to get garden client from idle cluster %s: %w", unannotatedClusterName, err)
			}
		}
		return virtualGardenConfig, unannotatedClusterClient, unannotatedClusterName, unannotatedGarden, true, nil
	}

	if opts.AllowMissingGarden {
		log.Printf("Warning: no idle or matching Garden cluster found for artifacts version '%s'. Proceeding with nil VirtualGardenConfig as AllowMissingGarden is enabled.", gardenerArtifactsVersion)
		return nil, unannotatedClusterClient, unannotatedClusterName, nil, false, nil
	}

	// No runtime cluster with the matching artifacts version was found.
	return nil, nil, "", nil, false, fmt.Errorf("no idle or matching Garden cluster found for artifacts version '%s'", gardenerArtifactsVersion)
}
