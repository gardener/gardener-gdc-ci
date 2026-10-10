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

package loadbalancer

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/loader"
	releaseConfig "github.com/gardener/gardener-gdc-ci/integration/pkg/config/release"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/gardener"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/kubernetes"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/pnp"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/subnet"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/virtualmachine"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
)

var (
	gardenerArtifactsVersion        = flag.String("gardener-artifacts-version", "", "The version string for Gardener artifacts")
	releaseConfigurationFilePath    = flag.String("release-configuration-file-path", "", "the path to the release configuration file")
	continuousConfigurationFilePath = flag.String("continuous-configuration-file-path", "", "the path to the continuous configuration file")
	virtualGardenProvider           = flag.String("virtual-garden-provider", "gke", "the provider type hosting the Virtual Garden ('gke' or 'gdc')")
)

const (
	waitForLBServiceTimeout         = 6 * time.Minute
	waitForPodReadyTimeout          = 5 * time.Minute
	waitForNamespaceDeletionTimeout = 5 * time.Minute
	pollInterval                    = 2 * time.Second
	positiveConnectivityTimeout     = 12 * time.Minute
	negativeConnectivityTimeout     = 30 * time.Second
	waitForSubnetReadyTimeout       = 1 * time.Minute
	leafSubnetPrefixLength          = 32
	branchSubnetPrefixLength        = 31

	secondaryProjectLBTimeout         = 12 * time.Minute
	secondaryProjectLBNegativeTimeout = 30 * time.Second
	deleteSubnetTimeout               = 3 * time.Minute
	deleteVMTimeout                   = 5 * time.Minute
	secondaryProjectLBRevokeTimeout   = 5 * time.Minute

	lbInternal                           = "internal"
	lbExternal                           = "external"
	lbTypeAnnotationKey                  = "networking.gke.io/load-balancer-type"
	internalLBSubnetAnnotationKey        = "networking.gke.io/load-balancer-subnet"
	externalLBIPAddressesAnnotationKey   = "networking.gke.io/load-balancer-ip-addresses"
	internalLBAllowProjectsAnnotationKey = "networking.gke.io/load-balancer-allow-projects"

	lbLifecycleNamespacePrefix = "lb-lifecycle-test-"
	elbTestSubnetPrefix        = "elb-test-"
	ilbTestSubnetPrefix        = "ilb-test-"
	leafSubnetSuffix           = "-leaf"
	branchSubnetSuffix         = "-branch"
)

