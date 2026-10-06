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

package continuous

import (
	"context"
	"fmt"
	"os"

	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"gopkg.in/yaml.v2"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
)

// LoadReleaseTestConfig orchestrates the loading and processing of the integration test configuration.
func LoadReleaseTestConfig(continuousConfigurationFilePath string) (*config.ReleaseTestConfig, error) {
	ctx := context.Background()

	pipelineCfg, err := loadPipelineConfig(continuousConfigurationFilePath)
	if err != nil {
		return nil, err
	}

	if pipelineCfg.GDC.CAData == "" {
		caData, err := config.FetchGDCCAData(pipelineCfg.GDC.ConsoleURL)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch GDC CA data: %w", err)
		}
		pipelineCfg.GDC.CAData = caData
	}

	gdcClient, err := config.GetGDCClient(ctx, pipelineCfg.GDC.CAData, pipelineCfg.GDC.ConsoleURL)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize gdcloud client: %w", err)
	}

	userClusterClients, err := config.GetGDCUserClusterClients([]config.RuntimeClusterConfig{pipelineCfg.Gardener.RuntimeCluster}, gdcClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get user cluster client: %w", err)
	}

	userClusterClient, ok := userClusterClients[pipelineCfg.Gardener.RuntimeCluster.Name]
	if !ok {
		return nil, fmt.Errorf("user cluster client for %q not found in clients map", pipelineCfg.Gardener.RuntimeCluster.Name)
	}

	virtualGardenConfig, err := config.GetGardenClient(ctx, userClusterClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get garden client: %w", err)
	}

	seedName := pipelineCfg.Gardener.SeedName
	if seedName == "" {
		return nil, fmt.Errorf("seedName must be specified in the continuous testing pipeline configuration")
	}

	globalClient, err := config.GetGlobalClient(gdcClient)
	if err != nil {
		return nil, fmt.Errorf("failed to get global client: %w", err)
	}

	releaseTestConfig := &config.ReleaseTestConfig{
		GDCClient: gdcClient,
		TestShoot: &config.ShootConfig{
			Name:          pipelineCfg.Gardener.ShootName,
			Namespace:     pipelineCfg.Gardener.ShootNamespace,
			VirtualGarden: virtualGardenConfig,
		},
		RuntimeClusterClient: userClusterClient,
		GlobalAPIClient:      globalClient,
		Seed: &config.SeedConfig{
			Name: seedName,
			HostCluster: &config.HostCluster{
				Shoot: &config.ShootConfig{
					Name:          seedName,
					Namespace:     gardenv1beta1constants.GardenNamespace,
					VirtualGarden: virtualGardenConfig,
				},
			},
		},
	}

	return releaseTestConfig, nil
}

// loadPipelineConfig reads a YAML file from the given path and unmarshals it into a pipelineConfig struct.
func loadPipelineConfig(continuousConfigurationFilePath string) (*pipelineConfig, error) {
	yamlFile, err := os.ReadFile(continuousConfigurationFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read YAML file at %s: %w", continuousConfigurationFilePath, err)
	}

	pipelineCfg := &pipelineConfig{}
	if err := yaml.Unmarshal(yamlFile, pipelineCfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML content: %w", err)
	}
	return pipelineCfg, nil
}
