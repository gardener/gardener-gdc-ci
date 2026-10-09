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

package teardown_custom

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	operatorv1alpha1 "github.com/gardener/gardener/pkg/apis/operator/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	pkgConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gdch"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/helm"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/subnet"
	"github.com/gardener/gardener-gdc-ci/integration/release/workarounds"
)

var (
	gardenerArtifactsVersion     = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	virtualGardenProvider        = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
	teardownLevel                = flag.String("teardown-level", "garden", "the teardown level ('garden', 'seed', 'shoot')")
	forceCleanup                 = flag.Bool("force", false, "force cleanup all resources including deletion-protected CRDs, subnets, and namespaces")
)

const (
	waitForDeletionTimeout = 40 * time.Minute
	pollingInterval        = 10 * time.Second
)

// TestCleanupResources cleans up all resources created by the release pipeline tests robustly.
func TestCleanupResources(t *testing.T) {
	if *releaseConfigurationFilePath == "" {
		t.Fatal("flag --release-configuration-file-path must be set")
	}
	if *gardenerArtifactsVersion == "" {
		t.Fatal("flag --gardener-artifacts-version must be set")
	}

	t.Logf("[Custom Teardown] Loading release configuration from %s", *releaseConfigurationFilePath)
	if *teardownLevel != "garden" && *teardownLevel != "seed" && *teardownLevel != "shoot" {
		t.Fatalf("Invalid teardown level %q. Must be 'garden', 'seed', or 'shoot'.", *teardownLevel)
	}

	opts := []releaseConfig.Option{
		releaseConfig.WithVirtualGardenProvider(*virtualGardenProvider),
		releaseConfig.WithAllowMissingGarden(),
	}

	releasePipelineCfg, err := releaseConfig.LoadReleaseTestConfig(
		*releaseConfigurationFilePath,
		*gardenerArtifactsVersion,
		opts...,
	)
	if err != nil {
		t.Fatalf("failed to load release configuration: %v", err)
	}

	if releasePipelineCfg != nil && releasePipelineCfg.GDCClient != nil {
		defer releasePipelineCfg.GDCClient.Cleanup()
	}

	ctx := context.Background()

	// 1. Delete Shoot cluster
	t.Run("Delete Shoot Cluster", func(t *testing.T) {
		var virtualGardenClient client.Client
		if releasePipelineCfg != nil && releasePipelineCfg.TestShoot != nil && releasePipelineCfg.TestShoot.VirtualGarden != nil {
			virtualGardenClient = releasePipelineCfg.TestShoot.VirtualGarden.Client
		}

		if virtualGardenClient != nil && !checkVirtualGardenConnectivity(ctx, virtualGardenClient) {
			t.Log("Virtual Garden API is unreachable. Skipping graceful Shoot deletion.")
			virtualGardenClient = nil
		}

		if virtualGardenClient == nil {
			t.Log("No Shoot resources found on Virtual Garden to delete.")
			return
		}

		shootList := &gardencorev1beta1.ShootList{}
		if err := virtualGardenClient.List(ctx, shootList); err != nil {
			t.Logf("Warning: Failed to list Shoot resources: %v", err)
			t.Log("No Shoot resources found on Virtual Garden to delete.")
			return
		}

		if len(shootList.Items) == 0 {
			t.Log("No Shoot resources found on Virtual Garden to delete.")
			return
		}

		for _, shootItem := range shootList.Items {
			shoot := shootItem
			shootKey := client.ObjectKey{Name: shoot.Name, Namespace: shoot.Namespace}
			t.Logf("Deleting Shoot cluster %s", shoot.Name)
			err := deleteAndWaitForDeletion(ctx, virtualGardenClient, &shoot, shootKey, waitForDeletionTimeout)
			if err != nil {
				if *forceCleanup {
					t.Logf("WARNING: Failed to delete Shoot cluster gracefully: %v. Initiating force cleanup...", err)

					if releasePipelineCfg != nil && releasePipelineCfg.Seed != nil && releasePipelineCfg.Seed.HostCluster != nil {
						hostCluster := releasePipelineCfg.Seed.HostCluster
						var seedClient client.Client
						var seedClientErr error
						var hostKubeconfigPath string

						if hostCluster.UserClusterClient != nil {
							seedClient = hostCluster.UserClusterClient
							t.Log("Using HostCluster UserClusterClient directly for machine operations.")
						} else if hostCluster.Shoot != nil {
							remoteShootCluster := hostCluster.Shoot
							remoteShootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
							hostKubeconfigPath, seedClientErr = gardener.GetShootKubeconfigPath(ctx, remoteShootCluster.VirtualGarden.Client, remoteShootKey)
							if seedClientErr != nil {
								t.Logf("Warning: Could not fetch seed kubeconfig path to clear stuck machines: %v", seedClientErr)
							} else {
								defer os.Remove(hostKubeconfigPath)
								cfg, buildErr := clientcmd.BuildConfigFromFlags("", hostKubeconfigPath)
								if buildErr != nil {
									t.Logf("Warning: Failed to build rest config from host kubeconfig: %v", buildErr)
								} else {
									cfg.QPS = 100.0
									cfg.Burst = 150
									seedClient, seedClientErr = client.New(cfg, client.Options{Scheme: virtualGardenClient.Scheme()})
									if seedClientErr != nil {
										t.Logf("Warning: Failed to create seed client: %v", seedClientErr)
										seedClient = nil
									}
								}
							}
						}

						if seedClient != nil {
							shootNamespace := fmt.Sprintf("shoot--%s--%s", shoot.Namespace, shoot.Name)
							t.Logf("Attempting to force-delete machines on Seed cluster namespace %s via client API", shootNamespace)

							machineList := &unstructured.UnstructuredList{}
							machineList.SetGroupVersionKind(schema.GroupVersionKind{
								Group:   "machine.sapcloud.io",
								Version: "v1alpha1",
								Kind:    "MachineList",
							})

							if listErr := seedClient.List(ctx, machineList, client.InNamespace(shootNamespace)); listErr != nil {
								t.Logf("Warning: Failed to list machines on Seed: %v", listErr)
							} else {
								for _, machineObj := range machineList.Items {
									m := machineObj
									t.Logf("Removing finalizers from machine %s", m.GetName())
									m.SetFinalizers(nil)
									if updateErr := seedClient.Update(ctx, &m); updateErr != nil {
										t.Logf("Warning: Failed to remove finalizer on machine %s: %v", m.GetName(), updateErr)
									} else {
										t.Logf("Removed finalizer on machine %s successfully", m.GetName())
									}
								}
							}
						} else {
							t.Log("Warning: Seed client is not available to clear stuck machines.")
						}
					} else {
						t.Log("Warning: Seed configuration is not available to clear stuck machines.")
					}

					// Force-remove finalizers on the Shoot itself
					t.Logf("Force-removing finalizers on Shoot resource %s", shoot.Name)
					shootObj := &gardencorev1beta1.Shoot{}
					if forceErr := forceRemoveFinalizers(ctx, virtualGardenClient, shootObj, shootKey); forceErr != nil {
						t.Logf("Warning: Failed to force-remove finalizers on Shoot: %v", forceErr)
					} else {
						t.Logf("Force-removed Shoot finalizers successfully.")
					}

					// Retry deletion check
					if retryErr := deleteAndWaitForDeletion(ctx, virtualGardenClient, &gardencorev1beta1.Shoot{}, shootKey, 5*time.Minute); retryErr != nil {
						t.Logf("Warning: Shoot deletion retry failed after force-cleanup: %v. Continuing teardown anyway.", retryErr)
					}
				} else {
					t.Fatalf("Failed to delete Shoot cluster gracefully: %v", err)
				}
			} else {
				t.Logf("Shoot cluster '%s' deleted successfully.", shootKey)
			}
		}
	})

	if *teardownLevel == "seed" || *teardownLevel == "garden" {
		// 3. Delete Seed cluster and Uninstall Gardenlet Helm Chart
		t.Run("Delete Seed Cluster and Uninstall Gardenlet Helm Chart", func(t *testing.T) {
			var virtualGardenClient client.Client
			if releasePipelineCfg != nil && releasePipelineCfg.TestShoot != nil && releasePipelineCfg.TestShoot.VirtualGarden != nil {
				virtualGardenClient = releasePipelineCfg.TestShoot.VirtualGarden.Client
			}

			if virtualGardenClient != nil && !checkVirtualGardenConnectivity(ctx, virtualGardenClient) {
				t.Log("Virtual Garden API is unreachable. Skipping graceful Seed deletion.")
				virtualGardenClient = nil
			}

			var targetWatchClient client.WithWatch
			var targetKubeconfigPath string
			var isRemoteShoot bool
			var remoteShootCluster *pkgConfig.ShootConfig
			var remoteShootKey client.ObjectKey

			if releasePipelineCfg != nil && releasePipelineCfg.GDC != nil && releasePipelineCfg.GDC.UseUserClusterAsSeed {
				targetWatchClient = releasePipelineCfg.RuntimeClusterClient
				if targetWatchClient == nil && releasePipelineCfg.Seed != nil && releasePipelineCfg.Seed.HostCluster != nil {
					targetWatchClient = releasePipelineCfg.Seed.HostCluster.UserClusterClient
				}
				clusterName := releasePipelineCfg.RuntimeClusterName
				if clusterName == "" && releasePipelineCfg.Seed != nil && releasePipelineCfg.Seed.HostCluster != nil {
					clusterName = releasePipelineCfg.Seed.HostCluster.UserClusterName
				}
				if clusterName != "" {
					targetKubeconfigPath = fmt.Sprintf("/tmp/%s-kubeconfig", clusterName)
				}
				if targetWatchClient != nil {
					// Grant cluster-admin to gardenlet so it can delete CRDs during seed deletion
					if err := workarounds.CreateGardenletCRDDeleterRoleBinding(ctx, targetWatchClient); err != nil {
						t.Logf("Warning: Failed to apply gardenlet CRD deleter workaround: %v", err)
					} else {
						t.Logf("Applied gardenlet CRD deleter workaround")
					}
				}
			} else if releasePipelineCfg != nil && releasePipelineCfg.Seed != nil && releasePipelineCfg.Seed.HostCluster != nil && releasePipelineCfg.Seed.HostCluster.Shoot != nil {
				remoteShootCluster = releasePipelineCfg.Seed.HostCluster.Shoot
				remoteShootKey = client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
				shootClients, err := gardener.NewShootClient(ctx, remoteShootCluster.VirtualGarden.Client, remoteShootKey)
				if err != nil {
					t.Logf("Warning: Failed to create remote Shoot cluster client: %v. Skipping Seed helm cleanup.", err)
				} else {
					targetWatchClient = shootClients.WatchClient
					isRemoteShoot = true
					// Grant cluster-admin to gardenlet so it can delete CRDs during seed deletion
					if err := workarounds.CreateGardenletCRDDeleterRoleBinding(ctx, targetWatchClient); err != nil {
						t.Logf("Warning: Failed to apply gardenlet CRD deleter workaround: %v", err)
					} else {
						t.Logf("Applied gardenlet CRD deleter workaround")
					}
				}
			}

			if virtualGardenClient == nil {
				t.Log("No Seed resources found on Virtual Garden to delete.")
			} else {
				seedList := &gardencorev1beta1.SeedList{}
				if err := virtualGardenClient.List(ctx, seedList); err != nil {
					t.Logf("Warning: Failed to list Seed resources: %v", err)
					t.Log("No Seed resources found on Virtual Garden to delete.")
				} else if len(seedList.Items) == 0 {
					t.Log("No Seed resources found on Virtual Garden to delete.")
				} else {
					if isRemoteShoot {
						restoreVPA := suppressRemoteShootVPAManagedResource(ctx, t, targetWatchClient, releasePipelineCfg, remoteShootKey)
						defer restoreVPA()
					}
					if targetWatchClient != nil {
						cleanupStaleSeedManagedResources(ctx, t, targetWatchClient, seedList.Items)
					}
					for _, seedItem := range seedList.Items {
						seed := seedItem
						seedKey := client.ObjectKey{Name: seed.Name}
						t.Logf("Deleting Seed cluster %s", seed.Name)
						if err := deleteAndWaitForDeletion(ctx, virtualGardenClient, &seed, seedKey, waitForDeletionTimeout); err != nil {
							if *forceCleanup {
								t.Logf("Warning: Failed to delete Seed cluster gracefully: %v. Attempting force cleanup...", err)
								seedObj := &gardencorev1beta1.Seed{}
								if forceErr := forceRemoveFinalizers(ctx, virtualGardenClient, seedObj, seedKey); forceErr != nil {
									t.Logf("Warning: Failed to force-remove finalizers on Seed: %v", forceErr)
								}
							} else {
								t.Fatalf("Failed to delete Seed cluster gracefully: %v", err)
							}
						} else {
							t.Logf("Seed cluster '%s' deleted successfully.", seedKey)
						}
					}
				}
			}

			if targetWatchClient != nil {
				var kubeconfigPath string
				if isRemoteShoot {
					shootKubeconfigPath, err := gardener.GetShootKubeconfigPath(ctx, remoteShootCluster.VirtualGarden.Client, remoteShootKey)
					if err != nil {
						t.Logf("Warning: Failed to get remote Shoot cluster kubeconfig path: %v", err)
						return
					}
					defer os.Remove(shootKubeconfigPath)
					kubeconfigPath = shootKubeconfigPath
				} else {
					kubeconfigPath = targetKubeconfigPath
				}

				if kubeconfigPath != "" {
					t.Log("Uninstalling gardenlet helm chart")
					opts := helm.UninstallOptions{
						KubeconfigPath: kubeconfigPath,
						ReleaseName:    "gardenlet",
						Namespace:      gardenv1beta1constants.GardenNamespace,
						IgnoreNotFound: true,
					}
					if _, err := helm.Uninstall(opts); err != nil {
						t.Logf("Warning: Failed to uninstall gardenlet helm chart: %v", err)
					} else {
						t.Logf("Gardenlet helm chart uninstalled successfully.")
					}
				}

				deleteGardenletDeployment(ctx, t, targetWatchClient, "Seed cluster")

				secret := &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "gardenlet-kubeconfig",
						Namespace: gardenv1beta1constants.GardenNamespace,
					},
				}
				if err := targetWatchClient.Delete(ctx, secret); err != nil {
					if !apierrors.IsNotFound(err) {
						t.Logf("Warning: Failed to delete gardenlet-kubeconfig secret: %v", err)
					}
				} else {
					t.Logf("gardenlet-kubeconfig secret for seed is deleted successfully.")
				}
			}
		})

		// 4. Remove CommitHash Annotation from remote Shoot cluster that hosts the Seed cluster
		t.Run("Remove CommitHash Annotation from remote Shoot cluster that hosts the Seed cluster", func(t *testing.T) {
			if releasePipelineCfg == nil || releasePipelineCfg.Seed == nil || releasePipelineCfg.Seed.HostCluster == nil || releasePipelineCfg.Seed.HostCluster.Shoot == nil {
				t.Skip("Skipping annotation removal as Seed/HostCluster config is incomplete (e.g. only-garden run)")
			}
			remoteShootCluster := releasePipelineCfg.Seed.HostCluster.Shoot
			t.Logf("Removing artifacts version annotation from remote shoot cluster %s", remoteShootCluster.Name)
			remoteShootKey := client.ObjectKey{Name: remoteShootCluster.Name, Namespace: remoteShootCluster.Namespace}
			removeArtifactsVersionAnnotation(ctx, t, remoteShootCluster.VirtualGarden.Client, &gardencorev1beta1.Shoot{}, remoteShootKey, *gardenerArtifactsVersion)
		})
	}

	if *teardownLevel == "garden" {
		// 5. Clean up Garden CR
		if *virtualGardenProvider == "gdc" {
			t.Run("Delete Garden Cluster and Network Policy", func(t *testing.T) {
				if releasePipelineCfg == nil || releasePipelineCfg.RuntimeClusterClient == nil {
					t.Log("Warning: RuntimeClusterClient is nil, skipping Garden CR deletion.")
					return
				}

				// Ensure gardenlet deployment is deleted and completely gone before deleting Garden CR
				deleteGardenletDeployment(ctx, t, releasePipelineCfg.RuntimeClusterClient, "runtime cluster")

				t.Logf("Deleting Garden resource %s", releasePipelineCfg.RuntimeClusterName)
				garden := &unstructured.Unstructured{}
				garden.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "operator.gardener.cloud",
					Version: "v1alpha1",
					Kind:    "Garden",
				})
				gardenKey := client.ObjectKey{Name: releasePipelineCfg.RuntimeClusterName}
				err := deleteAndWaitForDeletion(ctx, releasePipelineCfg.RuntimeClusterClient, garden, gardenKey, waitForDeletionTimeout)
				if err != nil {
					if *forceCleanup {
						t.Logf("WARNING: Failed to delete Garden CR gracefully: %v. Force-removing finalizers now...", err)
						if err := forceRemoveFinalizers(ctx, releasePipelineCfg.RuntimeClusterClient, garden, gardenKey); err != nil {
							t.Logf("Warning: failed to force-remove finalizers on Garden CR: %v", err)
						} else {
							t.Log("Force-removed finalizers from Garden CR successfully.")
							if err := releasePipelineCfg.RuntimeClusterClient.Delete(ctx, garden); err != nil && !apierrors.IsNotFound(err) {
								t.Logf("Warning: failed to delete Garden CR after finalizer removal: %v", err)
							}
						}
					} else {
						t.Fatalf("Failed to delete Garden CR gracefully: %v", err)
					}
				} else {
					t.Logf("Garden CR %q deleted successfully.", releasePipelineCfg.RuntimeClusterName)
				}

				// Clean up Ingress ProjectNetworkPolicy from the management cluster
				t.Logf("Deleting Ingress ProjectNetworkPolicy for user cluster %q", releasePipelineCfg.RuntimeClusterName)
				var zone string
				for _, uc := range releasePipelineCfg.GDC.UserClusters {
					if uc.Name == releasePipelineCfg.RuntimeClusterName {
						zone = uc.Zone
						break
					}
				}
				if zone == "" {
					zone = releasePipelineCfg.GDC.Zones[0]
				}
				mgmtClient, ok := releasePipelineCfg.ManagementClients[zone]
				if !ok {
					t.Logf("Warning: failed to get management client for zone %s, skipping ProjectNetworkPolicy deletion", zone)
				} else {
					pnp := &unstructured.Unstructured{}
					pnp.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   "networking.gdc.goog",
						Version: "v1",
						Kind:    "ProjectNetworkPolicy",
					})
					pnp.SetName(fmt.Sprintf("allow-ingress-to-%s-vuc", releasePipelineCfg.RuntimeClusterName))
					pnp.SetNamespace(releasePipelineCfg.GDC.Project)
					if err := mgmtClient.Delete(ctx, pnp); err != nil && !apierrors.IsNotFound(err) {
						t.Logf("Warning: failed to delete ProjectNetworkPolicy %s: %v", pnp.GetName(), err)
					} else {
						t.Logf("ProjectNetworkPolicy %s deleted successfully from management cluster", pnp.GetName())
					}
				}
			})

			// 5.4 Uninstall gardener-operator Helm chart and clean up namespaces
			t.Run("Uninstall Gardener Operator and Clean Up Namespaces", func(t *testing.T) {
				if releasePipelineCfg == nil || releasePipelineCfg.RuntimeClusterClient == nil {
					t.Log("Warning: RuntimeClusterClient is nil, skipping operator cleanup.")
					return
				}

				kubeconfigPath, err := getRuntimeClusterKubeconfig(releasePipelineCfg)
				if err != nil {
					t.Logf("Warning: Failed to get runtime cluster kubeconfig path: %v. Skipping operator helm cleanup.", err)
					return
				}
				defer os.Remove(kubeconfigPath)

				// 1. Uninstall gardener-operator helm release
				t.Log("Uninstalling gardener-operator helm chart")
				opts := helm.UninstallOptions{
					KubeconfigPath: kubeconfigPath,
					ReleaseName:    "gardener-operator",
					Namespace:      "garden",
					IgnoreNotFound: true,
				}
				if _, err := helm.Uninstall(opts); err != nil {
					t.Logf("Warning: Failed to uninstall gardener-operator helm chart: %v", err)
				} else {
					t.Log("Gardener operator helm chart uninstalled successfully.")
				}

				// Delete webhook configurations to prevent block
				deleteWebhook := func(group, kind, name string) {
					obj := &unstructured.Unstructured{}
					obj.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   group,
						Version: "v1",
						Kind:    kind,
					})
					obj.SetName(name)
					if err := releasePipelineCfg.RuntimeClusterClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
						t.Logf("Warning: failed to delete webhook config %s: %v", name, err)
					}
				}
				deleteWebhook("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "gardener-operator")
				deleteWebhook("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "gardener-operator")
				deleteWebhook("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "etcd-druid")
				deleteWebhook("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "etcd-druid")

				// Force-remove finalizers from Extension resources to prevent CRD deletion block
				t.Log("Force-removing finalizers from Extension operator resources...")
				extensionsList := &unstructured.UnstructuredList{}
				extensionsList.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "operator.gardener.cloud",
					Version: "v1alpha1",
					Kind:    "ExtensionList",
				})
				if err := releasePipelineCfg.RuntimeClusterClient.List(ctx, extensionsList); err == nil {
					for _, item := range extensionsList.Items {
						key := client.ObjectKey{Name: item.GetName()}
						if err := forceRemoveFinalizers(ctx, releasePipelineCfg.RuntimeClusterClient, &item, key); err != nil {
							t.Logf("Warning: failed to force-remove finalizers on Extension %s: %v", item.GetName(), err)
						}
					}
				}

				// Force-remove finalizers from ManagedResource resources to prevent CRD deletion block
				t.Log("Force-removing finalizers from ManagedResource resources...")
				managedResourcesList := &unstructured.UnstructuredList{}
				managedResourcesList.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "resources.gardener.cloud",
					Version: "v1alpha1",
					Kind:    "ManagedResourceList",
				})
				if err := releasePipelineCfg.RuntimeClusterClient.List(ctx, managedResourcesList); err == nil {
					for _, item := range managedResourcesList.Items {
						key := client.ObjectKey{Namespace: item.GetNamespace(), Name: item.GetName()}
						if err := forceRemoveFinalizers(ctx, releasePipelineCfg.RuntimeClusterClient, &item, key); err != nil {
							t.Logf("Warning: failed to force-remove finalizers on ManagedResource %s/%s: %v", item.GetNamespace(), item.GetName(), err)
						}
					}
				}

				// Wait for CRDs to be completely deleted to prevent race conditions during re-install
				t.Log("Waiting for gardener-operator CRDs to be completely deleted...")
				crdNames := []string{
					"gardens.operator.gardener.cloud",
					"extensions.operator.gardener.cloud",
					"backupbuckets.extensions.gardener.cloud",
					"dnsrecords.extensions.gardener.cloud",
					"etcdcopybackupstasks.druid.gardener.cloud",
					"etcdopstasks.druid.gardener.cloud",
					"etcds.druid.gardener.cloud",
					"extensions.extensions.gardener.cloud",
					"managedresources.resources.gardener.cloud",
				}
				for _, crdName := range crdNames {
					if err := deleteCRD(ctx, releasePipelineCfg.RuntimeClusterClient, crdName); err != nil {
						t.Logf("Warning: failed to delete CRD %s: %v", crdName, err)
					}

					crd := &unstructured.Unstructured{}
					crd.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   "apiextensions.k8s.io",
						Version: "v1",
						Kind:    "CustomResourceDefinition",
					})
					crdKey := client.ObjectKey{Name: crdName}
					if pollErr := wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
						err := releasePipelineCfg.RuntimeClusterClient.Get(ctx, crdKey, crd)
						if err != nil {
							if apierrors.IsNotFound(err) {
								return true, nil
							}
							return false, err
						}
						return false, nil
					}); pollErr != nil {
						t.Fatalf("CRD %s was not completely deleted within 5 minutes: %v", crdName, pollErr)
					} else {
						t.Logf("CRD %s completely deleted.", crdName)
					}
				}

				// Delete webhook configurations to prevent namespace deletion blocks
				t.Log("Force-removing gardener webhook configurations before namespace cleanup...")
				deleteWebhookConfig := func(group, kind, name string) {
					obj := &unstructured.Unstructured{}
					obj.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   group,
						Version: "v1",
						Kind:    kind,
					})
					obj.SetName(name)
					if err := releasePipelineCfg.RuntimeClusterClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
						t.Logf("Warning: failed to delete webhook config %s: %v", name, err)
					}
				}
				deleteWebhookConfig("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "gardener-operator")
				deleteWebhookConfig("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "gardener-operator")
				deleteWebhookConfig("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "gardener-resource-manager")
				deleteWebhookConfig("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "gardener-resource-manager")
				deleteWebhookConfig("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "etcd-druid")
				deleteWebhookConfig("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "etcd-druid")

				// 2. Delete namespaces: garden and extension-provider-gdch-*
				t.Log("Cleaning up garden and extension namespaces...")
				nsList := &corev1.NamespaceList{}
				if err := releasePipelineCfg.RuntimeClusterClient.List(ctx, nsList); err != nil {
					t.Logf("Warning: Failed to list namespaces on user cluster: %v", err)
					return
				}

				for _, nsItem := range nsList.Items {
					nsName := nsItem.Name
					if nsName == "garden" || strings.HasPrefix(nsName, "extension-provider-gdch-") {
						t.Logf("Deleting namespace %s...", nsName)
						nsObj := &corev1.Namespace{
							ObjectMeta: metav1.ObjectMeta{
								Name: nsName,
							},
						}
						// Delete and wait for deletion (timeout 5 minutes)
						nsKey := client.ObjectKey{Name: nsName}
						if err := deleteAndWaitForDeletion(ctx, releasePipelineCfg.RuntimeClusterClient, nsObj, nsKey, 2*time.Minute); err != nil {
							if *forceCleanup {
								t.Logf("Warning: Failed to delete namespace %s gracefully: %v. Attempting force cleanup...", nsName, err)
								forceCleanupNamespaceComponents(ctx, releasePipelineCfg.RuntimeClusterClient, nsName, t)
								if forceErr := forceRemoveFinalizers(ctx, releasePipelineCfg.RuntimeClusterClient, nsObj, nsKey); forceErr != nil {
									t.Logf("Warning: Failed to force-remove finalizers on namespace %s: %v", nsName, forceErr)
								}
								if retryErr := deleteAndWaitForDeletion(ctx, releasePipelineCfg.RuntimeClusterClient, nsObj, nsKey, 1*time.Minute); retryErr != nil {
									t.Logf("Warning: Namespace %s deletion retry failed: %v", nsName, retryErr)
								}
							} else {
								t.Errorf("Failed to delete namespace %s gracefully: %v", nsName, err)
							}
						} else {
							t.Logf("Namespace %s deleted successfully.", nsName)
						}
					}
				}
			})

			// 5.5 Delete ResourceRecordSet on Global API cluster
			if releasePipelineCfg != nil && releasePipelineCfg.GlobalAPIClient != nil && releasePipelineCfg.GDC != nil {
				t.Run("Delete Virtual Garden ResourceRecordSet", func(t *testing.T) {
					zone := ""
					for _, uc := range releasePipelineCfg.GDC.UserClusters {
						if uc.Name == releasePipelineCfg.RuntimeClusterName {
							zone = uc.Zone
							break
						}
					}
					if zone == "" && len(releasePipelineCfg.GDC.Zones) > 0 {
						zone = releasePipelineCfg.GDC.Zones[0]
					}
					dnsRecordName := fmt.Sprintf("%s-%s-%s-virtual-garden", releasePipelineCfg.RuntimeClusterName, releasePipelineCfg.GDC.Project, zone)
					rrs := &unstructured.Unstructured{}
					rrs.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   "networking.global.gdc.goog",
						Version: "v1",
						Kind:    "ResourceRecordSet",
					})
					rrs.SetName(dnsRecordName)
					rrs.SetNamespace(releasePipelineCfg.GDC.Project)
					if err := releasePipelineCfg.GlobalAPIClient.Delete(ctx, rrs); err != nil && !apierrors.IsNotFound(err) {
						t.Logf("Warning: failed to delete ResourceRecordSet %q: %v", dnsRecordName, err)
					} else {
						t.Logf("ResourceRecordSet %q deleted from Global API.", dnsRecordName)
					}
				})
			}

			// 5.6 Force Cleanup Garden CR (Fallback)
			if *forceCleanup {
				t.Run("Force Cleanup Garden CR Fallback", func(t *testing.T) {
					if releasePipelineCfg == nil || releasePipelineCfg.RuntimeClusterClient == nil {
						return
					}
					garden := &unstructured.Unstructured{}
					garden.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   "operator.gardener.cloud",
						Version: "v1alpha1",
						Kind:    "Garden",
					})
					gardenKey := client.ObjectKey{Name: releasePipelineCfg.RuntimeClusterName}

					// Check if Garden CR still exists
					err := releasePipelineCfg.RuntimeClusterClient.Get(ctx, gardenKey, garden)
					if err != nil {
						if apierrors.IsNotFound(err) {
							t.Log("Garden CR is already deleted.")
							return
						}
						t.Logf("Warning: failed to check Garden CR existence: %v", err)
						return
					}

					t.Log("Garden CR still exists after operator uninstallation. Force-removing webhooks and finalizers...")

					// 1. Delete webhook configurations to prevent block
					deleteWebhook := func(group, kind, name string) {
						obj := &unstructured.Unstructured{}
						obj.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   group,
							Version: "v1",
							Kind:    kind,
						})
						obj.SetName(name)
						if err := releasePipelineCfg.RuntimeClusterClient.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
							t.Logf("Warning: failed to delete webhook config %s: %v", name, err)
						}
					}
					deleteWebhook("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "gardener-operator")
					deleteWebhook("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "gardener-operator")
					deleteWebhook("admissionregistration.k8s.io", "ValidatingWebhookConfiguration", "etcd-druid")
					deleteWebhook("admissionregistration.k8s.io", "MutatingWebhookConfiguration", "etcd-druid")

					// 2. Force-remove finalizers from Garden CR
					if err := forceRemoveFinalizers(ctx, releasePipelineCfg.RuntimeClusterClient, garden, gardenKey); err != nil {
						t.Logf("Warning: failed to force-remove finalizers on Garden CR: %v", err)
					} else {
						t.Log("Force-removed finalizers from Garden CR successfully.")
					}

					// 3. Force-remove finalizers from Extension resources
					extensionsList := &unstructured.UnstructuredList{}
					extensionsList.SetGroupVersionKind(schema.GroupVersionKind{
						Group:   "operator.gardener.cloud",
						Version: "v1alpha1",
						Kind:    "ExtensionList",
					})
					if err := releasePipelineCfg.RuntimeClusterClient.List(ctx, extensionsList); err == nil {
						for _, item := range extensionsList.Items {
							key := client.ObjectKey{Name: item.GetName()}
							if err := forceRemoveFinalizers(ctx, releasePipelineCfg.RuntimeClusterClient, &item, key); err != nil {
								t.Logf("Warning: failed to force-remove finalizers on Extension %s: %v", item.GetName(), err)
							}
						}
					}

					// 4. Confirm deletion
					if err := deleteAndWaitForDeletion(ctx, releasePipelineCfg.RuntimeClusterClient, garden, gardenKey, 1*time.Minute); err != nil {
						t.Logf("Warning: Garden CR final deletion retry failed: %v", err)
					} else {
						t.Log("Garden CR force deleted successfully.")
					}
				})
			}
		} else {
			t.Run("Remove CommitHash Annotation from Garden CR on runtime cluster", func(t *testing.T) {
				if releasePipelineCfg == nil || releasePipelineCfg.RuntimeClusterClient == nil {
					t.Log("Warning: RuntimeClusterClient is nil, skipping Garden CR annotation removal.")
					return
				}
				t.Log("Removing artifacts version annotation from Garden CR")
				gardenList := &operatorv1alpha1.GardenList{}
				if err := releasePipelineCfg.RuntimeClusterClient.List(ctx, gardenList); err != nil {
					t.Logf("Warning: Failed to list Garden resources: %v", err)
					return
				}
				if len(gardenList.Items) == 0 {
					t.Logf("Warning: Garden cluster deployment not found for version %q", *gardenerArtifactsVersion)
					return
				}
				garden := gardenList.Items[0]
				gardenKey := client.ObjectKey{Name: garden.Name, Namespace: garden.Namespace}
				removeArtifactsVersionAnnotation(ctx, t, releasePipelineCfg.RuntimeClusterClient, &garden, gardenKey, *gardenerArtifactsVersion)
			})
		}

		// 6. Delete Parent Subnet and child subnets
		t.Run("DeleteParentSubnet", func(t *testing.T) {
			if releasePipelineCfg == nil || releasePipelineCfg.GDC == nil || releasePipelineCfg.GlobalAPIClient == nil {
				t.Log("Warning: GDC configuration is missing, skipping DeleteParentSubnet.")
				return
			}
			parentSubnetName := gardener.GetShootParentSubnetName(*gardenerArtifactsVersion)
			t.Logf("Cleaning up subnets for commit %s (parent: %s)", *gardenerArtifactsVersion, parentSubnetName)

			// Step A0: Clean up Zonal Subnets (ipam.gdc.goog/v1) across all zones
			if releasePipelineCfg.GDCClient != nil && len(releasePipelineCfg.GDC.Zones) > 0 {
				for _, zone := range releasePipelineCfg.GDC.Zones {
					t.Logf("Cleaning up zonal subnets in zone %s...", zone)
					zonalClient, err := pkgConfig.GetManagementClient(releasePipelineCfg.GDCClient, releasePipelineCfg.GDC.Org, zone)
					if err != nil {
						t.Logf("Warning: Failed to get zonal client for zone %s: %v", zone, err)
						continue
					}

					for iteration := 0; iteration < 10; iteration++ {
						zonalSubnetList := &unstructured.UnstructuredList{}
						zonalSubnetList.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   "ipam.gdc.goog",
							Version: "v1",
							Kind:    "SubnetList",
						})
						if err := zonalClient.List(ctx, zonalSubnetList, client.InNamespace(releasePipelineCfg.GDC.Project)); err != nil {
							t.Logf("Warning: Failed to list zonal subnets in zone %s: %v", zone, err)
							break
						}

						parentMap := make(map[string]string)
						childrenCount := make(map[string]int)
						subnetObjects := make(map[string]unstructured.Unstructured)

						for _, item := range zonalSubnetList.Items {
							s := item
							name := s.GetName()
							subnetObjects[name] = s
							parentRef, found, _ := unstructured.NestedMap(s.Object, "spec", "parentReference")
							if found && parentRef != nil {
								if pName, ok := parentRef["name"].(string); ok && pName != "" {
									parentMap[name] = pName
									childrenCount[pName]++
								}
							}
						}

						// A zonal subnet is related to this commit if its name contains the shoot name prefix (e.g. sh-...) or matches the parent
						shootName := fmt.Sprintf("sh-%s", releaseConfig.GetCommitHashOrSanitize(*gardenerArtifactsVersion))
						zonalShootPrefix := fmt.Sprintf("z-sh-%s", releaseConfig.GetCommitHashOrSanitize(*gardenerArtifactsVersion))
						isRelatedZonalSubnet := func(name string) bool {
							curr := name
							visited := make(map[string]bool)
							for curr != "" && !visited[curr] {
								visited[curr] = true
								if strings.Contains(curr, *gardenerArtifactsVersion) || strings.Contains(curr, shootName) || strings.HasPrefix(curr, zonalShootPrefix) {
									return true
								}
								curr = parentMap[curr]
							}
							return false
						}

						var leafZonalSubnets []string
						for name := range subnetObjects {
							if isRelatedZonalSubnet(name) && childrenCount[name] == 0 {
								leafZonalSubnets = append(leafZonalSubnets, name)
							}
						}

						if len(leafZonalSubnets) == 0 {
							break
						}

						t.Logf("Found %d leaf zonal subnets in zone %s: %v", len(leafZonalSubnets), zone, leafZonalSubnets)
						for _, leafName := range leafZonalSubnets {
							t.Logf("Deleting zonal subnet %s in zone %s...", leafName, zone)
							childKey := client.ObjectKey{Name: leafName, Namespace: releasePipelineCfg.GDC.Project}
							leafObj := subnetObjects[leafName]
							if err := zonalClient.Delete(ctx, &leafObj); err != nil && !apierrors.IsNotFound(err) {
								t.Logf("Warning: Failed to delete zonal subnet %s: %v. Force-removing finalizers...", leafName, err)
								if forceErr := forceRemoveFinalizers(ctx, zonalClient, &leafObj, childKey); forceErr != nil {
									t.Logf("Warning: Failed to force-remove finalizers on zonal subnet %s: %v", leafName, forceErr)
								}
							} else {
								t.Logf("Zonal subnet '%s' deleted successfully.", childKey)
							}
						}
					}
				}
			}

			// Step A: Iterative bottom-up deletion of all descendant subnets (leaves first)
			for iteration := 0; iteration < 10; iteration++ {
				subnetList := &unstructured.UnstructuredList{}
				subnetList.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "ipam.global.gdc.goog",
					Version: "v1",
					Kind:    "SubnetList",
				})
				if err := releasePipelineCfg.GlobalAPIClient.List(ctx, subnetList, client.InNamespace(releasePipelineCfg.GDC.Project)); err != nil {
					t.Logf("Warning: Failed to list subnets in namespace %s: %v", releasePipelineCfg.GDC.Project, err)
					break
				}

				// Build parent-child relationships
				parentMap := make(map[string]string)
				childrenCount := make(map[string]int)
				subnetObjects := make(map[string]unstructured.Unstructured)

				for _, item := range subnetList.Items {
					s := item
					name := s.GetName()
					subnetObjects[name] = s
					parentRef, found, _ := unstructured.NestedMap(s.Object, "spec", "parentReference")
					if found && parentRef != nil {
						if pName, ok := parentRef["name"].(string); ok && pName != "" {
							parentMap[name] = pName
							childrenCount[pName]++
						}
					}
				}

				// Helper to determine if a subnet descends from parentSubnetName
				isDescendant := func(name string) bool {
					curr := name
					visited := make(map[string]bool)
					for curr != "" && !visited[curr] {
						visited[curr] = true
						p := parentMap[curr]
						if p == parentSubnetName {
							return true
						}
						curr = p
					}
					return false
				}

				// Find leaf descendants (descendants with 0 remaining children)
				var leafDescendants []string
				for name := range subnetObjects {
					if isDescendant(name) && childrenCount[name] == 0 {
						leafDescendants = append(leafDescendants, name)
					}
				}

				if len(leafDescendants) == 0 {
					break
				}

				t.Logf("Found %d leaf descendant subnets in iteration %d: %v", len(leafDescendants), iteration, leafDescendants)
				for _, leafName := range leafDescendants {
					t.Logf("Deleting descendant subnet %s...", leafName)
					childKey := client.ObjectKey{Name: leafName, Namespace: releasePipelineCfg.GDC.Project}
					leafObj := subnetObjects[leafName]
					if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, childKey, 1*time.Minute, t); err != nil {
						t.Logf("Warning: Failed to delete descendant subnet %s gracefully: %v. Force-removing finalizers...", leafName, err)
						if forceErr := forceRemoveFinalizers(ctx, releasePipelineCfg.GlobalAPIClient, &leafObj, childKey); forceErr != nil {
							t.Logf("Warning: Failed to force-remove finalizers on descendant subnet %s: %v", leafName, forceErr)
						}
					} else {
						t.Logf("Descendant subnet '%s' deleted successfully.", childKey)
					}
				}
			}

			// Step B: Delete parent subnet
			t.Logf("Deleting parent subnet for commit %s", *gardenerArtifactsVersion)
			subnetKey := client.ObjectKey{Name: parentSubnetName, Namespace: releasePipelineCfg.GDC.Project}
			if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnetKey, waitForDeletionTimeout, t); err != nil {
				t.Logf("Warning: Failed to delete parent subnet gracefully: %v. Attempting force-removal...", err)
				parentObj := &unstructured.Unstructured{}
				parentObj.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "ipam.global.gdc.goog",
					Version: "v1",
					Kind:    "Subnet",
				})
				if forceErr := forceRemoveFinalizers(ctx, releasePipelineCfg.GlobalAPIClient, parentObj, subnetKey); forceErr != nil {
					t.Logf("Warning: Failed to force-remove finalizers on parent subnet: %v", forceErr)
				}
			} else {
				t.Logf("Parent subnet '%s' deleted successfully.", subnetKey)
			}
		})

		// 7. Check for leaked GDC resources
		t.Run("Check for Leaked GDC Resources", func(t *testing.T) {
			t.Log("[Custom Teardown] Auditing GDC project namespace for leaked physical resources...")
			checkForLeakedResources(ctx, t, releasePipelineCfg)
		})
	}
}