func TestLoadBalancerServiceLifecycle(t *testing.T) {
	if err := ipamglobalv1.AddToScheme(scheme.Scheme); err != nil {
		t.Fatalf("failed to add ipamglobalv1 to scheme: %v", err)
	}

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

	if releasePipelineCfg.GlobalAPIClient == nil {
		t.Fatal("GlobalAPIClient must be set in release configuration for this test")
	}
	if releasePipelineCfg.GDC.ParentPrivateSubnetGroup == "" {
		t.Fatal("ParentPrivateSubnetGroup must be set in release configuration for this test")
	}
	if releasePipelineCfg.GDC.ParentPublicSubnetGroup == "" {
		t.Fatal("ParentPublicSubnetGroup must be set in release configuration for this test")
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

	// Clean up any stale lb-lifecycle-test-* namespaces and elb-test-*/ilb-test-* subnets
	// left behind if a previous attempt timed out and skipped t.Cleanup().
	cleanupStaleLoadBalancerTestResources(ctx, t, watchClient, releasePipelineCfg.GlobalAPIClient, releasePipelineCfg.GDC.Project)

	suffix := uniqueSuffix()
	namespace := fmt.Sprintf("%s%s", lbLifecycleNamespacePrefix, suffix)
	elbLeafSubnetName := fmt.Sprintf("%s%s%s", elbTestSubnetPrefix, suffix, leafSubnetSuffix)
	elbBranchSubnetName := fmt.Sprintf("%s%s%s", elbTestSubnetPrefix, suffix, branchSubnetSuffix)
	ilbLeafSubnetName := fmt.Sprintf("%s%s%s", ilbTestSubnetPrefix, suffix, leafSubnetSuffix)
	ilbBranchSubnetName := fmt.Sprintf("%s%s%s", ilbTestSubnetPrefix, suffix, branchSubnetSuffix)

	// Register Subnet cleanup first so it runs LAST (after namespace cleanup)
	t.Cleanup(func() {
		if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, client.ObjectKey{Namespace: releasePipelineCfg.GDC.Project, Name: elbLeafSubnetName}, deleteSubnetTimeout, t); err != nil {
			t.Errorf("failed to cleanup External LB leaf subnet %s: %v", elbLeafSubnetName, err)
		}
		if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, client.ObjectKey{Namespace: releasePipelineCfg.GDC.Project, Name: elbBranchSubnetName}, deleteSubnetTimeout, t); err != nil {
			t.Errorf("failed to cleanup External LB branch subnet %s: %v", elbBranchSubnetName, err)
		}
		if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, client.ObjectKey{Namespace: releasePipelineCfg.GDC.Project, Name: ilbLeafSubnetName}, deleteSubnetTimeout, t); err != nil {
			t.Errorf("failed to cleanup Internal LB leaf subnet %s: %v", ilbLeafSubnetName, err)
		}
		if err := subnet.DeleteSubnet(ctx, releasePipelineCfg.GlobalAPIClient, client.ObjectKey{Namespace: releasePipelineCfg.GDC.Project, Name: ilbBranchSubnetName}, deleteSubnetTimeout, t); err != nil {
			t.Errorf("failed to cleanup Internal LB branch subnet %s: %v", ilbBranchSubnetName, err)
		}
	})

	t.Logf("Creating Namespace %s", namespace)
	err = kubernetes.CreateNamespace(ctx, watchClient, namespace)
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	t.Cleanup(func() {
		kubernetes.CleanupResources(t, watchClient, namespace)

		t.Logf("Waiting for namespace %s to be deleted", namespace)
		err := waitForNamespaceDeletion(ctx, t, watchClient, namespace)
		if err != nil {
			t.Errorf("Namespace %s was not deleted within timeout: %v", namespace, err)
		}
	})

	backendPodName := "lb-backend-pod"
	testerPodName := "lb-tester-pod"
	appLabel := map[string]string{"app": backendPodName}

	backendPodSpec := podSpec(client.ObjectKey{Name: backendPodName, Namespace: namespace}, releaseConfig.TestImageName)
	backendPodSpec.Labels = appLabel
	testerPodSpec := podSpec(client.ObjectKey{Name: testerPodName, Namespace: namespace}, releaseConfig.TestImageName)

	t.Logf("Creating backend pod %s in %s namespace", backendPodName, namespace)
	_, err = kubernetes.EnsurePod(ctx, watchClient, backendPodSpec, waitForPodReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Creating tester pod %s in %s namespace", testerPodName, namespace)
	_, err = kubernetes.EnsurePod(ctx, watchClient, testerPodSpec, waitForPodReadyTimeout)
	if err != nil {
		t.Fatal(err)
	}

	// Flow 1: External Load Balancer (ELB to ILB)
	t.Run("External Load Balancer", func(t *testing.T) {
		t.Parallel()
		serviceName := "lb-svc-elb-to-ilb"
		serviceSpec := serviceSpec(serviceName, namespace, appLabel, nil)
		var elbIP string
		var ilbIP string

		elbCreated := t.Run("Verify Creation of External Load Balancer Service", func(t *testing.T) {
			t.Logf("Creating ELB Service %s in %s namespace", serviceName, namespace)
			err = watchClient.Create(ctx, serviceSpec)
			if err != nil {
				t.Fatalf("failed to create service %s in namespace %s: %v", serviceName, namespace, err)
			}
			t.Logf("LoadBalancer Service %s in %s namespace created", serviceName, namespace)

			elbIP = waitForAndGetServiceIP(ctx, t, watchClient, namespace, serviceName)
		})
		if !elbCreated {
			t.Fatalf("Failed to create ELB service, aborting flow")
		}

		t.Run("Verify External Loadbalancer Connectivity from in-cluster pod", func(t *testing.T) {
			verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, elbIP)
		})

		t.Run("Verify External Loadbalancer Connectivity from outside the cluster", func(t *testing.T) {
			if err := verifyExternalConnectivity(ctx, t, elbIP, positiveConnectivityTimeout); err != nil {
				t.Fatalf("Failed to connect to the LoadBalancer at %s: %v", elbIP, err)
			}
		})

		elbConverted := t.Run("Convert ELB to ILB", func(t *testing.T) {
			t.Logf("Updating Service %s to be ILB in %s namespace", serviceName, namespace)

			svc := &corev1.Service{}
			if err := watchClient.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, svc); err != nil {
				t.Fatalf("failed to get service %s: %v", serviceName, err)
			}

			if svc.Annotations == nil {
				svc.Annotations = make(map[string]string)
			}
			svc.Annotations[lbTypeAnnotationKey] = lbInternal

			if err := watchClient.Update(ctx, svc); err != nil {
				t.Fatalf("failed to update service %s in namespace %s to ILB: %v", serviceName, namespace, err)
			}
			t.Logf("LoadBalancer Service %s updated to ILB", serviceName)

			t.Logf("Waiting for new LoadBalancer IP for service %s", serviceName)

			var err error
			ilbIP, err = waitForServiceIPToChange(ctx, t, watchClient, serviceName, namespace, elbIP)
			if err != nil {
				t.Fatalf("failed to get new IP for LB service %s after upgrade: %v", serviceName, err)
			}
			t.Logf("LoadBalancer IP changed from %s to %s", elbIP, ilbIP)
		})
		if !elbConverted {
			t.Fatalf("Failed to convert ELB to ILB, aborting flow")
		}

		t.Run("Verify Internal Loadbalancer Connectivity from in-cluster pod", func(t *testing.T) {
			verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, ilbIP)
		})

		t.Run("Verify Internal Loadbalancer should have no connectivity from outside the cluster", func(t *testing.T) {
			if err := verifyExternalConnectivity(ctx, t, ilbIP, negativeConnectivityTimeout); err == nil {
				t.Fatalf("Unexpectedly reached the internal service from outside the cluster")
			} else {
				t.Logf("Successfully verified that ILB is not accessible from outside (Error: %v)", err)
			}
		})
	})

	// Flow 2: Internal Load Balancer (ILB to ELB)
	t.Run("Internal Load Balancer", func(t *testing.T) {
		t.Parallel()
		serviceName := "lb-svc-ilb-to-elb"
		serviceSpec := serviceSpec(serviceName, namespace, appLabel, map[string]string{
			lbTypeAnnotationKey: lbInternal,
		})
		var ilbIP string
		var elbIP string

		ilbCreated := t.Run("Verify Creation of Internal Load Balancer Service", func(t *testing.T) {
			t.Logf("Creating ILB Service %s in %s namespace", serviceName, namespace)
			err = watchClient.Create(ctx, serviceSpec)
			if err != nil {
				t.Fatalf("failed to create service %s in namespace %s: %v", serviceName, namespace, err)
			}
			t.Logf("LoadBalancer Service %s in %s namespace created", serviceName, namespace)

			ilbIP = waitForAndGetServiceIP(ctx, t, watchClient, namespace, serviceName)
		})
		if !ilbCreated {
			t.Fatalf("Failed to create ILB service, aborting flow")
		}

		t.Run("Verify Internal Loadbalancer Connectivity from in-cluster pod", func(t *testing.T) {
			verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, ilbIP)
		})

		t.Run("Verify Internal Loadbalancer should have no connectivity from outside the cluster", func(t *testing.T) {
			if err := verifyExternalConnectivity(ctx, t, ilbIP, negativeConnectivityTimeout); err == nil {
				t.Fatalf("Unexpectedly reached the internal service from outside the cluster")
			} else {
				t.Logf("Successfully verified that ILB is not accessible from outside (Error: %v)", err)
			}
		})

		ilbConverted := t.Run("Convert ILB to ELB", func(t *testing.T) {
			t.Logf("Updating Service %s to be ELB in %s namespace", serviceName, namespace)

			svc := &corev1.Service{}
			if err := watchClient.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, svc); err != nil {
				t.Fatalf("failed to get service %s: %v", serviceName, err)
			}

			// Remove internal annotation
			delete(svc.Annotations, lbTypeAnnotationKey)

			if err := watchClient.Update(ctx, svc); err != nil {
				t.Fatalf("failed to update service %s in namespace %s to ELB: %v", serviceName, namespace, err)
			}
			t.Logf("LoadBalancer Service %s updated to ELB", serviceName)

			t.Logf("Waiting for new LoadBalancer IP for service %s", serviceName)

			var err error
			elbIP, err = waitForServiceIPToChange(ctx, t, watchClient, serviceName, namespace, ilbIP)
			if err != nil {
				t.Fatalf("failed to get new IP for LB service %s after downgrade: %v", serviceName, err)
			}
			t.Logf("LoadBalancer IP changed from %s to %s", ilbIP, elbIP)
		})
		if !ilbConverted {
			t.Fatalf("Failed to convert ILB to ELB, aborting flow")
		}

		t.Run("Verify External Loadbalancer Connectivity from in-cluster pod", func(t *testing.T) {
			verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, elbIP)
		})

		t.Run("Verify External Loadbalancer Connectivity from outside the cluster", func(t *testing.T) {
			if err := verifyExternalConnectivity(ctx, t, elbIP, positiveConnectivityTimeout); err != nil {
				t.Fatalf("Failed to connect to the LoadBalancer at %s: %v", elbIP, err)
			}
		})
	})

	t.Run("Service creation with invalid annotation combinations should fail", func(t *testing.T) {
		t.Parallel()
		scenarios := []struct {
			name        string
			annotations map[string]string
		}{
			{
				name: "External LB with internal subnet annotation",
				annotations: map[string]string{
					internalLBSubnetAnnotationKey: "any-subnet",
				},
			},
			{
				name: "Internal LB with external IP annotation",
				annotations: map[string]string{
					lbTypeAnnotationKey:                lbInternal,
					externalLBIPAddressesAnnotationKey: "any-subnet",
				},
			},
			{
				name: "Internal LB with IP address in internal subnet annotation",
				annotations: map[string]string{
					lbTypeAnnotationKey:           lbInternal,
					internalLBSubnetAnnotationKey: "1.2.3.4",
				},
			},
			{
				name: "External LB with IP address in external IP annotation",
				annotations: map[string]string{
					externalLBIPAddressesAnnotationKey: "1.2.3.4",
				},
			},
			{
				name: "External LB with load-balancer-allow-projects annotation",
				annotations: map[string]string{
					internalLBAllowProjectsAnnotationKey: "project-a",
				},
			},
			{
				name: "Internal LB with empty load-balancer-allow-projects annotation",
				annotations: map[string]string{
					lbTypeAnnotationKey:                  lbInternal,
					internalLBAllowProjectsAnnotationKey: "",
				},
			},
			{
				name: "Internal LB with empty project in load-balancer-allow-projects annotation",
				annotations: map[string]string{
					lbTypeAnnotationKey:                  lbInternal,
					internalLBAllowProjectsAnnotationKey: "a,,b",
				},
			},
			{
				name: "Internal LB with invalid project in load-balancer-allow-projects annotation",
				annotations: map[string]string{
					lbTypeAnnotationKey:                  lbInternal,
					internalLBAllowProjectsAnnotationKey: "111project",
				},
			},
		}

		dummyLabel := map[string]string{"app": "invalid-test"}

		for _, tc := range scenarios {
			t.Run(tc.name, func(t *testing.T) {
				svcName := fmt.Sprintf("lb-svc-invalid-%s", uniqueSuffix())
				svc := serviceSpec(svcName, namespace, dummyLabel, tc.annotations)

				t.Logf("Creating Service %s with invalid annotations", svcName)
				err := watchClient.Create(ctx, svc)
				if err == nil {
					t.Fatalf("Expected service creation to fail, but it succeeded")
				}

				t.Logf("Service creation failed as expected: %v", err)
			})
		}
	})

	t.Run("External LB with custom subnet", func(t *testing.T) {
		// This section of test runs sequentially because the gdcloud-k8s-auth-plugin is not thread-safe
		// therefore subnet creation using global api client fails on parallel execution of tests.
		t.Log("Ensuring branch subnet for External LB")
		subnetCIDRBranch, err := subnet.EnsureSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnet.SubnetOptions{
			Namespace:         releasePipelineCfg.GDC.Project,
			Name:              elbBranchSubnetName,
			ParentSubnetGroup: releasePipelineCfg.GDC.ParentPublicSubnetGroup,
			PrefixLength:      branchSubnetPrefixLength,
			SkipVPCLabel:      true,
		}, waitForSubnetReadyTimeout, t)
		if err != nil {
			t.Fatalf("failed to ensure branch subnet: %v", err)
		}
		t.Logf("Branch Subnet %s has CIDR %s", elbBranchSubnetName, subnetCIDRBranch)

		t.Log("Ensuring leaf subnet for External LB")
		subnetCIDRLeaf, err := subnet.EnsureSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnet.SubnetOptions{
			Namespace:         releasePipelineCfg.GDC.Project,
			Name:              elbLeafSubnetName,
			ParentSubnetGroup: elbBranchSubnetName,
			ParentType:        string(ipamv1.SingleSubnet),
			PrefixLength:      leafSubnetPrefixLength,
			SkipVPCLabel:      true,
		}, waitForSubnetReadyTimeout, t)
		if err != nil {
			t.Fatalf("failed to ensure leaf subnet: %v", err)
		}
		t.Logf("Leaf Subnet %s has CIDR %s", elbLeafSubnetName, subnetCIDRLeaf)

		scenarios := []struct {
			name        string
			subnetName  string
			subnetCIDR  string
			annotations map[string]string
		}{
			{
				name:       "Custom leaf subnet",
				subnetName: elbLeafSubnetName,
				subnetCIDR: subnetCIDRLeaf,
				annotations: map[string]string{
					externalLBIPAddressesAnnotationKey: elbLeafSubnetName,
				},
			},
			{
				name:       "Custom branch subnet",
				subnetName: elbBranchSubnetName,
				subnetCIDR: subnetCIDRBranch,
				annotations: map[string]string{
					externalLBIPAddressesAnnotationKey: elbBranchSubnetName,
				},
			},
		}

		for _, tc := range scenarios {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				svcName := fmt.Sprintf("lb-svc-external-custom-%s", uniqueSuffix())
				svc := serviceSpec(svcName, namespace, appLabel, tc.annotations)

				t.Logf("Creating Service %s with valid annotations", svcName)
				if err := watchClient.Create(ctx, svc); err != nil {
					t.Fatalf("failed to create service %s: %v", svcName, err)
				}

				t.Logf("Waiting for Service %s to get an IP", svcName)
				lbIP := waitForAndGetServiceIP(ctx, t, watchClient, namespace, svcName)
				t.Logf("Service %s successfully got an IP %s", svcName, lbIP)

				// Verify that lbIP is within tc.subnetCIDR
				_, ipnet, err := net.ParseCIDR(tc.subnetCIDR)
				if err != nil {
					t.Fatalf("failed to parse subnet CIDR %s: %v", tc.subnetCIDR, err)
				}
				parsedIP := net.ParseIP(lbIP)
				if parsedIP == nil {
					t.Fatalf("failed to parse service IP %s", lbIP)
				}
				if !ipnet.Contains(parsedIP) {
					t.Fatalf("Service IP %s is not within subnet CIDR %s", lbIP, tc.subnetCIDR)
				}
				t.Logf("Verified that Service IP %s is within subnet CIDR %s", lbIP, tc.subnetCIDR)

				// Connectivity verification
				t.Run("Verify External Loadbalancer Connectivity from in-cluster pod", func(t *testing.T) {
					verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, lbIP)
				})

				t.Run("Verify External Loadbalancer Connectivity from outside the cluster", func(t *testing.T) {
					if err := verifyExternalConnectivity(ctx, t, lbIP, positiveConnectivityTimeout); err != nil {
						t.Fatalf("Failed to connect to the LoadBalancer at %s: %v", lbIP, err)
					}
				})
			})
		}
	})

	t.Run("Internal LB with custom subnet", func(t *testing.T) {
		// This section of test runs sequentially because the gdcloud-k8s-auth-plugin is not thread-safe
		// therefore subnet creation using global api client fails on parallel execution of tests.
		t.Log("Ensuring branch subnet for Internal LB")
		subnetCIDRBranch, err := subnet.EnsureSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnet.SubnetOptions{
			Namespace:         releasePipelineCfg.GDC.Project,
			Name:              ilbBranchSubnetName,
			ParentSubnetGroup: releasePipelineCfg.GDC.ParentPrivateSubnetGroup,
			PrefixLength:      branchSubnetPrefixLength,
		}, waitForSubnetReadyTimeout, t)
		if err != nil {
			t.Fatalf("failed to ensure branch subnet: %v", err)
		}
		t.Logf("Branch Subnet %s has CIDR %s", ilbBranchSubnetName, subnetCIDRBranch)

		t.Log("Ensuring leaf subnet for Internal LB")
		subnetCIDRLeaf, err := subnet.EnsureSubnet(ctx, releasePipelineCfg.GlobalAPIClient, subnet.SubnetOptions{
			Namespace:         releasePipelineCfg.GDC.Project,
			Name:              ilbLeafSubnetName,
			ParentSubnetGroup: ilbBranchSubnetName,
			ParentType:        string(ipamv1.SingleSubnet),
			PrefixLength:      leafSubnetPrefixLength,
		}, waitForSubnetReadyTimeout, t)
		if err != nil {
			t.Fatalf("failed to ensure leaf subnet: %v", err)
		}
		t.Logf("Leaf Subnet %s has CIDR %s", ilbLeafSubnetName, subnetCIDRLeaf)

		scenarios := []struct {
			name        string
			subnetName  string
			subnetCIDR  string
			annotations map[string]string
		}{
			{
				name:       "Custom leaf subnet",
				subnetName: ilbLeafSubnetName,
				subnetCIDR: subnetCIDRLeaf,
				annotations: map[string]string{
					lbTypeAnnotationKey:           lbInternal,
					internalLBSubnetAnnotationKey: ilbLeafSubnetName,
				},
			},
			{
				name:       "Custom branch subnet",
				subnetName: ilbBranchSubnetName,
				subnetCIDR: subnetCIDRBranch,
				annotations: map[string]string{
					lbTypeAnnotationKey:           lbInternal,
					internalLBSubnetAnnotationKey: ilbBranchSubnetName,
				},
			},
		}

		for _, tc := range scenarios {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				svcName := fmt.Sprintf("lb-svc-internal-custom-%s", uniqueSuffix())
				svc := serviceSpec(svcName, namespace, appLabel, tc.annotations)

				t.Logf("Creating Service %s with valid annotations", svcName)
				if err := watchClient.Create(ctx, svc); err != nil {
					t.Fatalf("failed to create service %s: %v", svcName, err)
				}

				t.Logf("Waiting for Service %s to get an IP", svcName)
				lbIP := waitForAndGetServiceIP(ctx, t, watchClient, namespace, svcName)
				t.Logf("Service %s successfully got an IP %s", svcName, lbIP)

				// Verify that lbIP is within tc.subnetCIDR
				_, ipnet, err := net.ParseCIDR(tc.subnetCIDR)
				if err != nil {
					t.Fatalf("failed to parse subnet CIDR %s: %v", tc.subnetCIDR, err)
				}
				parsedIP := net.ParseIP(lbIP)
				if parsedIP == nil {
					t.Fatalf("failed to parse service IP %s", lbIP)
				}
				if !ipnet.Contains(parsedIP) {
					t.Fatalf("Service IP %s is not within subnet CIDR %s", lbIP, tc.subnetCIDR)
				}
				t.Logf("Verified that Service IP %s is within subnet CIDR %s", lbIP, tc.subnetCIDR)

				// Connectivity verification
				t.Run("Verify Internal Loadbalancer Connectivity from in-cluster pod", func(t *testing.T) {
					verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, lbIP)
				})

				t.Run("Verify Internal Loadbalancer should have no connectivity from outside the cluster", func(t *testing.T) {
					if err := verifyExternalConnectivity(ctx, t, lbIP, negativeConnectivityTimeout); err == nil {
						t.Fatalf("Unexpectedly reached the internal service from outside the cluster")
					} else {
						t.Logf("Successfully verified that ILB is not accessible from outside (Error: %v)", err)
					}
				})
			})
		}
	})

	t.Run("cross project internal load balancer", func(t *testing.T) {
		// This section of test runs sequentially because the gdcloud-k8s-auth-plugin is not thread-safe
		// therefore VM setup using management api client fails on parallel execution of tests.
		secondaryProject := releasePipelineCfg.GDC.SecondaryProject
		if secondaryProject == "" {
			t.Fatal("SecondaryProject must be set in release configuration for this test")
		}
		suffix := uniqueSuffix()
		vmName := fmt.Sprintf("lb-test-vm-%s", suffix)

		vmManager := setupSecondaryProjectVM(ctx, t, releasePipelineCfg, vmName, secondaryProject, releasePipelineCfg.GDC.Project)

		t.Run("internal load balancer service should be created with default cross project denied", func(t *testing.T) {
			t.Parallel()
			serviceName := "ilb-svc-cross-project-deny-" + suffix
			serviceSpec := serviceSpec(serviceName, namespace, appLabel, map[string]string{
				lbTypeAnnotationKey: lbInternal,
			})

			t.Logf("Creating Service %s in namespace %s", serviceName, namespace)
			err = watchClient.Create(ctx, serviceSpec)
			if err != nil {
				t.Fatalf("failed to create service: %v", err)
			}

			ilbIP := waitForAndGetServiceIP(ctx, t, watchClient, namespace, serviceName)

			t.Run("Verify in-cluster connectivity", func(t *testing.T) {
				verifyInClusterConnectivity(ctx, t, goClient, restConfig, namespace, testerPodName, ilbIP)
			})

			t.Run("Verify cross-project connectivity is denied by default", func(t *testing.T) {
				t.Logf("Verifying connectivity from VM via SSH (expecting failure)...")
				err := checkCrossProjectVMConnectivityFailure(ctx, vmManager, ilbIP, secondaryProjectLBNegativeTimeout)
				if err != nil {
					t.Fatalf("Expected connectivity to fail, but got error or timeout: %v", err)
				}
				t.Logf("Successfully verified connectivity is denied")
			})

			t.Run("Verify cross-project connectivity is allowed after mutation", func(t *testing.T) {
				t.Logf("Mutating service to allow project %s", secondaryProject)
				var svc corev1.Service
				if err := watchClient.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, &svc); err != nil {
					t.Fatalf("failed to get service: %v", err)
				}
				if svc.Annotations == nil {
					svc.Annotations = make(map[string]string)
				}
				svc.Annotations[internalLBAllowProjectsAnnotationKey] = secondaryProject
				if err := watchClient.Update(ctx, &svc); err != nil {
					t.Fatalf("failed to update service: %v", err)
				}

				t.Logf("Verifying connectivity after mutation (expecting success)...")
				err = checkCrossProjectVMConnectivitySuccess(ctx, vmManager, ilbIP, secondaryProjectLBTimeout)
				if err != nil {
					t.Fatalf("Failed to connect from cross-project VM after allowing access: %v", err)
				}
				t.Logf("Successfully verified connectivity after mutation")
			})
		})

		t.Run("internal load balancer service with allowed project should have access of cross project", func(t *testing.T) {
			t.Parallel()
			serviceName := "ilb-svc-cross-project-allow-" + suffix
			serviceSpec := serviceSpec(serviceName, namespace, appLabel, map[string]string{
				lbTypeAnnotationKey:                  lbInternal,
				internalLBAllowProjectsAnnotationKey: secondaryProject,
			})

			t.Logf("Creating Service %s in namespace %s", serviceName, namespace)
			err = watchClient.Create(ctx, serviceSpec)
			if err != nil {
				t.Fatalf("failed to create service: %v", err)
			}

			ilbIP := waitForAndGetServiceIP(ctx, t, watchClient, namespace, serviceName)

			t.Run("Verify cross-project connectivity is allowed initially", func(t *testing.T) {
				t.Logf("Verifying connectivity from VM via SSH (expecting success)...")
				err := checkCrossProjectVMConnectivitySuccess(ctx, vmManager, ilbIP, secondaryProjectLBTimeout)
				if err != nil {
					t.Fatalf("Failed to connect from cross-project VM: %v", err)
				}
				t.Logf("Successfully verified connectivity")
			})

			t.Run("Verify cross-project connectivity is denied for second service without annotation", func(t *testing.T) {
				serviceName2 := "ilb-svc-cross-project-deny-second-" + suffix
				serviceSpec2 := &corev1.Service{
					ObjectMeta: metav1.ObjectMeta{
						Name:      serviceName2,
						Namespace: namespace,
						Annotations: map[string]string{
							lbTypeAnnotationKey: lbInternal,
						},
					},
					Spec: corev1.ServiceSpec{
						Type:     corev1.ServiceTypeLoadBalancer,
						Selector: appLabel,
						Ports: []corev1.ServicePort{
							{
								Name:     "http",
								Protocol: corev1.ProtocolTCP,
								Port:     80,
							},
						},
					},
				}

				t.Logf("Creating Second Service %s in namespace %s", serviceName2, namespace)
				err = watchClient.Create(ctx, serviceSpec2)
				if err != nil {
					t.Fatalf("failed to create second service: %v", err)
				}

				ilbIP2 := waitForAndGetServiceIP(ctx, t, watchClient, namespace, serviceName2)

				t.Logf("Verifying connectivity to second ILB from VM via SSH (expecting failure)...")
				err = checkCrossProjectVMConnectivityFailure(ctx, vmManager, ilbIP2, secondaryProjectLBNegativeTimeout)
				if err != nil {
					t.Fatalf("Expected connectivity to second ILB to fail, but got error or timeout: %v", err)
				}
				t.Logf("Successfully verified connectivity to second ILB is denied")
			})

			t.Run("Verify cross-project connectivity is denied after removing allowed project", func(t *testing.T) {
				// Mutate: Remove project
				t.Logf("Mutating service to remove allowed projects")
				var svc corev1.Service
				if err := watchClient.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, &svc); err != nil {
					t.Fatalf("failed to get service: %v", err)
				}
				delete(svc.Annotations, internalLBAllowProjectsAnnotationKey)
				if err := watchClient.Update(ctx, &svc); err != nil {
					t.Fatalf("failed to update service: %v", err)
				}

				t.Logf("Waiting for connectivity to fail after removing allowed project")
				err = checkCrossProjectVMConnectivityFailure(ctx, vmManager, ilbIP, secondaryProjectLBRevokeTimeout)
				if err != nil {
					t.Fatalf("Failed to verify that connectivity was revoked: %v", err)
				}
				t.Logf("Successfully verified connectivity is denied after mutation")
			})
		})

		t.Run("internal load balancer service with wild card should have access of cross project", func(t *testing.T) {
			t.Parallel()
			serviceName := "ilb-svc-cross-project-wildcard-" + suffix
			serviceSpec := serviceSpec(serviceName, namespace, appLabel, map[string]string{
				lbTypeAnnotationKey:                  lbInternal,
				internalLBAllowProjectsAnnotationKey: "*",
			})

			t.Logf("Creating Service %s in namespace %s", serviceName, namespace)
			err = watchClient.Create(ctx, serviceSpec)
			if err != nil {
				t.Fatalf("failed to create service: %v", err)
			}

			ilbIP := waitForAndGetServiceIP(ctx, t, watchClient, namespace, serviceName)

			t.Run("Verify cross-project connectivity is allowed with wildcard", func(t *testing.T) {
				t.Logf("Verifying connectivity from VM via SSH (expecting success)...")
				err := checkCrossProjectVMConnectivitySuccess(ctx, vmManager, ilbIP, secondaryProjectLBTimeout)
				if err != nil {
					t.Fatalf("Failed to connect from cross-project VM: %v", err)
				}
				t.Logf("Successfully verified connectivity")
			})

			t.Run("Verify cross-project connectivity is allowed after replacing wildcard with specific projects", func(t *testing.T) {
				// Mutate: Replace wildcard with specific projects
				t.Logf("Mutating service to replace wildcard with specific projects")
				var svc corev1.Service
				if err := watchClient.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, &svc); err != nil {
					t.Fatalf("failed to get service: %v", err)
				}
				svc.Annotations[internalLBAllowProjectsAnnotationKey] = releasePipelineCfg.GDC.Project + "," + secondaryProject
				if err := watchClient.Update(ctx, &svc); err != nil {
					t.Fatalf("failed to update service: %v", err)
				}

				t.Logf("Verifying connectivity after mutation (expecting success)...")
				err = checkCrossProjectVMConnectivitySuccess(ctx, vmManager, ilbIP, secondaryProjectLBTimeout)
				if err != nil {
					t.Fatalf("Failed to connect from cross-project VM after mutation: %v", err)
				}
				t.Logf("Successfully verified connectivity after mutation")
			})
		})
	})

}

