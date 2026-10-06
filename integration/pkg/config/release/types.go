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
	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
)

// pipelineConfig holds the configurations for GCP and GDC air-gapped resources for the CI pipeline
type pipelineConfig struct {
	// GCP holds the configurations for GCP resources.
	GCP config.GCPConfig `yaml:"gcp"`

	// GDC holds the configurations for GDC air-gapped resources.
	GDC config.GDCConfig `yaml:"gdc"`

	// Gardener holds Gardener specific configurations.
	Gardener config.GardenerConfig `yaml:"gardener"`
}
