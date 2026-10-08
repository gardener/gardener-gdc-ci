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

package virtualmachine

import (
	"context"
	"testing"

	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGetGDCHMachineImage(t *testing.T) {
	tests := []struct {
		name          string
		images        []vmv1.VirtualMachineImage
		expectedImage string
		expectError   bool
	}{
		{
			name: "prefer ubuntu and pick latest",
			images: []vmv1.VirtualMachineImage{
				{ObjectMeta: metav1.ObjectMeta{Name: "rocky-8-v20241106-gdch", Namespace: defaultImageNamespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-20.04-v20241106-gdch", Namespace: defaultImageNamespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-22.04-v20250210-gdch", Namespace: defaultImageNamespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-24.04-v20250809-gdch", Namespace: defaultImageNamespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: "ubuntu-20.04-v20250809-gdch", Namespace: defaultImageNamespace}},
			},
			expectedImage: "ubuntu-24.04-v20250809-gdch",
		},
		{
			name: "fallback to first if no ubuntu",
			images: []vmv1.VirtualMachineImage{
				{ObjectMeta: metav1.ObjectMeta{Name: "rocky-8-v20241106-gdch", Namespace: defaultImageNamespace}},
				{ObjectMeta: metav1.ObjectMeta{Name: "rocky-9-v20250210-gdch", Namespace: defaultImageNamespace}},
			},
			expectedImage: "rocky-8-v20241106-gdch",
		},
		{
			name:        "no images",
			images:      []vmv1.VirtualMachineImage{},
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = vmv1.AddToScheme(scheme)

			objs := []runtime.Object{}
			for _, img := range tc.images {
				// Need to create a copy to avoid pointer sharing issues in loop
				imgCopy := img
				objs = append(objs, &imgCopy)
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
			m := &VMManager{client: client}

			imageName, _, err := m.getGDCHMachineImage(context.Background())

			if tc.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if imageName != tc.expectedImage {
				t.Errorf("expected image %s, got %s", tc.expectedImage, imageName)
			}
		})
	}
}

func TestFindMostSuitableMachineType(t *testing.T) {
	tests := []struct {
		name         string
		types        []vmv1.VirtualMachineType
		expectedType string
		expectError  bool
	}{
		{
			name: "prefer n2 over n3 and n4",
			types: []vmv1.VirtualMachineType{
				{ObjectMeta: metav1.ObjectMeta{Name: "n2-standard-2-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 2}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n3-standard-2-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 2}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n4-highmem-4-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 4}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
			},
			expectedType: "n2-standard-2-gdc",
		},
		{
			name: "prefer n3 over n4 when n2 not supported",
			types: []vmv1.VirtualMachineType{
				{ObjectMeta: metav1.ObjectMeta{Name: "n2-standard-2-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 2}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(false)}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n3-standard-4-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 4}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n4-highmem-4-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 4}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
			},
			expectedType: "n3-standard-4-gdc",
		},
		{
			name: "pick smallest in series",
			types: []vmv1.VirtualMachineType{
				{ObjectMeta: metav1.ObjectMeta{Name: "n3-standard-4-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 4}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
				{ObjectMeta: metav1.ObjectMeta{Name: "n3-standard-2-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 2}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
			},
			expectedType: "n3-standard-2-gdc",
		},
		{
			name: "fallback to smallest supported if no n2, n3, n4",
			types: []vmv1.VirtualMachineType{
				{ObjectMeta: metav1.ObjectMeta{Name: "a3-highgpu-1g-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 8}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
				{ObjectMeta: metav1.ObjectMeta{Name: "a3-highgpu-2g-gdc", Namespace: defaultImageNamespace}, Spec: vmv1.VirtualMachineTypeSpec{VCPUs: 4}, Status: vmv1.VirtualMachineTypeStatus{Supported: ptr.To(true)}},
			},
			expectedType: "a3-highgpu-2g-gdc",
		},
		{
			name:        "no supported types",
			types:       []vmv1.VirtualMachineType{},
			expectError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			_ = vmv1.AddToScheme(scheme)

			objs := []runtime.Object{}
			for _, typ := range tc.types {
				typCopy := typ
				objs = append(objs, &typCopy)
			}

			client := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
			m := &VMManager{client: client}

			machineType, err := m.findMostSuitableMachineType(context.Background())

			if tc.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if machineType != tc.expectedType {
				t.Errorf("expected machine type %s, got %s", tc.expectedType, machineType)
			}
		})
	}
}
