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

package gardener

import gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"

// ExtensionConfig holds the configuration for deploying a Gardener extension
type ExtensionConfig struct {
	// Name is the unique name of the extension (e.g., "networking-cilium", "os-gardenlinux")
	Name string
	// HelmChartRef is the OCI reference to the Helm chart for this extension
	HelmChartRef *string
	// ImagePullSecretName is the secret containing the credentials needed to pull the Extension Helm chart.
	ImagePullSecretName string
	// Resources defines what Kubernetes resource types this extension manages
	Resources []gardencorev1beta1.ControllerResource
	// Values are the helm chart values.
	Values map[string]interface{}
}

// ExtopConfig holds the configuration for deploying a Gardener extension using the operator.
type ExtopConfig struct {
	// Name is the unique name of the extension (e.g., "provider-gdch")
	Name string
	// AdmissionRuntimeHelmChartRef is the OCI reference to the Helm chart for gardener-extension-admission-gdch
	AdmissionRuntimeHelmChartRef *string
	// AdmissionApplicationHelmChartRef is the OCI reference to the Helm chart for admission-gdch-application
	AdmissionApplicationHelmChartRef *string
	// AdmissionHelmChartValues are the helm chart values for the admission helm chart.
	AdmissionHelmChartValues map[string]interface{}
	// AdmissionApplicationHelmChartRef is the OCI reference to the Helm chart for gardener-extension-provider-gdch
	ExtensionProviderHelmChartRef *string
	// ExtensionProviderRuntimeValues are the runtime cluster values for the extension provider.
	ExtensionProviderRuntimeValues map[string]interface{}
	// ExtensionProviderHelmChartValues are the helm chart values for the extension provider helm chart.
	ExtensionProviderHelmChartValues map[string]interface{}
	// ImagePullSecretName is the secret containing the credentials needed to pull the Extension Helm chart.
	ImagePullSecretName string
}
