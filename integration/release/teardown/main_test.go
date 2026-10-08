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

package teardown

import (
	"context"
	"flag"
	"fmt"
	"os"
	"testing"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	globalobjectv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/object/v1"
	objectv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/object/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/helm"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/subnet"
	"github.com/gardener/gardener-gdc-ci/integration/release/workarounds"
)

var (
	gardenerArtifactsVersion     = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	virtualGardenProvider        = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	waitForDeletionTimeout = 60 * time.Minute
	pollingInterval        = 10 * time.Second
)

// TestCleanupResources cleans up all resources created by the release pipeline tests.
func TestCleanupResources(t *testing.T) {
	if *releaseConfigurationFilePath == "" {
		t.Fatal("flag --release-configuration-file-path must be set")
	}
	if *gardenerArtifactsVersion == "" {
		t.Fatal("flag --gardener-artifacts-version must be set")
	}

	t.Logf("Loading release configuration from %s", *releaseConfigurationFilePath)
	releasePipelineCfg, err := releaseConfig.LoadReleaseTestConfig(
		*releaseConfigurationFilePath,
		*gardenerArtifactsVersion,
		releaseConfig.WithVirtualGardenProvider(*virtualGardenProvider),
	)
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releasePipelineCfg != nil && releasePipelineCfg.GDCClient != nil {
		defer releasePipelineCfg.GDCClient.Cleanup()
	}

	// Validate remote Shoot cluster configuration
	if releasePipelineCfg.Seed == nil || releasePipelineCfg.Seed.HostCluster == nil || releasePipelineCfg.Seed.HostCluster.Shoot == nil {
		t.Fatalf("Remote Shoot cluster configuration is not complete: SeedConfig %v", releasePipelineCfg.Seed)
	}

	// Validate test Shoot cluster configuration
	if releasePipelineCfg.TestShoot == nil || releasePipelineCfg.TestShoot.VirtualGarden == nil {
		t.Fatalf("Test Shoot cluster configuration is not complete: TestShootConfig %v", releasePipelineCfg.TestShoot)
	}

	ctx := context.Background()
	shootDeleted := t.Run("Delete Shoot Cluster", func(t *testing.T) {
		t.Logf("Deleting Shoot cluster %s", releasePipelineCfg.TestShoot.Name)
		shootKey := client.ObjectKey{Name: releasePipelineCfg.TestShoot.Name, Namespace: releasePipelineCfg.TestShoot.Namespace}
		if err := deleteAndWaitForDeletion(ctx, releasePipelineCfg.TestShoot.VirtualGarden.Client, &gardencorev1beta1.Shoot{}, shootKey); err != nil {
			t.Fatalf("Failed to delete Shoot cluster: %v", err)
		}
		t.Logf("Shoot cluster '%s' deleted successfully.", shootKey)
	})
	if !shootDeleted {
		t.Fatalf("Delete Shoot Cluster failed, stopping further cleanup and annotation removal to prevent premature release")
	}

	subnetDeleted := t.Run("DeleteParentSubnet", func(t *testing.T) {
		t.Logf("Deleting parent subnet for commit %s", *gardenerArtifactsVersion)
		subnetKey := client.ObjectKey{Name: gardener.GetShootParentSubnetName(*gardenerArtifactsVersion), Namespace: releasePipelineCfg.GDC.Project}
		if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnetKey, waitForDeletionTimeout, t); err != nil {
			t.Fatalf("Failed to delete parent subnet: %v", err)
		}
		t.Logf("Parent subnet '%s' deleted successfully.", subnetKey)
	})
	if !subnetDeleted {
		t.Fatalf("Failed to delete parent subnet, stopping further cleanup and annotation removal")
	}

	seedDeleted := t.Run("Delete Seed Cluster and Uninstall Gardenlet Helm Chart", func(t *testing.T) {
		remoteShootCluster := releasePipelineCfg.Seed.HostCluster.Shoot
		remoteShootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
		remoteShootClient, err := gardener.NewShootClient(ctx, remoteShootCluster.VirtualGarden.Client, remoteShootKey)
		if err != nil {
			t.Fatalf("Failed to create remote Shoot cluster client: %v", err)
		}

		// Grant cluster-admin to gardenlet so it can delete CRDs during seed deletion
		if err := workarounds.CreateGardenletCRDDeleterRoleBinding(ctx, remoteShootClient.WatchClient); err != nil {
			t.Logf("Warning: Failed to apply gardenlet CRD deleter workaround: %v", err)
		} else {
			t.Logf("Applied gardenlet CRD deleter workaround")
		}

		t.Logf("Deleting Seed cluster %s", releasePipelineCfg.Seed.Name)
		// Delete Seed cluster
		seedKey := client.ObjectKey{Name: releasePipelineCfg.Seed.Name}
		if err := deleteAndWaitForDeletion(ctx, releasePipelineCfg.TestShoot.VirtualGarden.Client, &gardencorev1beta1.Seed{}, seedKey); err != nil {
			t.Fatalf("Failed to delete Seed cluster: %v", err)
		}
		t.Logf("Seed cluster '%s' deleted successfully.", seedKey)

		assertAndCleanupSeedBucket(ctx, t, releasePipelineCfg)

		// Uninstall gardenlet helm chart on the remote Shoot cluster hosting the Seed cluster
		shootKubeconfigPath, err := gardener.GetShootKubeconfigPath(ctx, remoteShootCluster.VirtualGarden.Client, remoteShootKey)
		if err != nil {
			t.Fatalf("Failed to get remote Shoot cluster kubeconfig path: %v", err)
		}
		t.Cleanup(func() {
			if err := os.Remove(shootKubeconfigPath); err != nil {
				t.Logf("Warning: Failed to clean up shoot kubeconfig file %s: %v", shootKubeconfigPath, err)
			}
		})

		t.Log("Uninstalling gardenlet helm chart")
		opts := helm.UninstallOptions{
			KubeconfigPath: shootKubeconfigPath,
			ReleaseName:    "gardenlet",
			Namespace:      gardenv1beta1constants.GardenNamespace,
			IgnoreNotFound: true,
		}
		if _, err := helm.Uninstall(opts); err != nil {
			t.Fatalf("Failed to uninstall gardenlet helm chart: %v", err)
		}
		t.Logf("Gardenlet helm chart uninstalled successfully.")

		// Delete gardenlet-kubeconfig secret for existing seed cluster
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gardenlet-kubeconfig",
				Namespace: gardenv1beta1constants.GardenNamespace,
			},
		}
		if err := remoteShootClient.WatchClient.Delete(ctx, secret); err != nil {
			if !apierrors.IsNotFound(err) {
				t.Fatalf("Failed to delete gardenlet-kubeconfig secret: %v", err)
			}
		}
		t.Logf("gardenlet-kubeconfig secret for seed is deleted successfully.")
	})
	if !seedDeleted {
		t.Fatalf("Failed to delete Seed cluster, stopping annotation removal")
	}

	t.Run("Remove CommitHash Annotation from remote Shoot cluster that hosts the Seed cluster", func(t *testing.T) {
		remoteShootCluster := releasePipelineCfg.Seed.HostCluster.Shoot
		t.Logf("Removing artifacts version annotation from remote shoot cluster %s", remoteShootCluster.Name)
		remoteShootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
		removeAnnotation(ctx, t, remoteShootCluster.VirtualGarden.Client, &gardencorev1beta1.Shoot{}, remoteShootKey, releaseConfig.CommitHashAnnotation, *gardenerArtifactsVersion)
	})

	t.Run("Remove CommitHash Annotation from Garden CR on runtime cluster", func(t *testing.T) {
		t.Log("Removing artifacts version annotation from Garden CR")
		gardenList := &operatorv1alpha1.GardenList{}
		if err := releasePipelineCfg.RuntimeClusterClient.List(ctx, gardenList); err != nil {
			t.Fatalf("Failed to list Garden resources: %v", err)
		}
		if len(gardenList.Items) == 0 {
			t.Fatalf("Unexpected: The selected runtime cluster for artifacts version %q does not have a Garden cluster deployment", *gardenerArtifactsVersion)
		}
		garden := gardenList.Items[0]
		gardenKey := client.ObjectKey{Name: garden.Name, Namespace: garden.Namespace}
		removeAnnotation(ctx, t, releasePipelineCfg.RuntimeClusterClient, &garden, gardenKey, releaseConfig.CommitHashAnnotation, *gardenerArtifactsVersion)
	})
}

