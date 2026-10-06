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

package garden

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/utils/ptr"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
)

const (
	extensionProviderName            = "provider-gdch"
	admissionGdchRuntimeChart        = "admission-gdch-runtime-helm"
	admissionGdchApplicationChart    = "admission-gdch-application-helm"
	extensionAdmissionGdchRepository = "gardener-extension-admission-gdch"
)

var (
	gardenerArtifactsVersion     string
	mcmArtifactsVersion          string
	ccmArtifactsVersion          string
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
)

// init registers the command-line flags.
func init() {
	flag.StringVar(&gardenerArtifactsVersion, "gardener-artifacts-version", "", "The version string for Gardener artifacts")
	flag.StringVar(&mcmArtifactsVersion, "mcm-artifacts-version", "", "The version string for machine-controller-manager-provider-gdc artifacts")
	flag.StringVar(&ccmArtifactsVersion, "ccm-artifacts-version", "", "The version string for cloud-provider-gdc artifacts")
}

// TestCreateGardenCluster tests the creation of a seed cluster.
func TestCreateGardenCluster(t *testing.T) {
	validateFlags(t)
	ctx := context.Background()

	t.Logf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releaseConfigData, err := releaseConfig.LoadReleaseTestConfig(*releaseConfigurationFilePath, gardenerArtifactsVersion, releaseConfig.WithClusterAllocation())
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releaseConfigData != nil && releaseConfigData.GDCClient != nil {
		defer releaseConfigData.GDCClient.Cleanup()
	}

	t.Log("Creating image pull secret in runtime cluster")
	if err := gardener.CreateImagePullSecret(ctx, releaseConfigData.RuntimeClusterClient, releaseConfigData.ImagePullCredentials); err != nil {
		t.Fatalf("failed to create image pull secret: %v", err)
	}

	extopConfig, err := newGdchExtopConfig(releaseConfig.CandidateRegistryURL(), gardenerArtifactsVersion, releaseConfigData.Gardener)
	if err != nil {
		t.Fatalf("failed to create extop config: %v", err)
	}
	t.Log("Deploying operator extension on runtime cluster")
	if err := gardener.DeployOperatorExtension(ctx, releaseConfigData.RuntimeClusterClient, extopConfig); err != nil {
		t.Fatalf("failed to deploy gdch extension: %v", err)
	}

	t.Logf("Successfully deployed extension on runtime cluster.")
}

// validateFlags checks if all the required command-line flags are provided.
func validateFlags(t *testing.T) {
	if *releaseConfigurationFilePath == "" {
		t.Fatal("flag --release-configuration-file-path must be set")
	}

	if gardenerArtifactsVersion == "" {
		t.Fatal("--gardener-artifacts-version is a required flag")
	}
}

type imageOverwriteOptions struct {
	HarborInstanceURL string
	ImageTag          string
	MCMImageTag       string
	CCMImageTag       string
	GardenerConfig    *config.GardenerConfig
}

// newGdchExtopConfig creates a new ExtopConfig for the GDCH extension provider.
func newGdchExtopConfig(harborInstanceURL, imageTag string, gardenerConfig *config.GardenerConfig) (*gardener.ExtopConfig, error) {
	imageOverwrite, err := extensionProviderImageOverwrite(imageOverwriteOptions{
		HarborInstanceURL: harborInstanceURL,
		ImageTag:          imageTag,
		MCMImageTag:       mcmArtifactsVersion,
		CCMImageTag:       ccmArtifactsVersion,
		GardenerConfig:    gardenerConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create image vector: %w", err)
	}
	chartVersion := strings.TrimPrefix(gardenerArtifactsVersion, "v")

	return &gardener.ExtopConfig{
		Name:                             extensionProviderName,
		AdmissionRuntimeHelmChartRef:     ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, admissionGdchRuntimeChart, chartVersion)),
		AdmissionApplicationHelmChartRef: ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, admissionGdchApplicationChart, chartVersion)),
		AdmissionHelmChartValues: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, extensionAdmissionGdchRepository),
				"tag":        imageTag,
			},
			"replicaCount": 1,
		},
		ExtensionProviderHelmChartRef: ptr.To(fmt.Sprintf("%s/%s:%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchHelmChartName, chartVersion)),
		ExtensionProviderRuntimeValues: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchImageName),
				"tag":        imageTag,
				"pullPolicy": "Always",
			},
		},
		ExtensionProviderHelmChartValues: map[string]interface{}{
			"image": map[string]interface{}{
				"repository": fmt.Sprintf("%s/%s", harborInstanceURL, releaseConfig.ExtensionProviderGdchImageName),
				"tag":        imageTag,
				"pullPolicy": "Always",
			},
			"imageVectorOverwrite": imageOverwrite,
		},
		ImagePullSecretName: gardener.ImagePullSecretName,
	}, nil
}

// extensionProviderImageOverwrite generates the image vector overwrite YAML for the extension provider.
func extensionProviderImageOverwrite(opts imageOverwriteOptions) (string, error) {
	type image struct {
		Name       string `yaml:"name"`
		Repository string `yaml:"repository"`
		Tag        string `yaml:"tag"`
	}

	if opts.GardenerConfig == nil || opts.GardenerConfig.CSIImages == nil {
		return "", fmt.Errorf("gardener CSIImages configuration is missing")
	}
	csiImages := opts.GardenerConfig.CSIImages
	mcmTag := opts.MCMImageTag
	if mcmTag == "" {
		mcmTag = opts.ImageTag
	}
	ccmTag := opts.CCMImageTag
	if ccmTag == "" {
		ccmTag = opts.ImageTag
	}

	images := []image{
		{"machine-controller-manager-provider-gdch", fmt.Sprintf("%s/%s", opts.HarborInstanceURL, releaseConfig.MachineControllerManagerProviderImageName), mcmTag},
		{"cloud-controller-manager", fmt.Sprintf("%s/%s", opts.HarborInstanceURL, releaseConfig.CloudControllerManagerImageName), ccmTag},
		{"csi-driver", csiImages.CSIDriver.Repository, csiImages.CSIDriver.Tag},
		{"csi-provisioner", csiImages.CSIProvisioner.Repository, csiImages.CSIProvisioner.Tag},
		{"csi-attacher", csiImages.CSIAttacher.Repository, csiImages.CSIAttacher.Tag},
		{"csi-liveness-probe", csiImages.CSILivenessProbe.Repository, csiImages.CSILivenessProbe.Tag},
		{"csi-node-driver-registrar", csiImages.CSINodeDriverRegistrar.Repository, csiImages.CSINodeDriverRegistrar.Tag},
		{"csi-snapshotter", csiImages.CSISnapshotter.Repository, csiImages.CSISnapshotter.Tag},
		{"csi-resizer", csiImages.CSIResizer.Repository, csiImages.CSIResizer.Tag},
		{"csi-snapshot-controller", csiImages.CSISnapshotController.Repository, csiImages.CSISnapshotController.Tag},
	}

	imageVector := struct {
		Images []image `yaml:"images"`
	}{
		Images: images,
	}

	yamlBytes, err := yaml.Marshal(imageVector)
	if err != nil {
		return "", fmt.Errorf("failed to marshal image vector: %w", err)
	}
	return string(yamlBytes), nil
}
