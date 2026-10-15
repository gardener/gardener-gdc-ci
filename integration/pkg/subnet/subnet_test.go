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

package subnet

import (
	"context"
	"testing"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCleanupOrphanedServiceSubnets(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := ipamglobalv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add ipamglobalv1 to scheme: %v", err)
	}
	if err := globalnetworkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add globalnetworkingv1 to scheme: %v", err)
	}

	const ns = "sapbtp"

	activeExtFR := &globalnetworkingv1.ForwardingRuleExternal{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "aactive-fr",
			Namespace: ns,
		},
	}

	activeExtSubnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "g-svc-aactive-fr--extern-11111111",
			Namespace: ns,
			Annotations: map[string]string{
				forwardingRuleExternalAnnotationKey: "aactive-fr",
			},
		},
	}

	orphanedExtSubnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "g-svc-aorphaned-fr--extern-22222222",
			Namespace: ns,
			Annotations: map[string]string{
				forwardingRuleExternalAnnotationKey: "aorphaned-fr",
			},
		},
	}

	orphanedIntSubnetByOwnerRef := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "g-svc-aorphaned-int-fr--intern-33333333",
			Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "networking.global.gdc.goog/v1",
					Kind:       "ForwardingRuleInternal",
					Name:       "aorphaned-int-fr",
				},
			},
		},
	}

	inUseSubnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "g-svc-ainuse-fr--extern-44444444",
			Namespace: ns,
			Annotations: map[string]string{
				forwardingRuleExternalAnnotationKey: "ainuse-fr",
			},
		},
		Status: ipamglobalv1.SubnetStatus{
			ConsumerRef: &ipamv1.ConsumerRef{
				Kind: "ForwardingRuleExternal",
				Name: "ainuse-fr",
			},
		},
	}

	staleTestBranchSubnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "elb-test-v0-5-7-60f8069e-branch",
			Namespace: ns,
		},
	}

	parentPublicSubnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardener-release-pipeline-parent-public-subnet",
			Namespace: ns,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			activeExtFR,
			activeExtSubnet,
			orphanedExtSubnet,
			orphanedIntSubnetByOwnerRef,
			inUseSubnet,
			staleTestBranchSubnet,
			parentPublicSubnet,
		).
		Build()

	CleanupOrphanedServiceSubnets(ctx, c, ns)

	assertExists := func(name string, wantExists bool) {
		t.Helper()
		var got ipamglobalv1.Subnet
		err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &got)
		if wantExists && err != nil {
			t.Errorf("expected subnet %s to remain, got err=%v", name, err)
		}
		if !wantExists && !apierrors.IsNotFound(err) {
			t.Errorf("expected subnet %s to be deleted, got err=%v", name, err)
		}
	}

	assertExists(activeExtSubnet.Name, true)
	assertExists(inUseSubnet.Name, true)
	assertExists(parentPublicSubnet.Name, true)
	assertExists(orphanedExtSubnet.Name, false)
	assertExists(orphanedIntSubnetByOwnerRef.Name, false)
	assertExists(staleTestBranchSubnet.Name, false)
}