// forceRemoveFinalizers removes all finalizers from a resource.
func forceRemoveFinalizers(ctx context.Context, c client.Client, obj client.Object, key client.ObjectKey) error {
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
	obj.SetFinalizers(nil)
	return c.Patch(ctx, obj, patch)
}

func forceCleanupNamespaceComponents(ctx context.Context, c client.Client, namespace string, t *testing.T) {
	t.Logf("[Force Cleanup] Cleaning up remaining resources with finalizers in namespace %s...", namespace)

	// List of GVKs that commonly have finalizers blocking namespace deletion
	gvks := []schema.GroupVersionKind{
		{Group: "druid.gardener.cloud", Version: "v1alpha1", Kind: "EtcdList"},
		{Group: "resources.gardener.cloud", Version: "v1alpha1", Kind: "ManagedResourceList"},
		{Group: "fluentbit.fluent.io", Version: "v1alpha2", Kind: "FluentBitList"},
		{Group: "", Version: "v1", Kind: "SecretList"},
	}

	for _, gvk := range gvks {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk)
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			// Skip if CRD is not registered
			if apierrors.IsNotFound(err) || strings.Contains(err.Error(), "no matches for kind") {
				continue
			}
			t.Logf("Warning: failed to list %s in namespace %s: %v", gvk.Kind, namespace, err)
			continue
		}

		for _, item := range list.Items {
			t.Logf("[Force Cleanup] Removing finalizers from %s %q in namespace %s", item.GetKind(), item.GetName(), namespace)
			key := client.ObjectKey{Name: item.GetName(), Namespace: namespace}
			if err := forceRemoveFinalizers(ctx, c, &item, key); err != nil {
				t.Logf("Warning: failed to remove finalizer from %s %q: %v", item.GetKind(), item.GetName(), err)
			}
		}
	}
}

