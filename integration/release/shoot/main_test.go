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

package shoot

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	gardenv1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	securityv1alpha1 "github.com/gardener/gardener/pkg/apis/security/v1alpha1"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/subnet"
)

var (
	gardenerArtifactsVersion       = flag.String("gardener-artifacts-version", "", "The short artifacts version for gardener repo")
	externalDNSArtifactsVersion    = flag.String("external-dns-artifacts-version", "", "The short artifacts version for external-dns-management repo")
	externalDNSManagementImageName = flag.String("external-dns-management-image-name", "external-dnsman2-gdch", "The name of the external DNS management image")
	releaseConfigurationFilePath   = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	virtualGardenProvider          = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
	workerPoolMinimum              = flag.Int("worker-pool-minimum", 2, "The minimum number of machines per worker pool")
	workerPoolMaximum              = flag.Int("worker-pool-maximum", 2, "The maximum number of machines per worker pool")
)

const (
	gardenProjectName                   = "garden"
	waitForShootCreationTimeout         = 45 * time.Minute
	kubevirtStorageClassName            = "kubevirt"
	etcdMainStorageSize                 = "25Gi"
	etcdEventsStorageSize               = "10Gi"
	etcdMainPVCNameFormat               = "main-etcd-etcd-main-%d"
	etcdEventsPVCNameFormat             = "etcd-events-etcd-events-%d"
	cdiUsePopulatorAnnotationKey        = "cdi.kubevirt.io/storage.usePopulator"
	cdiBindImmediateRequestedAnnotation = "cdi.kubevirt.io/storage.bind.immediate.requested"
	selectedNodeAnnotationKey           = "volume.kubernetes.io/selected-node"
	stuckEtcdMountRecoveryThreshold     = 3 * time.Minute
	// subnetPrefixLength is set to /27 (32 IPs).
	// Rationale for /27:
	// 1. Capacity: A /27 subnet provides 32 IPs, which splitCIDR divides into /28 (16 IPs)
	//    per zone. This is plenty for the test worker VM pools (typically 3 nodes).
	// 2. Coexistence: The shared parent subnet group (release-pipeline-parent-subnet-group)
	//    is backed by a /26 CIDR (64 IPs total). Allocating /27 leaves 32 IPs in the pool.
	// Why /26 backfired:
	// - Allocating /26 consumed the entire 64-IP parent pool (100% utilization).
	// - Subsequent parallel test stages (e.g. 5.3 LoadBalancer services and 5.4 DNS lifecycle)
	//   dynamically request branch subnets (such as /31 for ILBs) from the same parent pool.
	// - With /26, IPAM failed with "No resource could allocate a /31 CIDR", causing LB IP
	//   allocation timeouts and cascading test failures.
	subnetPrefixLength        = 27
	waitForSubnetReadyTimeout = 5 * time.Minute
)

