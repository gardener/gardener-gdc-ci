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
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	ipamglobalv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/ipam/v1"
	globalnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/global/networking/v1"
	ipamv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/ipam/v1"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/diagnostics"
)

const (
	pollInterval                        = 2 * time.Second
	forwardingRuleExternalAnnotationKey = "networking.private.gdc.goog/forwarding-rule-external"
	forwardingRuleInternalAnnotationKey = "networking.private.gdc.goog/forwarding-rule-internal"
)

// SubnetOptions defines the options for EnsureSubnet.
type SubnetOptions struct {
	Namespace         string
	Name              string
	ParentSubnetGroup string
	ParentType        string
	PrefixLength      int32
	SkipVPCLabel      bool
}

func createOrUpdateSubnet(ctx context.Context, c client.Client, opts SubnetOptions) error {
	subnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, c, subnet, func() error {
		if subnet.Annotations == nil {
			subnet.Annotations = make(map[string]string)
		}
		subnet.Annotations[ipamv1.PauseAutoDivisionAnnotation] = "true"
		if subnet.Labels == nil {
			subnet.Labels = make(map[string]string)
		}
		// Add default VPC label to satisfy admission webhook requirements unless skipped.
		// Webhook requires child subnets to have consistent VPC labels with their parent.
		// (e.g., needed for ILB tests where parent has it, but skipped for ELB tests where parent lacks it).
		if !opts.SkipVPCLabel {
			subnet.Labels[ipamv1.VPCLabel] = "default-vpc"
		}
		subnet.Spec.Type = ipamv1.Branch

		parentType := opts.ParentType
		if parentType == "" {
			parentType = string(ipamv1.SubnetGroup)
		}

		subnet.Spec.ParentReference = &ipamv1.SubnetReference{
			Name:      opts.ParentSubnetGroup,
			Namespace: ptr.To(opts.Namespace),
			Type:      ipamv1.ReferenceType(parentType),
		}
		subnet.Spec.IPv4Request = &ipamv1.SubnetRequest{PrefixLength: ptr.To(opts.PrefixLength)}
		return nil
	})
	return err
}

// EnsureSubnet creates a subnet if it doesn't exist and waits for it to be ready.
// Returns the allocated CIDR.
func EnsureSubnet(ctx context.Context, c client.Client, opts SubnetOptions, timeout time.Duration, t *testing.T) (string, error) {
	t.Logf("Creating Subnet %s in namespace %s", opts.Name, opts.Namespace)

	var lastCreateErr error
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		if err := createOrUpdateSubnet(ctx, c, opts); err != nil {
			lastCreateErr = err
			t.Logf("Failed to create or update Subnet '%s/%s' (will retry): %v", opts.Namespace, opts.Name, err)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		if lastCreateErr != nil {
			return "", fmt.Errorf("failed to create Subnet '%s/%s': %w (poll error: %v)", opts.Namespace, opts.Name, lastCreateErr, err)
		}
		return "", fmt.Errorf("failed to create Subnet '%s/%s': %w", opts.Namespace, opts.Name, err)
	}

	t.Log("Waiting for the subnet to be Ready")
	var allocatedCIDR string
	err = wait.PollUntilContextTimeout(
		ctx,
		pollInterval,
		timeout,
		true,
		func(ctx context.Context) (bool, error) {
			s := &ipamglobalv1.Subnet{}
			if err := c.Get(ctx, client.ObjectKey{Name: opts.Name, Namespace: opts.Namespace}, s); err != nil {
				if !apierrors.IsNotFound(err) {
					t.Logf("Failed to get Subnet '%s/%s' while waiting for Ready (will retry): %v", opts.Namespace, opts.Name, err)
				}
				return false, nil
			}
			if meta.IsStatusConditionTrue(s.Status.Conditions, "Ready") && s.Status.IPv4Allocation != nil {
				allocatedCIDR = s.Status.IPv4Allocation.CIDR
				return true, nil
			}
			return false, nil
		},
	)
	if err != nil {
		subnetYAML := diagnostics.GetObjectYAML(ctx, c, client.ObjectKey{Name: opts.Name, Namespace: opts.Namespace}, &ipamglobalv1.Subnet{})
		return "", fmt.Errorf("subnet '%s/%s' did not become Ready (timeout):\n%s\nerror: %w", opts.Namespace, opts.Name, subnetYAML, err)
	}
	t.Logf("Subnet '%s/%s' is Ready.", opts.Namespace, opts.Name)

	return allocatedCIDR, nil
}

