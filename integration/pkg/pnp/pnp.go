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

package pnp

import (
	"context"
	"fmt"
	"time"

	gdchnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	k8snetworkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/diagnostics"
)

const (
	// AnyCIDR represents all IPv4 addresses.
	AnyCIDR = "0.0.0.0/0"

	// defaultPNPTimeout is the default timeout for waiting for a ProjectNetworkPolicy to be healthy.
	defaultPNPTimeout = 10 * time.Minute
	// defaultPNPPollInterval is the default poll interval for checking ProjectNetworkPolicy health.
	defaultPNPPollInterval = 5 * time.Second

	// conditionTypeReady is the condition type for Ready status.
	conditionTypeReady = "Ready"
)

// CreateIngressZonalProjectNetworkPolicy creates a zonal ProjectNetworkPolicy
// that allows ingress from all sources for workloads selected with the provided labels.
// It waits for the policy to become healthy before returning.
func CreateIngressZonalProjectNetworkPolicy(ctx context.Context, managementClient client.Client, key client.ObjectKey, matchLabels map[string]string) (*gdchnetworkingv1.ProjectNetworkPolicy, error) {
	spec := gdchnetworkingv1.ProjectNetworkPolicySpec{
		PolicyType: gdchnetworkingv1.PolicyTypeIngress,
		Subject: gdchnetworkingv1.ProjectNetworkPolicySubject{
			SubjectType: gdchnetworkingv1.PolicySubjectTypeUserWorkload,
			UserWorkloadSelector: &gdchnetworkingv1.WorkloadSelector{
				LabelSelector: &gdchnetworkingv1.WorkloadLabelSelector{
					Workloads: &metav1.LabelSelector{
						MatchLabels: matchLabels,
					},
				},
			},
		},
		Ingress: []gdchnetworkingv1.ProjectNetworkPolicyIngressRule{
			{
				From: []gdchnetworkingv1.ProjectNetworkPolicyPeer{
					{
						IPBlock: &k8snetworkingv1.IPBlock{
							CIDR: AnyCIDR,
						},
					},
				},
			},
		},
	}

	pnpObj, err := createProjectNetworkPolicy(ctx, managementClient, key, spec)
	if err != nil {
		return nil, fmt.Errorf("failed in CreateIngressZonalProjectNetworkPolicy/createProjectNetworkPolicy: %w", err)
	}

	if err := waitForProjectNetworkPolicyHealthy(ctx, managementClient, key, defaultPNPTimeout); err != nil {
		return nil, fmt.Errorf("failed in CreateIngressZonalProjectNetworkPolicy/waitForProjectNetworkPolicyHealthy: %w", err)
	}

	return pnpObj, nil
}

// CreateEgressZonalProjectNetworkPolicy creates a zonal ProjectNetworkPolicy
// that allows all egress to a specific project for workloads selected with the provided labels.
// It waits for the policy to become healthy before returning.
func CreateEgressZonalProjectNetworkPolicy(ctx context.Context, managementClient client.Client, key client.ObjectKey, targetProject string, matchLabels map[string]string) (*gdchnetworkingv1.ProjectNetworkPolicy, error) {
	spec := gdchnetworkingv1.ProjectNetworkPolicySpec{
		PolicyType: gdchnetworkingv1.PolicyTypeEgress,
		Subject: gdchnetworkingv1.ProjectNetworkPolicySubject{
			SubjectType: gdchnetworkingv1.PolicySubjectTypeUserWorkload,
			UserWorkloadSelector: &gdchnetworkingv1.WorkloadSelector{
				LabelSelector: &gdchnetworkingv1.WorkloadLabelSelector{
					Workloads: &metav1.LabelSelector{
						MatchLabels: matchLabels,
					},
				},
			},
		},
		Egress: []gdchnetworkingv1.ProjectNetworkPolicyEgressRule{
			{
				To: []gdchnetworkingv1.ProjectNetworkPolicyPeer{
					{
						Projects: &gdchnetworkingv1.PolicyProjects{
							MatchNames: []string{targetProject},
						},
					},
				},
			},
		},
	}

	pnpObj, err := createProjectNetworkPolicy(ctx, managementClient, key, spec)
	if err != nil {
		return nil, fmt.Errorf("failed in CreateEgressZonalProjectNetworkPolicy/createProjectNetworkPolicy: %w", err)
	}

	if err := waitForProjectNetworkPolicyHealthy(ctx, managementClient, key, defaultPNPTimeout); err != nil {
		return nil, fmt.Errorf("failed in CreateEgressZonalProjectNetworkPolicy/waitForProjectNetworkPolicyHealthy: %w", err)
	}

	return pnpObj, nil
}

// DeleteZonalProjectNetworkPolicy deletes a zonal ProjectNetworkPolicy.
func DeleteZonalProjectNetworkPolicy(ctx context.Context, managementClient client.Client, key client.ObjectKey) error {
	pnp := &gdchnetworkingv1.ProjectNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}

	if err := managementClient.Delete(ctx, pnp); err != nil {
		return client.IgnoreNotFound(err)
	}
	return nil
}

// createProjectNetworkPolicy creates or updates a ProjectNetworkPolicy with the given spec.
func createProjectNetworkPolicy(ctx context.Context, managementClient client.Client, key client.ObjectKey, spec gdchnetworkingv1.ProjectNetworkPolicySpec) (*gdchnetworkingv1.ProjectNetworkPolicy, error) {
	pnpObj := &gdchnetworkingv1.ProjectNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
		},
	}

	_, err := controllerutil.CreateOrUpdate(ctx, managementClient, pnpObj, func() error {
		pnpObj.Spec = spec
		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to ensure zonal ProjectNetworkPolicy: %w", err)
	}
	return pnpObj, nil
}

// waitForProjectNetworkPolicyHealthy polls the ProjectNetworkPolicy status until it is ready or times out.
func waitForProjectNetworkPolicyHealthy(ctx context.Context, managementClient client.Client, key client.ObjectKey, timeout time.Duration) error {
	pollErr := wait.PollUntilContextTimeout(ctx, defaultPNPPollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		pnp := &gdchnetworkingv1.ProjectNetworkPolicy{}
		err := managementClient.Get(ctx, key, pnp)
		if err != nil {
			return false, nil // Retry on error getting resource
		}

		for _, condition := range pnp.Status.Conditions {
			if condition.Type == conditionTypeReady {
				if condition.Status == metav1.ConditionTrue {
					return true, nil // Healthy
				}
				return false, nil // Not Healthy
			}
		}
		return false, nil // Not Healthy
	})
	if pollErr != nil {
		pnpYAML := diagnostics.GetObjectYAML(ctx, managementClient, key, &gdchnetworkingv1.ProjectNetworkPolicy{})
		return fmt.Errorf("ProjectNetworkPolicy %s/%s not healthy (timeout):\n%s\nerror: %w", key.Namespace, key.Name, pnpYAML, pollErr)
	}
	return nil
}
