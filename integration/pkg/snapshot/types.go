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

package snapshot

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v2"
)

// PartialSnapshotError represents an error when one or more resource snapshots failed to be captured,
// but other resources were successfully processed.
type PartialSnapshotError struct {
	Errors []error
}

func (e *PartialSnapshotError) Error() string {
	var errStrings []string
	for _, err := range e.Errors {
		errStrings = append(errStrings, err.Error())
	}
	return fmt.Sprintf("failed to capture some resources: %s", strings.Join(errStrings, "; "))
}

// ResourceTarget defines the APIVersion and Kind resource identifiers.
type ResourceTarget struct {
	// APIVersion specifies the versioned schema of the resource (e.g., v1, storage.k8s.io/v1).
	APIVersion string `yaml:"apiVersion"`
	// Kind specifies the Kubernetes resource kind (e.g., Pod, CSIDriver).
	Kind string `yaml:"kind"`
}

// NamespaceTarget specifies resources to snapshot within a specific namespace
// or namespaces matching a label selector
type NamespaceTarget struct {
	// Namespace specifies the target namespace.
	Namespace string `yaml:"namespace,omitempty"`
	// MatchLabels specifies a label selector to select namespaces.
	MatchLabels map[string]string `yaml:"matchLabels,omitempty"`
	// Resources is a list of ResourceTargets within this namespace.
	Resources []ResourceTarget `yaml:"resources"`
}

// ClusterSnapshotConfig defines the resources to capture for a specific cluster.
type ClusterSnapshotConfig struct {
	// ClusterName specifies the name of the cluster (e.g., shoot, garden, seed).
	ClusterName string `yaml:"clusterName"`
	// Targets is a list of namespace-specific snapshot targets.
	Targets []NamespaceTarget `yaml:"targets"`
}

// MultiClusterSnapshotResources defines resources to capture across multiple clusters.
type MultiClusterSnapshotResources struct {
	// Clusters is a list of cluster-specific snapshot configurations.
	Clusters []ClusterSnapshotConfig `yaml:"clusters"`
}

// LoadSnapshotResources reads and parses the MultiClusterSnapshotResources configuration from a YAML file path.
func LoadSnapshotResources(filePath string) (*MultiClusterSnapshotResources, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read snapshot resources file: %w", err)
	}

	var res MultiClusterSnapshotResources
	if err := yaml.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot resources YAML: %w", err)
	}

	return &res, nil
}