// deleteAndWaitForDeletion deletes a k8s resource and waits until it is gone.
func deleteAndWaitForDeletion(ctx context.Context, c client.Client, obj client.Object, key client.ObjectKey) error {
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get %T '%s' before deletion: %w", obj, key, err)
	}

	switch resource := obj.(type) {
	case *gardencorev1beta1.Shoot:
		patch := client.MergeFrom(resource.DeepCopyObject().(client.Object))

		// Gardener requires a special annotation to confirm shoot deletion.
		// confirmation.gardener.cloud/deletion=true
		if resource.Annotations == nil {
			resource.Annotations = make(map[string]string)
		}
		resource.Annotations[gardenv1beta1constants.ConfirmationDeletion] = "true"

		// If the shoot is in DeleteFailed state, apply the gardener operation retry annotation.
		// gardener.cloud/operation=retry
		if resource.Status.LastOperation != nil &&
			resource.Status.LastOperation.Type == gardencorev1beta1.LastOperationTypeDelete &&
			resource.Status.LastOperation.State == gardencorev1beta1.LastOperationStateFailed {
			resource.Annotations[gardenv1beta1constants.GardenerOperation] = gardenv1beta1constants.ShootOperationRetry
		}

		if err := c.Patch(ctx, resource, patch); err != nil {
			return fmt.Errorf("failed to annotate shoot '%s' for deletion: %w", key, err)
		}
	}

	if err := c.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to issue delete for %T '%s': %w", obj, key, err)
	}

	// Poll untill the resource is deleted.
	pollCtx, cancel := context.WithTimeout(ctx, waitForDeletionTimeout)
	defer cancel()

	return wait.PollUntilContextCancel(pollCtx, pollingInterval, true, func(ctx context.Context) (bool, error) {
		err := c.Get(ctx, key, obj)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		if obj.GetDeletionTimestamp().IsZero() {
			if delErr := c.Delete(ctx, obj); delErr != nil && !apierrors.IsNotFound(delErr) {
				return false, delErr
			}
		}
		return false, nil
	})
}

