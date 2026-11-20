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
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gardencorev1 "github.com/gardener/gardener/pkg/apis/core/v1"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	gardenerinternal "github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdcloud"
)

type fakeRoundTripper struct {
	response *http.Response
	err      error
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// restoreLoaderDependencies restores the package-level function variables that tests replace with test doubles
// once the calling test has finished, so that doubles (some of which capture t) do not leak into later tests.
func restoreLoaderDependencies(t *testing.T) {
	t.Helper()

	origGetShootKubeconfig := gardenerinternal.GetShootKubeconfig
	origGetGKEClusterClients := getGKEClusterClients
	origGdcloudInit := gdcloudInit
	origGetGDCUserClusterClients := commonConfig.GetGDCUserClusterClients
	origGetGardenClient := commonConfig.GetGardenClient
	origGetGlobalClient := commonConfig.GetGlobalClient
	origGetManagementClient := commonConfig.GetManagementClient
	origAccessLatestSecret := commonConfig.AccessLatestSecret
	origFetchGDCCAData := commonConfig.FetchGDCCAData
	origGetGDCClientWithLocalFallback := commonConfig.GetGDCClientWithLocalFallback

	t.Cleanup(func() {
		gardenerinternal.GetShootKubeconfig = origGetShootKubeconfig
		getGKEClusterClients = origGetGKEClusterClients
		gdcloudInit = origGdcloudInit
		commonConfig.GetGDCUserClusterClients = origGetGDCUserClusterClients
		commonConfig.GetGardenClient = origGetGardenClient
		commonConfig.GetGlobalClient = origGetGlobalClient
		commonConfig.GetManagementClient = origGetManagementClient
		commonConfig.AccessLatestSecret = origAccessLatestSecret
		commonConfig.FetchGDCCAData = origFetchGDCCAData
		commonConfig.GetGDCClientWithLocalFallback = origGetGDCClientWithLocalFallback
	})
}

func TestLoadReleaseTestConfig(t *testing.T) {
	restoreLoaderDependencies(t)
	gardenerinternal.GetShootKubeconfig = func(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey) ([]byte, error) {
		return []byte("fake-kubeconfig-data"), nil
	}

	// Create a runtime scheme and add the Garden API types to it.
	s := runtime.NewScheme()
	if err := operatorv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add operatorv1alpha1 to scheme: %v", err)
	}
	if err := gardencorev1beta1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add gardencorev1beta1 to scheme: %v", err)
	}
	if err := gardencorev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add gardencorev1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("Failed to add corev1 to scheme: %v", err)
	}

	// Create a fake Garden resource that is unannotated ("idle").
	unannotatedGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unannotated-garden",
		},
	}

	// Create another fake Garden that is already annotated with a different artifacts version.
	nonMatchingCommitHash := "abc123"
	annotatedGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{
			Name: "annotated-garden",
			Annotations: map[string]string{
				CommitHashAnnotation: "abc123",
			},
		},
	}

	unannotatedShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unannotated-shoot",
			Namespace: "garden-test", // shoots are namespaced
		},
		Spec: gardencorev1beta1.ShootSpec{
			Networking: &gardencorev1beta1.Networking{
				Pods:     ptr.To("10.0.0.0/16"),
				Services: ptr.To("10.1.0.0/16"),
			},
		},
	}

	fakeKubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unannotated-shoot.kubeconfig",
			Namespace: "garden-test",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://fake-shoot-cluster.com
  name: fake-shoot
contexts:
- context:
    cluster: fake-shoot
    user: fake-user
  name: fake-context
current-context: fake-context
users:
- name: fake-user
`),
		},
	}

	// Create a fake clients
	userClusterClient1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedGarden).Build()
	userClusterClient2 := fake.NewClientBuilder().WithScheme(s).WithObjects(annotatedGarden).Build()
	globalClient := fake.NewClientBuilder().Build()
	gardenClientForCluster1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedShoot, fakeKubeconfigSecret).Build()
	gardenClientForCluster2 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedShoot, fakeKubeconfigSecret).Build()

	// Mock the function that creates clients for user clusters.
	// Return two clients: one with an annotated garden and one with an unannotated one.
	getGKEClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{
			"cluster-1": userClusterClient1,
			"cluster-2": userClusterClient2,
		}, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{
			"cluster-1": userClusterClient1,
			"cluster-2": userClusterClient2,
		}, nil
	}
	// Mock other helper functions
	commonConfig.GetGardenClient = func(ctx context.Context, userClusterClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		kubeconfigB64 := base64.StdEncoding.EncodeToString([]byte(`
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://fake-shoot-cluster.com
  name: fake-shoot
contexts:
- context:
    cluster: fake-shoot
    user: fake-user
  name: fake-context
current-context: fake-context
users:
- name: fake-user
`))
		responseJSON := fmt.Sprintf(`{"status":{"kubeconfig":"%s"}}`, kubeconfigB64)
		responseBody := []byte(responseJSON)

		fakeResp := &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(responseBody)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}

		restConfig := &rest.Config{
			Transport: &fakeRoundTripper{response: fakeResp},
			Host:      "fake-host",
		}

		var gardenClient client.WithWatch
		if userClusterClient == userClusterClient1 {
			gardenClient = gardenClientForCluster1
		} else if userClusterClient == userClusterClient2 {
			gardenClient = gardenClientForCluster2
		} else {
			return nil, fmt.Errorf("unexpected user cluster client in GetGardenClient mock")
		}

		return &commonConfig.VirtualGardenConfig{
			Client: gardenClient,
			Host:   restConfig.Host,
			CAData: []byte("fake-ca-data"),
		}, nil
	}
	commonConfig.GetGlobalClient = func(_ *gdcloud.TestingClient) (client.WithWatch, error) {
		return globalClient, nil
	}
	commonConfig.GetManagementClient = func(_ *gdcloud.TestingClient, _, _ string) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	gdcloudInit = func(_ context.Context, _ *pipelineConfig) (*gdcloud.TestingClient, error) {
		return nil, nil
	}

	commonConfig.AccessLatestSecret = func(ctx context.Context, secretName string) ([]byte, error) {
		kubeconfig := "apiVersion: v1"
		return []byte(kubeconfig), nil
	}
	commonConfig.FetchGDCCAData = func(consoleURL string) (string, error) {
		return "mock-ca-data", nil
	}

	// Setup: Create a temporary YAML file for the test
	yamlContent := `