// deleteAndWaitForDeletion deletes a k8s resource and waits until it is gone.
func deleteAndWaitForDeletion(ctx context.Context, c client.Client, obj client.Object, key client.ObjectKey, timeout time.Duration) error {
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get %T '%s' before deletion: %w", obj, key, err)
	}

	switch resource := obj.(type) {
	case *gardencorev1beta1.Shoot:
		patch := client.MergeFrom(resource.DeepCopyObject().(client.Object))

		// Gardener requires a special annotation to confirm shoot deletion and operation retry.
		if resource.Annotations == nil {
			resource.Annotations = make(map[string]string)
		}
		resource.Annotations[gardenv1beta1constants.ConfirmationDeletion] = "true"
		resource.Annotations[gardenv1beta1constants.GardenerOperation] = gardenv1beta1constants.ShootOperationRetry

		if err := c.Patch(ctx, resource, patch); err != nil {
			return fmt.Errorf("failed to annotate shoot '%s' for deletion: %w", key, err)
		}
	case *unstructured.Unstructured:
		if resource.GetKind() == "Garden" {
			patch := client.MergeFrom(resource.DeepCopyObject().(client.Object))
			annotations := resource.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			annotations["confirmation.gardener.cloud/deletion"] = "true"
			resource.SetAnnotations(annotations)
			if err := c.Patch(ctx, resource, patch); err != nil {
				return fmt.Errorf("failed to annotate garden '%s' for deletion: %w", key, err)
			}
		}
	}

	if err := c.Delete(ctx, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to issue delete for %T '%s': %w", obj, key, err)
	}

	// Poll until the resource is deleted.
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
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