// removeAnnotation removes the specified annotation from a Kubernetes object.
// If expectedValue is non-empty, the annotation is only removed when its current value matches expectedValue.
func removeAnnotation(ctx context.Context, t *testing.T, c client.Client, obj client.Object, key client.ObjectKey, annotationKey, expectedValue string) {
	t.Helper()
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			t.Fatalf("Resource %T '%s' not found, skipping annotation removal.", obj, key)
		}
		t.Fatalf("Failed to get %T '%s': %v", obj, key, err)
	}

	annotations := obj.GetAnnotations()
	val, ok := annotations[annotationKey]
	if !ok || (expectedValue != "" && val != expectedValue) {
		t.Logf("No matching %s annotation found on %T '%s'.", annotationKey, obj, key)
		return
	}

	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	delete(annotations, annotationKey)
	obj.SetAnnotations(annotations)
	if err := c.Patch(ctx, obj, patch); err != nil {
		t.Fatalf("Failed to remove %s annotation from %T %s: %v", annotationKey, obj, key, err)
	}
	t.Logf("Removed %s annotation from %T '%s'.", annotationKey, obj, key)
}

func assertAndCleanupSeedBucket(ctx context.Context, t *testing.T, cfg *config.ReleaseTestConfig) {
	t.Helper()
	seedBucketName, err := resolveSeedBucketName(ctx, cfg)
	if err != nil {
		t.Fatalf("Failed to resolve Seed backup bucket name from host Shoot annotation: %v", err)
	}
	if seedBucketName == "" {
		t.Logf("No %s annotation found on host Shoot; skipping Seed bucket deletion assertion.", releaseConfig.SeedBucketNameAnnotation)
		return
	}

	t.Logf("Verifying Seed backup bucket '%s' is deleted", seedBucketName)
	if err := verifySeedBucketDeleted(ctx, cfg, seedBucketName); err != nil {
		t.Fatalf("Seed backup bucket deletion verification failed: %v", err)
	}
	t.Logf("Seed backup bucket '%s' verified deleted.", seedBucketName)

	remoteShoot := cfg.Seed.HostCluster.Shoot
	remoteShootKey := client.ObjectKey{Name: remoteShoot.Name, Namespace: remoteShoot.Namespace}
	removeAnnotation(ctx, t, remoteShoot.VirtualGarden.Client, &gardencorev1beta1.Shoot{}, remoteShootKey, releaseConfig.SeedBucketNameAnnotation, "")
}

