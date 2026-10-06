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

package storage

import (
	"context"
	"flag"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
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
	WaitForPVCBoundTimeout   = 5 * time.Minute
	WaitForPodReadyTimeout   = 20 * time.Minute
	WaitForPVCResizedTimeout = 10 * time.Minute
	pollInterval             = 2 * time.Second
	pollTimeout              = 60 * time.Second
	zoneTopologyKey          = "topology.kubernetes.io/zone"
)

func TestClusterStorageLifecycle(t *testing.T) {
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
	namespace := "storage-test-" + suffix
	t.Logf("Creating Namespace %s", namespace)
	err = kubernetes.CreateNamespace(ctx, watchClient, namespace)
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Cleaning up resources in namespace %s", namespace)
		kubernetes.CleanupResources(t, watchClient, namespace)
	})

	statefulSetName := "nginx-sts-" + suffix
	pvcName := "web-" + statefulSetName + "-0"
	podName := statefulSetName + "-0"
	mountPath := "/usr/share/nginx/html"
	testFileName := "test.txt"
	testFileContent := "persistence-is-working-" + suffix
	testFilePath := fmt.Sprintf("%s/%s", mountPath, testFileName)

	statefulSet := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      statefulSetName,
			Namespace: namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    ptr.To(int32(1)),
			ServiceName: statefulSetName,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": statefulSetName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": statefulSetName}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "nginx",
						Image: releaseConfig.TestImageName,
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "web",
							MountPath: mountPath,
						}},
					}},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: "web"},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
					},
				},
			}},
		},
	}

	stsCreated := t.Run("Verify Creation of Statefulset", func(t *testing.T) {
		t.Logf("Creating StatefulSet %s in %s namespace", statefulSetName, namespace)
		err := watchClient.Create(ctx, statefulSet)
		if err != nil {
			t.Fatalf("failed to create statefulset %s in namespace %s: %v", statefulSetName, namespace, err)
		}
		t.Logf("StatefulSet %s created", statefulSetName)

		if err := kubernetes.WaitForPVCBound(ctx, watchClient, namespace, pvcName, WaitForPVCBoundTimeout); err != nil {
			t.Fatal(err)
		}

		if err := kubernetes.WaitForPodReady(ctx, watchClient, namespace, podName, WaitForPodReadyTimeout); err != nil {
			t.Fatal(err)
		}
	})
	if !stsCreated {
		t.Fatalf("Failed to create StatefulSet, aborting flow")
	}

	t.Run("Verify Data Persistence After Pod Deletion", func(t *testing.T) {
		t.Logf("Writing test file to %s in pod %s", testFilePath, podName)
		writeCmd := []string{"sh", "-c", fmt.Sprintf("echo %s > %s", testFileContent, testFilePath)}
		if err := kubernetes.ExecPod(ctx, goClient, restConfig, writeCmd, namespace, podName); err != nil {
			t.Fatalf("Failed to write to file in pod: %v", err)
		}

		t.Logf("Verifying content was written into the pod")
		verifyCmd := []string{"grep", testFileContent, testFilePath}
		err = wait.PollUntilContextTimeout(
			ctx,
			pollInterval,
			pollTimeout,
			true,
			func(ctx context.Context) (bool, error) {
				t.Logf("Attempting to verify file content in pod %s in %s namespace ...", podName, namespace)
				err := kubernetes.ExecPod(ctx, goClient, restConfig, verifyCmd, namespace, podName)
				if err != nil {
					t.Logf("Verification attempt failed: %v. Retrying in %s.", err, pollInterval)
					return false, nil
				}
				t.Log("Verification successful.")
				return true, nil
			},
		)

		t.Logf("Deleting pod %s in %s namespace to trigger recreation", podName, namespace)
		podObj := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: namespace,
			},
		}
		gracePeriod := int64(0)
		if err := watchClient.Delete(ctx, podObj,
			client.PropagationPolicy(metav1.DeletePropagationForeground),
			client.GracePeriodSeconds(gracePeriod)); err != nil {
			t.Fatalf("Failed to delete pod %s in namespace %s: %v", podName, namespace, err)
		}

		t.Logf("Waiting for pod %s to be recreated by the StatefulSet controller", podName)
		if err := kubernetes.WaitForPodReady(ctx, watchClient, namespace, podName, WaitForPodReadyTimeout); err != nil {
			t.Fatal(err)
		}
		t.Logf("Pod %s has been recreated and is Ready", podName)

		t.Logf("Verifying content of %s in the new pod", testFilePath)
		err = wait.PollUntilContextTimeout(
			ctx,
			pollInterval,
			pollTimeout,
			true,
			func(ctx context.Context) (bool, error) {
				t.Logf("Attempting to verify file content in pod %s in %s namespace ...", podName, namespace)
				err := kubernetes.ExecPod(ctx, goClient, restConfig, verifyCmd, namespace, podName)
				if err != nil {
					t.Logf("Verification attempt failed: %v. Retrying in %s.", err, pollInterval)
					return false, nil
				}
				t.Log("Verification successful.")
				return true, nil
			},
		)
		t.Log("Successfully verified data persistence in StatefulSet.")
	})

	t.Run("Verify Volume Expansion", func(t *testing.T) {
		t.Logf("Attempting to expand PVC %s to %s", pvcName, "5Gi")
		t.Logf("Attempting to expand PVC %s", pvcName)

		pvc := &corev1.PersistentVolumeClaim{}
		err := watchClient.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: namespace}, pvc)
		if err != nil {
			t.Fatalf("Failed to retrieve PVC %s in namespace %s: %v", statefulSetName, namespace, err)
		}

		pvcToPatch := pvc.DeepCopy()
		newSize := resource.MustParse("5Gi")
		pvcToPatch.Spec.Resources.Requests[corev1.ResourceStorage] = newSize
		patch := client.MergeFrom(pvc)
		err = watchClient.Patch(ctx, pvcToPatch, patch)
		if err != nil {
			t.Fatalf("Failed to update pvc %s in namespace %s: %v", pvcName, namespace, err)
		}

		t.Log("Waiting for PVC expansion to complete...")
		err = kubernetes.WaitForPVCResized(ctx, watchClient, namespace, pvcName, newSize, WaitForPVCResizedTimeout)
		if err != nil {
			t.Fatal(err)
		}

		t.Logf("Verifying file content in pod %s in %s namespace", podName, namespace)
		verifyFileContentCmd := []string{"grep", testFileContent, testFilePath}
		err = kubernetes.ExecPod(ctx, goClient, restConfig, verifyFileContentCmd, namespace, podName)
		if err != nil {
			t.Fatalf("Verification failed after expansion. File content was lost or pod is unstable: pod: %s namespace:%s %v", podName, namespace, err)
		}
		t.Log("Successfully verified data persistence after volume expansion.")
	})

	t.Run("Verify Topology Aware Volume Provisioning", func(t *testing.T) {
		t.Logf("Retrieving Pod %s in namespace %s to identify scheduled node", podName, namespace)
		pod := &corev1.Pod{}
		if err := watchClient.Get(ctx, client.ObjectKey{Name: podName, Namespace: namespace}, pod); err != nil {
			t.Fatalf("Failed to get pod %s in namespace %s: %v", podName, namespace, err)
		}

		nodeName := pod.Spec.NodeName
		if nodeName == "" {
			t.Fatalf("Pod %s is not assigned to any node", podName)
		}
		t.Logf("Pod %s is scheduled on node %s", podName, nodeName)

		node := &corev1.Node{}
		if err := watchClient.Get(ctx, client.ObjectKey{Name: nodeName}, node); err != nil {
			t.Fatalf("Failed to get node %s: %v", nodeName, err)
		}

		nodeZone, ok := node.Labels[zoneTopologyKey]
		if !ok || nodeZone == "" {
			t.Fatalf("Node %s is missing zone topology label %s (labels: %+v)", nodeName, zoneTopologyKey, node.Labels)
		}
		t.Logf("Node %s has topology zone %s", nodeName, nodeZone)

		pvc := &corev1.PersistentVolumeClaim{}
		if err := watchClient.Get(ctx, client.ObjectKey{Name: pvcName, Namespace: namespace}, pvc); err != nil {
			t.Fatalf("Failed to retrieve PVC %s in namespace %s: %v", pvcName, namespace, err)
		}

		pvName := pvc.Spec.VolumeName
		if pvName == "" {
			t.Fatalf("PVC %s has empty VolumeName", pvcName)
		}

		pv := &corev1.PersistentVolume{}
		if err := watchClient.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
			t.Fatalf("Failed to retrieve PV %s: %v", pvName, err)
		}

		t.Logf("Verifying node affinity on PV %s", pvName)
		if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
			t.Fatalf("PV %s is missing node affinity required terms", pvName)
		}

		hasMatchingTopology := false
		for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
			for _, expr := range term.MatchExpressions {
				if expr.Key == zoneTopologyKey && expr.Operator == corev1.NodeSelectorOpIn {
					for _, val := range expr.Values {
						if val == nodeZone {
							hasMatchingTopology = true
							break
						}
					}
				}
			}
		}
		if !hasMatchingTopology {
			t.Fatalf("PV %s node affinity does not match node zone %s (nodeAffinity: %+v)", pvName, nodeZone, pv.Spec.NodeAffinity)
		}
		t.Logf("PV %s correctly has node affinity for zone %s", pvName, nodeZone)
	})
}

func uniqueSuffix() string {
	return uuid.NewString()[:16]
}