// Helper to remove a specific annotation value from an object and update it.
func removeArtifactsVersionAnnotation(ctx context.Context, t *testing.T, c client.Client, obj client.Object, key client.ObjectKey, gardenerArtifactsVersion string) {
	if err := c.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			t.Logf("Warning: Resource %T '%s' not found, skipping annotation removal.", obj, key)
			return
		}
		t.Logf("Warning: Failed to get %T '%s': %v", obj, key, err)
		return
	}

	annotations := obj.GetAnnotations()
	if val, ok := annotations[releaseConfig.CommitHashAnnotation]; ok && val == gardenerArtifactsVersion {
		patch := client.MergeFrom(obj.DeepCopyObject().(client.Object))
		delete(annotations, releaseConfig.CommitHashAnnotation)
		obj.SetAnnotations(annotations)
		if err := c.Patch(ctx, obj, patch); err != nil {
			t.Logf("Warning: Failed to remove annotation from %T %s: %v", obj, key, err)
		} else {
			t.Logf("Removed artifacts version annotation from %T '%s'.", obj, key)
		}
		return
	}
	t.Logf("No matching artifacts version annotation found on %T '%s'.", obj, key)
}

// checkForLeakedResources lists GDC VMs, Disks, and Services in the project namespace and checks if they contain the Shoot name.
func checkForLeakedResources(ctx context.Context, t *testing.T, cfg *pkgConfig.ReleaseTestConfig) {
	if cfg == nil || cfg.GDC == nil {
		t.Log("Warning: GDC configuration is missing, skipping leak check.")
		return
	}
	if cfg.GlobalAPIClient == nil {
		t.Log("Warning: GlobalAPIClient is nil, skipping leak check.")
		return
	}
	namespace := cfg.GDC.Project
	shootName := fmt.Sprintf("sh-%s", releaseConfig.GetCommitHashOrSanitize(*gardenerArtifactsVersion))
	if cfg.TestShoot != nil && cfg.TestShoot.Name != "" {
		shootName = cfg.TestShoot.Name
	}

	var leaks []string

	// 1. Check VirtualMachines
	vmList := &unstructured.UnstructuredList{}
	vmList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "virtualmachine.gdc.goog",
		Version: "v1",
		Kind:    "VirtualMachineList",
	})
	if err := cfg.GlobalAPIClient.List(ctx, vmList, client.InNamespace(namespace)); err != nil {
		t.Logf("Warning: Failed to list VirtualMachines for leak check: %v", err)
	} else {
		for _, vm := range vmList.Items {
			name := vm.GetName()
			if strings.Contains(name, shootName) {
				leaks = append(leaks, fmt.Sprintf("VirtualMachine: %s/%s", namespace, name))
			}
		}
	}

	// 2. Check VirtualMachineDisks
	diskList := &unstructured.UnstructuredList{}
	diskList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "virtualmachine.gdc.goog",
		Version: "v1",
		Kind:    "VirtualMachineDiskList",
	})
	if err := cfg.GlobalAPIClient.List(ctx, diskList, client.InNamespace(namespace)); err != nil {
		t.Logf("Warning: Failed to list VirtualMachineDisks for leak check: %v", err)
	} else {
		for _, disk := range diskList.Items {
			name := disk.GetName()
			if strings.Contains(name, shootName) {
				leaks = append(leaks, fmt.Sprintf("VirtualMachineDisk: %s/%s", namespace, name))
			}
		}
	}

	// 3. Check LoadBalancer Services
	svcList := &corev1.ServiceList{}
	if err := cfg.GlobalAPIClient.List(ctx, svcList, client.InNamespace(namespace)); err != nil {
		t.Logf("Warning: Failed to list Services for leak check: %v", err)
	} else {
		for _, svc := range svcList.Items {
			if svc.Spec.Type == corev1.ServiceTypeLoadBalancer {
				name := svc.GetName()
				if strings.Contains(name, shootName) {
					leaks = append(leaks, fmt.Sprintf("Service (LoadBalancer): %s/%s", namespace, name))
				}
			}
		}
	}

	if len(leaks) > 0 {
		t.Errorf("[LEAK DETECTED] Leaked resources in GDC namespace %s matching shoot %s:\n%s", namespace, shootName, strings.Join(leaks, "\n"))
	} else {
		t.Log("[Custom Teardown] Clean audit: No leaked GDC resources detected.")
	}
}