func uniqueSuffix() string {
	commitHash := releaseConfig.GetCommitHashOrSanitize(*gardenerArtifactsVersion)
	return commitHash + "-" + uuid.NewString()[:8]
}

func podSpec(key client.ObjectKey, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "test",
					Image: image,
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
}

func serviceSpec(name, namespace string, appLabel, annotations map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeLoadBalancer,
			Selector: appLabel,
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Protocol:   corev1.ProtocolTCP,
					Port:       80,
					TargetPort: intstr.FromInt32(80),
				},
				{
					Name:       "http-alt-443",
					Protocol:   corev1.ProtocolTCP,
					Port:       443,
					TargetPort: intstr.FromInt32(80),
				},
			},
		},
	}
}

func getServiceIP(ctx context.Context, c client.Client, name, namespace string) (string, error) {
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Name: name, Namespace: namespace}, svc); err != nil {
		return "", fmt.Errorf("failed to get service %s in %s namespace: %w", name, namespace, err)
	}
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		return "", fmt.Errorf("could not get the IP for LB service %s in %q namespace", name, namespace)
	}
	return svc.Status.LoadBalancer.Ingress[0].IP, nil
}

func verifyInClusterConnectivity(ctx context.Context, t *testing.T, goClient clientset.Interface, restConfig *rest.Config, namespace, podName, ip string) {
	t.Logf("Testing connectivity from tester pod %q to the LB service with IP %q", podName, ip)
	url := fmt.Sprintf("http://%s", ip)
	curlCommand := []string{"curl", url}

	err := wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		positiveConnectivityTimeout,
		true,
		func(ctx context.Context) (bool, error) {
			if err := kubernetes.ExecPod(ctx, goClient, restConfig, curlCommand, namespace, podName); err != nil {
				t.Logf("In-cluster connection attempt failed: %v. Retrying...", err)
				return false, nil
			}
			return true, nil
		},
	)
	if err != nil {
		t.Fatalf("failed to establish in-cluster connectivity after timeout: %v", err)
	}
}

