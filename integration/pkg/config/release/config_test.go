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

func TestLoadReleaseTestConfig(t *testing.T) {
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
	config, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation())

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
	_, err := LoadReleaseTestConfig(tmpFile.Name(), "new-hash")
	if err == nil {
		t.Error("Expected error when no matching cluster found and allocation disabled, got nil")
	}
}

func TestLoadReleaseTestConfig_MissingSeed(t *testing.T) {
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
	_, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation())

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
	config, err := LoadReleaseTestConfig(tmpFile.Name(), gardenerArtifactsVersion, WithClusterAllocation(), WithOnlyGarden())
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