func TestCreateShootCluster(t *testing.T) {
	validateFlags(t)

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

	// create shoot's parent subnet
	ctx := context.Background()
	t.Logf("Ensuring shoot parent subnet for commit %s", *gardenerArtifactsVersion)
	subnetName := gardener.GetShootParentSubnetName(*gardenerArtifactsVersion)
	shootNodeCIDR, err := subnet.EnsureSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnet.SubnetOptions{
		Namespace:         releasePipelineCfg.GDC.Project,
		Name:              subnetName,
		ParentSubnetGroup: releasePipelineCfg.GDC.ParentPrivateSubnetGroup,
		PrefixLength:      subnetPrefixLength,
	}, waitForSubnetReadyTimeout, t)
	if err != nil {
		t.Fatalf("failed to ensure subnet %s", err)
	}

	// Create Image Pull Secret
	gardenClient := releasePipelineCfg.TestShoot.VirtualGarden.Client
	t.Log("Creating image pull secret in virtual garden")
	if err := gardener.CreateImagePullSecret(ctx, gardenClient, releasePipelineCfg.ImagePullCredentials); err != nil {
		t.Fatalf("failed to create image pull secret: %v", err)
	}

	// Deploy Networking-Cilium extension
	t.Log("Deploying Networking-Cilium extension")
	if err := gardener.DeployExtension(ctx, gardenClient, newCiliumExtensionConfig(t, releasePipelineCfg)); err != nil {
		t.Fatalf("failed to deploy Cilium extension: %v", err)
	}

	// Deploy OS-Gardenlinux extension
	t.Log("Deploying OS-Gardenlinux extension")
	if err := gardener.DeployExtension(ctx, gardenClient, newGardenlinuxExtensionConfig(t, releasePipelineCfg)); err != nil {
		t.Fatalf("failed to deploy Gardenlinux extension: %v", err)
	}

	// Deploy External DNS extension
	t.Log("Deploying External DNS extension")
	if err := gardener.DeployExtension(ctx, gardenClient, newExternalDNSExtensionConfig(t, releasePipelineCfg, *externalDNSArtifactsVersion, *externalDNSManagementImageName)); err != nil {
		t.Fatalf("failed to deploy external DNS extension: %v", err)
	}

	// Create Gardener project
	t.Logf("Creating Gardener project %s", gardenProjectName)
	createGardenerProject(ctx, gardenClient, gardenProjectName, t)

	// Add required labels to the garden namespace
	t.Log("Labeling garden namespace")
	labelGardenNamespace(ctx, gardenClient, t)

	// Create Gardener's SecretBinding to reference the GDC cloudprovider credentials
	t.Log("Creating SecretBinding for GDC credentials")
	createCredentialsBinding(ctx, gardenClient, t)

	// Create Cloud profile
	t.Logf("Creating CloudProfile %s", cloudprofileName)
	createCloudprofile(ctx, gardenClient, releasePipelineCfg, t)

	// Create Shoot cluster
	t.Logf("Creating Shoot cluster %s", releasePipelineCfg.TestShoot.Name)
	createShoot(ctx, gardenClient, releasePipelineCfg, shootNodeCIDR, *gardenerArtifactsVersion, int32(*workerPoolMinimum), int32(*workerPoolMaximum), t)
}

func createShoot(ctx context.Context, gardenClient client.WithWatch, cfg *config.ReleaseTestConfig, shootNodeCIDR, gardenerArtifactsVersion string, workerMin, workerMax int32, t *testing.T) {
	shoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cfg.TestShoot.Name,
			Namespace: gardenv1beta1constants.GardenNamespace,
		},
	}

	shootSpec := newShootSpec(t, cfg, shootNodeCIDR, gardenerArtifactsVersion, workerMin, workerMax)

	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, shoot, func() error {
		shoot.Spec = shootSpec
		return nil
	})

	if err != nil {
		t.Fatalf("failed to create or update shoot %q/%q: %v", shoot.Namespace, shoot.Name, err)
	}
	shootKey := client.ObjectKey{Name: cfg.TestShoot.Name, Namespace: gardenv1beta1constants.GardenNamespace}
	watchCtx, cancelWatch := context.WithCancel(ctx)
	defer cancelWatch()
	go ensureSeedShootNamespaceWorkarounds(watchCtx, t, cfg, shootKey)

	err = gardener.WaitForShootReconciliation(ctx, gardenClient, shootKey, waitForShootCreationTimeout)
	if err != nil {
		t.Fatalf("shoot \"%q/%q\" reconciliation failed or timed out: %v", shoot.Namespace, shoot.Name, err)
	}
}

