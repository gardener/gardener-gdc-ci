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

package diagnostics

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// GetObjectYAML fetches a Kubernetes object by key and returns a structured YAML string representation.
func GetObjectYAML(ctx context.Context, c client.Client, key client.ObjectKey, obj client.Object) string {
	if err := c.Get(ctx, key, obj); err != nil {
		return fmt.Sprintf("failed to retrieve status (object not found or err: %v)", err)
	}
	objYAML, err := yaml.Marshal(obj)
	if err != nil {
		return fmt.Sprintf("failed to marshal object to YAML: %v (raw: %+v)", err, obj)
	}
	return string(objYAML)
}