// checkCrossProjectVMConnectivitySuccess verifies that connectivity from a VM via SSH to a target IP succeeds within the timeout.
func checkCrossProjectVMConnectivitySuccess(ctx context.Context, vmManager *virtualmachine.VMManager, targetIP string, timeout time.Duration) error {
	command := fmt.Sprintf("timeout 5 bash -c 'cat < /dev/null > /dev/tcp/%s/80'", targetIP)
	output, err := vmManager.RunSSHCommand(ctx, virtualmachine.SSHCommandOptions{
		Command: command,
		Timeout: timeout,
	})
	if err != nil {
		return fmt.Errorf("expected success but failed: %w, output: %s", err, output)
	}
	return nil
}

// checkCrossProjectVMConnectivityFailure verifies that connectivity from a VM via SSH to a target IP fails (timeouts) within the timeout.
func checkCrossProjectVMConnectivityFailure(ctx context.Context, vmManager *virtualmachine.VMManager, targetIP string, timeout time.Duration) error {
	// Command succeeds (exit 0) when connectivity fails (timeout)
	command := fmt.Sprintf("timeout 5 bash -c 'cat < /dev/null > /dev/tcp/%s/80' && exit 1 || exit 0", targetIP)
	output, err := vmManager.RunSSHCommand(ctx, virtualmachine.SSHCommandOptions{
		Command: command,
		Timeout: timeout,
	})
	if err != nil {
		return fmt.Errorf("expected connectivity to fail but it kept succeeding or timed out: %w, output: %s", err, output)
	}
	return nil
}