func TestCheckForLeakedResources_NilShoot(t *testing.T) {
	s := runtime.NewScheme()
	corev1.AddToScheme(s)

	fakeClient := fake.NewClientBuilder().WithScheme(s).Build()

	cfg := &pkgConfig.ReleaseTestConfig{
		GlobalAPIClient: fakeClient,
		GDC: &pkgConfig.GDCConfig{
			Project: "test-gdc-project",
		},
		TestShoot: nil,
	}

	testVer := "test-ver-12345"
	gardenerArtifactsVersion = &testVer

	// Should not panic when TestShoot is nil
	checkForLeakedResources(context.Background(), t, cfg)
}

func TestDeleteAndWaitForDeletion_Annotations(t *testing.T) {
	s := runtime.NewScheme()
	gardencorev1beta1.AddToScheme(s)

	shoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-shoot",
			Namespace: "test-ns",
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(shoot).Build()

	ctx := context.Background()
	key := client.ObjectKey{Name: "test-shoot", Namespace: "test-ns"}

	// Delete and check annotations
	if err := deleteAndWaitForDeletion(ctx, fakeClient, shoot, key, 5*time.Second); err != nil {
		t.Fatalf("deleteAndWaitForDeletion failed: %v", err)
	}

	// Verify annotations were set before delete
	if val := shoot.Annotations[gardenv1beta1constants.ConfirmationDeletion]; val != "true" {
		t.Errorf("Expected annotation %s=true, got %s", gardenv1beta1constants.ConfirmationDeletion, val)
	}
	if val := shoot.Annotations[gardenv1beta1constants.GardenerOperation]; val != gardenv1beta1constants.ShootOperationRetry {
		t.Errorf("Expected annotation %s=%s, got %s", gardenv1beta1constants.GardenerOperation, gardenv1beta1constants.ShootOperationRetry, val)
	}
}

