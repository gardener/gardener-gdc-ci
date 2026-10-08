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

package shootscaling

import (
	"context"
	"flag"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/loader"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
)

var (
	gardenerArtifactsVersion        = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath    = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	continuousConfigurationFilePath = flag.String("continuous-configuration-file-path", "", "the path to the continuous configuration file")
	virtualGardenProvider           = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	waitForReconciliationTimeout = 30 * time.Minute
	nodeScalingAmount            = 1
)

// TestShootClusterScaling is an integration test for scaling a Shoot's worker pool up and down.
func TestShootClusterScaling(t *testing.T) {
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

	// Fetch the initial state of the Shoot
	t.Logf("Fetching initial state for Shoot %s/%s", shootKey.Namespace, shootKey.Name)
	gardenClient := releasePipelineCfg.TestShoot.VirtualGarden.Client
	t.Logf("Creating shoot client for %s/%s", shootKey.Namespace, shootKey.Name)
	shootClients, err := gardener.NewShootClient(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to create shoot client: %v", err)
	}
	shootWatchClient := shootClients.WatchClient
	t.Log("Getting initial node count for shoot cluster")
	originalWorkerCount, err := getNodeCount(ctx, shootWatchClient)
	if err != nil {
		t.Fatalf("failed to get initial node count: %v", err)
	}
	t.Logf("Initial node count for shoot '%s' is %d", releasePipelineCfg.TestShoot.Name, originalWorkerCount)

	// Ensures the cluster is returned to its original state, even if a sub-test fails.
	t.Cleanup(func() {
		t.Logf("--- CLEANUP: Restoring worker count to original value of %d ---", originalWorkerCount)
		currentNodeCount, err := getNodeCount(ctx, shootWatchClient)
		if err != nil {
			t.Fatalf("failed to get current node count: %v", err)
		}
		// Only trigger a reconciliation if the count is different.
		if currentNodeCount != originalWorkerCount {
			scaleAmount := originalWorkerCount - currentNodeCount
			scaleAndVerifyShoot(t, ctx, gardenClient, shootKey, scaleAmount)
			t.Log("--- CLEANUP: Successfully restored worker count. ---")
		} else {
			t.Log("--- CLEANUP: Worker count is already at original value. No action needed. ---")
		}
	})

	scaleUpSuccess := t.Run("Scale Up", func(t *testing.T) {
		t.Logf("Scaling up shoot cluster %s by %d nodes", releasePipelineCfg.TestShoot.Name, nodeScalingAmount)
		scaleAndVerifyShoot(t, ctx, gardenClient, shootKey, nodeScalingAmount)
	})
	if !scaleUpSuccess {
		t.Fatalf("Failed to scale up shoot cluster, aborting flow")
	}

	t.Run("Scale Down", func(t *testing.T) {
		t.Logf("Scaling down shoot cluster %s by %d nodes", releasePipelineCfg.TestShoot.Name, nodeScalingAmount)
		scaleAndVerifyShoot(t, ctx, gardenClient, shootKey, -nodeScalingAmount)
	})
}

// scaleAndVerifyShoot is a helper function that encapsulates the core test logic:
// 1. Update the Shoot worker count.
// 2. Wait for the shoot reconciliation to complete.
// 3. Verify the actual number of nodes in the Shoot cluster matches the new count.
func scaleAndVerifyShoot(t *testing.T, ctx context.Context, gardenClient client.WithWatch, shootKey client.ObjectKey, scaleAmount int32) {
	t.Helper()
	shootClients, err := gardener.NewShootClient(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to create client for the shoot cluster: %v", err)
	}
	shootWatchClient := shootClients.WatchClient
	currentNodeCount, err := getNodeCount(ctx, shootWatchClient)
	if err != nil {
		t.Fatalf("failed to get current node count: %v", err)
	}
	targetNodeCount := currentNodeCount + scaleAmount
	t.Logf("Attempting to scale worker count to %d nodes", targetNodeCount)

	t.Logf("Updating Shoot spec in the Garden cluster")
	err = gardener.UpdateShoot(ctx, gardenClient, shootKey, func(shoot *gardencorev1beta1.Shoot) {
		if len(shoot.Spec.Provider.Workers) == 0 {
			t.Fatalf("Shoot %s has no worker pools", shootKey.Name)
		}
		t.Logf("Scale up the worker pool %s", shoot.Spec.Provider.Workers[0].Name)
		shoot.Spec.Provider.Workers[0].Minimum += scaleAmount
		shoot.Spec.Provider.Workers[0].Maximum += scaleAmount
	})
	if err != nil {
		t.Fatalf("failed to update shoot spec for scaling: %v", err)
	}

	t.Log("Waiting for Shoot reconciliation to complete...")
	if err := gardener.WaitForShootReconciliation(ctx, gardenClient, shootKey, waitForReconciliationTimeout); err != nil {
		t.Fatalf("shoot reconciliation failed or timed out: %v", err)
	}
	t.Log("Shoot reconciliation completed successfully.")

	t.Log("Verifying actual node count in the Shoot cluster...")
	err = waitForNodeCount(t, ctx, shootWatchClient, targetNodeCount, waitForReconciliationTimeout)
	if err != nil {
		t.Fatalf("failed to verify shoot's node count: %v", err)
	}

	t.Logf("Successfully verified that the Shoot cluster has %d nodes.", targetNodeCount)
}

func waitForNodeCount(t *testing.T, ctx context.Context, shootClient client.WithWatch, targetCount int32, timeout time.Duration) error {
	t.Logf("Waiting up to %v for node count to become %d", timeout, targetCount)
	pollingInterval := 10 * time.Second
	err := wait.PollUntilContextTimeout(ctx, pollingInterval, timeout, true, func(ctx context.Context) (bool, error) {
		currentCount, err := getNodeCount(ctx, shootClient)
		if err != nil {
			return false, err
		}
		t.Logf("Current node count is %d, waiting for %d", currentCount, targetCount)
		// Condition is met.
		if currentCount == targetCount {
			return true, nil
		}
		return false, nil
	})

	// Timeout was exceeded.
	if err != nil {
		return fmt.Errorf("timed out after %v waiting for node count to be %d", timeout, targetCount)
	}

	t.Logf("Successfully confirmed node count is %d", targetCount)
	return nil
}

func getNodeCount(ctx context.Context, shootClient client.WithWatch) (int32, error) {
	nodeList := &corev1.NodeList{}
	if err := shootClient.List(ctx, nodeList); err != nil {
		return 0, fmt.Errorf("failed to list nodes: %w", err)
	}
	return int32(len(nodeList.Items)), nil
}
