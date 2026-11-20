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

package release

import (
	"context"
	"fmt"
	"strings"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdcloud"
)

// GardenClusterStatus holds the status of a Garden cluster.
// ActiveJobStatus holds consolidated information about an active release job.
type ActiveJobStatus struct {
	GardenerArtifactsVersion string // The artifacts version associated with the release job.
	VirtualGarden            string // The name of the Virtual Garden cluster.
	RuntimeCluster           string // The runtime cluster (GDC user cluster or GKE cluster) hosting the Virtual Garden.
	CreatedSeed              string // The Seed cluster(s) created for this job.
	SoilCluster              string // The Shoot cluster(s) in GDC acting as seed hosts (Soil).
	CreatedShoot             string // All Shoot clusters deployed on the Virtual Garden.
}

// AvailableClusterStatus represents a cluster that is idle and available for use.
type AvailableClusterStatus struct {
	Name     string // The name of the cluster.
	Type     string // The type of the cluster (e.g., GDC Virtual Garden Host, GKE Virtual Garden Host, Shoot Seed Host).
	HostedBy string // Information about where the cluster is hosted.
}

// CLIConfig holds the configuration for the gardener release CLI.
type CLIConfig struct {
	pipelineConfig *pipelineConfig
	gdcClient      *gdcloud.TestingClient
}

// LoadCLIConfig loads the pipeline configuration and prepares the GDC environment.
func LoadCLIConfig(ctx context.Context, releaseConfigurationFilePath string) (*CLIConfig, error) {
	pipelineCfg, err := loadPipelineConfig(releaseConfigurationFilePath)
	if err != nil {
		return nil, err
	}

	ctxWithOpts := context.WithValue(ctx, optionsKey{}, &LoadOptions{LocalGDCloudSessionFallback: true})
	gdcClient, err := prepareGDC(ctxWithOpts, pipelineCfg)
	if err != nil {
		return nil, err
	}

	return &CLIConfig{pipelineConfig: pipelineCfg, gdcClient: gdcClient}, nil
}

// Cleanup releases any temporary gdcloud configuration directory held by the CLI config.
func (c *CLIConfig) Cleanup() error {
	if c == nil || c.gdcClient == nil {
		return nil
	}
	return c.gdcClient.Cleanup()
}

// GetStatus returns the status of both active release jobs, claimed clusters, and available clusters.
func (c *CLIConfig) GetStatus(ctx context.Context) ([]ActiveJobStatus, []AvailableClusterStatus, error) {
	var activeJobs []ActiveJobStatus
	var availableClusters []AvailableClusterStatus
	jobsMap := make(map[string]*ActiveJobStatus)

	inspectVirtualGardenHosts := func(clusters []config.RuntimeClusterConfig, clients map[string]client.WithWatch, hostType string, isGKE bool) error {
		for _, rc := range clusters {
			clusterName := rc.Name
			clusterClient, ok := clients[clusterName]
			if !ok {
				return fmt.Errorf("client not found for cluster %q", clusterName)
			}

			gardenList := &operatorv1alpha1.GardenList{}
			if err := clusterClient.List(ctx, gardenList); err != nil && !meta.IsNoMatchError(err) {
				return fmt.Errorf("failed to list Garden resources in cluster %q: %w", clusterName, err)
			}

			for _, garden := range gardenList.Items {
				gardenerArtifactsVersion := garden.GetAnnotations()[CommitHashAnnotation]
				if gardenerArtifactsVersion == "" {
					availableClusters = append(availableClusters, AvailableClusterStatus{
						Name:     clusterName,
						Type:     hostType,
						HostedBy: c.getHostedBy(clusterName, isGKE),
					})
					continue
				}

				job, exists := jobsMap[gardenerArtifactsVersion]
				if !exists {
					job = &ActiveJobStatus{GardenerArtifactsVersion: gardenerArtifactsVersion}
					jobsMap[gardenerArtifactsVersion] = job
				}
				job.VirtualGarden = garden.Name
				job.RuntimeCluster = c.getHostedBy(clusterName, isGKE)

				if virtualGardenConfig, err := config.GetGardenClient(ctx, clusterClient); err == nil {
					populateCreatedSeedAndShootResources(ctx, virtualGardenConfig.Client, gardenerArtifactsVersion, job)
				}
			}
		}
		return nil
	}

	// Check GKE Virtual Garden host clusters when configured
	if len(c.pipelineConfig.GCP.GKEClusters) > 0 {
		gkeClients, err := getGKEClusterClients(c.pipelineConfig.GCP.GKEClusters)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get gke cluster clients: %w", err)
		}
		if err := inspectVirtualGardenHosts(c.pipelineConfig.GCP.GKEClusters, gkeClients, "GKE (Virtual Garden Host)", true); err != nil {
			return nil, nil, err
		}
	}

	// Check GDC Virtual Garden host clusters when configured
	gdcVGClusters := c.pipelineConfig.GDC.VirtualGardenClusters
	if len(gdcVGClusters) == 0 && c.pipelineConfig.GDC.UseUserClusterAsSeed {
		gdcVGClusters = c.pipelineConfig.GDC.UserClusters
	}
	if len(gdcVGClusters) > 0 {
		gdcVGClients, err := config.GetGDCUserClusterClients(gdcVGClusters, c.gdcClient)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get gdc virtual garden cluster clients: %w", err)
		}
		if err := inspectVirtualGardenHosts(gdcVGClusters, gdcVGClients, "GDC (Virtual Garden Host)", false); err != nil {
			return nil, nil, err
		}
	}

	// Check the status of Shoot clusters (Soil Seed Hosts) on GDC user clusters
	if len(c.pipelineConfig.GDC.UserClusters) > 0 && !c.pipelineConfig.GDC.UseUserClusterAsSeed {
		gdcClients, err := config.GetGDCUserClusterClients(c.pipelineConfig.GDC.UserClusters, c.gdcClient)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get user cluster clients: %w", err)
		}

		for _, uc := range c.pipelineConfig.GDC.UserClusters {
			userClusterName := uc.Name
			clusterClient, ok := gdcClients[userClusterName]
			if !ok {
				return nil, nil, fmt.Errorf("client not found for GDC user cluster %q", userClusterName)
			}

			virtualGardenConfig, err := config.GetGardenClient(ctx, clusterClient)
			if err != nil {
				continue
			}

			shootList := &gardencorev1beta1.ShootList{}
			if err := virtualGardenConfig.Client.List(ctx, shootList); err != nil {
				continue
			}

			hostedBy := c.getHostedBy(userClusterName, false)
			for _, shoot := range shootList.Items {
				gardenerArtifactsVersion := shoot.GetAnnotations()[CommitHashAnnotation]
				if gardenerArtifactsVersion == "" {
					availableClusters = append(availableClusters, AvailableClusterStatus{
						Name:     shoot.Name,
						Type:     "Shoot (Seed Host)",
						HostedBy: fmt.Sprintf("%s\n(on %s)", userClusterName, hostedBy),
					})
					continue
				}

				if job, ok := jobsMap[gardenerArtifactsVersion]; ok {
					hostInfo := fmt.Sprintf("%s [ %s ]\n(on %s)", shoot.Name, getShootStatus(&shoot), hostedBy)
					if job.SoilCluster == "" {
						job.SoilCluster = hostInfo
					} else {
						job.SoilCluster += "\n" + hostInfo
					}
				}
			}
		}
	}

	for _, job := range jobsMap {
		activeJobs = append(activeJobs, *job)
	}

	return activeJobs, availableClusters, nil
}