gcp:
  gkeClusters:
    - name: "cluster-1"
      zone: "zone-a"
      project: "project-1"
    - name: "cluster-2"
      zone: "zone-b"
      project: "project-2"
gdc:
  project: "test-gdc-project"
  org: "test-org"
  region: "test-region"
  zones:
    - "zone-a"
  globalAPIURL: "https://api.test.com"
  consoleURL: "https://console.test.com"
  harborRegistryURL: "https://harbor.test.com"
  userClusters:
    - name: "cluster-1"
      zone: "zone-a"
      project: "project-1"
    - name: "cluster-2"
      zone: "zone-b"
      project: "project-2"
`
	tmpFile, err := os.CreateTemp("", "pipeline-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	t.Cleanup(func() {
		os.Remove(tmpFile.Name())
	})

	if _, err := tmpFile.Write([]byte(yamlContent)); err != nil {
		t.Fatalf("Failed to write to temp file: %v", err)
	}
	if err := tmpFile.Close(); err != nil {
		t.Fatalf("Failed to close temp file: %v", err)
	}

	// Execute the function under test
	// Override the package-level function variables with mock implementations.
	gardenerArtifactsVersion := "abcdefg"
	config, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation(), WithVirtualGardenProvider(VirtualGardenProviderGKE))

	// Assertions
	if err != nil {
		t.Fatalf("LoadReleaseTestConfig() returned an unexpected error: %v", err)
	}

	if config == nil {
		t.Fatalf("LoadReleaseTestConfig() returned nil config, expected a valid config")
	}

	// Assert that fields are populated correctly based on our mocks and constants
	if config.GlobalAPIClient != globalClient {
		t.Errorf("got GlobalClient %q, want %q", config.GlobalAPIClient, globalClient)
	}
	if config.TestShoot.VirtualGarden.Client != gardenClientForCluster1 {
		t.Errorf("got VirtualGarden.Client %q, want %q", config.TestShoot.VirtualGarden.Client, gardenClientForCluster1)
	}
	if config.TestShoot.VirtualGarden.Host != "fake-host" {
		t.Errorf("got VirtualGarden.Host %q, want %q", config.TestShoot.VirtualGarden.Host, "fake-host")
	}
	if string(config.TestShoot.VirtualGarden.CAData) != "fake-ca-data" {
		t.Errorf("got VirtualGarden.CAData %q, want %q", string(config.TestShoot.VirtualGarden.CAData), "fake-ca-data")
	}
	if config.RuntimeClusterClient != userClusterClient1 {
		t.Errorf("got RuntimeClusterClient %v, want %v", config.RuntimeClusterClient, userClusterClient1)
	}
	expectedShootName := fmt.Sprintf("sh-%s", gardenerArtifactsVersion)
	if config.TestShoot.Name != expectedShootName {
		t.Errorf("got ShootName %q, want %q", config.TestShoot.Name, expectedShootName)
	}
	if config.TestShoot.Namespace != "garden" {
		t.Errorf("got ShootNamespace %q, want %q", config.TestShoot.Namespace, gardenv1beta1constants.GardenNamespace)
	}
	if config.Seed.Network.PodsCIDR != "10.0.0.0/16" {
		t.Errorf("got Seed.Network.PodsCIDR %q, want %q", config.Seed.Network.PodsCIDR, "10.0.0.0/16")
	}
	if config.Seed.Network.ServicesCIDR != "10.1.0.0/16" {
		t.Errorf("got Seed.Network.ServicesCIDR %q, want %q", config.Seed.Network.ServicesCIDR, "10.1.0.0/16")
	}

	// Verify that the idle/unannotated garden (from the first client) was annotated by the `selectGardenCluster` logic.
	updatedGarden := &operatorv1alpha1.Garden{}
	err = userClusterClient1.Get(context.Background(), client.ObjectKey{Name: "unannotated-garden"}, updatedGarden)
	if err != nil {
		t.Fatalf("Failed to get updated garden from fake client: %v", err)
	}

	annotations := updatedGarden.GetAnnotations()
	if val, ok := annotations[CommitHashAnnotation]; !ok || val != gardenerArtifactsVersion {
		t.Errorf("Expected annotation %q to be set to %q, but got %v", CommitHashAnnotation, gardenerArtifactsVersion, annotations)
	}

	// Verify that the annotated garden still has the annotation after the `selectGardenCluster` call.
	updatedGarden = &operatorv1alpha1.Garden{}
	err = userClusterClient2.Get(context.Background(), client.ObjectKey{Name: "annotated-garden"}, updatedGarden)
	if err != nil {
		t.Fatalf("Failed to get updated garden from fake client: %v", err)
	}

	annotations = updatedGarden.GetAnnotations()
	if val, ok := annotations[CommitHashAnnotation]; !ok || val != nonMatchingCommitHash {
		t.Errorf("Expected annotation %q to be set to %q, but got %v", CommitHashAnnotation, nonMatchingCommitHash, val)
	}

	// Verify that the idle/unannotated shoot was annotated by the `selectSeedCluster` logic.
	updatedShoot := &gardencorev1beta1.Shoot{}
	err = config.Seed.HostCluster.Shoot.VirtualGarden.Client.Get(context.Background(), client.ObjectKey{Name: "unannotated-shoot", Namespace: "garden-test"}, updatedShoot)
	if err != nil {
		t.Fatalf("Failed to get updated shoot from fake client: %v", err)
	}

	shootAnnotations := updatedShoot.GetAnnotations()
	if val, ok := shootAnnotations[CommitHashAnnotation]; !ok || val != gardenerArtifactsVersion {
		t.Errorf("Expected shoot annotation %q to be set to %q, but got %v", CommitHashAnnotation, gardenerArtifactsVersion, shootAnnotations)
	}
}

func TestLoadReleaseTestConfig_NoAllocation(t *testing.T) {
	restoreLoaderDependencies(t)
	gardenerinternal.GetShootKubeconfig = func(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey) ([]byte, error) {
		return []byte("fake-kubeconfig-data"), nil
	}

	s := runtime.NewScheme()
	operatorv1alpha1.AddToScheme(s)
	gardencorev1beta1.AddToScheme(s)
	gardencorev1.AddToScheme(s)
	corev1.AddToScheme(s)

	// Only unannotated resources
	unannotatedGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "unannotated-garden"},
	}
	unannotatedShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{Name: "unannotated-shoot", Namespace: "garden-test"},
		Spec: gardencorev1beta1.ShootSpec{
			Networking: &gardencorev1beta1.Networking{
				Pods:     ptr.To("10.0.0.0/16"),
				Services: ptr.To("10.1.0.0/16"),
			},
		},
	}
	fakeKubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "unannotated-shoot.kubeconfig", Namespace: "garden-test"},
		Data:       map[string][]byte{"kubeconfig": []byte("...")},
	}

	client1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedGarden).Build()
	gardenClient1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedShoot, fakeKubeconfigSecret).Build()

	getGKEClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{"cluster-1": client1}, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{"cluster-1": client1}, nil
	}
	commonConfig.GetGardenClient = func(ctx context.Context, userClusterClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		return &commonConfig.VirtualGardenConfig{Client: gardenClient1, Host: "fake", CAData: []byte("fake")}, nil
	}
	commonConfig.GetGlobalClient = func(_ *gdcloud.TestingClient) (client.WithWatch, error) { return fake.NewClientBuilder().Build(), nil }
	commonConfig.GetManagementClient = func(_ *gdcloud.TestingClient, _, _ string) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	gdcloudInit = func(_ context.Context, _ *pipelineConfig) (*gdcloud.TestingClient, error) { return nil, nil }
	commonConfig.AccessLatestSecret = func(ctx context.Context, secretName string) ([]byte, error) { return []byte("conf"), nil }
	commonConfig.FetchGDCCAData = func(consoleURL string) (string, error) { return "mock", nil }

	yamlContent := `
