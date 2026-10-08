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

package networking

import (
	"context"
	"flag"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/loader"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/kubernetes"
)

var (
	gardenerArtifactsVersion        = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath    = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	continuousConfigurationFilePath = flag.String("continuous-configuration-file-path", "", "the path to the continuous configuration file")
	virtualGardenProvider           = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	waitForPodReadyTimeout = 4 * time.Minute
)

func TestClusterNetworking(t *testing.T) {
	releasePipelineCfg, err := loader.LoadConfig(loader.LoadConfigOptions{
		ReleaseConfigPath:     *releaseConfigurationFilePath,
		ContinuousConfigPath:  *continuousConfigurationFilePath,
		GardenerVersion:       *gardenerArtifactsVersion,
		VirtualGardenProvider: *virtualGardenProvider,
	})
	if err != nil {
		t.Fatalf("failed to load configuration: %v", err)
	}

	if releasePipelineCfg != nil && releasePipelineCfg.GDCClient != nil {
		defer releasePipelineCfg.GDCClient.Cleanup()
	}

	ctx := context.Background()
	shootKey := client.ObjectKey{Name: releasePipelineCfg.TestShoot.Name, Namespace: releasePipelineCfg.TestShoot.Namespace}

	t.Logf("Creating shoot client for %s/%s", releasePipelineCfg.TestShoot.Namespace, releasePipelineCfg.TestShoot.Name)
	shootClients, err := gardener.NewShootClient(ctx, releasePipelineCfg.TestShoot.VirtualGarden.Client, shootKey)
	if err != nil {
		t.Fatalf("failed to create shoot client: %v", err)
	}

	watchClient := shootClients.WatchClient
	goClient := shootClients.Client
	restConfig := shootClients.Config

	suffix := uniqueSuffix()
	namespace := "networking-test-" + suffix
	t.Logf("Creating Namespace %s", namespace)
	err = kubernetes.CreateNamespace(ctx, watchClient, namespace)
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Cleaning up resources in namespace %s", namespace)
		kubernetes.CleanupResources(t, watchClient, namespace)
	})

	podSpec := func(name string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "test", Image: releaseConfig.TestImageName, Command: []string{"sleep", "3600"}},
				},
				RestartPolicy: corev1.RestartPolicyNever,
			},
		}
	}

	t.Log("Getting node list from shoot cluster")
	nodeList, err := getNodeList(ctx, watchClient, t)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("PodToPodConnectivity", func(t *testing.T) {
		t.Logf("Creating client pod %s in %s namespace", "pod-to-pod-client-"+suffix, namespace)
		clientPodName := "pod-to-pod-client-" + suffix
		_, err := kubernetes.EnsurePod(ctx, watchClient, podSpec(clientPodName), waitForPodReadyTimeout)
		if err != nil {
			t.Fatal(err)
		}

		var serverPods []*corev1.Pod
		for i, node := range nodeList.Items {
			serverPodName := fmt.Sprintf("pod-to-pod-server-%d", i)
			serverSpec := podSpec(serverPodName)
			serverSpec.Spec.NodeName = node.Name

			t.Logf("Creating server pod %s on node %s...", serverPodName, node.Name)
			serverPod, err := kubernetes.EnsurePod(ctx, watchClient, serverSpec, waitForPodReadyTimeout)
			if err != nil {
				t.Fatalf("Failed to create server pod %s on node %s: %v", serverPodName, node.Name, err)
			}
			if serverPod.Status.PodIP == "" {
				t.Fatalf("Server pod %s on node %s was created but has no IP", serverPod.Name, node.Name)
			}
			serverPods = append(serverPods, serverPod)
		}

		for _, serverPod := range serverPods {
			pingServerPodCmd := []string{"ping", "-c", "3", serverPod.Status.PodIP}
			if err := kubernetes.ExecPod(ctx, goClient, restConfig, pingServerPodCmd, namespace, clientPodName); err != nil {
				t.Fatalf("Ping from pod %s to server pod %s (node: %s) failed: %v", clientPodName, serverPod.Name, serverPod.Spec.NodeName, err)
			}
			t.Logf("Successfully pinged server pod %s on node %s.", serverPod.Name, serverPod.Spec.NodeName)
		}
	})

	t.Run("PodToNodeConnectivity", func(t *testing.T) {
		t.Logf("Creating client pod %s in %s namespace", "pod-to-node-client-"+suffix, namespace)
		clientPodName := "pod-to-node-client-" + suffix
		_, err := kubernetes.EnsurePod(ctx, watchClient, podSpec(clientPodName), waitForPodReadyTimeout)
		if err != nil {
			t.Fatalf("Failed to create client pod for node connectivity test: %v", err)
		}
		for _, node := range nodeList.Items {
			var nodeIP string
			for _, addr := range node.Status.Addresses {
				if addr.Type == corev1.NodeInternalIP {
					nodeIP = addr.Address
					break
				}
			}
			if nodeIP == "" {
				t.Fatalf("could not find an internal IP for node %s", node.Name)
			}

			t.Logf("Pinging from pod %s to node %s (%s)", clientPodName, node.Name, nodeIP)
			pingNodeCmd := []string{"ping", "-c", "3", nodeIP}
			if err := kubernetes.ExecPod(ctx, goClient, restConfig, pingNodeCmd, namespace, clientPodName); err != nil {
				t.Fatalf("Ping from pod %s to node %s failed: %v", clientPodName, node.Name, err)
			}
			t.Logf("Successfully pinged node %s.", node.Name)
		}
	})
}

func getNodeList(ctx context.Context, c client.WithWatch, t *testing.T) (*corev1.NodeList, error) {
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		return nil, fmt.Errorf("failed to list nodes of the cluster: %w", err)
	}
	if len(nodeList.Items) == 0 {
		return nil, fmt.Errorf("no nodes found in the cluster")
	}
	t.Logf("Found %d nodes in the cluster.", len(nodeList.Items))
	return nodeList, nil
}

func uniqueSuffix() string {
	return uuid.NewString()[:16]
}