// ensureSeedShootNamespaceWorkarounds applies Seed-level workarounds in the Shoot's control-plane namespace:
//  1. Creates NetworkPolicy/allow-all-etcd so Cilium VXLAN on KubeVirt Seed nodes does not drop cross-node etcd peer traffic (2380) during bootstrap.
//  2. Pre-creates the 6 etcd-main/etcd-events PVCs with cdi.kubevirt.io/storage.usePopulator="false" and
//     cdi.kubevirt.io/storage.bind.immediate.requested="true" as soon as the Shoot control-plane namespace appears
//     (before etcd-druid creates the StatefulSets), so CreateVolume forwards both annotations on the initial DataVolume creation
//     without racing against ControllerPublishVolume's 5 QPS client rate limiter.
//  3. Annotates any additional Shoot control-plane PVCs (such as prometheus-db-prometheus-shoot-0) with the same CDI annotations.
func ensureSeedShootNamespaceWorkarounds(ctx context.Context, t *testing.T, cfg *config.ReleaseTestConfig, shootKey client.ObjectKey) {
	if cfg == nil || cfg.Seed == nil || cfg.Seed.HostCluster == nil {
		return
	}
	var seedClient client.Client
	if cfg.Seed.HostCluster.UserClusterClient != nil {
		seedClient = cfg.Seed.HostCluster.UserClusterClient
	} else if cfg.Seed.HostCluster.Shoot != nil && cfg.Seed.HostCluster.Shoot.VirtualGarden != nil {
		remoteShoot := cfg.Seed.HostCluster.Shoot
		remoteShootKey := client.ObjectKey{Name: remoteShoot.Name, Namespace: remoteShoot.Namespace}
		shootClients, err := gardener.NewShootClient(ctx, remoteShoot.VirtualGarden.Client, remoteShootKey)
		if err != nil {
			t.Logf("Warning: Could not create Seed client for Shoot namespace workarounds: %v", err)
			return
		}
		seedClient = shootClients.WatchClient
	}
	if seedClient == nil {
		return
	}

	shootNamespace := "shoot--" + shootKey.Namespace + "--" + shootKey.Name
	netpolCreated := false
	etcdPVCsPrecreated := false
	lastEtcdMountRecovery := make(map[string]time.Time)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ns := &corev1.Namespace{}
			if err := seedClient.Get(ctx, client.ObjectKey{Name: shootNamespace}, ns); err != nil {
				continue
			}

			// Allow cross-node etcd peer communication during initial 3-node etcd cluster bootstrap on KubeVirt Seeds.
			if !netpolCreated {
				np := &networkingv1.NetworkPolicy{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "allow-all-etcd",
						Namespace: shootNamespace,
					},
					Spec: networkingv1.NetworkPolicySpec{
						PodSelector: metav1.LabelSelector{},
						Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
						Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
						PolicyTypes: []networkingv1.PolicyType{
							networkingv1.PolicyTypeIngress,
							networkingv1.PolicyTypeEgress,
						},
					},
				}
				if err := seedClient.Create(ctx, np); err == nil || apierrors.IsAlreadyExists(err) {
					netpolCreated = true
				}
			}

			// Pre-create the 6 etcd StatefulSet PVCs with CDI immediate-bind annotations and spread
			// volume.kubernetes.io/selected-node across Ready Seed nodes before etcd-druid creates
			// the StatefulSets so CreateVolume + pvc_reconciler bind the volumes immediately instead of
			// colliding with all 6 ControllerPublishVolume calls when the Etcd pods are scheduled.
			if !etcdPVCsPrecreated {
				fsMode := corev1.PersistentVolumeFilesystem
				readyNodes := listReadySeedNodeNames(ctx, seedClient)
				precreateSpecs := []struct {
					nameFormat  string
					partOf      string
					storageSize string
				}{
					{nameFormat: etcdMainPVCNameFormat, partOf: "etcd-main", storageSize: etcdMainStorageSize},
					{nameFormat: etcdEventsPVCNameFormat, partOf: "etcd-events", storageSize: etcdEventsStorageSize},
				}
				allCreated := true
				for _, spec := range precreateSpecs {
					for ordinal := 0; ordinal < 3; ordinal++ {
						pvcName := fmt.Sprintf(spec.nameFormat, ordinal)
						annotations := map[string]string{
							cdiUsePopulatorAnnotationKey:        "false",
							cdiBindImmediateRequestedAnnotation: "true",
						}
						if len(readyNodes) > 0 {
							annotations[selectedNodeAnnotationKey] = readyNodes[ordinal%len(readyNodes)]
						}
						pvc := &corev1.PersistentVolumeClaim{
							ObjectMeta: metav1.ObjectMeta{
								Name:        pvcName,
								Namespace:   shootNamespace,
								Annotations: annotations,
								Labels: map[string]string{
									"app.kubernetes.io/managed-by": "etcd-druid",
									"app.kubernetes.io/part-of":    spec.partOf,
								},
							},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceStorage: resource.MustParse(spec.storageSize),
									},
								},
								StorageClassName: ptr.To(kubevirtStorageClassName),
								VolumeMode:       &fsMode,
							},
						}
						if err := seedClient.Create(ctx, pvc); err != nil && !apierrors.IsAlreadyExists(err) {
							allCreated = false
						}
					}
				}
				if allCreated {
					etcdPVCsPrecreated = true
				}
			}

			// Bypass CDI VolumeBlankSource populator and request immediate binding for any other PVCs created in shootNamespace.
			pvcList := &corev1.PersistentVolumeClaimList{}
			if err := seedClient.List(ctx, pvcList, client.InNamespace(shootNamespace)); err == nil {
				for _, item := range pvcList.Items {
					if item.Annotations[cdiUsePopulatorAnnotationKey] != "false" ||
						item.Annotations[cdiBindImmediateRequestedAnnotation] != "true" {
						pvc := item
						patch := client.MergeFrom(pvc.DeepCopy())
						if pvc.Annotations == nil {
							pvc.Annotations = make(map[string]string)
						}
						pvc.Annotations[cdiUsePopulatorAnnotationKey] = "false"
						pvc.Annotations[cdiBindImmediateRequestedAnnotation] = "true"
						_ = seedClient.Patch(ctx, &pvc, patch)
					}
				}
			}

			remediateStuckEtcdVolumeAttachments(ctx, seedClient, shootNamespace, lastEtcdMountRecovery)
		}
	}
}

