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

package loader

import (
	"fmt"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/continuous"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
)

// LoadConfigOptions holds the options for loading the configuration.
type LoadConfigOptions struct {
	ReleaseConfigPath     string
	ContinuousConfigPath  string
	GardenerVersion       string
	VirtualGardenProvider string
}

// LoadConfig loads the appropriate test configuration based on the provided options.
func LoadConfig(opts LoadConfigOptions) (*config.ReleaseTestConfig, error) {
	if opts.ReleaseConfigPath != "" && opts.ContinuousConfigPath != "" {
		return nil, fmt.Errorf("cannot specify both --release-configuration-file-path and --continuous-configuration-file-path")
	}

	if opts.ContinuousConfigPath != "" {
		return continuous.LoadReleaseTestConfig(opts.ContinuousConfigPath)
	}

	if opts.ReleaseConfigPath != "" {
		if opts.GardenerVersion == "" {
			return nil, fmt.Errorf("gardener artifacts version must be specified when loading release configuration")
		}
		var releaseOpts []release.Option
		if opts.VirtualGardenProvider != "" {
			releaseOpts = append(releaseOpts, release.WithVirtualGardenProvider(opts.VirtualGardenProvider))
		}
		return release.LoadReleaseTestConfig(opts.ReleaseConfigPath, opts.GardenerVersion, releaseOpts...)
	}

	return nil, fmt.Errorf("either --release-configuration-file-path or --continuous-configuration-file-path must be specified")
}