func waitForAndGetServiceIP(ctx context.Context, t *testing.T, watchClient client.WithWatch, namespace, serviceName string) string {
	t.Logf("Waiting for LoadBalancer IP for service %s", serviceName)
	if err := kubernetes.WaitForLoadBalancerIP(ctx, watchClient, namespace, serviceName, waitForLBServiceTimeout); err != nil {
		t.Fatalf("failed to allocate IP for LB service %s in %q namespace: %v", serviceName, namespace, err)
	}
	t.Logf("LoadBalancer Service %s has IP allocated", serviceName)
	ip, err := getServiceIP(ctx, watchClient, serviceName, namespace)
	if err != nil {
		t.Fatalf("failed to get IP after waiting: %v", err)
	}
	return ip
}

func verifyExternalConnectivity(ctx context.Context, t *testing.T, ip string, timeout time.Duration) error {
	t.Logf("Testing connectivity from outside the cluster to the LB service with IP %q", ip)
	urls := []string{
		fmt.Sprintf("http://%s:443", ip),
		fmt.Sprintf("http://%s", ip),
	}
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
	}
	return wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		timeout,
		true,
		func(ctx context.Context) (bool, error) {
			var lastErr error
			for _, url := range urls {
				resp, err := httpClient.Get(url)
				if err != nil {
					lastErr = err
					continue
				}
				statusCode := resp.StatusCode
				status := resp.Status
				_ = resp.Body.Close()

				if statusCode == http.StatusOK {
					t.Logf("Successfully reached the service at %s with status 200 OK!", url)
					return true, nil
				}
				lastErr = fmt.Errorf("non-200 status from %s: %s", url, status)
			}

			t.Logf("Connection attempt failed: %v. Retrying...", lastErr)
			return false, nil
		},
	)
}

