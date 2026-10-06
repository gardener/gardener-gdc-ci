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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gdchnetworkingv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/networking/v1"
	vmv1 "github.com/googlecloudplatform/google-distributed-cloud-apis/pkg/apis/public/virtualmachine/v1"

	"github.com/gardener/gardener-gdc-ci/integration/pkg/diagnostics"
	"github.com/gardener/gardener-gdc-ci/integration/pkg/pnp"
)

const (
	defaultDiskSize         = "15Gi"
	defaultImageNamespace   = "vm-system"
	defaultNetworkName      = "default"
	TestVMLabelKey          = "test-vm"
	ingressPNPPrefix        = "allow-ingress-"
	testSSHEmail            = "useraccess@test.com"
	defaultSSHTimeout       = 15 * time.Minute
	defaultDiskReadyTimeout = 15 * time.Minute
	machineTypeSeriesN2     = "n2-"
	machineTypeSeriesN3     = "n3-"
	machineTypeSeriesN4     = "n4-"
)

// VMManager handles operations on virtual machines.
type VMManager struct {
	client     client.Client
	name       string
	namespace  string
	sshKeyPath string
	ingressIP  string
	vm         *vmv1.VirtualMachine
	vmea       *vmv1.VirtualMachineExternalAccess
	vmar       *vmv1.VirtualMachineAccessRequest
	pnp        *gdchnetworkingv1.ProjectNetworkPolicy
	disk       *vmv1.VirtualMachineDisk
}

// NewManager creates a new VMManager.
func NewManager(client client.Client, key client.ObjectKey) *VMManager {
	return &VMManager{
		client:    client,
		name:      key.Name,
		namespace: key.Namespace,
	}
}