// resolveSeedBucketName reads the Seed backup bucket name (<seed.UID>) from the
// `integration.release.sapbtp.gardener/seed-bucket-name` annotation recorded on the host Shoot CR.
func resolveSeedBucketName(ctx context.Context, cfg *config.ReleaseTestConfig) (string, error) {
	if cfg == nil || cfg.Seed == nil || cfg.Seed.HostCluster == nil || cfg.Seed.HostCluster.Shoot == nil || cfg.Seed.HostCluster.Shoot.VirtualGarden == nil {
		return "", nil
	}
	remoteShoot := cfg.Seed.HostCluster.Shoot
	hostShoot := &gardencorev1beta1.Shoot{}
	hostShootKey := client.ObjectKey{Name: remoteShoot.Name, Namespace: remoteShoot.Namespace}
	if err := remoteShoot.VirtualGarden.Client.Get(ctx, hostShootKey, hostShoot); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	return hostShoot.GetAnnotations()[releaseConfig.SeedBucketNameAnnotation], nil
}

// verifySeedBucketDeleted asserts that the Gardener BackupBucket in Virtual Garden is deleted
// and that the underlying GDC Bucket resource (dual-zone or zonal) for the Seed is either
// already deleted (NotFound) or marked for deletion (deletionTimestamp != nil) while retained
// noncurrent backup versions await lifecyclePolicy expiration.
func verifySeedBucketDeleted(ctx context.Context, cfg *config.ReleaseTestConfig, bucketName string) error {
	if bucketName == "" {
		return nil
	}

	backupBucket := &gardencorev1beta1.BackupBucket{}
	backupBucketKey := client.ObjectKey{Name: bucketName}
	err := cfg.TestShoot.VirtualGarden.Client.Get(ctx, backupBucketKey, backupBucket)
	if err == nil {
		return fmt.Errorf("gardener BackupBucket %q still exists in Virtual Garden (deletionTimestamp=%v, lastError=%v)",
			backupBucketKey.Name, backupBucket.DeletionTimestamp, backupBucket.Status.LastError)
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to check Gardener BackupBucket %q in Virtual Garden: %w", backupBucketKey.Name, err)
	}

	gdcBucketKey := client.ObjectKey{
		Name:      bucketName,
		Namespace: cfg.GDC.Project,
	}
	if cfg.GDC.DualZoneBucketLocation != "" {
		globalBucket := &globalobjectv1.Bucket{}
		err := cfg.GlobalAPIClient.Get(ctx, gdcBucketKey, globalBucket)
		if err == nil {
			if globalBucket.DeletionTimestamp == nil {
				return fmt.Errorf("GDC dual-zone Bucket %q still exists without deletionTimestamp in namespace %q (conditions=%+v, errorStatus=%+v)",
					gdcBucketKey.Name, gdcBucketKey.Namespace, globalBucket.Status.Conditions, globalBucket.Status.ErrorStatus)
			}
			return nil
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check GDC dual-zone Bucket %q in namespace %q: %w", gdcBucketKey.Name, gdcBucketKey.Namespace, err)
		}
		return nil
	}

	for zone, mgmtClient := range cfg.ManagementClients {
		zonalBucket := &objectv1.Bucket{}
		err := mgmtClient.Get(ctx, gdcBucketKey, zonalBucket)
		if err == nil {
			if zonalBucket.DeletionTimestamp == nil {
				return fmt.Errorf("GDC zonal Bucket %q still exists without deletionTimestamp in zone %q namespace %q (conditions=%+v, errorStatus=%+v)",
					gdcBucketKey.Name, zone, gdcBucketKey.Namespace, zonalBucket.Status.Conditions, zonalBucket.Status.ErrorStatus)
			}
			continue
		}
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("failed to check GDC zonal Bucket %q in zone %q namespace %q: %w", gdcBucketKey.Name, zone, gdcBucketKey.Namespace, err)
		}
	}
	return nil
}