// DeleteSubnet deletes a subnet and waits until it is gone.
func DeleteSubnet(ctx context.Context, c client.Client, key client.ObjectKey, timeout time.Duration, t *testing.T) error {
	t.Logf("Deleting Subnet %s", key.Name)
	subnet := &ipamglobalv1.Subnet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}

	t.Logf("Waiting for Subnet %s to be deleted", key.Name)
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := wait.PollUntilContextCancel(pollCtx, pollInterval, true, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, key, subnet); err != nil {
			if apierrors.IsNotFound(err) {
				t.Logf("Subnet %s deleted successfully", key.Name)
				return true, nil
			}
			t.Logf("Failed to get Subnet %s during deletion (will retry): %v", key.Name, err)
			return false, nil
		}

		if subnet.DeletionTimestamp.IsZero() {
			// Retry c.Delete because the IPAM admission webhook rejects deletion
			// ("cannot delete subnet with children") while child resources (such as
			// LoadBalancer IP allocations) are still being asynchronously cleaned up.
			if err := c.Delete(ctx, subnet); client.IgnoreNotFound(err) != nil {
				t.Logf("Failed to delete subnet %s (will retry): %v", key.Name, err)
				return false, nil
			}
		}

		return false, nil
	})
	if err != nil {
		subnetYAML := diagnostics.GetObjectYAML(ctx, c, key, &ipamglobalv1.Subnet{})
		return fmt.Errorf("subnet %s not deleted (timeout):\n%s\nerror: %w", key.Name, subnetYAML, err)
	}
	return nil
}

// CleanupOrphanedServiceSubnets deletes orphaned g-svc-* LoadBalancer Subnets in the given project
// namespace whose owning ForwardingRuleExternal or ForwardingRuleInternal no longer exists and whose
// ConsumerRef has been cleared. On GDC Global API clusters, ownerReferences garbage collection does
// not automatically delete g-svc-* Subnets after their ForwardingRule is deleted, which can exhaust
// the /32 IP pool in platform/data-network-segment-anycast-group (IPAM0012).
func CleanupOrphanedServiceSubnets(ctx context.Context, c client.Client, namespace string) {
	if c == nil || namespace == "" {
		return
	}

	var extFRList globalnetworkingv1.ForwardingRuleExternalList
	if err := c.List(ctx, &extFRList, client.InNamespace(namespace)); err != nil {
		log.Printf("Warning: failed to list ForwardingRuleExternals in %s during orphaned subnet cleanup: %v", namespace, err)
		return
	}
	activeExtFRs := make(map[string]struct{}, len(extFRList.Items))
	for i := range extFRList.Items {
		activeExtFRs[extFRList.Items[i].Name] = struct{}{}
	}

	var intFRList globalnetworkingv1.ForwardingRuleInternalList
	if err := c.List(ctx, &intFRList, client.InNamespace(namespace)); err != nil {
		log.Printf("Warning: failed to list ForwardingRuleInternals in %s during orphaned subnet cleanup: %v", namespace, err)
		return
	}
	activeIntFRs := make(map[string]struct{}, len(intFRList.Items))
	for i := range intFRList.Items {
		activeIntFRs[intFRList.Items[i].Name] = struct{}{}
	}

	var subnetList ipamglobalv1.SubnetList
	if err := c.List(ctx, &subnetList, client.InNamespace(namespace)); err != nil {
		log.Printf("Warning: failed to list Subnets in %s during orphaned subnet cleanup: %v", namespace, err)
		return
	}

	deletedCount := 0
	for i := range subnetList.Items {
		s := &subnetList.Items[i]
		if !s.DeletionTimestamp.IsZero() {
			continue
		}
		if s.Status.ConsumerRef != nil || len(s.Status.ChildrenRefs) > 0 {
			continue
		}
		isTestLBSubnet := strings.HasPrefix(s.Name, "elb-test-") || strings.HasPrefix(s.Name, "ilb-test-")
		if !isTestLBSubnet && !strings.HasPrefix(s.Name, "g-svc-") {
			continue
		}

		if isTestLBSubnet || isOrphanedForwardingRuleSubnet(s, activeExtFRs, activeIntFRs) {
			if err := c.Delete(ctx, s); client.IgnoreNotFound(err) != nil {
				log.Printf("Warning: failed to delete orphaned LB subnet %s/%s: %v", namespace, s.Name, err)
			} else {
				deletedCount++
			}
		}
	}

	if deletedCount > 0 {
		log.Printf("Cleaned up %d orphaned LoadBalancer subnet(s) in namespace %s", deletedCount, namespace)
	}
}

func isOrphanedForwardingRuleSubnet(s *ipamglobalv1.Subnet, activeExtFRs, activeIntFRs map[string]struct{}) bool {
	if s.Annotations != nil {
		if extFR := s.Annotations[forwardingRuleExternalAnnotationKey]; extFR != "" {
			_, exists := activeExtFRs[extFR]
			return !exists
		}
		if intFR := s.Annotations[forwardingRuleInternalAnnotationKey]; intFR != "" {
			_, exists := activeIntFRs[intFR]
			return !exists
		}
	}

	for _, owner := range s.OwnerReferences {
		switch owner.Kind {
		case "ForwardingRuleExternal":
			_, exists := activeExtFRs[owner.Name]
			return !exists
		case "ForwardingRuleInternal":
			_, exists := activeIntFRs[owner.Name]
			return !exists
		}
	}

	return false
}