gcp:
  gkeClusters: [{name: "cluster-1", zone: "z", project: "p"}]
gdc:
  project: "p"
  org: "o"
  region: "r"
  zones: ["z"]
  globalAPIURL: "u"
  consoleURL: "c"
  harborRegistryURL: "h"
  userClusters: [{name: "cluster-1", zone: "z", project: "p"}]
`
	tmpFile, _ := os.CreateTemp("", "config-*.yaml")
	defer os.Remove(tmpFile.Name())
	tmpFile.Write([]byte(yamlContent))
	tmpFile.Close()

	// Test with allocateNewClusters = false (default)
	_, err := LoadReleaseTestConfig(tmpFile.Name(), "new-hash", WithVirtualGardenProvider(VirtualGardenProviderGKE))
	if err == nil {
		t.Error("Expected error when no matching cluster found and allocation disabled, got nil")
	}
}

func TestLoadReleaseTestConfig_MissingSeed(t *testing.T) {
	restoreLoaderDependencies(t)
	// Setup similar to TestLoadReleaseTestConfig but ensure seed selection fails
	gardenerinternal.GetShootKubeconfig = func(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey) ([]byte, error) {
		return []byte("fake-kubeconfig-data"), nil
	}

	s := runtime.NewScheme()
	operatorv1alpha1.AddToScheme(s)
	gardencorev1beta1.AddToScheme(s)
	gardencorev1.AddToScheme(s)
	corev1.AddToScheme(s)

	// Unannotated Garden
	unannotatedGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "unannotated-garden"},
	}

	// No Shoots in the garden (so seed selection will fail)
	// We need a garden client that returns no shoots.

	client1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedGarden).Build()
	// Garden client with NO shoots
	gardenClient1 := fake.NewClientBuilder().WithScheme(s).Build()

	getGKEClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{"cluster-1": client1}, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{"cluster-1": client1}, nil
	}
	commonConfig.GetGardenClient = func(ctx context.Context, userClusterClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		return &commonConfig.VirtualGardenConfig{Client: gardenClient1, Host: "fake", CAData: []byte("fake")}, nil
	}
	commonConfig.GetGlobalClient = func(_ *gdcloud.TestingClient) (client.WithWatch, error) { return fake.NewClientBuilder().Build(), nil }
	commonConfig.GetManagementClient = func(_ *gdcloud.TestingClient, _, _ string) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	gdcloudInit = func(_ context.Context, _ *pipelineConfig) (*gdcloud.TestingClient, error) { return nil, nil }
	commonConfig.AccessLatestSecret = func(ctx context.Context, secretName string) ([]byte, error) { return []byte("conf"), nil }
	commonConfig.FetchGDCCAData = func(consoleURL string) (string, error) { return "mock", nil }

	yamlContent := `