func waitForServiceIPToChange(ctx context.Context, t *testing.T, c client.Client, serviceName, namespace, oldIP string) (string, error) {
	var newIP string
	err := wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		waitForLBServiceTimeout,
		true,
		func(ctx context.Context) (bool, error) {
			ip, err := getServiceIP(ctx, c, serviceName, namespace)
			if err != nil {
				t.Logf("Waiting for IP change, current error: %v", err)
				return false, nil
			}
			if ip != oldIP && ip != "" {
				newIP = ip
				return true, nil
			}
			return false, nil
		},
	)
	return newIP, err
}

func waitForNamespaceDeletion(ctx context.Context, t *testing.T, c client.Client, namespace string) error {
	ns := &corev1.Namespace{}
	return wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		waitForNamespaceDeletionTimeout,
		true,
		func(ctx context.Context) (bool, error) {
			err := c.Get(ctx, client.ObjectKey{Name: namespace}, ns)
			if err != nil {
				if client.IgnoreNotFound(err) == nil {
					t.Logf("Namespace %s deleted successfully", namespace)
					return true, nil
				}
				return false, err
			}
			return false, nil
		},
	)
}

// getFirstManagementClient returns the management client for the first configured zone.
// It fails the test if no zones are configured or if the client is missing.
func getFirstManagementClient(t *testing.T, cfg *config.ReleaseTestConfig) client.WithWatch {
	if len(cfg.GDC.Zones) == 0 {
		t.Fatal("GDC.Zones must not be empty in release configuration")
	}
	zone := cfg.GDC.Zones[0]
	c, ok := cfg.ManagementClients[zone]
	if !ok {
		t.Fatalf("Management client for zone %s not found", zone)
	}
	return c
}