func (c *CLIConfig) getHostedBy(clusterName string, isGKE bool) string {
	if isGKE {
		for _, cfg := range c.pipelineConfig.GCP.GKEClusters {
			if cfg.Name == clusterName {
				return fmt.Sprintf("GKE: %s\n%s\n%s", cfg.Name, cfg.Project, cfg.Zone)
			}
		}
	} else { // GDC
		for _, cfg := range c.pipelineConfig.GDC.VirtualGardenClusters {
			if cfg.Name == clusterName {
				return fmt.Sprintf("GDC: %s\n%s\n%s", cfg.Name, cfg.Project, cfg.Zone)
			}
		}
		for _, cfg := range c.pipelineConfig.GDC.UserClusters {
			if cfg.Name == clusterName {
				return fmt.Sprintf("GDC: %s\n%s\n%s", cfg.Name, cfg.Project, cfg.Zone)
			}
		}
	}
	return clusterName // Fallback
}

func populateCreatedSeedAndShootResources(ctx context.Context, gardenClient client.Client, gardenerArtifactsVersion string, job *ActiveJobStatus) {
	seedList := &gardencorev1beta1.SeedList{}
	if err := gardenClient.List(ctx, seedList); err == nil {
		var createdSeeds []string
		sanitizedHash := GetCommitHashOrSanitize(gardenerArtifactsVersion)
		for _, seed := range seedList.Items {
			if strings.Contains(seed.Name, sanitizedHash) || seed.GetAnnotations()[CommitHashAnnotation] == gardenerArtifactsVersion {
				createdSeeds = append(createdSeeds, fmt.Sprintf("%s [ %s ]", seed.Name, getSeedStatus(&seed)))
			}
		}
		job.CreatedSeed = strings.Join(createdSeeds, "\n")
	}

	shootList := &gardencorev1beta1.ShootList{}
	if err := gardenClient.List(ctx, shootList); err == nil {
		var allShoots []string
		sanitizedHash := GetCommitHashOrSanitize(gardenerArtifactsVersion)
		for _, shoot := range shootList.Items {
			if strings.Contains(shoot.Name, sanitizedHash) || shoot.GetAnnotations()[CommitHashAnnotation] == gardenerArtifactsVersion {
				allShoots = append(allShoots, fmt.Sprintf("%s [ %s ]", shoot.Name, getShootStatus(&shoot)))
			}
		}
		job.CreatedShoot = strings.Join(allShoots, "\n")
	}
}

func getSeedStatus(seed *gardencorev1beta1.Seed) string {
	if seed.Status.LastOperation == nil {
		return "PENDING"
	}

	if seed.Status.LastOperation.Progress != 100 {
		return fmt.Sprintf("\033[31m%s\033[0m", "NOT READY")
	}

	for _, condition := range seed.Status.Conditions {
		if condition.Status == gardencorev1beta1.ConditionFalse {
			return fmt.Sprintf("\033[31m%s\033[0m", "NOT READY")
		}
	}
	return "\033[32mREADY\033[0m"
}

func getShootStatus(shoot *gardencorev1beta1.Shoot) string {
	if shoot.Status.LastOperation == nil {
		return "PENDING"
	}

	state := shoot.Status.LastOperation.State
	status := string(state)

	switch state {
	case gardencorev1beta1.LastOperationStateSucceeded:
		status = "\033[32mSUCCEEDED\033[0m"
	case gardencorev1beta1.LastOperationStateProcessing:
		status = "\033[34mPROCESSING\033[0m"
	case gardencorev1beta1.LastOperationStateError, gardencorev1beta1.LastOperationStateFailed:
		status = fmt.Sprintf("\033[31m%s\033[0m", state)
	}

	return fmt.Sprintf("%s (%s %d%%)", status, shoot.Status.LastOperation.Type, shoot.Status.LastOperation.Progress)
}