gcp:
  gkeClusters: [{name: "cluster-1", zone: "z", project: "p"}]
gdc:
  project: "p"
  org: "o"
  region: "r"
  zones: ["z"]
  globalAPIURL: "u"
  consoleURL: "c"
  harborRegistryURL: "h"
  userClusters: [{name: "cluster-1", zone: "z", project: "p"}]
`
	tmpFile, _ := os.CreateTemp("", "config-*.yaml")
	defer os.Remove(tmpFile.Name())
	tmpFile.Write([]byte(yamlContent))
	tmpFile.Close()

	// Run with allocation enabled
	gardenerArtifactsVersion := "missing-seed-test-hash"
	_, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation(), WithVirtualGardenProvider(VirtualGardenProviderGKE))

	// Expect error because seed selection failure should fail configuration loading unless WithOnlyGarden is set
	if err == nil {
		t.Fatalf("Expected error when seed is missing, got nil")
	}

	// Verify that the garden cluster was NOT annotated
	updatedGarden := &operatorv1alpha1.Garden{}
	if err := client1.Get(context.Background(), client.ObjectKey{Name: "unannotated-garden"}, updatedGarden); err != nil {
		t.Fatalf("Failed to get garden: %v", err)
	}

	ann := updatedGarden.GetAnnotations()
	if val, ok := ann[CommitHashAnnotation]; ok {
		t.Errorf("Expected annotation %s to not be set, but got %s", CommitHashAnnotation, val)
	}

	// Run with WithOnlyGarden enabled -> should succeed and annotate garden
	config, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation(), WithOnlyGarden(), WithVirtualGardenProvider(VirtualGardenProviderGKE))
	if err != nil {
		t.Fatalf("Expected no error when WithOnlyGarden is enabled, got %v", err)
	}
	if config == nil {
		t.Fatalf("Expected non-nil config when WithOnlyGarden is enabled")
	}
	if config.Seed != nil {
		t.Errorf("Expected config.Seed to be nil with WithOnlyGarden, got %v", config.Seed)
	}

	// Verify that the garden cluster was annotated with WithOnlyGarden
	if err := client1.Get(context.Background(), client.ObjectKey{Name: "unannotated-garden"}, updatedGarden); err != nil {
		t.Fatalf("Failed to get garden: %v", err)
	}

	ann = updatedGarden.GetAnnotations()
	if val, ok := ann[CommitHashAnnotation]; !ok || val != gardenerArtifactsVersion {
		t.Errorf("Expected annotation %s=%s, but got %v", CommitHashAnnotation, gardenerArtifactsVersion, ann)
	}
}

func TestLoadReleaseTestConfig_UseUserClusterAsSeed(t *testing.T) {
	restoreLoaderDependencies(t)
	gardenerinternal.GetShootKubeconfig = func(ctx context.Context, gardenClient client.Client, shootKey client.ObjectKey) ([]byte, error) {
		return []byte("fake-kubeconfig-data"), nil
	}

	s := runtime.NewScheme()
	operatorv1alpha1.AddToScheme(s)
	gardencorev1beta1.AddToScheme(s)
	gardencorev1.AddToScheme(s)
	corev1.AddToScheme(s)

	// Unannotated Garden
	unannotatedGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "unannotated-garden"},
	}
	unannotatedShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unannotated-shoot",
			Namespace: "garden-test",
		},
		Spec: gardencorev1beta1.ShootSpec{
			Networking: &gardencorev1beta1.Networking{
				Pods:     ptr.To("10.0.0.0/16"),
				Services: ptr.To("10.1.0.0/16"),
			},
		},
	}
	fakeKubeconfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unannotated-shoot.kubeconfig",
			Namespace: "garden-test",
		},
		Data: map[string][]byte{
			"kubeconfig": []byte("..."),
		},
	}

	client1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedGarden).Build()
	gardenClient1 := fake.NewClientBuilder().WithScheme(s).WithObjects(unannotatedShoot, fakeKubeconfigSecret).Build()

	getGKEClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{"cluster-1": client1}, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		return map[string]client.WithWatch{"cluster-1": client1}, nil
	}
	commonConfig.GetGardenClient = func(ctx context.Context, userClusterClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		return &commonConfig.VirtualGardenConfig{Client: gardenClient1, Host: "fake", CAData: []byte("fake")}, nil
	}
	commonConfig.GetGlobalClient = func(_ *gdcloud.TestingClient) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	commonConfig.GetManagementClient = func(_ *gdcloud.TestingClient, _, _ string) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	gdcloudInit = func(_ context.Context, _ *pipelineConfig) (*gdcloud.TestingClient, error) {
		return nil, nil
	}
	commonConfig.AccessLatestSecret = func(ctx context.Context, secretName string) ([]byte, error) {
		return []byte("conf"), nil
	}
	commonConfig.FetchGDCCAData = func(consoleURL string) (string, error) {
		return "mock", nil
	}

	yamlContent := `
gcp:
  gkeClusters: [{name: "cluster-1", zone: "z", project: "p"}]
gdc:
  project: "p"
  org: "o"
  region: "r"
  zones: ["z"]
  globalAPIURL: "u"
  consoleURL: "c"
  harborRegistryURL: "h"
  useUserClusterAsSeed: true
  userClusters: [{name: "cluster-1", zone: "z", project: "p"}]
