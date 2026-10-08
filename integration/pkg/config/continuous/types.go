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
	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
)

// pipelineConfig holds the configurations for the continuous testing pipeline
type pipelineConfig struct {
	// GCP holds the configurations for GCP resources.
	GCP config.GCPConfig `yaml:"gcp"`

	// GDC holds the configurations for GDC air-gapped resources.
	GDC config.GDCConfig `yaml:"gdc"`

	// Gardener holds Gardener specific configurations.
	Gardener GardenerConfig `yaml:"gardener"`
}

// GardenerConfig holds Gardener specific configurations for continuous testing.
type GardenerConfig struct {
	// RuntimeCluster is the GDC user cluster that hosts the Garden cluster.
	RuntimeCluster config.RuntimeClusterConfig `yaml:"runtimeCluster"`

	// ShootName is the name of the test shoot.
	ShootName string `yaml:"shootName"`

	// ShootNamespace is the namespace of the test shoot.
	ShootNamespace string `yaml:"shootNamespace"`

	// SeedName is the name of the seed.
	SeedName string `yaml:"seedName"`
}
