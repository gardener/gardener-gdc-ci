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

package dns

import (
	"context"
	"flag"
	"net"
	"testing"
	"time"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/config/loader"
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
	waitForLBServiceTimeout = 3 * time.Minute
	// DNS propagation can be delayed even after the RRset is active (b/417143104).
	// Waiting for 6 minutes gives the changes time to take effect.
	waitForDNSRecordTimeout = 15 * time.Minute
	pollInterval            = 5 * time.Second
)

func TestShootDNSLifecycle(t *testing.T) {
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

	gardenClient := releasePipelineCfg.TestShoot.VirtualGarden.Client

	ctx := context.Background()
	shootKey := client.ObjectKey{Name: releasePipelineCfg.TestShoot.Name, Namespace: releasePipelineCfg.TestShoot.Namespace}
	t.Logf("Getting shoot object for %s/%s", releasePipelineCfg.TestShoot.Namespace, releasePipelineCfg.TestShoot.Name)
	shootObj, err := gardener.GetShoot(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to get shoot %s/%s from garden cluster: %v", releasePipelineCfg.TestShoot.Namespace, releasePipelineCfg.TestShoot.Name, err)
	}
	shootDomain := shootObj.Spec.DNS.Domain

	// Fetch the initial state of the Shoot
	t.Logf("Creating shoot client for %s/%s", releasePipelineCfg.TestShoot.Namespace, releasePipelineCfg.TestShoot.Name)
	shootClients, err := gardener.NewShootClient(ctx, gardenClient, shootKey)
	if err != nil {
		t.Fatalf("failed to create shoot client: %v", err)
	}
	shootWatchClient := shootClients.WatchClient

	suffix := uniqueSuffix()
	namespace := "dns-test-" + suffix
	t.Logf("Creating Namespace %s...", namespace)
	err = kubernetes.CreateNamespace(ctx, shootWatchClient, namespace)
	if err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Cleaning up resources in namespace %s", namespace)
		kubernetes.CleanupResources(t, shootWatchClient, namespace)
	})

	serviceName := "dns-lb-svc"
	hostname := "release-" + suffix + "." + *shootDomain
	serviceSpec := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace,
			Annotations: map[string]string{
				"dns.gardener.cloud/class":    "garden",
				"dns.gardener.cloud/dnsnames": hostname,
				"dns.gardener.cloud/ttl":      "600",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeLoadBalancer,
			Ports: []corev1.ServicePort{
				{
					Name:     "http",
					Protocol: corev1.ProtocolTCP,
					Port:     80,
				},
			},
		},
	}

	t.Logf("Creating LB Service %q in %q namespace with hostname %q", serviceName, namespace, hostname)
	err = shootWatchClient.Create(ctx, serviceSpec)
	if err != nil {
		t.Fatalf("failed to create service %s in namespace %s: %v", serviceName, namespace, err)
	}
	t.Logf("LoadBalancer Service %s in %s namespace created", serviceName, namespace)

	t.Logf("Waiting for LoadBalancer IP for service %s", serviceName)
	if err := kubernetes.WaitForLoadBalancerIP(ctx, shootWatchClient, namespace, serviceName, waitForLBServiceTimeout); err != nil {
		t.Fatalf("failed to allocate External IP for LB service %s in %q namespace: %v", serviceName, namespace, err)
	}
	t.Logf("LoadBalancer Service %s has external IP allocated", serviceName)

	t.Logf("Getting External IP of the LB service %s", serviceName)
	svc := &corev1.Service{}
	if err := shootWatchClient.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: namespace}, svc); err != nil {
		t.Fatalf("failed to get service %s in %s namespace: %v", serviceName, namespace, err)
	}
	if len(svc.Status.LoadBalancer.Ingress) == 0 {
		t.Fatalf("Could not get the External IP for LB service %s in \"%s\" namespace: %v", serviceName, namespace, err)
	}
	externalIP := svc.Status.LoadBalancer.Ingress[0].IP

	t.Logf("Polling until DNS record for %q is propagated to %s", hostname, externalIP)
	err = wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		waitForDNSRecordTimeout,
		/*immediate= */ true,
		func(ctx context.Context) (done bool, err error) {
			ips, lookupErr := net.LookupHost(hostname)
			if lookupErr != nil {
				t.Logf("DNS lookup for %q failed: %v. Retrying...", hostname, lookupErr)
				return false, nil
			}

			// Check if the returned IP matches the expected IP.
			if len(ips) == 1 && ips[0] == externalIP {
				t.Logf("Success! DNS for %q resolved to the correct IP: %s", hostname, externalIP)
				return true, nil
			}

			// The DNS record exists but is incorrect (or has multiple IPs). Keep polling.
			t.Logf("DNS lookup for %q resolved to %v, expecting [%s]. Retrying...", hostname, ips, externalIP)
			return false, nil
		})

	if err != nil {
		t.Fatalf("DNS lookup for '%s' failed with an error: %v", hostname, err)
	}
}

func uniqueSuffix() string {
	return uuid.NewString()[:16]
}