`
	tmpFile, _ := os.CreateTemp("", "config-*.yaml")
	defer os.Remove(tmpFile.Name())
	tmpFile.Write([]byte(yamlContent))
	tmpFile.Close()

	gardenerArtifactsVersion := "user-cluster-seed-hash"
	config, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation(), WithVirtualGardenProvider("gdc"))

	if err != nil {
		t.Fatalf("LoadReleaseTestConfig() returned an unexpected error: %v", err)
	}
	if config == nil {
		t.Fatalf("LoadReleaseTestConfig() returned nil config, expected a valid config")
	}
	if config.Seed == nil {
		t.Fatalf("Expected non-nil config.Seed")
	}
	if config.Seed.HostCluster == nil {
		t.Fatalf("Expected non-nil config.Seed.HostCluster")
	}
	if config.Seed.HostCluster.UserClusterName != "cluster-1" {
		t.Errorf("got UserClusterName %q, want %q", config.Seed.HostCluster.UserClusterName, "cluster-1")
	}
	if config.Seed.HostCluster.UserClusterClient != client1 {
		t.Errorf("got UserClusterClient %v, want %v", config.Seed.HostCluster.UserClusterClient, client1)
	}
	if config.Seed.HostCluster.Shoot != nil {
		t.Errorf("expected Seed.HostCluster.Shoot to be nil, got %v", config.Seed.HostCluster.Shoot)
	}
	if config.Seed.Network.PodsCIDR != "10.244.0.0/16" {
		t.Errorf("got PodsCIDR %q, want %q", config.Seed.Network.PodsCIDR, "10.244.0.0/16")
	}
	if config.Seed.Network.ServicesCIDR != "10.96.0.0/12" {
		t.Errorf("got ServicesCIDR %q, want %q", config.Seed.Network.ServicesCIDR, "10.96.0.0/12")
	}

	// Verify that unannotatedShoot was NOT annotated because findShootClusterToHostSeed was bypassed.
	updatedShoot := &gardencorev1beta1.Shoot{}
	if err := gardenClient1.Get(context.Background(), client.ObjectKey{Name: "unannotated-shoot", Namespace: "garden-test"}, updatedShoot); err != nil {
		t.Fatalf("Failed to get shoot: %v", err)
	}
	shootAnnotations := updatedShoot.GetAnnotations()
	if _, ok := shootAnnotations[CommitHashAnnotation]; ok {
		t.Errorf("Expected unannotated-shoot to have no %s annotation, but got %v", CommitHashAnnotation, shootAnnotations)
	}
}

func TestLoadReleaseTestConfig_GDCVirtualGardenClusters(t *testing.T) {
	restoreLoaderDependencies(t)

	s := runtime.NewScheme()
	_ = operatorv1alpha1.AddToScheme(s)
	_ = gardencorev1beta1.AddToScheme(s)
	_ = gardencorev1.AddToScheme(s)
	_ = corev1.AddToScheme(s)

	busyVGGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gardener-grover-4-busy",
			Annotations: map[string]string{
				CommitHashAnnotation: "other-active-version",
			},
		},
	}
	idleVGGarden1 := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "gardener-grover-4"},
	}
	idleVGGarden2 := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "gardener-grover-5"},
	}
	permanentSoilGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "gardener-grover-3"},
	}
	soilShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh-grv-1-a",
			Namespace: "garden",
		},
		Spec: gardencorev1beta1.ShootSpec{
			Networking: &gardencorev1beta1.Networking{
				Pods:     ptr.To("100.64.0.0/16"),
				Services: ptr.To("100.65.0.0/16"),
			},
		},
	}

	vgRuntimeBusy := fake.NewClientBuilder().WithScheme(s).WithObjects(busyVGGarden).Build()
	vgRuntimeIdle1 := fake.NewClientBuilder().WithScheme(s).WithObjects(idleVGGarden1).Build()
	vgRuntimeIdle2 := fake.NewClientBuilder().WithScheme(s).WithObjects(idleVGGarden2).Build()
	soilRuntime := fake.NewClientBuilder().WithScheme(s).WithObjects(permanentSoilGarden).Build()

	vgGardenClient1 := fake.NewClientBuilder().WithScheme(s).Build()
	vgGardenClient2 := fake.NewClientBuilder().WithScheme(s).Build()
	soilGardenClient := fake.NewClientBuilder().WithScheme(s).WithObjects(soilShoot).Build()

	getGKEClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		t.Fatalf("getGKEClusterClients should not be called when GDC virtualGardenClusters are configured")
		return nil, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		res := make(map[string]client.WithWatch, len(userClusters))
		for _, uc := range userClusters {
			switch uc.Name {
			case "gardener-grover-busy":
				res[uc.Name] = vgRuntimeBusy
			case "gardener-grover-4":
				res[uc.Name] = vgRuntimeIdle1
			case "gardener-grover-5":
				res[uc.Name] = vgRuntimeIdle2
			case "gardener-grover-3":
				res[uc.Name] = soilRuntime
			default:
				return nil, fmt.Errorf("unexpected cluster %q", uc.Name)
			}
		}
		return res, nil
	}
	commonConfig.GetGardenClient = func(ctx context.Context, userClusterClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		switch userClusterClient {
		case vgRuntimeIdle1:
			return &commonConfig.VirtualGardenConfig{Client: vgGardenClient1, Host: "https://vg-4.example.com", CAData: []byte("ca-4")}, nil
		case vgRuntimeIdle2:
			return &commonConfig.VirtualGardenConfig{Client: vgGardenClient2, Host: "https://vg-5.example.com", CAData: []byte("ca-5")}, nil
		case soilRuntime:
			return &commonConfig.VirtualGardenConfig{Client: soilGardenClient, Host: "https://soil-3.example.com", CAData: []byte("ca-3")}, nil
		default:
			return nil, fmt.Errorf("unexpected runtime client")
		}
	}
	commonConfig.GetGlobalClient = func(_ *gdcloud.TestingClient) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	commonConfig.GetManagementClient = func(_ *gdcloud.TestingClient, _, _ string) (client.WithWatch, error) {
		return fake.NewClientBuilder().Build(), nil
	}
	gdcloudInit = func(_ context.Context, _ *pipelineConfig) (*gdcloud.TestingClient, error) { return nil, nil }
	commonConfig.AccessLatestSecret = func(_ context.Context, _ string) ([]byte, error) { return []byte("conf"), nil }
	commonConfig.FetchGDCCAData = func(_ string) (string, error) { return "mock-ca", nil }

	yamlContent := `