// Create creates a VM and all associated resources for testing.
func (m *VMManager) Create(ctx context.Context) error {
	keyPath, err := generateSSHKey(m.name)
	if err != nil {
		return fmt.Errorf("failed to generate SSH key for VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.sshKeyPath = keyPath

	pubKey, err := readPublicKey(keyPath)
	if err != nil {
		return fmt.Errorf("failed to read public key for VM %s/%s: %w", m.namespace, m.name, err)
	}

	imageName, imageNamespace, err := m.getGDCHMachineImage(ctx)
	if err != nil {
		return fmt.Errorf("failed to auto-discover machine image: %w", err)
	}

	machineType, err := m.findMostSuitableMachineType(ctx)
	if err != nil {
		return fmt.Errorf("failed to auto-discover machine type: %w", err)
	}

	key := client.ObjectKey{Name: m.name, Namespace: m.namespace}

	disk, err := m.createVirtualMachineDisk(ctx, defaultDiskSize, imageName, imageNamespace)
	if err != nil {
		return fmt.Errorf("failed to create disk for VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.disk = disk

	_, err = m.waitForVirtualMachineDiskReady(ctx, defaultDiskReadyTimeout)
	if err != nil {
		return fmt.Errorf("failed to wait for disk for VM %s/%s to be ready: %w", m.namespace, m.name, err)
	}

	vm, err := m.createVirtualMachine(ctx, machineType, m.name, defaultNetworkName, []vmv1.StartupScript{})
	if err != nil {
		return fmt.Errorf("failed to create VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.vm = vm

	_, err = m.waitForVirtualMachineReady(ctx, 10*time.Minute)
	if err != nil {
		return fmt.Errorf("failed to wait for VM %s/%s to be ready: %w", m.namespace, m.name, err)
	}

	vmea, err := m.createVirtualMachineExternalAccess(ctx, []vmv1.ServicePort{
		{
			Name:     "ssh",
			Port:     22,
			Protocol: corev1.ProtocolTCP,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create VMEA for VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.vmea = vmea

	err = wait.PollUntilContextTimeout(ctx, 5*time.Second, 5*time.Minute, true, func(ctx context.Context) (bool, error) {
		err := m.client.Get(ctx, key, vmea)
		if err != nil {
			return false, nil
		}
		if vmea.Status.IngressIP != "" {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		vmeaObj := &vmv1.VirtualMachineExternalAccess{}
		if getErr := m.client.Get(ctx, key, vmeaObj); getErr == nil {
			return fmt.Errorf("VirtualMachineExternalAccess failed to get IngressIP for VM %s/%s (timeout): status=%+v, err=%w", m.namespace, m.name, vmeaObj.Status, err)
		}
		return fmt.Errorf("VirtualMachineExternalAccess failed to get IngressIP for VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.ingressIP = vmea.Status.IngressIP

	pnpObj, err := pnp.CreateIngressZonalProjectNetworkPolicy(ctx, m.client, client.ObjectKey{Name: ingressPNPPrefix + m.name, Namespace: m.namespace}, map[string]string{TestVMLabelKey: m.name})
	if err != nil {
		return fmt.Errorf("failed to create Ingress PNP for VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.pnp = pnpObj

	vmarObj, err := m.createVirtualMachineAccessRequest(ctx, m.name, defaultUser, strings.TrimSpace(pubKey)+" "+testSSHEmail, "24h")
	if err != nil {
		return fmt.Errorf("failed to create VMAR for VM %s/%s: %w", m.namespace, m.name, err)
	}
	m.vmar = vmarObj

	err = waitForSSH(ctx, m.ingressIP, m.sshKeyPath, defaultSSHTimeout)
	if err != nil {
		vmYAML := diagnostics.GetObjectYAML(ctx, m.client, key, &vmv1.VirtualMachine{})
		vmeaYAML := diagnostics.GetObjectYAML(ctx, m.client, key, &vmv1.VirtualMachineExternalAccess{})
		return fmt.Errorf("failed to wait for SSH for VM %s/%s (timeout):\n--- VM YAML ---\n%s\n--- VMEA YAML ---\n%s\nerror: %w", m.namespace, m.name, vmYAML, vmeaYAML, err)
	}

	return nil
}

// getGDCHMachineImage returns a supported GDCH machine image.
// It prefers Ubuntu images and picks the latest one based on name sorting.
// Falls back to the first available image if no Ubuntu image is found.
func (m *VMManager) getGDCHMachineImage(ctx context.Context) (string, string, error) {
	var vmImageList vmv1.VirtualMachineImageList
	if err := m.client.List(ctx, &vmImageList, client.InNamespace(defaultImageNamespace)); err != nil {
		return "", "", fmt.Errorf("failed to list VirtualMachineImages: %w", err)
	}

	if len(vmImageList.Items) == 0 {
		return "", "", fmt.Errorf("no virtual machine images found")
	}

	var ubuntuImages []vmv1.VirtualMachineImage
	for _, img := range vmImageList.Items {
		if strings.Contains(strings.ToLower(img.Name), "ubuntu") {
			ubuntuImages = append(ubuntuImages, img)
		}
	}

	if len(ubuntuImages) > 0 {
		// Sort descending alphabetically to get the latest version and date
		sort.Slice(ubuntuImages, func(i, j int) bool {
			return ubuntuImages[i].Name > ubuntuImages[j].Name
		})
		fmt.Printf("Selected Ubuntu image: %s/%s\n", ubuntuImages[0].Namespace, ubuntuImages[0].Name)
		return ubuntuImages[0].Name, ubuntuImages[0].Namespace, nil
	}

	fmt.Printf("Selected fallback image: %s/%s\n", vmImageList.Items[0].Namespace, vmImageList.Items[0].Name)
	return vmImageList.Items[0].Name, vmImageList.Items[0].Namespace, nil
}

// findMostSuitableMachineType searches for the machine type that satisfies certain criteria.
// It prefers n2, then n3, then n4 series, and picks the one with the lowest amount of CPUs.
// Falls back to the machine with the lowest amount of CPUs among all supported machines if none of the preferred series are found.
func (m *VMManager) findMostSuitableMachineType(ctx context.Context) (string, error) {
	var vmTypeList vmv1.VirtualMachineTypeList
	if err := m.client.List(ctx, &vmTypeList, client.InNamespace(defaultImageNamespace)); err != nil {
		return "", fmt.Errorf("failed to list VirtualMachineTypes: %w", err)
	}

	var supportedTypes []vmv1.VirtualMachineType
	for _, vmType := range vmTypeList.Items {
		if vmType.Status.Supported != nil && !*vmType.Status.Supported {
			continue
		}
		supportedTypes = append(supportedTypes, vmType)
	}

	if len(supportedTypes) == 0 {
		return "", fmt.Errorf("no suitable supported machine type found")
	}

	// Try preferred series in order
	for _, prefix := range []string{machineTypeSeriesN2, machineTypeSeriesN3, machineTypeSeriesN4} {
		if name, minCpu := findSmallestMachineTypeByPrefix(supportedTypes, prefix); minCpu != nil {
			return name, nil
		}
	}

	// Fallback to smallest among all supported
	var minCpu *uint32
	var machineName string
	for _, vmType := range supportedTypes {
		if minCpu == nil || vmType.Spec.VCPUs < *minCpu {
			minCpu = ptr.To(vmType.Spec.VCPUs)
			machineName = vmType.Name
		}
	}

	return machineName, nil
}

// findSmallestMachineTypeByPrefix finds the machine type with the lowest CPU count that starts with the given prefix.
func findSmallestMachineTypeByPrefix(types []vmv1.VirtualMachineType, prefix string) (string, *uint32) {
	var minCpu *uint32
	var name string
	for _, t := range types {
		if strings.HasPrefix(t.Name, prefix) {
			if minCpu == nil || t.Spec.VCPUs < *minCpu {
				minCpu = ptr.To(t.Spec.VCPUs)
				name = t.Name
			}
		}
	}
	return name, minCpu
}

// createVirtualMachineDisk creates a VirtualMachineDisk.
func (m *VMManager) createVirtualMachineDisk(ctx context.Context, size, imageName, imageNamespace string) (*vmv1.VirtualMachineDisk, error) {
	disk := &vmv1.VirtualMachineDisk{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.name,
			Namespace: m.namespace,
		},
		Spec: vmv1.VirtualMachineDiskSpec{
			Size: resource.MustParse(size),
			Source: &vmv1.DiskSource{
				Image: &vmv1.ImageDiskSource{
					Name:      imageName,
					Namespace: imageNamespace,
				},
			},
		},
	}

	if err := m.client.Create(ctx, disk); err != nil {
		return nil, fmt.Errorf("failed to create VirtualMachineDisk: %w", err)
	}
	return disk, nil
}

// waitForVirtualMachineDiskReady waits for the disk to be in Ready condition.
func (m *VMManager) waitForVirtualMachineDiskReady(ctx context.Context, timeout time.Duration) (*vmv1.VirtualMachineDisk, error) {
	disk := &vmv1.VirtualMachineDisk{}
	key := client.ObjectKey{Name: m.name, Namespace: m.namespace}
	err := wait.PollUntilContextTimeout(ctx, 15*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		err := m.client.Get(ctx, key, disk)
		if err != nil {
			return false, nil
		}
		// Check the "Ready" condition instead of the Succeeded phase status (e.g., DiskPhaseSucceeded)
		// because the phase status sync in GDC can experience cosmetic lag even when the underlying
		// storage is fully ready and provisioned.
		for _, cond := range disk.Status.Conditions {
			if string(cond.Type) == "Ready" && string(cond.Status) == "True" {
				return true, nil
			}
		}
		if disk.Status.Phase == vmv1.DiskPhaseFailed {
			return false, fmt.Errorf("disk provisioning failed")
		}
		return false, nil
	})
	if err != nil {
		diskYAML := diagnostics.GetObjectYAML(ctx, m.client, key, &vmv1.VirtualMachineDisk{})
		return nil, fmt.Errorf("failed to wait for VirtualMachineDisk to be ready (timeout):\n%s\nerror: %w", diskYAML, err)
	}
	return disk, nil
}

// createVirtualMachine creates a VirtualMachine.
func (m *VMManager) createVirtualMachine(ctx context.Context, machineType, diskName, networkName string, startupScripts []vmv1.StartupScript) (*vmv1.VirtualMachine, error) {
	vm := &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.name,
			Namespace: m.namespace,
			Labels: map[string]string{
				TestVMLabelKey: m.name,
			},
		},
		Spec: vmv1.VirtualMachineSpec{
			Compute: vmv1.Compute{
				VirtualMachineType: machineType,
			},
			Disks: []vmv1.DiskAttachment{
				{
					VirtualMachineDiskRef: corev1.LocalObjectReference{
						Name: diskName,
					},
					Boot:       ptr.To(true),
					AutoDelete: ptr.To(true),
				},
			},
			Network: &vmv1.NetworkSpec{
				Interfaces: []vmv1.NetworkInterfaceSpec{
					{
						Network: networkName,
					},
				},
			},
			StartupScripts: startupScripts,
		},
	}

	if err := m.client.Create(ctx, vm); err != nil {
		return nil, fmt.Errorf("failed to create VirtualMachine: %w", err)
	}
	return vm, nil
}

// createVirtualMachineExternalAccess creates a VirtualMachineExternalAccess.
func (m *VMManager) createVirtualMachineExternalAccess(ctx context.Context, ports []vmv1.ServicePort) (*vmv1.VirtualMachineExternalAccess, error) {
	vmea := &vmv1.VirtualMachineExternalAccess{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.name,
			Namespace: m.namespace,
		},
		Spec: vmv1.VirtualMachineExternalAccessSpec{
			Enabled: true,
			Ports:   ports,
		},
	}
	if err := m.client.Create(ctx, vmea); err != nil {
		return nil, fmt.Errorf("failed to create VirtualMachineExternalAccess: %w", err)
	}
	return vmea, nil
}

// waitForVirtualMachineReady waits for the VM to be in Running state.
func (m *VMManager) waitForVirtualMachineReady(ctx context.Context, timeout time.Duration) (*vmv1.VirtualMachine, error) {
	vm := &vmv1.VirtualMachine{}
	key := client.ObjectKey{Name: m.name, Namespace: m.namespace}
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		err := m.client.Get(ctx, key, vm)
		if err != nil {
			// We return false, nil (retry) on all GET errors (including NotFound) because GDC resource
			// creation is asynchronous; failing immediately on transient errors would prematurely crash the poll.
			return false, nil
		}
		if vm.Status.State == vmv1.VirtualMachineStateRunning {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		vmYAML := diagnostics.GetObjectYAML(ctx, m.client, key, &vmv1.VirtualMachine{})
		return nil, fmt.Errorf("error waiting for VirtualMachine to be ready (timeout):\n%s\nerror: %w", vmYAML, err)
	}
	return vm, nil
}

// Delete deletes all resources created for a test VM.
func (m *VMManager) Delete(ctx context.Context, timeout time.Duration) error {
	// Cleanup SSH key files safely
	if m.sshKeyPath != "" {
		dir := filepath.Dir(m.sshKeyPath)
		if strings.HasPrefix(dir, os.TempDir()) {
			_ = os.RemoveAll(dir)
		}
	}

	var errs []error

	if m.vmar != nil {
		if err := client.IgnoreNotFound(m.client.Delete(ctx, m.vmar)); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VMAR for VM %s/%s: %w", m.namespace, m.name, err))
		}
	}
	if m.pnp != nil {
		if err := pnp.DeleteZonalProjectNetworkPolicy(ctx, m.client, client.ObjectKey{Name: m.pnp.GetName(), Namespace: m.pnp.GetNamespace()}); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete PNP for VM %s/%s: %w", m.namespace, m.name, err))
		}
	}
	if m.vmea != nil {
		if err := client.IgnoreNotFound(m.client.Delete(ctx, m.vmea)); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VMEA for VM %s/%s: %w", m.namespace, m.name, err))
		}
	}

	// Delete VM and wait
	if m.vm != nil {
		if err := m.deleteVirtualMachine(ctx, timeout); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete VM %s/%s: %w", m.namespace, m.name, err))
		}
	}

	// Delete Disk if it wasn't deleted by VM deletion (e.g. if VM creation failed)
	if m.disk != nil {
		if err := client.IgnoreNotFound(m.client.Delete(ctx, m.disk)); err != nil {
			errs = append(errs, fmt.Errorf("failed to delete Disk for VM %s/%s: %w", m.namespace, m.name, err))
		}
	}

	return errors.Join(errs...)
}

// deleteVirtualMachine deletes a VirtualMachine and waits for it to be gone.
func (m *VMManager) deleteVirtualMachine(ctx context.Context, timeout time.Duration) error {
	vm := &vmv1.VirtualMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.name,
			Namespace: m.namespace,
		},
	}
	if err := m.client.Delete(ctx, vm); err != nil {
		return client.IgnoreNotFound(err)
	}

	key := client.ObjectKey{Name: m.name, Namespace: m.namespace}
	err := wait.PollUntilContextTimeout(ctx, 5*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		err := m.client.Get(ctx, key, vm)
		if err != nil {
			if client.IgnoreNotFound(err) == nil {
				return true, nil
			}
			return false, err
		}
		return false, nil
	})
	if err != nil {
		vmYAML := diagnostics.GetObjectYAML(ctx, m.client, key, &vmv1.VirtualMachine{})
		return fmt.Errorf("failed to delete VM %s/%s (timeout):\n%s\nerror: %w", m.namespace, m.name, vmYAML, err)
	}
	return nil
}
