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
	initialShoot, err := gardener.GetShoot(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to get Shoot %s/%s: %v", shootKey.Namespace, shootKey.Name, err)
	}
	if len(initialShoot.Spec.Provider.Workers) == 0 {
		t.Fatalf("Shoot %s/%s has no worker pools", shootKey.Namespace, shootKey.Name)
	}
	originalPoolMin := initialShoot.Spec.Provider.Workers[0].Minimum
	originalPoolMax := initialShoot.Spec.Provider.Workers[0].Maximum
	originalWorkerCount := totalDesiredWorkers(initialShoot.Spec.Provider.Workers)

	t.Logf("Creating shoot client for %s/%s", shootKey.Namespace, shootKey.Name)
	shootClients, err := gardener.NewShootClient(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to create shoot client: %v", err)
	}
	shootWatchClient := shootClients.WatchClient
	t.Log("Getting initial node count for shoot cluster")
	initialNodeCount, err := getNodeCount(ctx, shootWatchClient)
	if err != nil {
		t.Fatalf("failed to get initial node count: %v", err)
	}
	t.Logf("Initial node count for shoot '%s' is %d (pool %q min=%d, max=%d, total desired=%d)",
		releasePipelineCfg.TestShoot.Name, initialNodeCount, initialShoot.Spec.Provider.Workers[0].Name, originalPoolMin, originalPoolMax, originalWorkerCount)

	// Ensures the cluster is returned to its original state, even if a sub-test fails
	// before the new node registers in the Shoot cluster.
	t.Cleanup(func() {
		t.Logf("--- CLEANUP: Ensuring worker pool %q is restored to min=%d, max=%d (total nodes=%d) ---",
			initialShoot.Spec.Provider.Workers[0].Name, originalPoolMin, originalPoolMax, originalWorkerCount)
		curShoot, getErr := gardener.GetShoot(ctx, gardenClient, shootKey)
		if getErr != nil {
			t.Fatalf("failed to get Shoot during cleanup: %v", getErr)
		}
		currentNodeCount, nodeErr := getNodeCount(ctx, shootWatchClient)
		if nodeErr != nil {
			t.Fatalf("failed to get current node count during cleanup: %v", nodeErr)
		}
		specModified := len(curShoot.Spec.Provider.Workers) > 0 &&
			(curShoot.Spec.Provider.Workers[0].Minimum != originalPoolMin || curShoot.Spec.Provider.Workers[0].Maximum != originalPoolMax)
		if specModified || currentNodeCount != originalWorkerCount {
			scaleAndVerifyShoot(t, ctx, gardenClient, shootKey, originalPoolMin, originalPoolMax, originalWorkerCount)
			t.Log("--- CLEANUP: Successfully restored worker count. ---")
		} else {
			t.Log("--- CLEANUP: Worker spec and node count are already at original values. No action needed. ---")
		}
	})

	scaleUpSuccess := t.Run("Scale Up", func(t *testing.T) {
		t.Logf("Scaling up shoot cluster %s by %d nodes", releasePipelineCfg.TestShoot.Name, nodeScalingAmount)
		scaleAndVerifyShoot(t, ctx, gardenClient, shootKey,
			originalPoolMin+nodeScalingAmount, originalPoolMax+nodeScalingAmount, originalWorkerCount+nodeScalingAmount)
	})
	if !scaleUpSuccess {
		t.Fatalf("Failed to scale up shoot cluster, aborting flow")
	}

	t.Run("Scale Down", func(t *testing.T) {
		t.Logf("Scaling down shoot cluster %s by %d nodes", releasePipelineCfg.TestShoot.Name, nodeScalingAmount)
		scaleAndVerifyShoot(t, ctx, gardenClient, shootKey, originalPoolMin, originalPoolMax, originalWorkerCount)
	})
}

func totalDesiredWorkers(workers []gardencorev1beta1.Worker) int32 {
	var total int32
	for _, w := range workers {
		total += w.Minimum
	}
	return total
}

// scaleAndVerifyShoot is a helper function that encapsulates the core test logic:
// 1. Update the Shoot worker pool minimum and maximum to explicit target values.
// 2. Wait for the shoot reconciliation to complete.
// 3. Verify the actual number of nodes in the Shoot cluster matches targetNodeCount.
func scaleAndVerifyShoot(t *testing.T, ctx context.Context, gardenClient client.WithWatch, shootKey client.ObjectKey, targetPoolMin, targetPoolMax, targetNodeCount int32) {
	t.Helper()
	shootClients, err := gardener.NewShootClient(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to create client for the shoot cluster: %v", err)
	}
	shootWatchClient := shootClients.WatchClient
	t.Logf("Attempting to set worker pool[0] min=%d, max=%d (target total nodes=%d)", targetPoolMin, targetPoolMax, targetNodeCount)

	// When scaling down a worker pool whose targetPoolMax > targetPoolMin, temporarily clamp
	// Maximum to targetPoolMin so Gardener immediately reduces the MachineDeployment replicas
	// instead of leaving the extra node within [targetPoolMin, targetPoolMax] for Cluster Autoscaler.
	// Also check shoot.Spec.Provider.Workers[0].Minimum > targetPoolMin inside UpdateShoot so
	// t.Cleanup recovery clamps Maximum even if Scale Up failed before the new VM registered as a Node.
	currentNodeCount, countErr := getNodeCount(ctx, shootWatchClient)
	clampMaxForScaleDown := countErr == nil && currentNodeCount > targetNodeCount && targetPoolMax > targetPoolMin

	t.Logf("Updating Shoot spec in the Garden cluster")
	err = gardener.UpdateShoot(ctx, gardenClient, shootKey, func(shoot *gardencorev1beta1.Shoot) {
		if len(shoot.Spec.Provider.Workers) == 0 {
			t.Fatalf("Shoot %s has no worker pools", shootKey.Name)
		}
		if shoot.Spec.Provider.Workers[0].Minimum > targetPoolMin && targetPoolMax > targetPoolMin {
			clampMaxForScaleDown = true
		}
		effectiveMax := targetPoolMax
		if clampMaxForScaleDown {
			effectiveMax = targetPoolMin
		}
		t.Logf("Updating worker pool %s to min=%d, max=%d", shoot.Spec.Provider.Workers[0].Name, targetPoolMin, effectiveMax)
		shoot.Spec.Provider.Workers[0].Minimum = targetPoolMin
		shoot.Spec.Provider.Workers[0].Maximum = effectiveMax
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

	if clampMaxForScaleDown {
		t.Logf("Restoring worker pool[0] max to %d after scale-down", targetPoolMax)
		err = gardener.UpdateShoot(ctx, gardenClient, shootKey, func(shoot *gardencorev1beta1.Shoot) {
			if len(shoot.Spec.Provider.Workers) == 0 {
				return
			}
			shoot.Spec.Provider.Workers[0].Minimum = targetPoolMin
			shoot.Spec.Provider.Workers[0].Maximum = targetPoolMax
		})
		if err != nil {
			t.Errorf("post-scale-down restore: failed to restore worker pool[0] maximum to %d (scale-down to %d nodes already verified): %v", targetPoolMax, targetNodeCount, err)
		} else if err := gardener.WaitForShootReconciliation(ctx, gardenClient, shootKey, waitForReconciliationTimeout); err != nil {
			t.Errorf("post-scale-down restore: shoot reconciliation failed after restoring worker pool[0] maximum to %d (scale-down to %d nodes already verified): %v", targetPoolMax, targetNodeCount, err)
		}
	}
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