// setupSecondaryProjectVM creates a VM and Egress PNP in the management cluster for cross-project testing.
// It uses t.Cleanup to ensure resources are deleted in reverse order.
func setupSecondaryProjectVM(ctx context.Context, t *testing.T, releasePipelineCfg *config.ReleaseTestConfig, vmName, secondaryProject, gdcProject string) *virtualmachine.VMManager {
	mgmtClient := getFirstManagementClient(t, releasePipelineCfg)
	vmManager := virtualmachine.NewManager(mgmtClient, client.ObjectKey{Name: vmName, Namespace: secondaryProject})

	t.Logf("Creating Shared Test Virtual Machine %s in namespace %s", vmName, secondaryProject)
	err := vmManager.Create(ctx)
	if err != nil {
		t.Fatalf("failed to create test VM: %v", err)
	}
	t.Cleanup(func() {
		t.Logf("Deleting Shared Test Virtual Machine %s", vmName)
		if err := vmManager.Delete(ctx, deleteVMTimeout); err != nil {
			t.Logf("Warning: failed to delete test VM %s: %v", vmName, err)
		}
	})

	pnpName := fmt.Sprintf("allow-egress-%s-to-%s", vmName, gdcProject)
	t.Logf("Creating Egress ProjectNetworkPolicy %s in namespace %s to project %s", pnpName, secondaryProject, gdcProject)
	_, err = pnp.CreateEgressZonalProjectNetworkPolicy(ctx, mgmtClient, client.ObjectKey{Name: pnpName, Namespace: secondaryProject}, gdcProject, map[string]string{virtualmachine.TestVMLabelKey: vmName})
	if err != nil {
		t.Fatalf("failed to create egress PNP: %v", err)
	}
	t.Cleanup(func() {
		t.Logf("Deleting Egress ProjectNetworkPolicy %s in namespace %s", pnpName, secondaryProject)
		if err := pnp.DeleteZonalProjectNetworkPolicy(ctx, mgmtClient, client.ObjectKey{Name: pnpName, Namespace: secondaryProject}); err != nil {
			t.Logf("Warning: failed to delete egress PNP %s: %v", pnpName, err)
		}
	})

	return vmManager
}