func TestCleanupSeedCluster_UseUserClusterAsSeed(t *testing.T) {
	s := runtime.NewScheme()
	corev1.AddToScheme(s)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardenlet-kubeconfig",
			Namespace: gardenv1beta1constants.GardenNamespace,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(s).WithObjects(secret).Build()
	ctx := context.Background()

	if err := fakeClient.Delete(ctx, secret); err != nil {
		t.Fatalf("failed to delete gardenlet-kubeconfig secret: %v", err)
	}

	// Verify it is deleted
	err := fakeClient.Get(ctx, client.ObjectKey{Name: "gardenlet-kubeconfig", Namespace: gardenv1beta1constants.GardenNamespace}, &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("expected NotFound error after deletion, got %v", err)
	}
}

func getRuntimeClusterKubeconfig(releaseConfigData *pkgConfig.ReleaseTestConfig) (string, error) {
	if releaseConfigData.GDC != nil && releaseConfigData.GDCClient != nil && len(releaseConfigData.GDC.UserClusters) > 0 {
		for _, u := range releaseConfigData.GDC.UserClusters {
			if u.Name == releaseConfigData.RuntimeClusterName {
				return gdch.GetUserClusterKubeconfig(releaseConfigData.GDCClient, u.Zone, u.Project, u.Name)
			}
		}
	}
	return "", fmt.Errorf("could not resolve kubeconfig path for runtime cluster %s", releaseConfigData.RuntimeClusterName)
}

