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

package workarounds

import (
	"context"
	"fmt"

	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateGardenletCRDDeleterRoleBinding grants permission to delete CRDs to the gardenlet service account.
// NOTE: This is a workaround needed for gardener version v1.134.1 and should be deleted later.
func CreateGardenletCRDDeleterRoleBinding(ctx context.Context, c client.Client) error {
	cr := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gardenlet-crd-deleter",
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"apiextensions.k8s.io"},
				Resources: []string{"customresourcedefinitions"},
				Verbs:     []string{"delete", "update"},
			},
		},
	}
	if err := c.Create(ctx, cr); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ClusterRole gardenlet-crd-deleter: %w", err)
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gardenlet-crd-deleter",
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     "gardenlet-crd-deleter",
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      "gardenlet",
				Namespace: "garden",
			},
		},
	}
	if err := c.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create ClusterRoleBinding gardenlet-crd-deleter: %w", err)
	}

	return nil
}