gdc:
  project: "sapbtp"
  org: "gdc1"
  region: "us-west6"
  zones: ["us-west6-a"]
  globalAPIURL: "https://global-api.example.com"
  consoleURL: "https://console.example.com"
  harborRegistryURL: "harbor.example.com/sap-gardener"
  virtualGardenClusters:
    - name: "gardener-grover-busy"
      zone: "us-west6-a"
      project: "sapbtp"
    - name: "gardener-grover-4"
      zone: "us-west6-a"
      project: "sapbtp"
    - name: "gardener-grover-5"
      zone: "us-west6-a"
      project: "sapbtp"
  userClusters:
    - name: "gardener-grover-3"
      zone: "us-west6-a"
      project: "sapbtp"
`
	tmpFile, err := os.CreateTemp("", "gdc-vg-config-*.yaml")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())
	if _, err := tmpFile.Write([]byte(yamlContent)); err != nil {
		t.Fatalf("Failed to write temp file: %v", err)
	}
	_ = tmpFile.Close()

	targetVersion := "v0.5.3-dev-gdc1234"
	cfg, err := LoadReleaseTestConfig(tmpFile.Name(), targetVersion, WithClusterAllocation())
	if err != nil {
		t.Fatalf("LoadReleaseTestConfig() returned unexpected error: %v", err)
	}

	if cfg.RuntimeClusterName != "gardener-grover-4" {
		t.Errorf("got RuntimeClusterName %q, want %q (first idle cluster in slice order)", cfg.RuntimeClusterName, "gardener-grover-4")
	}
	if cfg.TestShoot.VirtualGarden.Client != vgGardenClient1 {
		t.Errorf("expected VirtualGarden client for gardener-grover-4")
	}
	if cfg.Seed == nil || cfg.Seed.HostCluster == nil || cfg.Seed.HostCluster.Shoot == nil {
		t.Fatalf("expected remote Shoot seed host configuration, got %+v", cfg.Seed)
	}
	if cfg.Seed.HostCluster.Shoot.Name != "sh-grv-1-a" {
		t.Errorf("got Seed host Shoot %q, want %q", cfg.Seed.HostCluster.Shoot.Name, "sh-grv-1-a")
	}

	// Verify gardener-grover-4 Garden was annotated, while permanent soil Garden on gardener-grover-3 was NOT touched.
	gotVG := &operatorv1alpha1.Garden{}
	if err := vgRuntimeIdle1.Get(context.Background(), client.ObjectKey{Name: "gardener-grover-4"}, gotVG); err != nil {
		t.Fatalf("failed to get gardener-grover-4 Garden: %v", err)
	}
	if gotVG.GetAnnotations()[CommitHashAnnotation] != targetVersion {
		t.Errorf("got gardener-grover-4 annotation %q, want %q", gotVG.GetAnnotations()[CommitHashAnnotation], targetVersion)
	}

	gotSoilGarden := &operatorv1alpha1.Garden{}
	if err := soilRuntime.Get(context.Background(), client.ObjectKey{Name: "gardener-grover-3"}, gotSoilGarden); err != nil {
		t.Fatalf("failed to get permanent soil Garden: %v", err)
	}
	if _, annotated := gotSoilGarden.GetAnnotations()[CommitHashAnnotation]; annotated {
		t.Errorf("permanent soil Garden on gardener-grover-3 must not be annotated, got %v", gotSoilGarden.GetAnnotations())
	}
}

func TestLoadReleaseTestConfig_RejectsInvalidVirtualGardenSelection(t *testing.T) {
	restoreLoaderDependencies(t)

	// An invalid selection must be rejected before any cluster is contacted, claimed or annotated.
	commonConfig.FetchGDCCAData = func(string) (string, error) {
		t.Error("FetchGDCCAData must not be called for an invalid Virtual Garden selection")
		return "", fmt.Errorf("unexpected call")
	}
	gdcloudInit = func(context.Context, *pipelineConfig) (*gdcloud.TestingClient, error) {
		t.Error("gdcloudInit must not be called for an invalid Virtual Garden selection")
		return nil, fmt.Errorf("unexpected call")
	}
	getGKEClusterClients = func([]commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		t.Error("getGKEClusterClients must not be called for an invalid Virtual Garden selection")
		return nil, fmt.Errorf("unexpected call")
	}
	commonConfig.GetGDCUserClusterClients = func([]commonConfig.RuntimeClusterConfig, *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		t.Error("GetGDCUserClusterClients must not be called for an invalid Virtual Garden selection")
		return nil, fmt.Errorf("unexpected call")
	}

	const gdcBase = `