func deleteCRD(ctx context.Context, c client.Client, crdName string) error {
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apiextensions.k8s.io",
		Version: "v1",
		Kind:    "CustomResourceDefinition",
	})
	err := c.Get(ctx, client.ObjectKey{Name: crdName}, crd)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// Remove deletion-protected label and finalizers
	labels := crd.GetLabels()
	if labels != nil {
		delete(labels, "gardener.cloud/deletion-protected")
		crd.SetLabels(labels)
	}
	crd.SetFinalizers(nil)

	if err := c.Update(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to remove protection from CRD %s: %w", crdName, err)
	}

	// Now delete it
	if err := c.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("failed to delete CRD %s: %w", crdName, err)
	}

	return nil
}

func checkVirtualGardenConnectivity(ctx context.Context, c client.Client) bool {
	if c == nil {
		return false
	}
	sanityCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var nsList corev1.NamespaceList
	if err := c.List(sanityCtx, &nsList, client.Limit(1)); err != nil {
		return false
	}
	return true
}

func deleteGardenletDeployment(ctx context.Context, t *testing.T, targetClient client.WithWatch, clusterName string) {
	t.Logf("Deleting and waiting for gardenlet deployment to be deleted on %s", clusterName)
	gardenletDeploy := &unstructured.Unstructured{}
	gardenletDeploy.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "Deployment",
	})
	gardenletKey := client.ObjectKey{
		Name:      "gardenlet",
		Namespace: gardenv1beta1constants.GardenNamespace,
	}
	if err := deleteAndWaitForDeletion(ctx, targetClient, gardenletDeploy, gardenletKey, 2*time.Minute); err != nil {
		t.Logf("Warning: Failed to wait for gardenlet deployment deletion on %s: %v", clusterName, err)
	} else {
		t.Logf("Gardenlet deployment deleted and confirmed gone successfully on %s.", clusterName)
	}
}