func listReadySeedNodeNames(ctx context.Context, c client.Client) []string {
	nodeList := &corev1.NodeList{}
	if err := c.List(ctx, nodeList); err != nil {
		return nil
	}
	var names []string
	for _, n := range nodeList.Items {
		if n.Spec.Unschedulable {
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeReady && cond.Status == corev1.ConditionTrue {
				names = append(names, n.Name)
				break
			}
		}
	}
	sort.Strings(names)
	return names
}

// TODO(gkecl/2393636): Interim CI mitigation only. Remove this self-healing once the
// third_party/kubevirt/csi-driver fix for the premature VolumeAttachment.Status.Attached=true
// fast-path has rolled out to the staging GDC Seed clusters.
//
// remediateStuckEtcdVolumeAttachments detects etcd-* pods stuck in ContainerCreating for >3 minutes
// due to KubeVirt CSI's premature VolumeAttachment.Status.Attached=true fast-path (when EnsureVolumeAvailable
// timed out on the first attempt and NodeStageVolume fails with "couldn't find device by serial id").
// Deleting the stale VolumeAttachment and pod triggers csi-attacher to detach and cleanly re-attach the volume.
//
// It runs inside the ensureSeedShootNamespaceWorkarounds goroutine, so it logs via log.Printf rather than
// testing.T to avoid "Log in goroutine after Test... has completed" panics when the watch context is cancelled.
func remediateStuckEtcdVolumeAttachments(ctx context.Context, seedClient client.Client, shootNamespace string, lastRecovery map[string]time.Time) {
	podList := &corev1.PodList{}
	if err := seedClient.List(ctx, podList, client.InNamespace(shootNamespace), client.MatchingLabels{"app.kubernetes.io/managed-by": "etcd-druid"}); err != nil {
		return
	}
	now := time.Now()
	for _, pod := range podList.Items {
		if pod.Status.Phase != corev1.PodPending || now.Sub(pod.CreationTimestamp.Time) < stuckEtcdMountRecoveryThreshold {
			continue
		}
		if last, ok := lastRecovery[pod.Name]; ok && now.Sub(last) < stuckEtcdMountRecoveryThreshold {
			continue
		}
		eventList := &corev1.EventList{}
		if err := seedClient.List(ctx, eventList, client.InNamespace(shootNamespace)); err != nil {
			continue
		}
		stuckSerialID := false
		for _, ev := range eventList.Items {
			if ev.InvolvedObject.Kind == "Pod" && ev.InvolvedObject.Name == pod.Name &&
				ev.Reason == "FailedMount" && strings.Contains(ev.Message, "couldn't find device by serial id") {
				stuckSerialID = true
				break
			}
		}
		if !stuckSerialID {
			continue
		}

		var pvNames []string
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				pvc := &corev1.PersistentVolumeClaim{}
				if err := seedClient.Get(ctx, client.ObjectKey{Name: vol.PersistentVolumeClaim.ClaimName, Namespace: shootNamespace}, pvc); err == nil && pvc.Spec.VolumeName != "" {
					pvNames = append(pvNames, pvc.Spec.VolumeName)
				}
			}
		}
		if len(pvNames) > 0 {
			vaList := &storagev1.VolumeAttachmentList{}
			if err := seedClient.List(ctx, vaList); err == nil {
				for _, va := range vaList.Items {
					if va.Spec.Source.PersistentVolumeName != nil {
						for _, pvName := range pvNames {
							if *va.Spec.Source.PersistentVolumeName == pvName {
								vaCopy := va
								log.Printf("Deleting stale VolumeAttachment %s for stuck pod %s/%s (PV %s)", va.Name, shootNamespace, pod.Name, pvName)
								_ = seedClient.Delete(ctx, &vaCopy)
							}
						}
					}
				}
			}
		}
		podCopy := pod
		log.Printf("Deleting stuck Etcd pod %s/%s to re-trigger CSI volume attachment", shootNamespace, pod.Name)
		_ = seedClient.Delete(ctx, &podCopy)
		lastRecovery[pod.Name] = now
	}
}