// cleanupStaleLoadBalancerTestResources removes any leftover lb-lifecycle-test-* namespaces on the Shoot
// and elb-test-*/ilb-test-* subnets in the Global API project namespace from a prior timed-out attempt,
// polling until all stale resources and their child IP allocations are completely removed so retries
// do not hit IPAM0012 IP shortage when the parent public subnet group has a single /31 CIDR.
func cleanupStaleLoadBalancerTestResources(ctx context.Context, t *testing.T, shootClient client.WithWatch, globalClient client.Client, project string) {
	// Step 1: Poll until all stale lb-lifecycle-test-* Shoot namespaces (and their LoadBalancer Services) are deleted
	// so cloud-controller-manager releases any allocated IPs from the custom test subnets.
	if err := wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		waitForNamespaceDeletionTimeout,
		true,
		func(ctx context.Context) (bool, error) {
			var nsList corev1.NamespaceList
			if err := shootClient.List(ctx, &nsList); err != nil {
				t.Logf("Warning: failed to list Shoot namespaces during stale cleanup (will retry): %v", err)
				return false, nil
			}
			remaining := 0
			for _, ns := range nsList.Items {
				if strings.HasPrefix(ns.Name, lbLifecycleNamespacePrefix) {
					remaining++
					if ns.DeletionTimestamp.IsZero() {
						t.Logf("Deleting stale LoadBalancer test namespace %s...", ns.Name)
						kubernetes.CleanupResources(t, shootClient, ns.Name)
					}
				}
			}
			return remaining == 0, nil
		},
	); err != nil {
		t.Logf("Warning: timed out waiting for stale %s* namespaces to be deleted: %v", lbLifecycleNamespacePrefix, err)
	}

	// Step 2: Poll until all stale elb-test-*-leaf and ilb-test-*-leaf subnets are deleted.
	// The IPAM admission webhook rejects subnet deletion ("cannot delete subnet with children") while
	// LoadBalancer IPAddress child objects are still being asynchronously cleaned up, so we poll and retry Delete.
	deleteSubnetsBySuffix := func(suffix string) {
		if err := wait.PollUntilContextTimeout(
			ctx,
			pollInterval,
			waitForLBServiceTimeout,
			true,
			func(ctx context.Context) (bool, error) {
				var subnetList ipamglobalv1.SubnetList
				if err := globalClient.List(ctx, &subnetList, client.InNamespace(project)); err != nil {
					t.Logf("Warning: failed to list Subnets in %s during stale cleanup (will retry): %v", project, err)
					return false, nil
				}
				remaining := 0
				for i := range subnetList.Items {
					s := &subnetList.Items[i]
					if (strings.HasPrefix(s.Name, elbTestSubnetPrefix) || strings.HasPrefix(s.Name, ilbTestSubnetPrefix)) && strings.HasSuffix(s.Name, suffix) {
						remaining++
						if s.DeletionTimestamp.IsZero() {
							t.Logf("Deleting stale LoadBalancer subnet %s/%s...", project, s.Name)
							if err := globalClient.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
								t.Logf("Waiting for child IP allocations to release before deleting subnet %s/%s: %v", project, s.Name, err)
							}
						}
					}
				}
				return remaining == 0, nil
			},
		); err != nil {
			t.Logf("Warning: timed out waiting for stale *%s subnets in %s to be deleted: %v", suffix, project, err)
		}
	}

	// Leaf subnets must be completely deleted before their parent branch subnets can be deleted.
	deleteSubnetsBySuffix(leafSubnetSuffix)
	deleteSubnetsBySuffix(branchSubnetSuffix)
}