gdc:
  project: "p"
  org: "o"
  region: "r"
  zones: ["z"]
  globalAPIURL: "u"
  consoleURL: "c"
  harborRegistryURL: "h"
  userClusters: [{name: "soil-cluster", zone: "z", project: "p"}]
`
	const gkePool = `
gcp:
  gkeClusters: [{name: "gke-cluster", zone: "z", project: "p"}]
`

	tests := []struct {
		name    string
		config  string
		opts    []Option
		wantErr string
	}{
		{
			name:    "unknown provider",
			config:  gkePool + gdcBase,
			opts:    []Option{WithVirtualGardenProvider("gcp")},
			wantErr: `unsupported virtual garden provider "gcp"`,
		},
		{
			name:    "provider is case-sensitive",
			config:  gkePool + gdcBase,
			opts:    []Option{WithVirtualGardenProvider("GDC")},
			wantErr: `unsupported virtual garden provider "GDC"`,
		},
		{
			name:    "gdc provider without virtualGardenClusters does not fall back to the Soil pool",
			config:  gdcBase,
			opts:    []Option{WithVirtualGardenProvider(VirtualGardenProviderGDC), WithClusterAllocation()},
			wantErr: "gdc.virtualGardenClusters must be configured",
		},
		{
			name:    "misspelled virtualGardenClusters key",
			config:  gdcBase + `  virtualGardenCluster: [{name: "vg-cluster", zone: "z", project: "p"}]` + "\n",
			opts:    []Option{WithVirtualGardenProvider(VirtualGardenProviderGDC), WithClusterAllocation()},
			wantErr: "gdc.virtualGardenClusters must be configured",
		},
		{
			name:    "provider defaults to gdc even when only gkeClusters are configured",
			config:  gkePool + gdcBase,
			opts:    []Option{WithClusterAllocation()},
			wantErr: "gdc.virtualGardenClusters must be configured",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "pipeline-config.yaml")
			if err := os.WriteFile(configPath, []byte(tc.config), 0600); err != nil {
				t.Fatalf("Failed to write config file: %v", err)
			}

			cfg, err := LoadReleaseTestConfig(configPath, "v0.5.3-dev-invalid1", tc.opts...)
			if err == nil {
				t.Fatalf("LoadReleaseTestConfig() = %+v, want error containing %q", cfg, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("LoadReleaseTestConfig() error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestCLIConfig_GetStatus_GDCVirtualGardenClusters(t *testing.T) {
	restoreLoaderDependencies(t)

	s := runtime.NewScheme()
	_ = operatorv1alpha1.AddToScheme(s)
	_ = gardencorev1beta1.AddToScheme(s)
	_ = corev1.AddToScheme(s)

	activeVer := "v0.5.3-dev-active1"
	activeGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gardener-grover-4",
			Annotations: map[string]string{CommitHashAnnotation: activeVer},
		},
	}
	idleGarden := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{Name: "gardener-grover-5"},
	}
	createdSeed := &gardencorev1beta1.Seed{
		ObjectMeta: metav1.ObjectMeta{Name: "seed-active1"},
	}
	createdShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{Name: "sh-active1", Namespace: "garden"},
	}
	claimedSoilShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sh-grv-1-a",
			Namespace:   "garden",
			Annotations: map[string]string{CommitHashAnnotation: activeVer},
		},
	}
	idleSoilShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{Name: "sh-grv-2-a", Namespace: "garden"},
	}

	vgActiveRuntime := fake.NewClientBuilder().WithScheme(s).WithObjects(activeGarden).Build()
	vgIdleRuntime := fake.NewClientBuilder().WithScheme(s).WithObjects(idleGarden).Build()
	soilRuntime := fake.NewClientBuilder().WithScheme(s).Build()

	vgActiveGardenClient := fake.NewClientBuilder().WithScheme(s).WithObjects(createdSeed, createdShoot).Build()
	soilGardenClient := fake.NewClientBuilder().WithScheme(s).WithObjects(claimedSoilShoot, idleSoilShoot).Build()

	getGKEClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig) (map[string]client.WithWatch, error) {
		t.Fatalf("getGKEClusterClients must not be called when GCP.GKEClusters is empty")
		return nil, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		res := make(map[string]client.WithWatch, len(userClusters))
		for _, uc := range userClusters {
			switch uc.Name {
			case "gardener-grover-4":
				res[uc.Name] = vgActiveRuntime
			case "gardener-grover-5":
				res[uc.Name] = vgIdleRuntime
			case "gardener-grover-3":
				res[uc.Name] = soilRuntime
			}
		}
		return res, nil
	}
	commonConfig.GetGardenClient = func(_ context.Context, runtimeClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		switch runtimeClient {
		case vgActiveRuntime:
			return &commonConfig.VirtualGardenConfig{Client: vgActiveGardenClient}, nil
		case soilRuntime:
			return &commonConfig.VirtualGardenConfig{Client: soilGardenClient}, nil
		default:
			return nil, fmt.Errorf("unexpected client")
		}
	}

	cliCfg := &CLIConfig{
		pipelineConfig: &pipelineConfig{
			GDC: commonConfig.GDCConfig{
				VirtualGardenClusters: []commonConfig.RuntimeClusterConfig{
					{Name: "gardener-grover-4", Zone: "us-west6-a", Project: "sapbtp"},
					{Name: "gardener-grover-5", Zone: "us-west6-a", Project: "sapbtp"},
				},
				UserClusters: []commonConfig.RuntimeClusterConfig{
					{Name: "gardener-grover-3", Zone: "us-west6-a", Project: "sapbtp"},
				},
			},
		},
	}

	activeJobs, avail, err := cliCfg.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus() returned error: %v", err)
	}
	if len(activeJobs) != 1 {
		t.Fatalf("got %d active jobs, want 1", len(activeJobs))
	}
	if activeJobs[0].VirtualGarden != "gardener-grover-4" {
		t.Errorf("got VirtualGarden %q, want %q", activeJobs[0].VirtualGarden, "gardener-grover-4")
	}
	if activeJobs[0].SoilCluster == "" {
		t.Errorf("expected SoilCluster to be populated on active job")
	}
	if len(avail) != 2 {
		t.Fatalf("got %d available clusters, want 2: %+v", len(avail), avail)
	}
	if avail[0].Name != "gardener-grover-5" || avail[0].Type != "GDC (Virtual Garden Host)" {
		t.Errorf("unexpected available[0]: %+v", avail[0])
	}
	if avail[1].Name != "sh-grv-2-a" || avail[1].Type != "Shoot (Seed Host)" {
		t.Errorf("unexpected available[1]: %+v", avail[1])
	}
}

func TestLoadReleaseTestConfig_LocalGDCloudFallbackAndSkipPullCredentials(t *testing.T) {
	restoreLoaderDependencies(t)

	s := runtime.NewScheme()
	_ = operatorv1alpha1.AddToScheme(s)
	_ = gardencorev1beta1.AddToScheme(s)
	_ = corev1.AddToScheme(s)

	ver := "v0.5.3-dev-teardown1"
	gardenObj := &operatorv1alpha1.Garden{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gardener-grover-4",
			Namespace:   "garden",
			Annotations: map[string]string{CommitHashAnnotation: ver},
		},
	}
	podsCIDR := "100.96.0.0/16"
	servicesCIDR := "100.64.0.0/13"
	soilShoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sh-grv-1-a",
			Namespace:   "garden",
			Annotations: map[string]string{CommitHashAnnotation: ver},
		},
		Spec: gardencorev1beta1.ShootSpec{
			Networking: &gardencorev1beta1.Networking{
				Pods:     &podsCIDR,
				Services: &servicesCIDR,
			},
		},
	}

	vgRuntime := fake.NewClientBuilder().WithScheme(s).WithObjects(gardenObj).Build()
	soilRuntime := fake.NewClientBuilder().WithScheme(s).Build()
	vgClient := fake.NewClientBuilder().WithScheme(s).Build()
	soilVGClient := fake.NewClientBuilder().WithScheme(s).WithObjects(soilShoot).Build()

	usedLocalFallback := false
	commonConfig.GetGDCClientWithLocalFallback = func(_ context.Context, _, _ string) (*gdcloud.TestingClient, error) {
		usedLocalFallback = true
		return &gdcloud.TestingClient{}, nil
	}
	commonConfig.AccessLatestSecret = func(_ context.Context, secretName string) ([]byte, error) {
		t.Fatalf("AccessLatestSecret(%q) must not be called when using local gdcloud fallback and skipping pull credentials", secretName)
		return nil, nil
	}
	commonConfig.GetGDCUserClusterClients = func(userClusters []commonConfig.RuntimeClusterConfig, _ *gdcloud.TestingClient) (map[string]client.WithWatch, error) {
		res := make(map[string]client.WithWatch, len(userClusters))
		for _, uc := range userClusters {
			if uc.Name == "gardener-grover-4" {
				res[uc.Name] = vgRuntime
			} else {
				res[uc.Name] = soilRuntime
			}
		}
		return res, nil
	}
	commonConfig.GetGardenClient = func(_ context.Context, runtimeClient client.Client) (*commonConfig.VirtualGardenConfig, error) {
		if runtimeClient == vgRuntime {
			return &commonConfig.VirtualGardenConfig{Client: vgClient}, nil
		}
		return &commonConfig.VirtualGardenConfig{Client: soilVGClient}, nil
	}
	commonConfig.GetGlobalClient = func(_ *gdcloud.TestingClient) (client.WithWatch, error) {
		return fake.NewClientBuilder().WithScheme(s).Build(), nil
	}
	commonConfig.GetManagementClient = func(_ *gdcloud.TestingClient, _, _ string) (client.WithWatch, error) {
		return fake.NewClientBuilder().WithScheme(s).Build(), nil
	}

	cfgPath := filepath.Join(t.TempDir(), "cfg.yaml")
	cfgYAML := `
gdc:
  project: sapbtp
  org: gdc1
  zones: [us-west6-a]
  consoleURL: https://console.example.com
  caData: ZmFrZS1jYQ==
  virtualGardenClusters:
    - name: gardener-grover-4
      zone: us-west6-a
      project: sapbtp
  userClusters:
    - name: gardener-grover-3
      zone: us-west6-a
      project: sapbtp
`
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0600); err != nil {
		t.Fatalf("failed to write temp config YAML: %v", err)
	}

	cfg, err := LoadReleaseTestConfig(
		cfgPath,
		ver,
		WithVirtualGardenProvider(VirtualGardenProviderGDC),
		WithLocalGDCloudSessionFallback(),
		WithSkipImagePullCredentials(),
	)
	if err != nil {
		t.Fatalf("LoadReleaseTestConfig() unexpected error: %v", err)
	}
	if !usedLocalFallback {
		t.Errorf("expected GetGDCClientWithLocalFallback to be invoked")
	}
	if len(cfg.ImagePullCredentials) != 0 {
		t.Errorf("expected empty ImagePullCredentials when skipped, got %q", string(cfg.ImagePullCredentials))
	}
}