func createCloudprofile(ctx context.Context, gardenClient client.WithWatch, cfg *config.ReleaseTestConfig, t *testing.T) {
	cloudprofile := &gardencorev1beta1.CloudProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: cloudprofileName,
		},
	}
	harborRegistry := parseHarborUrl(cfg.GDC.HarborRegistryURL, t)
	cloudProfileSpec := newCloudProfileSpec(t, cfg, harborRegistry)

	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, cloudprofile, func() error {
		cloudprofile.Spec = cloudProfileSpec
		return nil
	})

	if err != nil {
		t.Fatalf("failed to create or update CloudProfile: %q: %v", cloudprofileName, err)
	}
}

// validateFlags checks that all required command-line flags have been provided.
func validateFlags(t *testing.T) error {
	// A map of flag names to their string pointer values for easy validation.
	requiredFlags := map[string]*string{
		"gardener-artifacts-version":      gardenerArtifactsVersion,
		"external-dns-artifacts-version":  externalDNSArtifactsVersion,
		"release-configuration-file-path": releaseConfigurationFilePath,
	}

	for name, value := range requiredFlags {
		if *value == "" {
			t.Fatalf("--%s is a required flag", name)
		}
	}

	if *workerPoolMinimum <= 0 {
		t.Fatalf("--worker-pool-minimum must be greater than 0, got %d", *workerPoolMinimum)
	}
	if *workerPoolMaximum < *workerPoolMinimum {
		t.Fatalf("--worker-pool-maximum (%d) must be greater than or equal to --worker-pool-minimum (%d)", *workerPoolMaximum, *workerPoolMinimum)
	}

	return nil
}

func parseHarborUrl(harborRegistryURL string, t *testing.T) *harborRegistry {
	parsedURL, err := url.Parse(harborRegistryURL)
	if err != nil {
		t.Fatalf("failed to parse harborRegistryURL %q: %v; URL must be in the format 'harbor.example.com/my-project'", harborRegistryURL, err)
	}

	// If no scheme specified, Prepend the default scheme and re-parse
	if parsedURL.Scheme == "" {
		parsedURL, err = url.Parse("https://" + harborRegistryURL)
	}

	if parsedURL.Host == "" {
		t.Fatalf("Harbor URL is not specified")
	}

	if parsedURL.Path == "" {
		t.Fatalf("Harbor project is not specified")
	}

	fullPathWithoutTrailingSlash := strings.TrimSuffix(parsedURL.Host+parsedURL.Path, "/")
	return &harborRegistry{
		host:     parsedURL.Host,
		fullPath: fullPathWithoutTrailingSlash,
	}
}

func labelGardenNamespace(ctx context.Context, gardenClient client.Client, t *testing.T) {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "garden",
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, ns, func() error {
		if ns.Labels == nil {
			ns.Labels = make(map[string]string)
		}
		ns.Labels["project.gardener.cloud/name"] = "garden"
		ns.Labels["gardener.cloud/role"] = "project"
		return nil
	})
	if err != nil {
		t.Fatalf("failed to update garden namespace: %v", err)
	}
}

func createCredentialsBinding(ctx context.Context, gardenClient client.Client, t *testing.T) {
	credentialsBinding := &securityv1alpha1.CredentialsBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      credentialsBindingName,
			Namespace: gardenProjectName,
		},
		Provider: securityv1alpha1.CredentialsBindingProvider{
			Type: "gdch",
		},
		CredentialsRef: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Secret",
			Name:       releaseConfig.GDCCredentialsSecretName,
			Namespace:  gardenProjectName,
		},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, gardenClient, credentialsBinding, func() error {
		return nil
	})
	if err != nil {
		t.Fatalf("failed to create credentials binding: %v", err)
	}
}

func createGardenerProject(ctx context.Context, gardenClient client.WithWatch, projectName string, t *testing.T) {
	project := &gardencorev1beta1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectName,
			Namespace: gardenv1beta1constants.GardenNamespace,
		},
		Spec: gardencorev1beta1.ProjectSpec{
			Namespace: ptr.To(gardenv1beta1constants.GardenNamespace),
		},
	}
	err := gardenClient.Create(ctx, project)
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("failed to create gardener project: %v", err)
	}
}