func suppressRemoteShootVPAManagedResource(ctx context.Context, t *testing.T, targetWatchClient client.Client, releaseConfigData *pkgConfig.ReleaseTestConfig, remoteShootKey client.ObjectKey) func() {
	var cleanups []func()

	if targetWatchClient != nil {
		crb := &unstructured.Unstructured{}
		crb.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   "rbac.authorization.k8s.io",
			Version: "v1",
			Kind:    "ClusterRoleBinding",
		})
		crbKey := client.ObjectKey{Name: "gardener.cloud:target:resource-manager"}
		if err := targetWatchClient.Get(ctx, crbKey, crb); err == nil {
			origSubjects, hasSubjects, _ := unstructured.NestedSlice(crb.Object, "subjects")
			if hasSubjects && len(origSubjects) > 0 {
				unstructured.RemoveNestedField(crb.Object, "subjects")
				if updateErr := targetWatchClient.Update(ctx, crb); updateErr == nil {
					t.Logf("Temporarily suspended ClusterRoleBinding %s on remote Shoot %s during Seed deletion", crbKey.Name, remoteShootKey)
					cleanups = append(cleanups, func() {
						latestCRB := &unstructured.Unstructured{}
						latestCRB.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   "rbac.authorization.k8s.io",
							Version: "v1",
							Kind:    "ClusterRoleBinding",
						})
						if getErr := targetWatchClient.Get(ctx, crbKey, latestCRB); getErr == nil {
							_ = unstructured.SetNestedSlice(latestCRB.Object, origSubjects, "subjects")
							if restoreErr := targetWatchClient.Update(ctx, latestCRB); restoreErr != nil {
								t.Logf("Warning: Failed to restore subjects on ClusterRoleBinding %s: %v", crbKey.Name, restoreErr)
							} else {
								t.Logf("Restored subjects on ClusterRoleBinding %s after Seed deletion", crbKey.Name)
							}
						}
					})
				}
			}
		}
	}

	if releaseConfigData != nil && releaseConfigData.GDC != nil && releaseConfigData.GDCClient != nil {
		if userClusterClients, err := pkgConfig.GetGDCUserClusterClients(releaseConfigData.GDC.UserClusters, releaseConfigData.GDCClient); err == nil {
			shootNamespace := fmt.Sprintf("shoot--%s--%s", remoteShootKey.Namespace, remoteShootKey.Name)
			for clusterName, ucClient := range userClusterClients {
				mr := &unstructured.Unstructured{}
				mr.SetGroupVersionKind(schema.GroupVersionKind{
					Group:   "resources.gardener.cloud",
					Version: "v1alpha1",
					Kind:    "ManagedResource",
				})
				mrKey := client.ObjectKey{Name: "shoot-core-vpa", Namespace: shootNamespace}
				if err := ucClient.Get(ctx, mrKey, mr); err != nil {
					continue
				}
				ann := mr.GetAnnotations()
				if ann == nil {
					ann = make(map[string]string)
				}
				ann["resources.gardener.cloud/ignore"] = "true"
				mr.SetAnnotations(ann)
				if err := ucClient.Update(ctx, mr); err == nil {
					t.Logf("Temporarily set resources.gardener.cloud/ignore=true on %s/shoot-core-vpa in %s during Seed deletion", shootNamespace, clusterName)
					targetUCClient := ucClient
					targetClusterName := clusterName
					cleanups = append(cleanups, func() {
						latestMR := &unstructured.Unstructured{}
						latestMR.SetGroupVersionKind(schema.GroupVersionKind{
							Group:   "resources.gardener.cloud",
							Version: "v1alpha1",
							Kind:    "ManagedResource",
						})
						if err := targetUCClient.Get(ctx, mrKey, latestMR); err != nil {
							return
						}
						latestAnn := latestMR.GetAnnotations()
						if latestAnn != nil {
							delete(latestAnn, "resources.gardener.cloud/ignore")
							latestMR.SetAnnotations(latestAnn)
							_ = targetUCClient.Update(ctx, latestMR)
							t.Logf("Restored %s/shoot-core-vpa reconciliation in %s after Seed deletion", shootNamespace, targetClusterName)
						}
					})
				}
			}
		}
	}

	return func() {
		for _, fn := range cleanups {
			fn()
		}
	}
}

func cleanupStaleSeedManagedResources(ctx context.Context, t *testing.T, seedClient client.Client, activeSeeds []gardencorev1beta1.Seed) {
	activeNames := make(map[string]bool, len(activeSeeds))
	for _, s := range activeSeeds {
		activeNames["referenced-resources-"+s.Name] = true
	}
	mrList := &unstructured.UnstructuredList{}
	mrList.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "resources.gardener.cloud",
		Version: "v1alpha1",
		Kind:    "ManagedResourceList",
	})
	if err := seedClient.List(ctx, mrList, client.InNamespace(gardenv1beta1constants.GardenNamespace)); err != nil {
		return
	}
	for _, item := range mrList.Items {
		name := item.GetName()
		if strings.HasPrefix(name, "referenced-resources-seed-") && !activeNames[name] {
			mr := item
			t.Logf("Removing stale ManagedResource %s/%s before Seed deletion", mr.GetNamespace(), name)
			mr.SetFinalizers(nil)
			_ = seedClient.Update(ctx, &mr)
			_ = seedClient.Delete(ctx, &mr)
		}
	}
}
